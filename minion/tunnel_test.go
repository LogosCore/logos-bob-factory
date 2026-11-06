package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"io"
	mrand "math/rand/v2"
	"net"
	"os"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/logoscore/logos-golang-protocol/protocol"
)

// echoHost is a host that sends back what it gets, and ends its side once
// the other ended its: its address.
func echoHost(t *testing.T) (string, int) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = c.Close() }()
				_, _ = io.Copy(c, c)
				_ = c.(*net.TCPConn).CloseWrite()
			}()
		}
	}()
	a := ln.Addr().(*net.TCPAddr)
	return a.IP.String(), a.Port
}

func closedPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	return ln.Addr().(*net.TCPAddr).Port
}

var socksTask = bytes.Repeat([]byte{0x50}, 16)

// startSocks is a runner running a socks task, its resends quick.
func startSocks(t *testing.T, args protocol.Args) *runner {
	t.Helper()
	r := newRunner(description{Ops: []uint64{opSocksProxy}}, &schedule{interval: time.Second})
	r.tun.resend = 100 * time.Millisecond
	r.start(protocol.TaskItem{T: protocol.ItemTask, Task: socksTask, Cmd: opSocksProxy, Args: args})
	t.Cleanup(func() { r.tun.closeAll() })
	return r
}

// tunnelCore is core's side of the minion's tunnels: what its tools send each
// stream, what comes back, the minion's replies and closes. lose loses its
// replies now and then; swap sends its data items in reverse order.
type tunnelCore struct {
	t       *testing.T
	r       *runner
	streams map[uint64]*coreStream
	lose    func(round int) bool
	swap    bool
	// noResend: core never sends again what the minion did not ack.
	noResend bool
	round    int
	// other: the minion's tunnel items about no stream core opened.
	other []any
}

type coreStream struct {
	// Toward the host: send, the eof after it when eof; sent up to sent,
	// last in round sentIn; the minion's next and the room it gives.
	send   []byte
	eof    bool
	sent   uint64
	sentIn int
	acked  uint64
	limit  uint64
	// From the host: got in order, eofGot.
	got    []byte
	eofGot bool
	ackDue bool
	reply  *protocol.TunnelReplyItem
	closed *protocol.TunnelCloseItem
}

func newTunnelCore(t *testing.T, r *runner) *tunnelCore {
	return &tunnelCore{t: t, r: r, streams: map[uint64]*coreStream{}}
}

// open asks the minion for a stream to host:port, whose tool sends send
// and then ends.
func (c *tunnelCore) open(id uint64, host string, port int, send []byte) *coreStream {
	st := &coreStream{send: send, eof: true}
	c.streams[id] = st
	c.r.take(coreSays(c.t, protocol.TunnelItem{T: protocol.ItemTunnel, Task: socksTask, Stream: id, Host: host, Port: uint64(port)}), nil)
	return st
}

