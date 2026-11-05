package main

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/logoscore/logos-golang-protocol/protocol"
)

// Tasks (docs: contracts/minion-messages.md): the minion asks for one task
// at a time, acks it, runs it in the background and sends its output as
// result parts: formed at each check-in from what the task wrote since, so a
// message that fails is sent again as it was ((task, seq) stays the same).
// It checks in sooner when it has output. A task id it saw in the last 24
// hours runs once. The commands are the build's (description.Ops).

// maxOutPerMessage keeps a message's output well under the 768 KiB limit;
// the rest goes in the next check-ins, which come at once. A connection
// that carries less (its channel's max_message, BuildConnection.MaxMessage)
// gets two thirds of what it carries (perMessage).
const maxOutPerMessage = 512 << 10

// lsChunk is how many directory entries one listing piece carries at most:
// a big directory goes in pieces (docs: contracts/minion-messages.md,
// Listings), several to a result part while the message has room, and a
// piece is cut sooner when its entries would not fit half a message.
const lsChunk = 500

// taskMemory is how long the minion remembers a task id.
const taskMemory = 24 * time.Hour

// task is a task the minion runs: its output not yet sent, and how it ended.
type task struct {
	id      []byte
	cmd     uint64
	nextSeq uint64
	out     []byte
	// chunks are structured output not yet sent, a result part each,
	// encoded (protocol.ResultData).
	chunks [][]byte
	// pieces are listings not yet sent, with their encoded sizes; listings
	// the task has started, numbering the next.
	pieces   []protocol.LsPiece
	sizes    []int
	listings uint64
	ended    bool
	ok       bool
	exit     *int64
	err      string
	sentEnd  bool
	cancel   context.CancelFunc
	// xfer is the file a download sends (download.go); nil for others.
	xfer *transfer
	// recv is the file an upload receives (upload.go); nil for others.
	recv *receiving
}

// runner holds the tasks of a session and what the next message says.
type runner struct {
	mu       sync.Mutex
	greeting string
	ops      []uint64
	sched    *schedule
	seen     map[string]time.Time
	acks     [][]byte
	errs     []protocol.ErrItem
	sleepUpd *protocol.SleepUpdateItem
	tasks    []*task
	// inflight is the last message's items when it failed: sent again first.
	inflight []any
	// exitID: the exit task, whose end delivered stops the minion.
	exitID []byte
	wake   chan struct{}
	// switches are connect tasks asking the session to move (conns.go).
	switches chan switchReq
	// limit is the largest message the connection in use carries
	// (connection.MessageLimit), set by the session loop: what the next
	// message keeps within.
	limit int
	// poll is how soon core asked the minion to come back (a poll item:
	// a stream of its may go on any moment); 0 for none. It holds until
	// the next message goes.
	poll time.Duration
	// helloAsked: core asked for a hello (rehello); hello makes it.
	helloAsked bool
	hello      func() protocol.HelloItem
	// maxTasks is how many tasks it runs at once: the build's, then core's
	// (a max_tasks); maxTasksSay: core's came, and the next message says
	// the minion has it. persistent: the connection in use stays open, so
	// a get_tasks is a standing allowance there, not this message's.
	maxTasks    uint64
	maxTasksSay bool
	persistent  bool
	// exec carries out a task's command: run, but in tests.
	exec func(ctx context.Context, t *task, ti protocol.TaskItem)
}

// setLimit says the largest message the connection in use carries.
func (r *runner) setLimit(n int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.limit = n
}

// perMessageLocked is the room a message gives output, data, listings and
// a chunk: two thirds of what the connection carries, maxOutPerMessage at
// most, leaving the rest to acks, errs and the message's own keys.
func (r *runner) perMessageLocked() int {
	if r.limit <= 0 {
		return maxOutPerMessage
	}
	return min(maxOutPerMessage, r.limit*2/3)
}

