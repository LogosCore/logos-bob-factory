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
// hours runs once. The commands are the build's (description.Commands).

// maxOutPerMessage keeps a message's output well under the 768 KiB limit;
// the rest goes in the next check-ins, which come at once.
const maxOutPerMessage = 512 << 10

// lsChunk is how many directory entries one listing piece carries: a big
// directory goes in pieces (docs: contracts/minion-messages.md, Listings),
// several to a result part while the message has room.
const lsChunk = 500

// streamPoll is how often the minion checks in while a stream that ran is
// held (docs: contracts/minion-messages.md, Streaming a file).
const streamPoll = time.Second

// taskMemory is how long the minion remembers a task id.
const taskMemory = 24 * time.Hour

// task is a task the minion runs: its output not yet sent, and how it ended.
type task struct {
	id      []byte
	cmd     string
	nextSeq uint64
	out     []byte
	// chunks are structured output not yet sent, a result part each,
	// encoded (protocol.ResultData).
	chunks [][]byte
	// pieces are listings not yet sent, with their encoded sizes; listings
	// the task has started, numbering the next.
	pieces   []protocol.ListingPiece
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
	commands []string
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
}

func newRunner(d description, sched *schedule) *runner {
	return &runner{greeting: d.Greeting, commands: d.Commands, sched: sched, seen: map[string]time.Time{}, wake: make(chan struct{}, 1)}
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
	if len(r.inflight) > 0 || len(r.acks) > 0 || len(r.errs) > 0 || r.sleepUpd != nil {
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
// acks, errs, a sleep_update and output, and a get_tasks when the minion is
// free.
func (r *runner) outgoing() []any {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.inflight) > 0 {
		return r.inflight
	}
	var items []any
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
	// Parts while the message has room: output, then a chunk of data each,
	// then listing pieces, as many to a part as fit.
	room := maxOutPerMessage
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
				if size > room && room < maxOutPerMessage {
					break parts // the next message, which comes at once
				}
				part.Data, t.chunks, room = t.chunks[0], t.chunks[1:], max(room-size, 0)
			} else if len(t.pieces) > 0 {
				for len(t.pieces) > 0 && (t.sizes[0] <= room || (len(part.Ls) == 0 && room == maxOutPerMessage)) {
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
	if !r.busyLocked() {
		one := uint64(1)
		items = append(items, protocol.GetTasksItem{T: protocol.ItemGetTasks, Max: &one})
	}
	return items
}

func (r *runner) busyLocked() bool {
	for _, t := range r.tasks {
		if !t.sentEnd {
			return true
		}
	}
	return false
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
					r.refused(it.Xfer, e.Code+": "+e.Msg)
				case protocol.AckItem:
					if it.Xfer != nil {
						r.refused(it.Xfer, e.Code+": "+e.Msg)
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

// pollEvery is how soon to check in while a stream that ran is held: its
// downloader may come back, or catch up, any moment. Zero: the schedule.
func (r *runner) pollEvery() time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, t := range r.tasks {
		if x := t.xfer; x != nil && x.stream && x.started && x.held && !x.over {
			return streamPoll
		}
		// A stream core sends, held once it began.
		if rx := t.recv; rx != nil && rx.held && rx.have > 0 && !rx.over {
			return streamPoll
		}
	}
	return 0
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
	r.errs = append(r.errs, protocol.ErrItem{T: protocol.ItemErr, Code: protocol.ErrItemUnknownTask, Msg: "no such transfer", Item: &item})
}

func (r *runner) queueErr(code, msg string, item int) {
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
	if ti.Cmd == "exit" {
		r.exitID = ti.Task
	}
	r.mu.Unlock()
	log.Printf("task %s: %s %v", key[:8], ti.Cmd, ti.Args)
	go func() {
		defer cancel()
		r.run(ctx, t, ti)
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
	r.errs = append(r.errs, protocol.ErrItem{T: protocol.ItemErr, Code: protocol.ErrItemUnknownTask, Msg: "no such task running", Item: &item})
}

// write adds output to a task (text, or a chunk of structured output: a
// map, or a struct such as protocol.DirListing), and asks for an early
// check-in.
func (r *runner) write(t *task, out []byte, data any) {
	var chunk []byte
	if data != nil {
		b, err := protocol.ResultData(data)
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

// list adds a directory's listing to a task's result, in pieces of lsChunk
// entries: the first names the directory, the last says so. err: it could
// not be read at all; partial: not all of it.
func (r *runner) list(t *task, path, sep, parent string, entries []protocol.DirEntry, partial bool, errText string) {
	r.mu.Lock()
	id := t.listings
	t.listings++
	for i, part := 0, uint64(0); i == 0 || i < len(entries); i, part = i+lsChunk, part+1 {
		p := protocol.ListingPiece{ID: id, Part: part, Entries: entries[i:min(i+lsChunk, len(entries))]}
		if part == 0 {
			p.Path, p.Sep, p.Parent, p.Err = path, sep, parent, errText
		}
		p.Last = i+lsChunk >= len(entries)
		p.Partial = p.Last && partial
		size := 64
		if b, err := protocol.ResultData(p); err == nil {
			size = len(b)
		}
		t.pieces, t.sizes = append(t.pieces, p), append(t.sizes, size)
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
	log.Printf("task %s: %s %s", hex.EncodeToString(t.id)[:8], t.cmd, state)
}

// run carries out a task's command.
func (r *runner) run(ctx context.Context, t *task, ti protocol.TaskItem) {
	if !slices.Contains(r.commands, ti.Cmd) {
		r.end(t, false, nil, fmt.Sprintf("this minion was not built with %q", ti.Cmd))
		return
	}
	switch ti.Cmd {
	case "hello":
		r.write(t, []byte(r.greeting+"\n"), nil)
		r.end(t, true, nil, "")
	case "ls":
		r.ls(t, ti.Args)
	case "download":
		r.download(ctx, t, ti.Args)
	case "stream_download":
		r.sendFile(ctx, t, ti.Args, true)
	case "upload":
		r.upload(ctx, t, ti.Args, "file")
	case "stream_upload":
		r.upload(ctx, t, ti.Args, "stream")
	case "rm":
		r.rm(t, ti.Args)
	case "shell":
		r.shell(ctx, t, ti.Args)
	case "sleep":
		r.sleep(t, ti.Args)
	case "exit":
		r.write(t, []byte("bye\n"), nil)
		r.end(t, true, nil, "")
	default:
		r.end(t, false, nil, fmt.Sprintf("%q is not a command this minion knows", ti.Cmd))
	}
}

// ls lists a directory: a listing (docs: contracts/minion-messages.md,
// Listings), from which core makes the task's dir_listing. A directory it
// cannot read is a listing that says so, and the task fails.
func (r *runner) ls(t *task, args map[string]any) {
	path, _ := protocol.ArgString(args, "path")
	if path == "" {
		path = "."
	}
	abs, err := filepath.Abs(path)
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
			r.list(t, abs, sep, parent, nil, false, err.Error())
		}
		r.end(t, false, nil, err.Error())
		return
	}
	entries := make([]protocol.DirEntry, 0, len(dir))
	for _, e := range dir {
		entries = append(entries, dirEntry(abs, e))
	}
	// Entries read before an error: what there is, said partial.
	r.list(t, abs, sep, parent, entries, err != nil, "")
	r.end(t, true, nil, "")
}

// dirEntry is a directory's entry as a dir_listing has it: a link as the
// link (its own size and time), with its target.
func dirEntry(dir string, e os.DirEntry) protocol.DirEntry {
	d := protocol.DirEntry{Name: e.Name(), Type: entryType(e.Type()), Hidden: strings.HasPrefix(e.Name(), ".")}
	if info, err := e.Info(); err == nil {
		if !info.IsDir() {
			size := uint64(max(info.Size(), 0))
			d.Size = &size
		}
		mod := info.ModTime().Unix()
		d.Modified = &mod
		d.Mode = info.Mode().String()
		d.Owner, d.Group = owners(info)
	}
	if d.Type == protocol.DirEntryLink {
		if target, err := os.Readlink(filepath.Join(dir, e.Name())); err == nil {
			d.Target = target
		}
	}
	return d
}

func entryType(m fs.FileMode) string {
	switch {
	case m.IsDir():
		return protocol.DirEntryDir
	case m&fs.ModeSymlink != 0:
		return protocol.DirEntryLink
	case m.IsRegular():
		return protocol.DirEntryFile
	}
	return protocol.DirEntryOther
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

// shell runs a command in the system shell, its output streamed, within its
// timeout.
func (r *runner) shell(ctx context.Context, t *task, args map[string]any) {
	command, _ := protocol.ArgString(args, "command")
	timeout, ok := protocol.ArgDuration(args, "timeout")
	if !ok || timeout <= 0 {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", command)
	// Its own process group, all of it killed on a cancel or the timeout: the
	// shell's children (a sleep) would otherwise live on holding the output
	// open, and the task would never end. WaitDelay ends it even if one that
	// left the group still holds it.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
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
func (r *runner) sleep(t *task, args map[string]any) {
	interval, ok := protocol.ArgDuration(args, "interval")
	if !ok || interval <= 0 {
		r.end(t, false, nil, "an interval is required")
		return
	}
	jitter, ok := protocol.ArgInt(args, "jitter")
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
