package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"net"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/fxamacker/cbor/v2"
	"github.com/logoscore/logos-golang-protocol/protocol"
)

// Tunnels (docs: contracts/minion-messages/tunnels.md): the socks command
// (op 15, action tunnel) runs until core cancels it, and while it runs core
// opens streams in it, each a TCP connection the minion makes from its host
// to the destination core names (a name resolved here, so internal names
// work). Both sides carry a stream's bytes in their messages, by offset,
// within a window each way: each keeps what it sent until the other's ack
// covers it, sends again from the ack what stays unacked tunnelResend,
// probes a window shut that long with one byte past it, and keeps what
// comes ahead of its next (messages crossing) until the bytes before it
// come. Core speaks SOCKS to the operators' tools; the minion only
// connects, relays and closes.

const (
	// tunnelWindow is the room the minion gives core on a stream: what it
	// holds of it for a host slower than core. The default room, so its
	// replies leave it out.
	tunnelWindow = protocol.DefaultTunnelRoom
	// tunnelResend: what went and was not acked in that long goes again
	// from the ack, and a window shut that long with bytes waiting is
	// probed.
	tunnelResend = 5 * time.Second
	// tunnelHeld is the most the minion reads of a host ahead of core's
	// ack, whatever room core gives.
	tunnelHeld = 1 << 20
	// tunnelRead is one read of a host, or write to it, at most.
	tunnelRead = 32 << 10
	// tunnelMinPiece is the least a tunnel_data carries when the stream has
	// more: a message nearly full leaves it to the next.
	tunnelMinPiece = 1 << 10
	// tunnelItemSize is what a tunnel item takes besides its bytes,
	// generously.
	tunnelItemSize = 64
	// tunnelMaxItems is the most tunnel items one message carries.
	tunnelMaxItems = 192
)

// tunnels are a session's tunnel tasks and their streams.
type tunnels struct {
	mu     sync.Mutex
	byTask map[string]*tunnel
	// closes answer items for a task the minion runs no tunnel of.
	closes []protocol.TunnelCloseItem
	// turn is where the next message starts sending data: every stream has
	// its turn.
	turn  int
	nudge func()
	// resend is tunnelResend, shorter in tests; dial connects a stream.
	resend time.Duration
	dial   func(ctx context.Context, address string, timeout time.Duration) (net.Conn, error)
}

func newTunnels(nudge func()) *tunnels {
	return &tunnels{byTask: map[string]*tunnel{}, nudge: nudge, resend: tunnelResend, dial: dialTCP}
}

func dialTCP(ctx context.Context, address string, timeout time.Duration) (net.Conn, error) {
	d := net.Dialer{Timeout: timeout}
	return d.DialContext(ctx, "tcp", address)
}

// tunnel is a tunnel task's: its limits, its streams, what it owes core.
type tunnel struct {
	task       []byte
	ctx        context.Context
	maxStreams int
	timeout    time.Duration
	streams    map[uint64]*tstream
	replies    []protocol.TunnelReplyItem
	closes     []protocol.TunnelCloseItem
}

// tstream is one stream: the minion's connection to its destination.
type tstream struct {
	t  *tunnel
	id uint64
	// conn is nil while it connects; reply is the answer core had (said
	// again when core asks again).
	conn  net.Conn
	reply *protocol.TunnelReplyItem
	gone  bool
	done  chan struct{}

	// From core, toward the host: taken up to next (its eof counted), not
	// yet written: in; ahead, what came past next.
	in         []byte
	next       uint64
	ahead      []protocol.TunnelDataItem
	aheadBytes int
	eofIn      bool
	// shut: the host has core's eof (its write side closed).
	shut     bool
	ackDue   bool
	ackedEnd uint64
	inSignal chan struct{}
	// finishing: core said done; the stream goes once the host has it all.
	finishing bool

	// Toward core: the host's bytes from base (core's next) not yet acked,
	// sent up to sent (the eof one past them), last at sentAt; nothing past
	// limit, the furthest core's acks reached.
	out       []byte
	base      uint64
	sent      uint64
	sentAt    time.Time
	limit     uint64
	eofOut    bool
	upDone    bool
	outSignal chan struct{}
}