func newRunner(d description, sched *schedule) *runner {
	return &runner{greeting: d.Greeting, ops: d.Ops, sched: sched, seen: map[string]time.Time{}, wake: make(chan struct{}, 1),
		switches: make(chan switchReq, 1), maxTasks: d.maxTasks()}
}

// setPersistent says whether the connection in use stays open (a WebSocket,
// a TCP connection).
func (r *runner) setPersistent(p bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.persistent = p
}

// maxTasksNow is how many tasks the minion runs at once now.
func (r *runner) maxTasksNow() uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.maxTasks
}

// nudge asks for an early check-in: there is output to send.
func (r *runner) nudge() {
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

// pending: something waits to be sent.
func (r *runner) pending() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.inflight) > 0 || len(r.acks) > 0 || len(r.errs) > 0 || r.sleepUpd != nil || r.helloAsked || r.maxTasksSay {
		return true
	}
	if feedback.pending() {
		return true
	}
	for _, t := range r.tasks {
		if len(t.out) > 0 || len(t.chunks) > 0 || len(t.pieces) > 0 || (t.ended && !t.sentEnd) || t.xfer.sending() || t.recv.asking() || t.recv.telling() {
			return true
		}
	}
	return false
}

// outgoing is the next message's items: the failed message's again, or
// acks, errs, a sleep_update, a max_tasks (core set the limit), output, and
// a get_tasks while the limit leaves room.
func (r *runner) outgoing() []any {
	r.mu.Lock()
	defer r.mu.Unlock()
	// The wait core asked for was for this message.
	r.poll = 0
	if len(r.inflight) > 0 {
		return r.inflight
	}
	var items []any
	if r.helloAsked && r.hello != nil {
		// With the limit it has now (core's, if core set one): said here,
		// under the lock the closure must not take.
		h := r.hello()
		n := r.maxTasks
		h.MaxTasks = &n
		items = append(items, h)
	}
	r.helloAsked = false
	if len(r.acks) > 0 {
		items = append(items, protocol.AckItem{T: protocol.ItemAck, Tasks: r.acks})
		r.acks = nil
	}
	for _, e := range r.errs {
		items = append(items, e)
	}
	r.errs = nil
	if r.sleepUpd != nil {
		items = append(items, *r.sleepUpd)
		r.sleepUpd = nil
	}
	// Feedback first, in the room a message has (most of it, to leave the
	// tasks theirs), then parts while the message has room: output, then a
	// chunk of data each, then listing pieces, as many to a part as fit.
	full := r.perMessageLocked()
	room := full
	if fb, left := feedback.take(full/2, true); len(fb) > 0 {
		for _, f := range fb {
			items = append(items, f)
		}
		room = max(full-(full/2-left), 0)
	}
parts:
	for _, t := range r.tasks {
		for !t.sentEnd && (len(t.out) > 0 || len(t.chunks) > 0 || len(t.pieces) > 0 || t.ended) {
			part := protocol.ResultItem{T: protocol.ItemResult, Task: t.id, Seq: t.nextSeq}
			if len(t.out) > 0 {
				n := min(len(t.out), room)
				if n == 0 {
					break parts
				}
				part.Out, t.out, room = t.out[:n], t.out[n:], room-n
			} else if len(t.chunks) > 0 {
				size := len(t.chunks[0])
				if size > room && room < full {
					break parts // the next message, which comes at once
				}
				part.Data, t.chunks, room = t.chunks[0], t.chunks[1:], max(room-size, 0)
			} else if len(t.pieces) > 0 {
				for len(t.pieces) > 0 && (t.sizes[0] <= room || (len(part.Ls) == 0 && room == full)) {
					part.Ls = append(part.Ls, t.pieces[0])
					room = max(room-t.sizes[0], 0)
					t.pieces, t.sizes = t.pieces[1:], t.sizes[1:]
				}
				if len(part.Ls) == 0 {
					break parts // the next message, which comes at once
				}
			}
			if t.ended && len(t.out) == 0 && len(t.chunks) == 0 && len(t.pieces) == 0 {
				ok := t.ok
				part.Done, part.OK, part.Exit, part.Err = true, &ok, t.exit, t.err
				t.sentEnd = true
			}
			t.nextSeq++
			items = append(items, part)
			if room == 0 {
				break parts
			}
		}
	}
	// A file's next chunk, one per message, in the room left.
	for _, t := range r.tasks {
		if !t.xfer.sending() || room == 0 {
			continue
		}
		c, err := t.xfer.chunk(t, room)
		if err != nil {
			t.xfer.finish(err.Error())
			continue
		}
		items = append(items, c)
		room -= len(c.Data)
	}
	// A file core sends: the ask for its next chunk, which core's answer
	// brings.
	for _, t := range r.tasks {
		if t.recv.asking() || t.recv.telling() {
			items = append(items, t.recv.ask())
		}
	}
	// The limit core set, said back: core sends it until the minion has it.
	if r.maxTasksSay {
		items = append(items, protocol.MaxTasksItem{T: protocol.ItemMaxTasks, Max: r.maxTasks})
		r.maxTasksSay = false
	}
	// Tasks, as many as the limit leaves room for. On a persistent
	// connection the limit itself: a standing allowance, which core counts
	// the tasks under way against. Through a call, this message's: the
	// room left (none asked when there is none).
	if r.persistent {
		max := r.maxTasks
		items = append(items, protocol.GetTasksItem{T: protocol.ItemGetTasks, Max: &max})
	} else if room := r.roomLocked(); room > 0 {
		items = append(items, protocol.GetTasksItem{T: protocol.ItemGetTasks, Max: &room})
	}
	return items
}