// exchange is one message of the minion's and core's reply to it.
func (c *tunnelCore) exchange() {
	c.round++
	items := c.r.outgoing()
	c.r.delivered(items)
	for _, it := range items {
		switch it := it.(type) {
		case protocol.TunnelReplyItem:
			if st := c.streams[it.Stream]; st != nil {
				st.reply = &it
				st.limit = protocol.TunnelRoom(it.Room)
			} else {
				c.other = append(c.other, it)
			}
		case protocol.TunnelDataItem:
			st := c.streams[it.Stream]
			if st == nil {
				c.other = append(c.other, it)
				continue
			}
			st.ackDue = true
			if next := uint64(len(st.got)); it.Off == next && !st.eofGot {
				st.got = append(st.got, it.Data...)
				st.eofGot = it.EOF
			}
		case protocol.TunnelAckItem:
			if st := c.streams[it.Stream]; st != nil && it.Next >= st.acked {
				if it.Next > st.acked {
					st.sentIn = c.round
				}
				st.acked = it.Next
				st.limit = max(st.limit, it.Next+protocol.TunnelRoom(it.Room))
			}
		case protocol.TunnelCloseItem:
			if st := c.streams[it.Stream]; st != nil {
				st.closed = &it
			} else {
				c.other = append(c.other, it)
			}
		}
	}
	var data, rest []any
	for id, st := range c.streams {
		if st.reply == nil || st.reply.Result != protocol.TunnelOpen || st.closed != nil {
			continue
		}
		if st.ackDue {
			next := uint64(len(st.got))
			if st.eofGot {
				next++
			}
			room := uint64(protocol.DefaultTunnelRoom)
			rest = append(rest, protocol.TunnelAckItem{T: protocol.ItemTunnelAck, Task: socksTask, Stream: id, Next: next, Room: &room})
			st.ackDue = false
		}
		end := uint64(len(st.send))
		if st.sent > st.acked && c.round-st.sentIn > 20 && !c.noResend {
			st.sent = st.acked // unacked a while: again from the ack
		}
		for st.sent < min(end, st.limit) || (st.eof && st.sent == end) {
			n := min(min(end, st.limit)-st.sent, protocol.MaxTunnelData)
			d := protocol.TunnelDataItem{T: protocol.ItemTunnelData, Task: socksTask, Stream: id, Off: st.sent, Data: st.send[st.sent : st.sent+n]}
			st.sent += n
			if st.eof && st.sent == end {
				d.EOF = true
				st.sent++
			}
			data = append(data, d)
			st.sentIn = c.round
		}
	}
	if c.swap {
		for i, j := 0, len(data)-1; i < j; i, j = i+1, j-1 {
			data[i], data[j] = data[j], data[i]
		}
	}
	if c.lose != nil && c.lose(c.round) {
		return
	}
	c.r.take(coreSays(c.t, append(rest, data...)...), items)
}

// until exchanges until done says so, failing after ten seconds.
func (c *tunnelCore) until(what string, done func() bool) {
	c.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !done() {
		if time.Now().After(deadline) {
			for id, st := range c.streams {
				c.t.Logf("core %d: sent %d acked %d limit %d send %d eof %v | got %d eofGot %v closed %+v", id, st.sent, st.acked, st.limit, len(st.send), st.eof, len(st.got), st.eofGot, st.closed)
			}
			c.r.tun.mu.Lock()
			for _, tn := range c.r.tun.byTask {
				for id, m := range tn.streams {
					c.t.Logf("minion %d: next %d in %d eofIn %v shut %v ackDue %v | base %d sent %d out %d limit %d eofOut %v upDone %v finishing %v", id, m.next, len(m.in), m.eofIn, m.shut, m.ackDue, m.base, m.sent, len(m.out), m.limit, m.eofOut, m.upDone, m.finishing)
				}
			}
			c.r.tun.mu.Unlock()
			c.t.Fatalf("never: %s", what)
		}
		c.exchange()
		time.Sleep(2 * time.Millisecond)
	}
}

func randomBytes(n int) []byte {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return b
}

// A stream core opens reaches the host from the minion: what core's tool
// sends comes back from an echo host, 512 KiB both ways through 256 KiB
// windows, and the stream ends done once both ends closed. The reply says
// the socket's address.
func TestTunnelRelaysAStream(t *testing.T) {
	r := startSocks(t, nil)
	core := newTunnelCore(t, r)
	host, port := echoHost(t)
	st := core.open(1, host, port, randomBytes(512<<10))
	core.until("the stream ends done", func() bool { return st.closed != nil })
	if st.reply == nil || st.reply.Result != protocol.TunnelOpen || st.reply.Bound == "" {
		t.Fatalf("reply %+v", st.reply)
	}
	if st.closed.Reason != protocol.TunnelCloseDone || !st.eofGot || !bytes.Equal(st.got, st.send) {
		t.Fatalf("close %+v, eof %v, %d bytes back of %d", st.closed, st.eofGot, len(st.got), len(st.send))
	}
}