func signal(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

// socksArgs are a socks task's limits: how many streams at once, and how
// long a connect may take.
func socksArgs(args protocol.Args) (int, time.Duration) {
	maxStreams, ok := protocol.ArgInt(args, socksMaxStreams)
	if !ok || maxStreams <= 0 {
		maxStreams = 256
	}
	timeout, ok := protocol.ArgDuration(args, socksTimeout)
	if !ok || timeout <= 0 {
		timeout = 10 * time.Second
	}
	return int(min(maxStreams, 1<<16)), timeout
}

// open makes the task's tunnel, as the task is taken: core opens streams in
// it once it has the task's ack.
func (ts *tunnels) open(ctx context.Context, task []byte, args protocol.Args) {
	maxStreams, timeout := socksArgs(args)
	ts.mu.Lock()
	defer ts.mu.Unlock()
	ts.byTask[string(task)] = &tunnel{task: task, ctx: ctx, maxStreams: maxStreams, timeout: timeout, streams: map[uint64]*tstream{}}
}

// close ends the task's tunnel and its streams: how many it had.
func (ts *tunnels) close(task []byte) int {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	t := ts.byTask[string(task)]
	if t == nil {
		return 0
	}
	delete(ts.byTask, string(task))
	n := len(t.streams)
	for _, st := range t.streams {
		ts.drop(st, false)
	}
	return n
}

// closeAll ends every tunnel: the session is over.
func (ts *tunnels) closeAll() {
	ts.mu.Lock()
	var tasks [][]byte
	for _, t := range ts.byTask {
		tasks = append(tasks, t.task)
	}
	ts.mu.Unlock()
	for _, task := range tasks {
		ts.close(task)
	}
}

// socks runs a tunnel task (its tunnel made as it was taken): streams come
// and go in it until core cancels it, and close with it. It ends ok.
func (r *runner) socks(ctx context.Context, t *task) {
	<-ctx.Done()
	n := r.tun.close(t.id)
	log.Printf("task %x: the tunnel closes, with %d stream(s)", t.id[:4], n)
	r.end(t, true, nil, "")
}

// take reads one of core's tunnel items; an error says why it does not
// read.
func (ts *tunnels) take(raw cbor.RawMessage) error {
	switch protocol.ItemType(raw) {
	case protocol.ItemTunnel:
		var it protocol.TunnelItem
		if err := readTunnelItem(raw, &it); err != nil {
			return err
		}
		ts.openStream(it)
	case protocol.ItemTunnelData:
		var d protocol.TunnelDataItem
		if err := readTunnelItem(raw, &d); err != nil {
			return err
		}
		ts.takeData(d)
	case protocol.ItemTunnelAck:
		var a protocol.TunnelAckItem
		if err := readTunnelItem(raw, &a); err != nil {
			return err
		}
		ts.takeAck(a)
	case protocol.ItemTunnelClose:
		var c protocol.TunnelCloseItem
		if err := readTunnelItem(raw, &c); err != nil {
			return err
		}
		ts.takeClose(c)
	case protocol.ItemTunnelDatagram:
		var d protocol.TunnelDatagramItem
		if err := readTunnelItem(raw, &d); err != nil {
			return err
		}
		// No UDP association opens here yet: no such stream.
		ts.mu.Lock()
		ts.closeUnknown(d.Task, d.Stream)
		ts.mu.Unlock()
	}
	return nil
}

func readTunnelItem(raw cbor.RawMessage, v interface{ Validate() error }) error {
	if err := protocol.UnmarshalCBOR(raw, v); err != nil {
		return err
	}
	return v.Validate()
}

// find is the task's stream; nil, with the reason core is told, when the
// minion has none.
func (ts *tunnels) find(task []byte, id uint64) (*tstream, int) {
	t := ts.byTask[string(task)]
	if t == nil {
		return nil, protocol.TunnelCloseTaskEnded
	}
	if st := t.streams[id]; st != nil {
		return st, 0
	}
	return nil, protocol.TunnelCloseUnknown
}

// closeUnknown answers an item about a stream the minion does not have.
func (ts *tunnels) closeUnknown(task []byte, id uint64) {
	_, reason := ts.find(task, id)
	if reason == 0 {
		reason = protocol.TunnelCloseUnknown
	}
	c := protocol.TunnelCloseItem{T: protocol.ItemTunnelClose, Task: task, Stream: id, Reason: reason}
	if t := ts.byTask[string(task)]; t != nil {
		t.closes = append(t.closes, c)
	} else {
		ts.closes = append(ts.closes, c)
	}
	ts.nudge()
}

// openStream connects a stream core opens; asked again, it answers again.
func (ts *tunnels) openStream(it protocol.TunnelItem) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	t := ts.byTask[string(it.Task)]
	if t == nil {
		ts.closeUnknown(it.Task, it.Stream)
		return
	}
	if st := t.streams[it.Stream]; st != nil {
		if st.reply != nil {
			t.replies = append(t.replies, *st.reply)
			ts.nudge()
		}
		return
	}
	refuse := func(result int, msg string) {
		t.replies = append(t.replies, protocol.TunnelReplyItem{T: protocol.ItemTunnelReply, Task: t.task, Stream: it.Stream, Result: result, Msg: msg})
		ts.nudge()
	}
	switch {
	case it.UDP:
		refuse(protocol.TunnelFailed, "this minion carries no UDP yet")
		return
	case len(t.streams) >= t.maxStreams:
		refuse(protocol.TunnelNotAllowed, strconv.Itoa(t.maxStreams)+" streams open already")
		return
	}
	st := &tstream{t: t, id: it.Stream, done: make(chan struct{}), inSignal: make(chan struct{}, 1), outSignal: make(chan struct{}, 1),
		limit: protocol.TunnelRoom(it.Room), ackedEnd: tunnelWindow}
	t.streams[it.Stream] = st
	go ts.connect(st, net.JoinHostPort(it.Host, strconv.FormatUint(it.Port, 10)))
}