// roomLocked is how many more tasks the limit leaves room for: the limit
// less the tasks whose end has not gone yet.
func (r *runner) roomLocked() uint64 {
	running := uint64(0)
	for _, t := range r.tasks {
		if !t.sentEnd {
			running++
		}
	}
	if running >= r.maxTasks {
		return 0
	}
	return r.maxTasks - running
}

// delivered: the message went through. Tasks whose end it carried are done
// with; the exit task's end stops the minion.
func (r *runner) delivered(items []any) (exit bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.inflight = nil
	r.tasks = slices.DeleteFunc(r.tasks, func(t *task) bool { return t.sentEnd })
	// A chunk delivered: the next goes from where core's ack says, taken
	// with the answer; none said, this one again.
	for _, t := range r.tasks {
		if t.xfer != nil {
			t.xfer.waiting = false
		}
	}
	// An ask answered by no chunk (core had no room in its reply) is asked
	// again; take, after this, clears it too when a chunk came.
	for _, t := range r.tasks {
		if t.recv != nil {
			t.recv.asked = false
		}
	}
	for _, it := range items {
		if res, ok := it.(protocol.ResultItem); ok && res.Done && r.exitID != nil && slices.Equal(res.Task, r.exitID) {
			return true
		}
	}
	return false
}

// failed: the message did not go through; it is sent again as it was.
func (r *runner) failed(items []any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.inflight = items
}

