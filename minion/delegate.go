package main

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/fxamacker/cbor/v2"
	"github.com/logoscore/logos-golang-protocol/protocol"
)

// Delegation (docs: contracts/minion-messages/delegation/): while a task of
// the delegate command runs (opDelegate, action delegate), the minion is an
// instance of the TCP channel's bind-persistent kind for the minions of its
// operation: core sends it the opens of the targets that name its session
// as their delegate, and it dials each as the TCP channel would, then
// carries the frames both ways in delegate items (the channel's framing on
// the wire: tcp.go's readTCPFrame and writeTCPFrame). The minion it dials
// is not changed: a BOB listening for the TCP channel, or any minion that
// speaks its framing. It never reads what it carries.

// delegateServes are what the delegate command serves, in its build
// schema's order: an open's serve names its place. The reverse delegate
// serves one channel kind, the TCP channel's reverse kinds, at place 1
// (delegate_reverse.go).
var delegateServes = []struct{ module, kind string }{{"tcp", protocol.KindBindPersistent}}

// delegateWriteQueue is how many of core's frames wait for a connection's
// writer: past it one is dropped (a message, which its minion's side sends
// again).
const delegateWriteQueue = 64

// delegates are the session's delegate tasks and what goes through them.
type delegates struct {
	mu    sync.Mutex
	nudge func()
	// byTask are the delegate tasks running, by task id.
	byTask map[string]*delegation
	// out are the items for core, in order: answers, closes, frames (cut
	// in pieces as messages have room).
	out []protocol.DelegateItem
	// dial reaches a target: net's, but in tests.
	dial func(ctx context.Context, address string, timeout time.Duration) (net.Conn, error)
}

// delegation is one delegate task's.
type delegation struct {
	task []byte
	// conns are its connections, by core's number.
	conns map[uint64]*delegateConn
	// answered are the answers it gave core's opens, by number: an open
	// sent again gets its answer again; dialing, one under way.
	answered map[uint64]protocol.DelegateItem
	dialing  map[uint64]bool
	join     protocol.DelegateJoiner
	closed   bool
	// A reverse delegate's (delegate_reverse.go): its listener, the number
	// of the next connection or exchange it takes (it numbers them), the
	// exchanges waiting for core's reply, by number.
	ln    net.Listener
	nextN uint64
	syncs map[uint64]chan protocol.DelegateItem
}

// delegateConn is a connection the minion dialed for core.
type delegateConn struct {
	c      net.Conn
	seq    uint64
	writes chan delegateFrame
	// byCore: core closed it, which a close has answered already.
	byCore bool
}

type delegateFrame struct{ id, data []byte }

func newDelegates(nudge func()) *delegates {
	return &delegates{nudge: nudge, byTask: map[string]*delegation{}, dial: dialTCP}
}

// open makes a delegate task's delegation, as the task is taken: core sends
// its opens once it has the task's ack.
func (ds *delegates) open(task []byte) {
	ds.mu.Lock()
	defer ds.mu.Unlock()
	if _, ok := ds.byTask[string(task)]; !ok {
		ds.byTask[string(task)] = &delegation{task: task, conns: map[uint64]*delegateConn{}, answered: map[uint64]protocol.DelegateItem{},
			dialing: map[uint64]bool{}, join: protocol.DelegateJoiner{MaxHeld: 8 << 20}, syncs: map[uint64]chan protocol.DelegateItem{}}
	}
}

// close ends a delegate task's delegation: its connections close. It
// returns how many were open.
func (ds *delegates) close(task []byte) int {
	ds.mu.Lock()
	d := ds.byTask[string(task)]
	delete(ds.byTask, string(task))
	var conns []*delegateConn
	var ln net.Listener
	if d != nil {
		d.closed = true
		for _, dc := range d.conns {
			conns = append(conns, dc)
		}
		d.conns = nil
		ln = d.ln
		// Exchanges waiting for core's reply give up: their minions get none.
		for n, ch := range d.syncs {
			close(ch)
			delete(d.syncs, n)
		}
	}
	ds.mu.Unlock()
	if ln != nil {
		_ = ln.Close()
	}
	for _, dc := range conns {
		_ = dc.c.Close()
	}
	return len(conns)
}