// Core's replies lost on the way (a third of them) lose no byte and double
// none: the minion sends again from core's ack what went unacked, and
// probes a window an ack lost left shut.
func TestTunnelSurvivesLostReplies(t *testing.T) {
	r := startSocks(t, nil)
	core := newTunnelCore(t, r)
	// A third of them, at random: a fixed pattern could fall in step with
	// the fake core's own resends, losing every one.
	rng := mrand.New(mrand.NewPCG(1, 2))
	core.lose = func(int) bool { return rng.IntN(3) == 0 }
	host, port := echoHost(t)
	st := core.open(1, host, port, randomBytes(768<<10))
	core.until("the stream ends done", func() bool { return st.closed != nil })
	if !bytes.Equal(st.got, st.send) {
		t.Fatalf("%d bytes back of %d, not the same", len(st.got), len(st.send))
	}
}

// Core's data items crossing (the later first) are kept, and the host gets
// the bytes in order.
func TestTunnelTakesCoresDataAhead(t *testing.T) {
	r := startSocks(t, nil)
	r.tun.resend = time.Hour // no resend could cover for an item dropped
	core := newTunnelCore(t, r)
	core.swap, core.noResend = true, true
	host, port := echoHost(t)
	st := core.open(1, host, port, randomBytes(512<<10))
	core.until("the stream ends done", func() bool { return st.closed != nil })
	if !bytes.Equal(st.got, st.send) {
		t.Fatalf("%d bytes back of %d, not the same", len(st.got), len(st.send))
	}
}

// A destination nothing listens on is refused, as the minion's connect
// says; past max_streams core is told not allowed; asked again for a
// stream it has, the minion answers again.
func TestTunnelOpening(t *testing.T) {
	r := startSocks(t, protocol.Args{socksProxyMaxStreams: int64(1)})
	core := newTunnelCore(t, r)
	host, port := echoHost(t)
	open := core.open(1, host, port, nil)
	open.eof = false
	core.until("the first opens", func() bool { return open.reply != nil })
	second := core.open(2, host, port, nil)
	core.until("the second is answered", func() bool { return second.reply != nil })
	if second.reply.Result != protocol.TunnelNotAllowed {
		t.Errorf("past max_streams: %+v", second.reply)
	}
	first := *open.reply
	open.reply = nil
	r.take(coreSays(t, protocol.TunnelItem{T: protocol.ItemTunnel, Task: socksTask, Stream: 1, Host: host, Port: uint64(port)}), nil)
	core.until("the first answered again", func() bool { return open.reply != nil })
	if open.reply.Result != protocol.TunnelOpen || open.reply.Bound != first.Bound {
		t.Errorf("answered again %+v, first %+v", open.reply, first)
	}
	r.take(coreSays(t, protocol.TunnelCloseItem{T: protocol.ItemTunnelClose, Task: socksTask, Stream: 1, Reason: protocol.TunnelCloseReset}), nil)
	refused := core.open(3, "127.0.0.1", closedPort(t), nil)
	core.until("the refused one is answered", func() bool { return refused.reply != nil })
	if refused.reply.Result != protocol.TunnelRefused || refused.reply.Msg == "" {
		t.Errorf("nothing listening: %+v", refused.reply)
	}
}

// Items about a stream the minion does not have get a close: no such
// stream (3), or for a task it runs no tunnel of, task ended (4); a close
// for one gone is nothing, and an item that does not read is refused.
func TestTunnelUnknown(t *testing.T) {
	r := startSocks(t, nil)
	core := newTunnelCore(t, r)
	other := bytes.Repeat([]byte{9}, 16)
	r.take(coreSays(t,
		protocol.TunnelDataItem{T: protocol.ItemTunnelData, Task: socksTask, Stream: 7, Data: []byte("x")},
		protocol.TunnelAckItem{T: protocol.ItemTunnelAck, Task: other, Stream: 1, Next: 1},
		protocol.TunnelItem{T: protocol.ItemTunnel, Task: other, Stream: 1, Host: "h", Port: 1},
		protocol.TunnelCloseItem{T: protocol.ItemTunnelClose, Task: socksTask, Stream: 8, Reason: protocol.TunnelCloseReset},
		protocol.TunnelDataItem{T: protocol.ItemTunnelData, Task: socksTask, Stream: 0, Data: []byte("x")},
	), nil)
	items := r.outgoing()
	reasons := map[uint64][]int{}
	for _, it := range items {
		if c, ok := it.(protocol.TunnelCloseItem); ok {
			reasons[c.Stream] = append(reasons[c.Stream], c.Reason)
		}
	}
	if len(reasons) != 2 || len(reasons[7]) != 1 || reasons[7][0] != protocol.TunnelCloseUnknown ||
		len(reasons[1]) != 2 || reasons[1][0] != protocol.TunnelCloseTaskEnded || reasons[1][1] != protocol.TunnelCloseTaskEnded {
		t.Errorf("closes %v", reasons)
	}
	bad := 0
	for _, it := range items {
		if e, ok := it.(protocol.ErrItem); ok && e.Code == protocol.ErrItemBadMessage && e.Item != nil && *e.Item == 4 {
			bad++
		}
	}
	if bad != 1 {
		t.Errorf("stream 0 not refused: %+v", items)
	}
	_ = core
}