// take reads core's items: tasks to run (each once), cancels, and how far
// core has a file being sent, or why it does not take it (an err naming one
// of the sent items, a chunk).
func (r *runner) take(answer protocol.CoreMessage, sent []any) {
	for i, raw := range answer.Items {
		switch protocol.ItemType(raw) {
		case protocol.ItemAck:
			var a protocol.AckItem
			if protocol.UnmarshalCBOR(raw, &a) == nil && a.Validate() == nil && a.Xfer != nil {
				r.acked(a.Xfer, *a.Next, a.Hold)
			}
		case protocol.ItemErr:
			var e protocol.ErrItem
			if protocol.UnmarshalCBOR(raw, &e) == nil && e.Item != nil && *e.Item >= 0 && *e.Item < len(sent) {
				switch it := sent[*e.Item].(type) {
				case protocol.ChunkItem:
					r.refused(it.Xfer, e.Msg)
				case protocol.AckItem:
					if it.Xfer != nil {
						r.refused(it.Xfer, e.Msg)
					}
				}
			}
		case protocol.ItemChunk:
			var c protocol.ChunkItem
			if protocol.UnmarshalCBOR(raw, &c) != nil || c.Validate() != nil {
				r.queueErr(protocol.ErrItemBadMessage, "the chunk does not read", i)
				continue
			}
			r.chunk(c, i)
		case protocol.ItemTask:
			var t protocol.TaskItem
			if protocol.UnmarshalCBOR(raw, &t) != nil || t.Validate() != nil {
				r.queueErr(protocol.ErrItemBadMessage, "the task does not read", i)
				continue
			}
			r.start(t)
		case protocol.ItemCancel:
			var c protocol.CancelItem
			if protocol.UnmarshalCBOR(raw, &c) != nil || c.Validate() != nil {
				r.queueErr(protocol.ErrItemBadMessage, "the cancel does not read", i)
				continue
			}
			r.stop(c.Task, i)
		case protocol.ItemPoll:
			// Come back sooner than the schedule: a stream of the minion's
			// may go on any moment.
			var p protocol.PollItem
			if protocol.UnmarshalCBOR(raw, &p) == nil && p.Validate() == nil {
				r.mu.Lock()
				r.poll = time.Duration(p.Within) * time.Millisecond
				r.mu.Unlock()
			}
		case protocol.ItemMaxTasks:
			// How many tasks to run at once, from now: said back in the next
			// message, whatever it was, so core knows the minion has it.
			var m protocol.MaxTasksItem
			if protocol.UnmarshalCBOR(raw, &m) != nil || m.Validate() != nil {
				r.queueErr(protocol.ErrItemBadMessage, "the max_tasks does not read", i)
				continue
			}
			r.mu.Lock()
			if r.maxTasks != m.Max {
				log.Printf("max tasks %d, as core says (was %d)", m.Max, r.maxTasks)
			}
			r.maxTasks, r.maxTasksSay = m.Max, true
			r.mu.Unlock()
			r.nudge()
		case protocol.ItemRehello:
			// Core asks what host and process this is: a hello, in the
			// next message.
			r.mu.Lock()
			r.helloAsked = true
			r.mu.Unlock()
			r.nudge()
		case protocol.ItemStageResponse, protocol.ItemHelloResponse:
			// Answers of staging and hello (a rehello's): nothing to do.
		case protocol.ItemReack:
			// Core asks which of these tasks the minion still has (it is
			// back over a new connection, or as a new process).
			var rk protocol.ReackItem
			if protocol.UnmarshalCBOR(raw, &rk) != nil || rk.Validate() != nil {
				r.queueErr(protocol.ErrItemBadMessage, "the reack does not read", i)
				continue
			}
			r.reack(rk.Tasks, i)
		case protocol.ItemResume:
			r.queueErr(protocol.ErrItemUnknownItem, "this minion keeps its tasks' state: it takes no resume", i)
		default:
			r.queueErr(protocol.ErrItemUnknownItem, fmt.Sprintf("this minion does not take %s", protocol.ItemName(protocol.ItemType(raw))), i)
		}
	}
}

// acked takes how far core has a file: all of it ends the transfer. For a
// stream it is where to send from (it may go back: a download resumed
// from earlier), and hold says to wait; the whole without hold ends it.
func (r *runner) acked(xfer []byte, next uint64, hold bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, t := range r.tasks {
		// A stream core sends: hold says it has nothing yet.
		if rx := t.recv; rx != nil && slices.Equal(rx.offer.Xfer, xfer) && !rx.over {
			rx.held, rx.asked = hold, false
			continue
		}
		x := t.xfer
		if x == nil || !slices.Equal(x.id, xfer) || x.over {
			continue
		}
		x.acked, x.waiting = min(next, x.total), false
		if x.stream {
			x.announced, x.held = true, hold
			if !hold {
				x.started = true
				if x.acked == x.total {
					x.finish("")
				}
			}
			continue
		}
		if x.acked == x.total && x.hashed == x.total {
			x.finish("")
		}
	}
}