// closeAll ends every delegation: the session ended.
func (ds *delegates) closeAll() {
	ds.mu.Lock()
	var tasks [][]byte
	for _, d := range ds.byTask {
		tasks = append(tasks, d.task)
	}
	ds.mu.Unlock()
	for _, t := range tasks {
		ds.close(t)
	}
}

// pending: items wait for core.
func (ds *delegates) pending() bool {
	ds.mu.Lock()
	defer ds.mu.Unlock()
	return len(ds.out) > 0
}

// queueLocked adds an item for core.
func (ds *delegates) queueLocked(d protocol.DelegateItem) {
	d.T = protocol.ItemDelegate
	ds.out = append(ds.out, d)
}

// say queues an item for core and asks for a message.
func (ds *delegates) say(d protocol.DelegateItem) {
	ds.mu.Lock()
	ds.queueLocked(d)
	ds.mu.Unlock()
	ds.nudge()
}

// take reads one of core's delegate items.
func (ds *delegates) take(raw cbor.RawMessage) error {
	it, err := protocol.ReadDelegate(raw, false)
	if err != nil {
		if it.Answerable() && it.Step != protocol.DelegateClose {
			ds.say(protocol.DelegateItem{Task: it.Task, Step: protocol.DelegateClose, N: it.N, Reason: protocol.DelegateClosed,
				Msg: "this minion does not take it: " + err.Error()})
			return nil
		}
		return err
	}
	ds.mu.Lock()
	d := ds.byTask[string(it.Task)]
	if d == nil {
		if it.Step != protocol.DelegateClose {
			ds.queueLocked(protocol.DelegateItem{Task: it.Task, Step: protocol.DelegateClose, N: it.N, Reason: protocol.DelegateTaskEnded,
				Msg: "no such delegate task"})
		}
		ds.mu.Unlock()
		ds.nudge()
		return nil
	}
	switch it.Step {
	case protocol.DelegateOpen:
		ds.openLocked(d, it)
	case protocol.DelegateCall:
		ds.queueLocked(protocol.DelegateItem{Task: it.Task, Step: protocol.DelegateAnswer, N: it.N,
			Failure: &protocol.LinkFailure{Kind: protocol.LinkFailOther, Msg: "this delegate serves no bind-call kind"}})
	case protocol.DelegateFrame:
		ds.frameLocked(d, it)
	case protocol.DelegateClose:
		ds.closeLocked(d, it.N)
	case protocol.DelegateReply:
		ds.replyLocked(d, it)
	}
	ds.mu.Unlock()
	ds.nudge()
	return nil
}

// openLocked takes core's open: once each, its answer again when it comes
// again.
func (ds *delegates) openLocked(d *delegation, it protocol.DelegateItem) {
	if a, ok := d.answered[it.N]; ok {
		ds.queueLocked(a)
		return
	}
	if d.dialing[it.N] {
		return
	}
	if it.Serve < 1 || it.Serve > uint64(len(delegateServes)) {
		a := protocol.DelegateItem{Task: d.task, Step: protocol.DelegateAnswer, N: it.N,
			Failure: &protocol.LinkFailure{Kind: protocol.LinkFailOther, Msg: fmt.Sprintf("serve %d is not one this delegate serves", it.Serve)}}
		d.answered[it.N] = a
		ds.queueLocked(a)
		return
	}
	d.dialing[it.N] = true
	go ds.dialFor(d, it)
}