// Core's cancel ends the tunnel task ok, its connections closed with it.
func TestTunnelCancel(t *testing.T) {
	r := startSocks(t, nil)
	core := newTunnelCore(t, r)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	accepted := make(chan net.Conn, 1)
	go func() {
		c, err := ln.Accept()
		if err == nil {
			accepted <- c
		}
	}()
	st := core.open(1, "127.0.0.1", ln.Addr().(*net.TCPAddr).Port, nil)
	st.eof = false
	core.until("it opens", func() bool { return st.reply != nil })
	host := <-accepted
	defer func() { _ = host.Close() }()
	r.take(coreSays(t, protocol.CancelItem{T: protocol.ItemCancel, Task: socksTask}), nil)
	end := waitEnd(t, r, 5*time.Second)
	if !end.ok {
		t.Errorf("the task ended %+v", end)
	}
	_ = host.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := host.Read(make([]byte, 1)); err == nil || errors.Is(err, os.ErrDeadlineExceeded) {
		t.Errorf("the host's connection stays open: %v", err)
	}
}

// Core's poll of 0 means at once, not none; a stream's resend due wakes
// the session even with no poll.
func TestPollZeroAndTunnelDue(t *testing.T) {
	r := startSocks(t, nil)
	r.take(coreSays(t, protocol.PollItem{T: protocol.ItemPoll, Within: 0}), nil)
	if p, ok := r.pollEvery(); !ok || p != 0 {
		t.Fatalf("poll 0: %s %v", p, ok)
	}
	r.outgoing()
	if _, ok := r.pollEvery(); ok {
		t.Fatal("a poll after the message it was for")
	}
	core := newTunnelCore(t, r)
	host, port := echoHost(t)
	st := core.open(1, host, port, nil)
	st.eof = false
	core.until("it opens", func() bool { return st.reply != nil })
	// The host's bytes, sent and never acked: a resend is due.
	r.tun.mu.Lock()
	for _, s := range r.tun.byTask[string(socksTask)].streams {
		s.out, s.sent, s.sentAt = []byte("abc"), 3, time.Now()
	}
	r.tun.mu.Unlock()
	if p, ok := r.pollEvery(); !ok || p <= 0 || p > r.tun.resend {
		t.Fatalf("a resend due: %s %v", p, ok)
	}
}

// A connect that fails says why, as SOCKS can tell its tool.
func TestDialResult(t *testing.T) {
	sys := func(e error) error {
		return &net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", e)}
	}
	for name, c := range map[string]struct {
		err  error
		want int
	}{
		"refused":     {sys(syscall.ECONNREFUSED), protocol.TunnelRefused},
		"no route":    {sys(syscall.EHOSTUNREACH), protocol.TunnelHostUnreachable},
		"no network":  {sys(syscall.ENETUNREACH), protocol.TunnelNetUnreachable},
		"no name":     {&net.OpError{Op: "dial", Err: &net.DNSError{Err: "no such host", Name: "intranet", IsNotFound: true}}, protocol.TunnelHostUnreachable},
		"timed out":   {context.DeadlineExceeded, protocol.TunnelTimedOut},
		"kernel gave": {sys(syscall.ETIMEDOUT), protocol.TunnelTimedOut},
		"other":       {errors.New("something else"), protocol.TunnelFailed},
	} {
		if got := dialResult(c.err); got != c.want {
			t.Errorf("%s: %d, want %d", name, got, c.want)
		}
	}
	if got := clip("ab\xffcdé", 5); got != "ab?cd" {
		t.Errorf("clip: %q", got)
	}
	if got := clip(strconv.Itoa(12345), 10); got != "12345" {
		t.Errorf("clip short: %q", got)
	}
}

