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
// connects, relays and closes. A UDP association (a tunnel item that says
// udp) is a UDP socket of the minion's: core's tunnel_datagram items go out
// of it, each to the destination it names, and what comes back to it goes
// to core with its source; as UDP is, no acks and no resending, and a
// datagram that finds the queue full (udpQueue) is dropped.
//
// A reverse port forward (op 17) goes the other way: the minion listens on
// its host where the task's args say, tells core where (tunnel_listen), and
// opens a stream for each connection that comes there: its tunnel item,
// numbered by the minion, names who connected, and core, which connects it
// to a destination the minion never learns, answers with tunnel_reply. The
// connection waits, unread, for that answer: open, its bytes go both ways
// as any stream's; not, or no answer within the task's timeout and
// reverseSlack more, it is reset. A UDP one (its udp arg) listens for
// datagrams (tunnel_udp.go): a stream for each address they come from, its
// datagrams held until core answers, and ended (done) once it idled
// udpIdle.

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
	// udpQueue is the most of an association's datagrams the minion holds
	// for core; udpRead one datagram's room.
	udpQueue = 256 << 10
	udpRead  = 64<<10 + 512
	// reverseSlack is how much longer than the task's timeout (core's
	// connect) a connection to a reverse forward waits for core's answer.
	reverseSlack = 30 * time.Second
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
	// resend is tunnelResend, slack reverseSlack, udpIdle
	// protocol.TunnelUDPIdle, each shorter in tests; dial connects a
	// stream.
	resend  time.Duration
	slack   time.Duration
	udpIdle time.Duration
	dial    func(ctx context.Context, address string, timeout time.Duration) (net.Conn, error)
}