// pollEvery is how soon to check in when core asked for sooner than the
// schedule (a poll: a stream the minion holds may go on any moment, its
// downloader or uploader there). Zero: the schedule. Over a persistent
// connection core pushes instead, and asks for none.
func (r *runner) pollEvery() time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.poll
}

// refused ends a transfer core does not take, or does not send.
func (r *runner) refused(xfer []byte, why string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, t := range r.tasks {
		if x := t.xfer; x != nil && slices.Equal(x.id, xfer) {
			x.finish("core refused the file: " + why)
		}
		if x := t.recv; x != nil && slices.Equal(x.offer.Xfer, xfer) {
			x.finish("core did not send the file: " + why)
		}
	}
}

// chunk takes a chunk of a file core sends; one for no transfer the minion
// has gets an unknown_task.
func (r *runner) chunk(c protocol.ChunkItem, item int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, t := range r.tasks {
		if x := t.recv; x != nil && slices.Equal(x.offer.Xfer, c.Xfer) {
			x.take(c)
			return
		}
	}
	// A first chunk names its task: core ends the task the minion does not
	// have.
	r.errs = append(r.errs, protocol.ErrItem{T: protocol.ItemErr, Code: protocol.ErrItemUnknownTask, Msg: "no such transfer", Item: &item, Task: c.Task})
}

func (r *runner) queueErr(code int, msg string, item int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.errs = append(r.errs, protocol.ErrItem{T: protocol.ItemErr, Code: code, Msg: msg, Item: &item})
}

// start acks a task and runs it, unless it ran already.
func (r *runner) start(ti protocol.TaskItem) {
	key := hex.EncodeToString(ti.Task)
	r.mu.Lock()
	for k, at := range r.seen {
		if time.Since(at) > taskMemory {
			delete(r.seen, k)
		}
	}
	r.acks = append(r.acks, ti.Task)
	if _, ran := r.seen[key]; ran {
		r.mu.Unlock()
		log.Printf("task %s again: acked, not run again", key[:8])
		return
	}
	r.seen[key] = time.Now()
	ctx, cancel := context.WithCancel(context.Background())
	t := &task{id: ti.Task, cmd: ti.Cmd, cancel: cancel}
	r.tasks = append(r.tasks, t)
	if ti.Cmd == opExit {
		r.exitID = ti.Task
	}
	r.mu.Unlock()
	log.Printf("task %s: command %d %v", key[:8], ti.Cmd, ti.Args)
	go func() {
		defer cancel()
		// A command that panics fails its task, and says so to the
		// operators, rather than taking the minion down.
		defer func() {
			if v := recover(); v != nil {
				report(protocol.FeedbackError, "task_panic", t.id, "task %s (command %d) crashed: %v", key[:8], ti.Cmd, v)
				r.end(t, false, nil, fmt.Sprintf("the command crashed: %v", v))
				r.nudge()
			}
		}()
		if r.exec != nil {
			r.exec(ctx, t, ti)
		} else {
			r.run(ctx, t, ti)
		}
		r.nudge()
	}()
}

// stop cancels a running task; one the minion does not have gets an
// unknown_task.
func (r *runner) stop(id []byte, item int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, t := range r.tasks {
		if slices.Equal(t.id, id) {
			if !t.ended {
				log.Printf("task %s: cancelled", hex.EncodeToString(id)[:8])
				t.cancel()
			}
			return
		}
	}
	// Named, so core ends it (one this process never had: it restarted).
	r.errs = append(r.errs, protocol.ErrItem{T: protocol.ItemErr, Code: protocol.ErrItemUnknownTask, Msg: "no such task running", Item: &item, Task: id})
}