// Nothing goes past core's room but, once the window has been shut the
// resend's time with bytes waiting, a probe: one byte past it, for core's
// answer to say its room again. What went unacked that long goes again
// from the ack.
func TestTunnelProbesAndResends(t *testing.T) {
	now := time.Now()
	st := &tstream{t: &tunnel{task: socksTask}, id: 1, conn: &net.TCPConn{}, out: bytes.Repeat([]byte{'h'}, 100), sentAt: now}
	if d := st.dataItem(now, 1<<20, time.Second); d != nil {
		t.Fatalf("past a shut window: %+v", d)
	}
	if d := st.dataItem(now.Add(time.Second), 1<<20, time.Second); d == nil || d.Off != 0 || len(d.Data) != 1 || d.EOF {
		t.Fatalf("no probe: %+v", d)
	}
	st.limit, st.sent = 1000, 60
	if d := st.dataItem(now, 1<<20, time.Second); d == nil || d.Off != 60 || len(d.Data) != 40 {
		t.Fatalf("what has not gone: %+v", d)
	}
	if d := st.dataItem(now.Add(time.Second), 1<<20, time.Second); d == nil || d.Off != 0 || len(d.Data) != 100 {
		t.Fatalf("again from the ack: %+v", d)
	}
	st.sent, st.eofOut = 100, true
	if d := st.dataItem(now, 1<<20, time.Second); d == nil || len(d.Data) != 0 || !d.EOF || d.Off != 100 {
		t.Fatalf("the eof alone: %+v", d)
	}
}

// udpEchoHost is a UDP host that sends back what it gets.
func udpEchoHost(t *testing.T) *net.UDPAddr {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pc.Close() })
	go func() {
		buf := make([]byte, 64<<10)
		for {
			n, from, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			_, _ = pc.WriteTo(buf[:n], from)
		}
	}()
	return pc.LocalAddr().(*net.UDPAddr)
}

// A UDP association is a socket of the minion's: core's datagrams go out
// of it to the destinations they name (one sent twice goes once), what
// comes back goes to core with its source; core's close closes it.
func TestTunnelCarriesDatagrams(t *testing.T) {
	r := startSocks(t, nil)
	echo := udpEchoHost(t)
	r.take(coreSays(t, protocol.TunnelItem{T: protocol.ItemTunnel, Task: socksTask, Stream: 1, UDP: true}), nil)
	var reply *protocol.TunnelReplyItem
	deadline := time.Now().Add(5 * time.Second)
	for reply == nil && time.Now().Before(deadline) {
		items := r.outgoing()
		r.delivered(items)
		for _, it := range items {
			if rep, ok := it.(protocol.TunnelReplyItem); ok {
				reply = &rep
			}
		}
		time.Sleep(2 * time.Millisecond)
	}
	if reply == nil || reply.Result != protocol.TunnelOpen || reply.Bound == "" {
		t.Fatalf("reply %+v", reply)
	}
	dgram := func(seq uint64, data string) protocol.TunnelDatagramItem {
		return protocol.TunnelDatagramItem{T: protocol.ItemTunnelDatagram, Task: socksTask, Stream: 1, Seq: seq, Host: "127.0.0.1",
			Port: uint64(echo.Port), Data: []byte(data)}
	}
	r.take(coreSays(t, dgram(0, "one"), dgram(1, "two"), dgram(1, "two"), dgram(2, "three")), nil)
	got := map[string]int{}
	var settled time.Time
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && (settled.IsZero() || time.Since(settled) < 300*time.Millisecond) {
		items := r.outgoing()
		r.delivered(items)
		for _, it := range items {
			if d, ok := it.(protocol.TunnelDatagramItem); ok {
				if d.Host != "127.0.0.1" || d.Port != uint64(echo.Port) {
					t.Fatalf("from %s:%d", d.Host, d.Port)
				}
				got[string(d.Data)]++
			}
		}
		if len(got) == 3 && settled.IsZero() {
			settled = time.Now()
		}
		time.Sleep(2 * time.Millisecond)
	}
	for _, p := range []string{"one", "two", "three"} {
		if got[p] != 1 {
			t.Errorf("%q came back %d times: %v", p, got[p], got)
		}
	}
	r.take(coreSays(t, protocol.TunnelCloseItem{T: protocol.ItemTunnelClose, Task: socksTask, Stream: 1, Reason: protocol.TunnelCloseDone}), nil)
	r.tun.mu.Lock()
	left := len(r.tun.byTask[string(socksTask)].streams)
	r.tun.mu.Unlock()
	if left != 0 {
		t.Errorf("%d streams after the close", left)
	}
}