// connect makes the stream's connection, within the task's timeout, and
// answers core: open, with the socket's address, or why not.
func (ts *tunnels) connect(st *tstream, address string) {
	conn, err := ts.dial(st.t.ctx, address, st.t.timeout)
	ts.mu.Lock()
	defer ts.mu.Unlock()
	if st.gone {
		if conn != nil {
			_ = conn.Close()
		}
		return
	}
	r := protocol.TunnelReplyItem{T: protocol.ItemTunnelReply, Task: st.t.task, Stream: st.id}
	if err != nil {
		r.Result, r.Msg = dialResult(err), clip(err.Error(), protocol.MaxTunnelMsg)
		st.t.replies = append(st.t.replies, r)
		ts.drop(st, false)
		ts.nudge()
		return
	}
	r.Bound = clip(conn.LocalAddr().String(), protocol.MaxTunnelBound)
	st.conn, st.reply = conn, &r
	st.t.replies = append(st.t.replies, r)
	go ts.readHost(st)
	go ts.writeHost(st)
	ts.nudge()
}

// dialResult is the reply's result for a connect that failed.
func dialResult(err error) int {
	var dns *net.DNSError
	var ne net.Error
	switch {
	case errors.As(err, &dns):
		return protocol.TunnelHostUnreachable
	case refusedErr(err):
		return protocol.TunnelRefused
	case hostUnreachableErr(err):
		return protocol.TunnelHostUnreachable
	case netUnreachableErr(err):
		return protocol.TunnelNetUnreachable
	case timedOutErr(err), errors.Is(err, context.DeadlineExceeded), errors.As(err, &ne) && ne.Timeout():
		return protocol.TunnelTimedOut
	}
	return protocol.TunnelFailed
}

// clip is s within n bytes, valid UTF-8.
func clip(s string, n int) string {
	s = strings.ToValidUTF8(s, "?")
	for len(s) > n {
		_, size := utf8.DecodeLastRuneInString(s)
		s = s[:len(s)-size]
	}
	return s
}

// readHost carries what the host sends toward core, while core's room
// lasts.
func (ts *tunnels) readHost(st *tstream) {
	buf := make([]byte, tunnelRead)
	for {
		ts.mu.Lock()
		for !st.gone && st.outFree() == 0 {
			ts.mu.Unlock()
			select {
			case <-st.outSignal:
			case <-st.done:
			}
			ts.mu.Lock()
		}
		if st.gone {
			ts.mu.Unlock()
			return
		}
		n := min(len(buf), st.outFree())
		conn := st.conn
		ts.mu.Unlock()
		k, err := conn.Read(buf[:n])
		ts.mu.Lock()
		if st.gone {
			ts.mu.Unlock()
			return
		}
		st.out = append(st.out, buf[:k]...)
		switch {
		case errors.Is(err, io.EOF):
			st.eofOut = true
		case err != nil:
			ts.reset(st)
		}
		ts.mu.Unlock()
		ts.nudge()
		if err != nil {
			return
		}
	}
}

