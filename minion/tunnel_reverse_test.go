package main

import (
	"bytes"
	"errors"
	"io"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/logoscore/logos-golang-protocol/protocol"
)

// startReverse is a runner running a reverse forward listening on
// 127.0.0.1, at a free port unless args say, its resends quick.
func startReverse(t *testing.T, args protocol.Args) *runner {
	t.Helper()
	r := newRunner(description{Ops: []uint64{opReverseTunnel}}, &schedule{interval: time.Second})
	r.tun.resend = 100 * time.Millisecond
	if args == nil {
		args = protocol.Args{}
	}
	if _, ok := args[reverseTunnelBind]; !ok {
		args[reverseTunnelBind] = "127.0.0.1"
	}
	if _, ok := args[reverseTunnelListen]; !ok {
		args[reverseTunnelListen] = int64(0)
	}
	r.start(protocol.TaskItem{T: protocol.ItemTask, Task: socksTask, Cmd: opReverseTunnel, Args: args})
	t.Cleanup(func() { r.tun.closeAll() })
	return r
}

// openAs answers each stream the minion opens as core would: result, and
// for an open one the stream core then has, sending send.
func openAs(result int, send func() []byte) func(protocol.TunnelItem) (protocol.TunnelReplyItem, *coreStream) {
	return func(it protocol.TunnelItem) (protocol.TunnelReplyItem, *coreStream) {
		r := protocol.TunnelReplyItem{T: protocol.ItemTunnelReply, Task: it.Task, Stream: it.Stream, Result: result}
		if result != protocol.TunnelOpen {
			return r, nil
		}
		return r, &coreStream{send: send(), eof: true}
	}
}

// dialListener connects to where the minion said it listens.
func dialListener(t *testing.T, core *tunnelCore) net.Conn {
	t.Helper()
	core.until("it listens", func() bool { return len(core.listens) > 0 })
	c, err := net.Dial("tcp", core.listens[0].Bound)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// resetSoon reports whether c's peer resets it (or closes it) within five
// seconds: a read that fails other than by the deadline.
func resetSoon(c net.Conn) bool {
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, err := c.Read(make([]byte, 1))
	return err != nil && !errors.Is(err, os.ErrDeadlineExceeded)
}

// A reverse forward listens where its args say and tells core where. A
// connection there is a stream the minion opens, numbered from 1 and
// naming who connected; core's reply opens it, and 300 KiB go each way
// through the 256 KiB windows until both ends close and the stream ends
// done. The connection waits for the reply: nothing is read before it.
func TestReverseForwardRelays(t *testing.T) {
	r := startReverse(t, nil)
	core := newTunnelCore(t, r)
	toClient := randomBytes(300 << 10)
	core.answer = openAs(protocol.TunnelOpen, func() []byte { return toClient })
	client := dialListener(t, core)
	if b := core.listens[0].Bound; !strings.HasPrefix(b, "127.0.0.1:") || strings.HasSuffix(b, ":0") {
		t.Errorf("listening at %q", b)
	}
	fromClient := randomBytes(300 << 10)
	got := make(chan []byte, 1)
	go func() {
		_, _ = client.Write(fromClient)
		_ = client.(*net.TCPConn).CloseWrite()
	}()
	go func() {
		b, _ := io.ReadAll(client)
		got <- b
	}()
	core.until("the stream ends done", func() bool { st := core.streams[1]; return st != nil && st.closed != nil })
	st := core.streams[1]
	if st.closed.Reason != protocol.TunnelCloseDone || !bytes.Equal(st.got, fromClient) || !st.eofGot {
		t.Errorf("closed %+v, core got %d bytes of %d (eof %v)", st.closed, len(st.got), len(fromClient), st.eofGot)
	}
	select {
	case b := <-got:
		if !bytes.Equal(b, toClient) {
			t.Errorf("the client got %d bytes of %d", len(b), len(toClient))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the client never got core's eof")
	}
	open := core.opens[0]
	local := client.LocalAddr().(*net.TCPAddr)
	if len(core.opens) != 1 || open.Stream != 1 || open.Host != "127.0.0.1" || open.Port != uint64(local.Port) || open.UDP {
		t.Errorf("opens %+v, the client at %s", core.opens, local)
	}
	if len(core.listens) != 1 {
		t.Errorf("listens %+v", core.listens)
	}
}

// Core's refusal resets the connection at once, and the minion forgets the
// stream; the next connection is stream 2.
func TestReverseForwardRefused(t *testing.T) {
	r := startReverse(t, nil)
	core := newTunnelCore(t, r)
	core.answer = openAs(protocol.TunnelRefused, nil)
	client := dialListener(t, core)
	core.until("core answered", func() bool { return len(core.opens) > 0 })
	if !resetSoon(client) {
		t.Fatal("the refused connection stays open")
	}
	r.tun.mu.Lock()
	n := len(r.tun.byTask[string(socksTask)].streams)
	r.tun.mu.Unlock()
	if n != 0 {
		t.Errorf("%d streams kept", n)
	}
	next, err := net.Dial("tcp", core.listens[0].Bound)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = next.Close() }()
	core.until("the next opens", func() bool { return len(core.opens) > 1 })
	if core.opens[1].Stream != 2 {
		t.Errorf("the next stream is %d", core.opens[1].Stream)
	}
}

// Past max_streams a connection is reset at once, never opened. A stream
// core does not answer is opened again every resend, and given up after
// the task's timeout and the slack: reset, and core told so.
func TestReverseForwardLimits(t *testing.T) {
	r := startReverse(t, protocol.Args{reverseTunnelMaxStreams: int64(1), reverseTunnelTimeout: int64(300)})
	r.tun.slack = 300 * time.Millisecond
	core := newTunnelCore(t, r)
	first := dialListener(t, core)
	core.until("its open goes again", func() bool { return len(core.opens) >= 2 })
	if d, due := r.tun.due(); !due || d > r.tun.resend {
		t.Errorf("the open's resend is not due: %v %v", d, due)
	}
	second, err := net.Dial("tcp", core.listens[0].Bound)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = second.Close() }()
	if !resetSoon(second) {
		t.Error("a connection past max_streams stays open")
	}
	gaveUp := func() bool {
		for _, it := range core.other {
			if c, ok := it.(protocol.TunnelCloseItem); ok && c.Stream == 1 && c.Reason == protocol.TunnelCloseReset {
				return true
			}
		}
		return false
	}
	core.until("it gives up on core", gaveUp)
	if !resetSoon(first) {
		t.Error("the connection core never answered stays open")
	}
	for _, o := range core.opens {
		if o.Stream != 1 || o.Host != core.opens[0].Host || o.Port != core.opens[0].Port {
			t.Errorf("opens %+v", core.opens)
		}
	}
}