// A forward's task is a tunnel as a socks one is: the same streams, its
// limits its own params' (max_streams is its fifth).
func TestForwardTask(t *testing.T) {
	r := newRunner(description{Ops: []uint64{opPortForward}}, &schedule{interval: time.Second})
	r.tun.resend = 100 * time.Millisecond
	r.start(protocol.TaskItem{T: protocol.ItemTask, Task: socksTask, Cmd: opPortForward,
		Args: protocol.Args{portForwardHost: "10.0.0.5", portForwardTo: int64(3389), portForwardMaxStreams: int64(1), portForwardTimeout: int64(2000)}})
	t.Cleanup(func() { r.tun.closeAll() })
	r.tun.mu.Lock()
	tn := r.tun.byTask[string(socksTask)]
	r.tun.mu.Unlock()
	if tn == nil || tn.maxStreams != 1 || tn.timeout != 2*time.Second {
		t.Fatalf("tunnel %+v", tn)
	}
	core := newTunnelCore(t, r)
	host, port := echoHost(t)
	st := core.open(1, host, port, randomBytes(64<<10))
	core.until("the stream ends done", func() bool { return st.closed != nil })
	if !bytes.Equal(st.got, st.send) {
		t.Fatalf("%d bytes back of %d", len(st.got), len(st.send))
	}
	second := core.open(2, host, port, nil)
	second.eof = false
	third := core.open(3, host, port, nil)
	core.until("both answered", func() bool { return second.reply != nil && third.reply != nil })
	if second.reply.Result != protocol.TunnelOpen || third.reply.Result != protocol.TunnelNotAllowed {
		t.Errorf("past its max_streams: %+v %+v", second.reply, third.reply)
	}
}

// A tunnel task holds no place in the minion's max tasks: with a limit of
// 1, a running SOCKS proxy still leaves room for a task, a shell none.
func TestTunnelsHoldNoPlace(t *testing.T) {
	one := uint64(1)
	r := newRunner(description{Ops: []uint64{opSocksProxy, opShell}, MaxTasks: &one}, &schedule{interval: time.Second})
	r.exec = func(ctx context.Context, tk *task, ti protocol.TaskItem) {
		if tunnelCmd(ti.Cmd) {
			r.socks(ctx, tk)
			return
		}
		<-ctx.Done()
		r.end(tk, false, nil, "cancelled")
	}
	room := func() uint64 {
		t.Helper()
		for _, it := range r.outgoing() {
			if g, ok := it.(protocol.GetTasksItem); ok {
				return *g.Max
			}
		}
		return 0
	}
	r.start(protocol.TaskItem{T: protocol.ItemTask, Task: socksTask, Cmd: opSocksProxy})
	t.Cleanup(func() { r.tun.closeAll() })
	if n := room(); n != 1 {
		t.Fatalf("beside a SOCKS proxy: room %d", n)
	}
	r.start(protocol.TaskItem{T: protocol.ItemTask, Task: bytes.Repeat([]byte{0x51}, 16), Cmd: opShell})
	if n := room(); n != 0 {
		t.Fatalf("beside a shell: room %d", n)
	}
}