// outFree is how much more of the host the minion may read now.
func (st *tstream) outFree() int {
	if st.eofOut {
		return 0
	}
	return max(int(min(st.limit-min(st.limit, st.base), tunnelHeld))-len(st.out), 0)
}

// writeHost writes core's bytes to the host, giving the room back as it
// goes, and ends the host's end with core's eof.
func (ts *tunnels) writeHost(st *tstream) {
	for {
		ts.mu.Lock()
		for !st.gone && len(st.in) == 0 && (!st.eofIn || st.shut) && !st.finishing {
			ts.mu.Unlock()
			select {
			case <-st.inSignal:
			case <-st.done:
			}
			ts.mu.Lock()
		}
		if st.gone {
			ts.mu.Unlock()
			return
		}
		if len(st.in) == 0 {
			if st.eofIn && !st.shut {
				st.shut = true
				if cw, ok := st.conn.(interface{ CloseWrite() error }); ok {
					_ = cw.CloseWrite()
				}
			}
			if st.finishing {
				ts.drop(st, false)
			} else {
				ts.doneIfOver(st)
			}
			ts.mu.Unlock()
			return
		}
		piece := st.in[:min(len(st.in), 2*tunnelRead)]
		conn := st.conn
		ts.mu.Unlock()
		_, err := conn.Write(piece)
		ts.mu.Lock()
		if st.gone {
			ts.mu.Unlock()
			return
		}
		if err != nil {
			ts.reset(st)
			ts.mu.Unlock()
			return
		}
		st.in = st.in[len(piece):]
		if len(st.in) == 0 {
			st.in = nil
		}
		wake := st.roomGrew()
		ts.mu.Unlock()
		if wake {
			ts.nudge()
		}
	}
}

// roomGrew marks an ack due once the host took enough to say so: a quarter
// of the window.
func (st *tstream) roomGrew() bool {
	if st.ackDue || st.eofIn {
		return false
	}
	if st.next+uint64(tunnelWindow-len(st.in)) >= st.ackedEnd+tunnelWindow/4 {
		st.ackDue = true
		return true
	}
	return false
}

// reset ends the stream as an abort: core is told, the connection reset.
func (ts *tunnels) reset(st *tstream) {
	if st.gone {
		return
	}
	st.t.closes = append(st.t.closes, protocol.TunnelCloseItem{T: protocol.ItemTunnelClose, Task: st.t.task, Stream: st.id, Reason: protocol.TunnelCloseReset})
	ts.drop(st, true)
	ts.nudge()
}

// doneIfOver ends the stream once both directions ended and each side has
// the other's eof, telling core.
func (ts *tunnels) doneIfOver(st *tstream) {
	if st.gone || !st.upDone || !st.shut {
		return
	}
	st.t.closes = append(st.t.closes, protocol.TunnelCloseItem{T: protocol.ItemTunnelClose, Task: st.t.task, Stream: st.id, Reason: protocol.TunnelCloseDone})
	ts.drop(st, false)
	ts.nudge()
}

// drop forgets the stream and closes its connection (reset: aborted).
func (ts *tunnels) drop(st *tstream, reset bool) {
	if st.gone {
		return
	}
	st.gone = true
	close(st.done)
	delete(st.t.streams, st.id)
	if st.conn == nil {
		return
	}
	if tc, ok := st.conn.(*net.TCPConn); ok && reset {
		_ = tc.SetLinger(0)
	}
	_ = st.conn.Close()
}