func newTunnels(nudge func()) *tunnels {
	return &tunnels{byTask: map[string]*tunnel{}, nudge: nudge, resend: tunnelResend, slack: reverseSlack, udpIdle: protocol.TunnelUDPIdle,
		dial: dialTCP}
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

	// A reverse port forward's: its streams are the minion's, the last
	// numbered last; its listener, the address it listens at, and whether
	// core is still to be told it (a tunnel_listen).
	reverse   bool
	last      uint64
	ln        net.Listener
	bound     string
	sayListen bool
	// A UDP reverse forward's: its socket, and its streams by the address
	// their datagrams come from.
	pc       net.PacketConn
	bySource map[string]*tstream
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

	// A stream the minion opens (a reverse forward's): the connection that
	// came, held unread until core answers (then it is conn); its tunnel
	// item while core has not, last sent at openSent.
	accepted net.Conn
	opening  *protocol.TunnelItem
	openSent time.Time

	// A UDP reverse forward's: the address its datagrams come from (its
	// key in bySource), sent to core once it answered open (sourceOpen),
	// and when a datagram last went either way.
	source     *net.UDPAddr
	sourceKey  string
	sourceOpen bool
	active     time.Time

	// A UDP association's: its socket, the datagrams it got for core (and
	// their bytes), numbered from dseq; core's taken, by their numbers.
	pc         net.PacketConn
	dgrams     []protocol.TunnelDatagramItem
	dgramBytes int
	dseq       uint64
	seen       replayWindow

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

// tunnelCmd: a command of a tunnel action (its build's socks_proxy,
// port_forward, reverse_port_forward), whose task is a tunnel.
func tunnelCmd(cmd uint64) bool {
	return cmd == opSocksProxy || cmd == opPortForward || cmd == opReversePortForward
}

// tunnelArgs are a tunnel task's limits, by its command's params (a socks
// proxy's, a forward's, a reverse forward's): how many streams at once,
// and how long a connect may take (a reverse forward's is core's, which a
// connection waits on). Where its streams go is core's to say, in their
// tunnel items (a reverse forward's, never).
func tunnelArgs(cmd uint64, args protocol.Args) (int, time.Duration) {
	numMax, numTimeout := uint64(socksProxyMaxStreams), uint64(socksProxyTimeout)
	switch cmd {
	case opPortForward:
		numMax, numTimeout = portForwardMaxStreams, portForwardTimeout
	case opReversePortForward:
		numMax, numTimeout = reversePortForwardMaxStreams, reversePortForwardTimeout
	}
	maxStreams, ok := protocol.ArgInt(args, numMax)
	if !ok || maxStreams <= 0 {
		maxStreams = 256
	}
	timeout, ok := protocol.ArgDuration(args, numTimeout)
	if !ok || timeout <= 0 {
		timeout = 10 * time.Second
	}
	return int(min(maxStreams, 1<<16)), timeout
}

// open makes the task's tunnel, as the task is taken: core opens streams in
// it once it has the task's ack.
func (ts *tunnels) open(ctx context.Context, task []byte, cmd uint64, args protocol.Args) {
	maxStreams, timeout := tunnelArgs(cmd, args)
	ts.mu.Lock()
	defer ts.mu.Unlock()
	ts.byTask[string(task)] = &tunnel{task: task, ctx: ctx, maxStreams: maxStreams, timeout: timeout, streams: map[uint64]*tstream{},
		reverse: cmd == opReversePortForward}
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
	if t.ln != nil {
		_ = t.ln.Close()
	}
	if t.pc != nil {
		_ = t.pc.Close()
	}
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

// socks runs a tunnel task, a socks proxy's or a forward's (its tunnel made
// as it was taken): streams come and go in it until core cancels it, and
// close with it. It ends ok.
func (r *runner) socks(ctx context.Context, t *task) {
	<-ctx.Done()
	n := r.tun.close(t.id)
	log.Printf("task %x: the tunnel closes, with %d stream(s)", t.id[:4], n)
	r.end(t, true, nil, "")
}

// reverse runs a reverse port forward (its tunnel made as it was taken):
// it listens where its args say (bind, 0.0.0.0 unless they say; listen;
// udp), tells core where, and opens a stream for each connection that
// comes (each address datagrams come from), until core cancels it, when it
// ends ok, its listener and streams closed. A listener that does not open,
// or fails, ends it failed, saying why.
func (r *runner) reverse(ctx context.Context, t *task, args protocol.Args) {
	bind, ok := protocol.ArgString(args, reversePortForwardBind)
	if !ok || bind == "" {
		bind = "0.0.0.0"
	}
	listen, _ := protocol.ArgInt(args, reversePortForwardListen)
	udp, _ := protocol.ArgBool(args, reversePortForwardUDP)
	err := protocol.TunnelListen(bind, listen)
	address := net.JoinHostPort(bind, strconv.FormatInt(listen, 10))
	switch {
	case err != nil:
	case udp:
		var pc net.PacketConn
		if pc, err = r.tun.listenUDP(t.id, address); err == nil {
			stop := context.AfterFunc(ctx, func() { _ = pc.Close() })
			log.Printf("task %x: listening at %s (udp)", t.id[:4], pc.LocalAddr())
			err = r.tun.sources(t.id, pc)
			stop()
			_ = pc.Close()
		}
	default:
		var ln net.Listener
		if ln, err = r.tun.listen(t.id, address); err == nil {
			stop := context.AfterFunc(ctx, func() { _ = ln.Close() })
			log.Printf("task %x: listening at %s", t.id[:4], ln.Addr())
			err = r.tun.accept(t.id, ln)
			stop()
			_ = ln.Close()
		}
	}
	n := r.tun.close(t.id)
	if ctx.Err() != nil {
		log.Printf("task %x: the reverse forward closes, with %d stream(s)", t.id[:4], n)
		r.end(t, true, nil, "")
		return
	}
	log.Printf("task %x: the reverse forward ends: %v", t.id[:4], err)
	r.end(t, false, nil, err.Error())
}

// listen opens a reverse forward's listener at address, and has core told
// where it listens. An error says why it did not open.
func (ts *tunnels) listen(task []byte, address string) (net.Listener, error) {
	ln, err := net.Listen("tcp", address)
	if err != nil {
		return nil, err
	}
	ts.mu.Lock()
	defer ts.mu.Unlock()
	t := ts.byTask[string(task)]
	if t == nil {
		_ = ln.Close()
		return nil, errors.New("the tunnel closed as it opened")
	}
	t.ln, t.bound, t.sayListen = ln, clip(ln.Addr().String(), protocol.MaxTunnelBound), true
	ts.nudge()
	return ln, nil
}

// accept takes the listener's connections, each a stream the minion opens,
// until the listener closes (the task's end) or fails: why it stopped.
// What the host runs short of (files, buffers) is waited out, as net/http
// does.
func (ts *tunnels) accept(task []byte, ln net.Listener) error {
	ts.mu.Lock()
	t := ts.byTask[string(task)]
	ts.mu.Unlock()
	if t == nil {
		return errors.New("the tunnel is closed")
	}
	var wait time.Duration
	for {
		c, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) || t.ctx.Err() != nil || !acceptAgain(err) {
				return err
			}
			wait = min(max(2*wait, 5*time.Millisecond), time.Second)
			select {
			case <-t.ctx.Done():
				return t.ctx.Err()
			case <-time.After(wait):
			}
			continue
		}
		wait = 0
		ts.opened(t, c)
	}
}