// reack answers core's question about tasks (item, a reack): an ack names
// those the minion has (running, or ended with an end core has not had),
// an unknown_task each it does not, which core ends.
func (r *runner) reack(ids [][]byte, item int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, id := range ids {
		if slices.ContainsFunc(r.tasks, func(t *task) bool { return slices.Equal(t.id, id) }) {
			r.acks = append(r.acks, id)
			continue
		}
		r.errs = append(r.errs, protocol.ErrItem{T: protocol.ItemErr, Code: protocol.ErrItemUnknownTask,
			Msg: "no such task: this process does not have it", Item: &item, Task: id})
	}
}

// write adds output to a task (text, or a chunk of structured output: a
// map, or a struct such as protocol.DirListing), and asks for an early
// check-in.
func (r *runner) write(t *task, out []byte, data any) {
	var chunk []byte
	if data != nil {
		b, err := protocol.WireResultData(data)
		if err != nil {
			log.Printf("task %s: data does not encode: %v", hex.EncodeToString(t.id)[:8], err)
		} else {
			chunk = b
		}
	}
	r.mu.Lock()
	t.out = append(t.out, out...)
	if chunk != nil {
		t.chunks = append(t.chunks, chunk)
	}
	r.mu.Unlock()
	r.nudge()
}

// entry is a directory's entry as the minion reads it: as a listing sends
// it, with its owner and group by name, which each piece numbers once.
type entry struct {
	protocol.LsEntry
	owner, group string
}

// list adds a directory's listing to a task's result, in pieces of lsChunk
// entries at most, cut sooner so a piece takes half a message at most: the
// first names the directory, the last says so. errCode: it could not be
// read at all (a protocol.ListingErr); partial: not all of it.
func (r *runner) list(t *task, path, sep, parent string, entries []entry, partial bool, errCode int) {
	r.mu.Lock()
	id := t.listings
	t.listings++
	budget := r.perMessageLocked() / 2
	for i, part := 0, uint64(0); i == 0 || i < len(entries); part++ {
		p := protocol.LsPiece{ID: id, Part: part}
		if part == 0 {
			p.Path, p.Sep, p.Parent, p.Err = path, sep, parent, errCode
		}
		// Room for the piece's own keys, path and owners, then entries
		// while they fit (one at least, so a listing always moves on).
		used := 64 + len(p.Path) + len(p.Parent)
		for i < len(entries) && len(p.Entries) < lsChunk {
			e := entries[i]
			size := 16 + len(e.owner) + len(e.group)
			if b, err := protocol.MarshalCBOR(e.LsEntry); err == nil {
				size += len(b)
			}
			if len(p.Entries) > 0 && used+size > budget {
				break
			}
			e.Owner, e.Group = p.Owner(e.owner), p.Owner(e.group)
			p.Entries = append(p.Entries, e.LsEntry)
			used += size
			i++
		}
		p.Last = i >= len(entries)
		p.Partial = p.Last && partial
		size := 64
		if b, err := protocol.MarshalCBOR(p); err == nil {
			size = len(b)
		}
		t.pieces, t.sizes = append(t.pieces, p), append(t.sizes, size)
		if len(entries) == 0 {
			break
		}
	}
	r.mu.Unlock()
	r.nudge()
}

// end records how a task ended.
func (r *runner) end(t *task, ok bool, exit *int64, errText string) {
	r.mu.Lock()
	t.ended, t.ok, t.exit, t.err = true, ok, exit, errText
	r.mu.Unlock()
	state := "ok"
	if !ok {
		state = "failed: " + errText
	}
	log.Printf("task %s: command %d %s", hex.EncodeToString(t.id)[:8], t.cmd, state)
}