// takeData takes core's bytes at the minion's next, within the room it
// gave. One ahead of next is kept within the room until the bytes before
// it come; what the minion has already is acked again and dropped.
func (ts *tunnels) takeData(d protocol.TunnelDataItem) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	st, _ := ts.find(d.Task, d.Stream)
	if st == nil {
		ts.closeUnknown(d.Task, d.Stream)
		return
	}
	st.ackDue = true
	defer ts.nudge()
	switch {
	case st.eofIn || d.End() <= st.next:
		return
	case d.Off > st.next:
		// Its bytes within the room (its eof needs none).
		if d.Off+uint64(len(d.Data)) <= st.next+uint64(tunnelWindow-len(st.in)) && st.aheadBytes+len(d.Data) <= tunnelWindow {
			d.Data = bytes.Clone(d.Data)
			st.ahead = append(st.ahead, d)
			st.aheadBytes += len(d.Data)
		}
		return
	}
	st.take(d)
	for moved := true; moved && !st.eofIn; {
		moved = false
		kept := st.ahead[:0]
		for _, a := range st.ahead {
			switch {
			case a.End() <= st.next:
				st.aheadBytes -= len(a.Data)
			case a.Off <= st.next:
				st.aheadBytes -= len(a.Data)
				st.take(a)
				moved = true
			default:
				kept = append(kept, a)
			}
		}
		st.ahead = kept
	}
	signal(st.inSignal)
}

// take takes d's bytes from next on (d starts at or before it and ends
// past it), within the room the minion gave.
func (st *tstream) take(d protocol.TunnelDataItem) {
	data := d.Data[min(st.next-d.Off, uint64(len(d.Data))):]
	eof := d.EOF
	if free := tunnelWindow - len(st.in); len(data) > free {
		data, eof = data[:free], false
	}
	st.in = append(st.in, data...)
	st.next += uint64(len(data))
	if eof {
		st.eofIn = true
		st.next++
		st.ahead, st.aheadBytes = nil, 0
	}
}

// takeAck drops what core has of the host's bytes and takes the room it
// gives (unchanged when it says none); one below the last is nothing, one
// past what the minion sent is not an ack of its.
func (ts *tunnels) takeAck(a protocol.TunnelAckItem) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	st, _ := ts.find(a.Task, a.Stream)
	if st == nil {
		ts.closeUnknown(a.Task, a.Stream)
		return
	}
	end := st.base + uint64(len(st.out))
	if st.conn == nil || st.upDone || a.Next < st.base || a.Next > end+1 || (a.Next == end+1 && !st.eofOut) {
		return
	}
	room := st.limit - min(st.limit, st.base)
	if a.Room != nil {
		room = *a.Room
	}
	if a.Next > st.base {
		st.sentAt = time.Now()
	}
	if a.Next == end+1 {
		st.out, st.base, st.upDone = nil, end, true
	} else {
		st.out = st.out[a.Next-st.base:]
		st.base = a.Next
	}
	st.sent = max(st.sent, a.Next)
	st.limit = max(st.limit, a.Next+room)
	signal(st.outSignal)
	if st.upDone {
		ts.doneIfOver(st)
	}
}

// takeClose: core ended the stream. Done, it goes once the host has every
// byte; any other reason (one the minion does not know reads as a reset)
// resets the connection at once.
func (ts *tunnels) takeClose(c protocol.TunnelCloseItem) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	st, _ := ts.find(c.Task, c.Stream)
	if st == nil {
		return
	}
	if c.Reason == protocol.TunnelCloseDone && st.conn != nil && len(st.in) > 0 {
		st.finishing = true
		signal(st.inSignal)
		return
	}
	ts.drop(st, c.Reason != protocol.TunnelCloseDone)
}

// items are what the tunnels send core in a message of room bytes and most
// items: closes, replies, acks, then data, a stream's item in turn while
// room lasts; and the bytes they take.
func (ts *tunnels) items(room, most int) ([]any, int) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	var out []any
	used := 0
	put := func(it any, size int) bool {
		if len(out) >= most || used+size > room {
			return false
		}
		out = append(out, it)
		used += size
		return true
	}
	for len(ts.closes) > 0 && put(ts.closes[0], tunnelItemSize) {
		ts.closes = ts.closes[1:]
	}
	var open []*tstream
	for _, t := range ts.sorted() {
		for len(t.closes) > 0 && put(t.closes[0], tunnelItemSize) {
			t.closes = t.closes[1:]
		}
		for len(t.replies) > 0 && put(t.replies[0], tunnelItemSize) {
			t.replies = t.replies[1:]
		}
		for _, st := range t.sortedStreams() {
			if st.conn != nil {
				open = append(open, st)
			}
		}
	}
	for _, st := range open {
		if st.ackDue && put(st.ackItem(), tunnelItemSize) {
			st.ackDue = false
			st.ackedEnd = st.next + uint64(tunnelWindow-len(st.in))
		}
	}
	if n := len(open); n > 0 {
		start := ts.turn % n
		ts.turn++
		now := time.Now()
	rounds:
		for moved := true; moved; {
			moved = false
			for i := range n {
				st := open[(start+i)%n]
				d := st.dataItem(now, room-used-tunnelItemSize, ts.resend)
				if d == nil {
					continue
				}
				if !put(*d, len(d.Data)+tunnelItemSize) {
					break rounds
				}
				st.sent, st.sentAt = d.End(), now
				moved = true
			}
		}
	}
	return out, used
}