// opened opens a stream for a connection that came to the listener: its
// tunnel item names who connected. One past max_streams, or that comes as
// the task ends, is reset; one core does not answer within the task's
// timeout and reverseSlack more, too, and core is told.
func (ts *tunnels) opened(t *tunnel, c net.Conn) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	host, port := peer(c)
	if ts.byTask[string(t.task)] != t || len(t.streams) >= t.maxStreams || host == "" {
		resetConn(c)
		return
	}
	t.last++
	st := &tstream{t: t, id: t.last, accepted: c, done: make(chan struct{}), inSignal: make(chan struct{}, 1),
		outSignal: make(chan struct{}, 1), ackedEnd: tunnelWindow,
		opening: &protocol.TunnelItem{T: protocol.ItemTunnel, Task: t.task, Stream: t.last, Host: host, Port: port}}
	t.streams[st.id] = st
	time.AfterFunc(t.timeout+ts.slack, func() { ts.gaveUp(st) })
	ts.nudge()
}

// peer is the address that connected, as a tunnel item names it: "" when
// it does not fit one.
func peer(c net.Conn) (string, uint64) {
	host, port, err := net.SplitHostPort(c.RemoteAddr().String())
	if err != nil || host == "" || len(host) > protocol.MaxTunnelHost {
		return "", 0
	}
	p, err := strconv.ParseUint(port, 10, 16)
	if err != nil || p == 0 {
		return "", 0
	}
	return host, p
}

// gaveUp ends a stream the minion opened that core did not answer in time:
// its connection reset, and core told, should it open it late.
func (ts *tunnels) gaveUp(st *tstream) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	if st.gone || st.opening == nil {
		return
	}
	ts.reset(st)
}

// openDue: the stream's tunnel item goes now: never sent, or sent resend
// ago with no answer.
func (st *tstream) openDue(now time.Time, resend time.Duration) bool {
	return st.opening != nil && (st.openSent.IsZero() || now.Sub(st.openSent) >= resend)
}

// takeReply: core connected a stream the minion opened, or could not.
// Open, its bytes go both ways from now, within the room core gives; not,
// its connection is reset. A reply again is nothing; one that says open
// for a stream gone gets a close, so core drops its side too.
func (ts *tunnels) takeReply(r protocol.TunnelReplyItem) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	st, _ := ts.find(r.Task, r.Stream)
	if st == nil {
		if r.Result == protocol.TunnelOpen {
			ts.closeUnknown(r.Task, r.Stream)
		}
		return
	}
	if st.opening == nil {
		return
	}
	if st.source != nil {
		// A UDP stream: open, its datagrams held so far go, and every one
		// after; not, they are dropped with it.
		if r.Result != protocol.TunnelOpen {
			ts.drop(st, false)
			return
		}
		st.opening, st.sourceOpen, st.active = nil, true, time.Now()
		ts.nudge()
		return
	}
	if r.Result != protocol.TunnelOpen {
		ts.drop(st, true)
		return
	}
	st.conn, st.accepted, st.opening = st.accepted, nil, nil
	st.limit = protocol.TunnelRoom(r.Room)
	go ts.readHost(st)
	go ts.writeHost(st)
	ts.nudge()
}