// run carries out a task's command.
func (r *runner) run(ctx context.Context, t *task, ti protocol.TaskItem) {
	if !slices.Contains(r.ops, ti.Cmd) {
		r.end(t, false, nil, fmt.Sprintf("this minion was not built with command %d", ti.Cmd))
		return
	}
	switch ti.Cmd {
	case opHello:
		r.write(t, []byte(r.greeting+"\n"), nil)
		r.end(t, true, nil, "")
	case opLs:
		r.ls(t, ti.Args)
	case opDownload:
		r.download(ctx, t, ti.Args)
	case opStreamDownload:
		r.sendFile(ctx, t, ti.Args, true)
	case opUpload:
		r.upload(ctx, t, ti.Args, false)
	case opStreamUpload:
		r.upload(ctx, t, ti.Args, true)
	case opRm:
		r.rm(t, ti.Args)
	case opWalk:
		r.walk(ctx, t, ti.Args)
	case opShell:
		r.shell(ctx, t, ti.Args)
	case opSleep:
		r.sleep(t, ti.Args)
	case opSwitch:
		r.switchConnection(ctx, t, ti.Args)
	case opVolumes:
		r.volumes(t)
	case opFeedback:
		r.sendFeedback(t, ti.Args)
	case opExit:
		r.exit(t)
	default:
		r.end(t, false, nil, fmt.Sprintf("command %d is not one this minion knows", ti.Cmd))
	}
}

// exitWait is how long exit waits for the tasks it stops to end.
const exitWait = 10 * time.Second