// dialFor dials what core's open names, as the TCP channel would (the
// target's host and port, else the build connection's listen_port, within
// its connect_timeout and the open's deadline), and answers it.
func (ds *delegates) dialFor(d *delegation, it protocol.DelegateItem) {
	address, timeout, err := tcpTarget(it.Values, time.Duration(it.Deadline)*time.Millisecond)
	// A target this delegate cannot dial as it is fails as other; a dial
	// that does not reach it, as unreachable or timed out.
	kind := protocol.LinkFailOther
	var c net.Conn
	if err == nil {
		c, err = ds.dial(context.Background(), address, timeout)
		kind = protocol.LinkFailUnreachable
	}
	a := protocol.DelegateItem{Task: d.task, Step: protocol.DelegateAnswer, N: it.N}
	if err != nil {
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() {
			kind = protocol.LinkFailTimeout
		}
		a.Failure = &protocol.LinkFailure{Kind: kind, Msg: clip(err.Error(), protocol.MaxDelegateMsg)}
		log.Printf("delegate %x: open %d: %v", d.task[:4], it.N, err)
	} else if tcp, ok := c.RemoteAddr().(*net.TCPAddr); ok {
		if ip := tcp.IP.To4(); ip != nil {
			a.Peer = ip
		} else {
			a.Peer = tcp.IP.To16()
		}
	}
	ds.mu.Lock()
	delete(d.dialing, it.N)
	if err == nil && d.closed {
		// The task ended while it dialed.
		_ = c.Close()
		a.Failure, a.Peer = &protocol.LinkFailure{Kind: protocol.LinkFailOther, Msg: "the delegate task ended"}, nil
		err = errors.New("ended")
	}
	if err == nil {
		dc := &delegateConn{c: c, writes: make(chan delegateFrame, delegateWriteQueue)}
		d.conns[it.N] = dc
		go ds.writer(dc)
		go ds.reader(d, it.N, dc, nil)
		log.Printf("delegate %x: open %d: connected to %s", d.task[:4], it.N, address)
	}
	d.answered[it.N] = a
	ds.queueLocked(a)
	ds.mu.Unlock()
	ds.nudge()
}

// tcpTarget is where an open of the TCP channel's kind dials, and within
// how long: the target's host and port (the listen port when it says
// none), its connect_timeout, the open's deadline at most. A proxy the
// target names this delegate does not dial through.
func tcpTarget(values map[string]any, deadline time.Duration) (string, time.Duration, error) {
	host, _ := values["host"].(string)
	if host == "" {
		return "", 0, errors.New("the target names no host")
	}
	if p, _ := values["proxy"].(string); p != "" {
		return "", 0, errors.New("this delegate dials no proxy: the target's proxy is the TCP channel's")
	}
	port := intValue(values["port"])
	if port == 0 {
		port = intValue(values["listen_port"])
	}
	if port < 1 || port > 65535 {
		return "", 0, errors.New("the target names no port")
	}
	timeout := 10 * time.Second
	if s, ok := values["connect_timeout"].(string); ok {
		if t, err := time.ParseDuration(s); err == nil && t > 0 {
			timeout = t
		}
	}
	if deadline > 0 && deadline < timeout {
		timeout = deadline
	}
	return net.JoinHostPort(host, strconv.FormatInt(port, 10)), timeout, nil
}

// intValue is a value's integer as CBOR decodes one (any of its widths); 0
// for none.
func intValue(v any) int64 {
	switch n := v.(type) {
	case uint64:
		return int64(min(n, 1<<31)) //nolint:gosec // bounded
	case int64:
		return n
	case int:
		return int64(n)
	case float64:
		return int64(n)
	}
	return 0
}

// reader carries the dialed minion's frames to core, in order, until the
// connection ends: then core is told, unless it closed it. A reverse
// delegate's connection says its first frame's serve and peer (first):
// that frame opens the connection in core.
func (ds *delegates) reader(d *delegation, n uint64, dc *delegateConn, first *protocol.DelegateItem) {
	var why error
	for {
		id, data, err := readTCPFrame(dc.c)
		if err != nil {
			why = err
			break
		}
		ds.mu.Lock()
		f := protocol.DelegateItem{Task: d.task, Step: protocol.DelegateFrame, N: n, Seq: dc.seq, ID: id[:], Frame: data}
		if first != nil && dc.seq == 0 {
			f.Serve, f.Peer = first.Serve, first.Peer
		}
		ds.queueLocked(f)
		dc.seq++
		ds.mu.Unlock()
		ds.nudge()
	}
	ds.mu.Lock()
	owned := d.conns[n] == dc
	if owned {
		delete(d.conns, n)
		d.join.Drop(n)
	}
	byCore := dc.byCore
	ds.mu.Unlock()
	close(dc.writes)
	_ = dc.c.Close()
	if owned && !byCore && !d.closed {
		log.Printf("delegate %x: connection %d ended: %v", d.task[:4], n, why)
		ds.say(protocol.DelegateItem{Task: d.task, Step: protocol.DelegateClose, N: n, Reason: protocol.DelegateClosed,
			Msg: clip(why.Error(), protocol.MaxDelegateMsg)})
	}
}