// resetConn closes c as an abort (an RST), as a refused connection is.
func resetConn(c net.Conn) {
	if tc, ok := c.(*net.TCPConn); ok {
		_ = tc.SetLinger(0)
	}
	_ = c.Close()
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
	case protocol.ItemTunnelReply:
		var r protocol.TunnelReplyItem
		if err := readTunnelItem(raw, &r); err != nil {
			return err
		}
		ts.takeReply(r)
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
		ts.sendDatagram(d)
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
	if t.reverse {
		// A reverse forward's streams are the minion's: core opens none.
		t.closes = append(t.closes, protocol.TunnelCloseItem{T: protocol.ItemTunnelClose, Task: t.task, Stream: it.Stream,
			Reason: protocol.TunnelCloseTaskEnded})
		ts.nudge()
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
	if len(t.streams) >= t.maxStreams {
		refuse(protocol.TunnelNotAllowed, strconv.Itoa(t.maxStreams)+" streams open already")
		return
	}
	if it.UDP {
		ts.openUDP(t, it.Stream, refuse)
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
	if st.pc != nil {
		_ = st.pc.Close()
	}
	if st.source != nil {
		// A UDP stream: its address opens a new one next time; what it
		// held goes with it.
		if st.t.bySource[st.sourceKey] == st {
			delete(st.t.bySource, st.sourceKey)
		}
		st.opening, st.sourceOpen, st.dgrams, st.dgramBytes = nil, false, nil, 0
	}
	if st.accepted != nil {
		// Never answered, or not opened: reset, as a refused connection is.
		resetConn(st.accepted)
		st.accepted, st.opening = nil, nil
	}
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
	now := time.Now()
	for _, t := range ts.sorted() {
		for len(t.closes) > 0 && put(t.closes[0], tunnelItemSize) {
			t.closes = t.closes[1:]
		}
		if t.sayListen && put(protocol.TunnelListenItem{T: protocol.ItemTunnelListen, Task: t.task, Bound: t.bound}, tunnelItemSize) {
			t.sayListen = false
		}
		for _, st := range t.sortedStreams() {
			if st.openDue(now, ts.resend) && put(*st.opening, len(st.opening.Host)+tunnelItemSize) {
				st.openSent = now
			}
		}
		for len(t.replies) > 0 && put(t.replies[0], tunnelItemSize) {
			t.replies = t.replies[1:]
		}
		for _, st := range t.sortedStreams() {
			if st.conn != nil || st.datagrams() {
				open = append(open, st)
			}
		}
	}
	for _, st := range open {
		if !st.datagrams() && st.ackDue && put(st.ackItem(), tunnelItemSize) {
			st.ackDue = false
			st.ackedEnd = st.next + uint64(tunnelWindow-len(st.in))
		}
	}
	if n := len(open); n > 0 {
		start := ts.turn % n
		ts.turn++
	rounds:
		for moved := true; moved; {
			moved = false
			for i := range n {
				st := open[(start+i)%n]
				if st.datagrams() {
					if len(st.dgrams) == 0 {
						continue
					}
					d := st.dgrams[0]
					size := len(d.Data) + len(d.Host) + tunnelItemSize
					if !put(d, size) {
						if len(out) == 0 && most > 0 {
							// It fits no message: dropped, as UDP is.
							st.takeDatagram()
							continue
						}
						break rounds
					}
					st.takeDatagram()
					moved = true
					continue
				}
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
		if len(t.closes) > 0 || len(t.replies) > 0 || t.sayListen {
			return true
		}
		for _, st := range t.streams {
			if st.openDue(now, ts.resend) {
				return true
			}
			if st.datagrams() && len(st.dgrams) > 0 {
				return true
			}
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
			if st.opening != nil && !st.openSent.IsZero() {
				// Its tunnel item again, while core has not answered.
				if at := st.openSent.Add(ts.resend); soonest.IsZero() || at.Before(soonest) {
					soonest = at
				}
				continue
			}
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