// sorted is the tunnels by task; sortedStreams a tunnel's streams by
// number.
func (ts *tunnels) sorted() []*tunnel {
	out := make([]*tunnel, 0, len(ts.byTask))
	for _, t := range ts.byTask {
		out = append(out, t)
	}
	slices.SortFunc(out, func(a, b *tunnel) int { return bytes.Compare(a.task, b.task) })
	return out
}

func (t *tunnel) sortedStreams() []*tstream {
	out := make([]*tstream, 0, len(t.streams))
	for _, st := range t.streams {
		out = append(out, st)
	}
	slices.SortFunc(out, func(a, b *tstream) int {
		switch {
		case a.id < b.id:
			return -1
		case a.id > b.id:
			return 1
		}
		return 0
	})
	return out
}

func (st *tstream) ackItem() protocol.TunnelAckItem {
	room := uint64(tunnelWindow - len(st.in))
	return protocol.TunnelAckItem{T: protocol.ItemTunnelAck, Task: st.t.task, Stream: st.id, Next: st.next, Room: &room}
}

// dataItem is the stream's next tunnel_data in room bytes: what was not
// sent (from the ack again once a send went unacked resend long), up to
// core's room, then the eof once every byte went (the eof needs no room);
// a probe, one byte past a window shut that long with bytes waiting; nil
// for nothing, or too little room.
func (st *tstream) dataItem(now time.Time, room int, resend time.Duration) *protocol.TunnelDataItem {
	if st.upDone || room <= 0 {
		return nil
	}
	end := st.base + uint64(len(st.out))
	sent := st.sent
	quiet := now.Sub(st.sentAt) >= resend
	if sent > st.base && quiet {
		sent = st.base
	}
	n := 0
	if limit := min(end, st.limit); sent < limit {
		n = int(limit - sent)
	}
	eof := st.eofOut && sent <= end && sent+uint64(n) == end
	if n == 0 && !eof {
		if sent >= end || !quiet {
			return nil
		}
		n = 1
	}
	if most := min(room, protocol.MaxTunnelData); n > most {
		if most < tunnelMinPiece {
			return nil
		}
		n, eof = most, false
	}
	from := int(sent - st.base)
	d := protocol.TunnelDataItem{T: protocol.ItemTunnelData, Task: st.t.task, Stream: st.id, Off: sent, EOF: eof}
	if n > 0 {
		d.Data = st.out[from : from+n]
	}
	return &d
}

// pending: the tunnels have something to send now.
func (ts *tunnels) pending() bool {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	if len(ts.closes) > 0 {
		return true
	}
	now := time.Now()
	for _, t := range ts.byTask {
		if len(t.closes) > 0 || len(t.replies) > 0 {
			return true
		}
		for _, st := range t.streams {
			if st.conn != nil && (st.ackDue || st.dataItem(now, protocol.MaxTunnelData, ts.resend) != nil) {
				return true
			}
		}
	}
	return false
}

// due is how soon a stream sends again unasked (from its ack, or a probe of
// a shut window): the session checks in then, even over a persistent
// connection. False for none.
func (ts *tunnels) due() (time.Duration, bool) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	var soonest time.Time
	for _, t := range ts.byTask {
		for _, st := range t.streams {
			if st.conn == nil || st.upDone {
				continue
			}
			end := st.base + uint64(len(st.out))
			waiting := st.sent > st.base || (st.sent < end && st.sent >= st.limit)
			if at := st.sentAt.Add(ts.resend); waiting && (soonest.IsZero() || at.Before(soonest)) {
				soonest = at
			}
		}
	}
	if soonest.IsZero() {
		return 0, false
	}
	return max(time.Until(soonest), time.Millisecond), true
}