// writer writes core's frames to the dialed minion, in order.
func (ds *delegates) writer(dc *delegateConn) {
	for f := range dc.writes {
		_ = dc.c.SetWriteDeadline(time.Now().Add(time.Minute))
		if err := writeTCPFrame(dc.c, f.id, f.data); err != nil {
			_ = dc.c.Close()
			for range dc.writes {
			}
			return
		}
	}
}

// frameLocked takes one of core's frames for connection it.N: joined from
// its pieces, written to the dialed minion. One for a connection the
// minion does not have gets a close.
func (ds *delegates) frameLocked(d *delegation, it protocol.DelegateItem) {
	dc := d.conns[it.N]
	if dc == nil {
		ds.queueLocked(protocol.DelegateItem{Task: d.task, Step: protocol.DelegateClose, N: it.N, Reason: protocol.DelegateNoConnection,
			Msg: "no such connection"})
		return
	}
	frame, done, err := d.join.Add(it, time.Now())
	if err != nil || !done {
		return
	}
	select {
	case dc.writes <- delegateFrame{id: it.ID, data: frame}:
	default:
		log.Printf("delegate %x: connection %d: a frame dropped, its writer behind", d.task[:4], it.N)
	}
}

// closeLocked is core closing connection n: closed, and the close answered.
func (ds *delegates) closeLocked(d *delegation, n uint64) {
	if dc := d.conns[n]; dc != nil {
		dc.byCore = true
		delete(d.conns, n)
		_ = dc.c.Close()
		log.Printf("delegate %x: connection %d closed by core", d.task[:4], n)
	}
	d.join.Drop(n)
	ds.queueLocked(protocol.DelegateItem{Task: d.task, Step: protocol.DelegateClose, N: n, Reason: protocol.DelegateClosed})
}

// items are the items for core that fit room bytes of a message: whole, or
// a frame's first piece when it does not fit (the rest goes next), at most
// most of them; used, the bytes they take.
func (ds *delegates) items(room, most int) (out []any, used int) {
	ds.mu.Lock()
	defer ds.mu.Unlock()
	ds.expireLocked()
	for len(ds.out) > 0 && len(out) < most {
		d := ds.out[0]
		b, err := protocol.MarshalCBOR(d)
		if err != nil {
			ds.out = ds.out[1:]
			continue
		}
		if len(b)+16 <= room-used {
			out = append(out, d)
			used += len(b) + 16
			ds.out = ds.out[1:]
			continue
		}
		// A frame that does not fit: the piece that does, the rest after.
		fits := room - used - (len(b) - len(d.Frame)) - 64
		if len(d.Frame) == 0 || fits < 512 {
			break
		}
		head, tail := cutDelegate(d, fits)
		out = append(out, head)
		used = room
		ds.out[0] = tail
		break
	}
	return out, used
}

// cutDelegate cuts d's frame (or piece of one) after n bytes: two pieces of
// the same frame.
func cutDelegate(d protocol.DelegateItem, n int) (protocol.DelegateItem, protocol.DelegateItem) {
	size := d.Size
	if size == 0 {
		size = uint64(len(d.Frame))
	}
	head, tail := d, d
	head.Frame, head.Size = d.Frame[:n], size
	tail.Frame, tail.Off, tail.Size = d.Frame[n:], d.Off+uint64(n), size
	return head, tail
}

// expireLocked drops pieces of core's frames that never completed.
func (ds *delegates) expireLocked() {
	for _, d := range ds.byTask {
		d.join.Expire(time.Now().Add(-2 * time.Minute))
	}
}

// runDelegate runs a delegate task until core stops it (made as it was
// taken): its connections close then, and it ends ok.
func (r *runner) runDelegate(ctx context.Context, t *task) {
	<-ctx.Done()
	n := r.dlg.close(t.id)
	log.Printf("task %s: the delegate stops, with %d connection(s)", hex.EncodeToString(t.id)[:8], n)
	r.end(t, true, nil, "")
}