// exit stops the minion for good: the tasks running beside it first, each
// cancelled and ending so (their ends go before exit's: core is told how
// each one went), then its own end, whose delivery stops the minion. A task
// that does not end within exitWait is left: the minion goes all the same.
func (r *runner) exit(t *task) {
	r.mu.Lock()
	var others []*task
	for _, o := range r.tasks {
		if o != t && !o.ended {
			others = append(others, o)
			o.cancel()
		}
	}
	r.mu.Unlock()
	if len(others) > 0 {
		log.Printf("exit: stopping %d task(s) first", len(others))
		deadline := time.Now().Add(exitWait)
		for time.Now().Before(deadline) {
			r.mu.Lock()
			left := slices.ContainsFunc(others, func(o *task) bool { return !o.ended })
			r.mu.Unlock()
			if !left {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	r.write(t, []byte("bye\n"), nil)
	r.end(t, true, nil, "")
}

// ls lists a directory: a listing (docs: contracts/minion-messages.md,
// Listings), from which core makes the task's dir_listing. A directory it
// cannot read is a listing that says so, and the task fails.
func (r *runner) ls(t *task, args protocol.Args) {
	path, _ := protocol.ArgString(args, lsPath)
	if path == "" {
		path = "."
	}
	abs, err := hostPath(path)
	if err != nil {
		r.end(t, false, nil, err.Error())
		return
	}
	parent := filepath.Dir(abs)
	if parent == abs {
		parent = "" // a root
	}
	sep := string(filepath.Separator)
	dir, err := os.ReadDir(abs)
	if err != nil && len(dir) == 0 {
		// Not a directory at all is no listing: only one it may not read.
		if errors.Is(err, fs.ErrPermission) {
			r.list(t, abs, sep, parent, nil, false, protocol.ListingErrDenied)
		}
		r.end(t, false, nil, err.Error())
		return
	}
	entries := make([]entry, 0, len(dir))
	for _, e := range dir {
		entries = append(entries, dirEntry(abs, e))
	}
	// Entries read before an error: what there is, said partial.
	r.list(t, abs, sep, parent, entries, err != nil, 0)
	r.end(t, true, nil, "")
}

// dirEntry is a directory's entry as a listing sends it: a link as the link
// (its own size, time and mode), with its target.
func dirEntry(dir string, e os.DirEntry) entry {
	d := entry{LsEntry: protocol.LsEntry{Name: e.Name(), Type: entryType(e.Type()), Hidden: strings.HasPrefix(e.Name(), ".")}}
	if info, err := e.Info(); err == nil {
		if !info.IsDir() {
			size := uint64(max(info.Size(), 0))
			d.Size = &size
		}
		mod := info.ModTime().Unix()
		d.Modified = &mod
		d.Mode = modeBits(info)
		// Hidden by its name (a dot first) or, on Windows, its attribute.
		d.Hidden = d.Hidden || hiddenFile(info)
		d.owner, d.group = owners(info)
	}
	switch {
	case d.Type == protocol.EntryLink:
		if target, err := os.Readlink(filepath.Join(dir, e.Name())); err == nil {
			d.Target = target
		}
	case e.Type()&fs.ModeIrregular != 0:
		// A Windows junction (C:\Users\All Users, Application Data) is a
		// reparse point Go calls irregular, neither folder nor link: it is a
		// link to a folder, with a target to show. Walks do not enter it either
		// way, as they enter no link (a junction can loop).
		if target, err := os.Readlink(filepath.Join(dir, e.Name())); err == nil {
			d.Type, d.Target = protocol.EntryLink, target
		}
	}
	return d
}

func entryType(m fs.FileMode) int {
	switch {
	case m.IsDir():
		return protocol.EntryDir
	case m&fs.ModeSymlink != 0:
		return protocol.EntryLink
	case m.IsRegular():
		return protocol.EntryFile
	}
	return protocol.EntryOther
}

// taskWriter feeds a running process's output to its task as it comes.
type taskWriter struct {
	r *runner
	t *task
}

func (w taskWriter) Write(p []byte) (int, error) {
	w.r.write(w.t, append([]byte(nil), p...), nil)
	return len(p), nil
}

// shell runs a command in the system shell (sh on Unix, cmd.exe on
// Windows), its output streamed, within its timeout.
func (r *runner) shell(ctx context.Context, t *task, args protocol.Args) {
	command, _ := protocol.ArgString(args, shellCommand)
	timeout, ok := protocol.ArgDuration(args, shellTimeout)
	if !ok || timeout <= 0 {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	// The host's shell (shell_unix.go, shell_windows.go), all it starts
	// ended on a cancel or the timeout. WaitDelay ends the task even if a
	// child that got away still holds the output.
	cmd := shellCmd(ctx, command)
	cmd.WaitDelay = 2 * time.Second
	w := taskWriter{r: r, t: t}
	cmd.Stdout, cmd.Stderr = w, w
	if err := cmd.Start(); err != nil {
		r.end(t, false, nil, "could not start: "+err.Error())
		return
	}
	err := cmd.Wait()
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		r.end(t, false, nil, fmt.Sprintf("stopped after its timeout, %s", timeout))
	case errors.Is(ctx.Err(), context.Canceled):
		r.end(t, false, nil, "cancelled")
	default:
		var exitErr *exec.ExitError
		if err != nil && !errors.As(err, &exitErr) {
			r.end(t, false, nil, err.Error())
			return
		}
		state := cmd.ProcessState
		if ws, ok := state.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			// Killed by a signal: no exit code, said in err.
			r.end(t, true, nil, "killed by "+ws.Signal().String())
			return
		}
		code := int64(state.ExitCode())
		r.end(t, true, &code, "")
	}
}

// sleep changes the check-in schedule, and tells core.
func (r *runner) sleep(t *task, args protocol.Args) {
	interval, ok := protocol.ArgDuration(args, sleepInterval)
	if !ok || interval <= 0 {
		r.end(t, false, nil, "an interval is required")
		return
	}
	jitter, ok := protocol.ArgInt(args, sleepJitter)
	if !ok {
		jitter = 10
	}
	r.mu.Lock()
	r.sched.interval, r.sched.jitter = interval, float64(jitter)
	r.sleepUpd = &protocol.SleepUpdateItem{T: protocol.ItemSleepUpdate, Interval: uint64(interval.Milliseconds()), Jitter: uint64(jitter)}
	r.mu.Unlock()
	r.write(t, []byte(fmt.Sprintf("check-ins every %s ± %d%%\n", interval, jitter)), nil)
	r.end(t, true, nil, "")
}