// Core's cancel ends the task ok: the listener closes, and so does a
// connection waiting on core.
func TestReverseForwardCancel(t *testing.T) {
	r := startReverse(t, nil)
	core := newTunnelCore(t, r)
	held := dialListener(t, core)
	core.until("it opens", func() bool { return len(core.opens) > 0 })
	r.take(coreSays(t, protocol.CancelItem{T: protocol.ItemCancel, Task: socksTask}), nil)
	end := waitEnd(t, r, 5*time.Second)
	if !end.ok {
		t.Errorf("the task ended %+v", end)
	}
	if c, err := net.Dial("tcp", core.listens[0].Bound); err == nil {
		_ = c.Close()
		t.Error("the listener stays open")
	}
	if !resetSoon(held) {
		t.Error("the waiting connection stays open")
	}
}

// A listener that does not open ends the task failed, saying why, and
// nothing tells core it listens: an address in use, or a port no port is.
func TestReverseForwardListenFails(t *testing.T) {
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = taken.Close() }()
	for _, c := range []struct {
		args protocol.Args
		want string
	}{
		{protocol.Args{reverseTunnelListen: int64(taken.Addr().(*net.TCPAddr).Port)}, "listen tcp"},
		{protocol.Args{reverseTunnelListen: int64(70000)}, "listen: a port"},
	} {
		r := startReverse(t, c.args)
		end := waitEnd(t, r, 5*time.Second)
		if end.ok || !strings.Contains(end.err, c.want) {
			t.Errorf("%v: ended %+v", c.args, end)
		}
		for _, it := range r.outgoing() {
			if _, ok := it.(protocol.TunnelListenItem); ok {
				t.Errorf("%v: a tunnel_listen all the same", c.args)
			}
		}
	}
}

// A reverse forward's streams are the minion's: a tunnel from core gets a
// close, task ended (4); core's reply that opens a stream the minion does
// not have gets no such stream (3); one that refuses it, nothing.
func TestReverseForwardUnknown(t *testing.T) {
	r := startReverse(t, nil)
	r.take(coreSays(t,
		protocol.TunnelItem{T: protocol.ItemTunnel, Task: socksTask, Stream: 5, Host: "h", Port: 1},
		protocol.TunnelReplyItem{T: protocol.ItemTunnelReply, Task: socksTask, Stream: 9, Result: protocol.TunnelOpen},
		protocol.TunnelReplyItem{T: protocol.ItemTunnelReply, Task: socksTask, Stream: 10, Result: protocol.TunnelRefused},
	), nil)
	reasons := map[uint64][]int{}
	for _, it := range r.outgoing() {
		if c, ok := it.(protocol.TunnelCloseItem); ok {
			reasons[c.Stream] = append(reasons[c.Stream], c.Reason)
		}
	}
	if len(reasons) != 2 || len(reasons[5]) != 1 || reasons[5][0] != protocol.TunnelCloseTaskEnded ||
		len(reasons[9]) != 1 || reasons[9][0] != protocol.TunnelCloseUnknown {
		t.Errorf("closes %v", reasons)
	}
}

// A reverse forward holds no place in max tasks, as every tunnel: with a
// limit of 1 a shell still has room beside it.
func TestReverseForwardHoldsNoPlace(t *testing.T) {
	if !tunnelCmd(opReverseTunnel) {
		t.Fatal("not a tunnel command")
	}
	one := uint64(1)
	r := newRunner(description{Ops: []uint64{opReverseTunnel, opShell}, MaxTasks: &one}, &schedule{interval: time.Second})
	r.start(protocol.TaskItem{T: protocol.ItemTask, Task: socksTask, Cmd: opReverseTunnel,
		Args: protocol.Args{reverseTunnelBind: "127.0.0.1", reverseTunnelListen: int64(0)}})
	t.Cleanup(func() { r.tun.closeAll() })
	asked := false
	for _, it := range r.outgoing() {
		if g, ok := it.(protocol.GetTasksItem); ok {
			asked = true
			if *g.Max != 1 {
				t.Errorf("room beside a reverse forward: %d", *g.Max)
			}
		}
	}
	if !asked {
		t.Error("no get_tasks beside a reverse forward")
	}
}
