package main

import (
	"bufio"
	"bytes"
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/fxamacker/cbor/v2"
	"github.com/logoscore/logos-golang-protocol/protocol"
)

var tcpKeys = protocol.AES256CBCHMACSHA256Material{EncKey: bytes.Repeat([]byte{3}, 32), MACKey: bytes.Repeat([]byte{4}, 32)}

// tcpEndpoint is a connection through the TCP channel of kind, dialing
// addr.
func tcpEndpoint(t *testing.T, kind, addr string) connection {
	t.Helper()
	host, port, _ := net.SplitHostPort(addr)
	p, _ := strconv.Atoi(port)
	return connection{protocol.BuildConnection{Endpoint: protocol.ChannelEndpointExport{Name: "tcp-r", Kind: kind,
		Channel: protocol.ExportChannel{ModuleName: "tcp", SchemaVersion: 1},
		Params:  map[string]any{"host": host, "port": float64(p), "connect_timeout": "2s", "request_timeout": "3s"}}}}
}

// coreAnswer is core's sealed answer to the minion's frame under id: a
// message re its id with items.
func coreAnswer(t *testing.T, id [16]byte, frame []byte, items ...any) []byte {
	t.Helper()
	plain, err := protocol.OpenFrame(tcpKeys, id, protocol.FrameMinionToCore, frame)
	if err != nil {
		t.Fatal(err)
	}
	var m protocol.MinionMessage
	if err := protocol.UnmarshalCBOR(plain, &m); err != nil {
		t.Fatal(err)
	}
	return coreMessage(t, id, m.ID, nil, items...)
}

// coreMessage is a sealed core message under id: re, or one core starts
// (id).
func coreMessage(t *testing.T, id [16]byte, re, msgID []byte, items ...any) []byte {
	t.Helper()
	cm := protocol.CoreMessage{V: protocol.MinionMessageV3, Re: re, ID: msgID}
	if msgID != nil {
		cm.At = uint64(time.Now().Unix())
	}
	for _, it := range items {
		b, _ := protocol.MarshalCBOR(it)
		cm.Items = append(cm.Items, cbor.RawMessage(b))
	}
	plain, _ := protocol.MarshalCBOR(cm)
	sealed, err := protocol.SealFrame(tcpKeys, id, protocol.FrameCoreToMinion, plain)
	if err != nil {
		t.Fatal(err)
	}
	return sealed
}

// fakeTCPChannel listens, and hands each connection that comes to serve.
func fakeTCPChannel(t *testing.T, serve func(c net.Conn)) string {
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
			go serve(c)
		}
	}()
	return ln.Addr().String()
}

// A reverse call through the TCP channel: a connection for the exchange,
// core's reply under the same id; the id alone is nothing for the minion;
// a connection closed without a reply fails.
func TestTCPReverseCall(t *testing.T) {
	id := [16]byte{9}
	answer := func(c net.Conn) {
		defer func() { _ = c.Close() }()
		gotID, frame, err := readTCPFrame(bufio.NewReader(c))
		if err != nil {
			return
		}
		_ = writeTCPFrame(c, gotID[:], coreAnswer(t, gotID, frame, protocol.PollItem{T: protocol.ItemPoll, Within: 7}))
	}
	conn := tcpEndpoint(t, protocol.KindReverseCall, fakeTCPChannel(t, answer))
	got, err := exchange(conn, id, tcpKeys, protocol.GetTasksItem{T: protocol.ItemGetTasks})
	if err != nil || len(got.Items) != 1 || protocol.ItemType(got.Items[0]) != protocol.ItemPoll {
		t.Fatalf("the reply %+v %v", got, err)
	}
	nothing := tcpEndpoint(t, protocol.KindReverseCall, fakeTCPChannel(t, func(c net.Conn) {
		defer func() { _ = c.Close() }()
		gotID, _, err := readTCPFrame(bufio.NewReader(c))
		if err == nil {
			_ = writeTCPFrame(c, gotID[:], nil)
		}
	}))
	if got, err := exchange(nothing, id, tcpKeys); err != nil || len(got.Items) != 0 {
		t.Fatalf("nothing for the minion: %+v %v", got, err)
	}
	refused := tcpEndpoint(t, protocol.KindReverseCall, fakeTCPChannel(t, func(c net.Conn) {
		_, _, _ = readTCPFrame(bufio.NewReader(c))
		_ = c.Close()
	}))
	if _, err := exchange(refused, id, tcpKeys); err == nil {
		t.Fatal("a call closed without a reply worked")
	}
}

// A reverse-persistent connection through the TCP channel: exchanges over
// one connection, core's own messages pushed over it, its close lost.
func TestTCPReversePersistent(t *testing.T) {
	id := [16]byte{8}
	conns := make(chan net.Conn, 2)
	addr := fakeTCPChannel(t, func(c net.Conn) { conns <- c })
	l := newTCPDialLink(tcpEndpoint(t, protocol.KindReversePersistent, addr))
	defer l.close()
	type result struct {
		m   protocol.CoreMessage
		err error
	}
	done := make(chan result, 1)
	go func() {
		m, err := l.exchange(id, tcpKeys, protocol.GetTasksItem{T: protocol.ItemGetTasks})
		done <- result{m, err}
	}()
	channel := <-conns
	r := bufio.NewReader(channel)
	gotID, frame, err := readTCPFrame(r)
	if err != nil || gotID != id {
		t.Fatalf("the minion's frame: %v", err)
	}
	_ = writeTCPFrame(channel, id[:], coreAnswer(t, id, frame))
	if res := <-done; res.err != nil {
		t.Fatalf("the exchange: %v", res.err)
	}
	// Core pushes a task.
	_ = writeTCPFrame(channel, id[:], coreMessage(t, id, nil, protocol.NewMessageID(), protocol.CancelItem{T: protocol.ItemCancel,
		Task: bytes.Repeat([]byte{1}, 16)}))
	select {
	case <-l.pushed().readyCh():
		if m := l.pushed().take(); len(m) != 1 || len(m[0].Items) != 1 || protocol.ItemType(m[0].Items[0]) != protocol.ItemCancel {
			t.Fatalf("the pushes %+v", m)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no push")
	}
	_ = channel.Close()
	select {
	case <-l.lostCh():
	case <-time.After(5 * time.Second):
		t.Fatal("the close not lost")
	}
	// The next exchange dials again.
	go func() { _, _ = l.exchange(id, tcpKeys) }()
	select {
	case <-conns:
	case <-time.After(5 * time.Second):
		t.Fatal("not dialed again")
	}
}

// freePort is a port nothing listens on now.
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return p
}

var reverseDelegateTask = bytes.Repeat([]byte{0xdf}, 16)

// startReverseDelegate is a runner running a reverse delegate of kind,
// listening on 127.0.0.1 at a free port, which it returns once listening.
func startReverseDelegate(t *testing.T, kind string) (*runner, string) {
	t.Helper()
	r := newRunner(description{Ops: []uint64{opReverseDelegate}}, &schedule{interval: time.Second})
	port := freePort(t)
	r.start(protocol.TaskItem{T: protocol.ItemTask, Task: reverseDelegateTask, Cmd: opReverseDelegate,
		Args: protocol.Args{reverseDelegateBind: "127.0.0.1", reverseDelegateListen: int64(port), reverseDelegateKind: kind}})
	t.Cleanup(func() { r.dlg.closeAll() })
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	deadline := time.Now().Add(5 * time.Second)
	for {
		c, err := net.Dial("tcp", addr)
		if err == nil {
			_ = c.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("not listening: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	// The probe's own connection: a stray one, closed at once.
	return r, addr
}

// A reverse delegate of reverse-persistent: each minion that dials it a
// connection core opens with its first frame (serve 1, the minion's
// address), frames both ways, and its close.
func TestReverseDelegatePersistent(t *testing.T) {
	r, addr := startReverseDelegate(t, protocol.KindReversePersistent)
	minion, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = minion.Close() }()
	id := bytes.Repeat([]byte{7}, 16)
	_ = writeTCPFrame(minion, id, bytes.Repeat([]byte{1}, 80))
	var first protocol.DelegateItem
	for deadline := time.Now().Add(5 * time.Second); first.Step == 0 && time.Now().Before(deadline); {
		for _, d := range toCore(t, r) {
			if d.Step == protocol.DelegateFrame {
				first = d
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if first.Serve != 1 || first.Seq != 0 || !bytes.Equal(first.Peer, []byte{127, 0, 0, 1}) || !bytes.Equal(first.Task, reverseDelegateTask) ||
		len(first.Frame) != 80 || first.N < 1 {
		t.Fatalf("the first frame %+v", first)
	}
	n := first.N
	// Its second frame names no serve.
	_ = writeTCPFrame(minion, id, bytes.Repeat([]byte{2}, 80))
	if f := awaitCore(t, r, protocol.DelegateFrame); f.Serve != 0 || f.Seq != 1 || f.N != n {
		t.Fatalf("the second frame %+v", f)
	}
	fromCore(t, r, protocol.DelegateItem{Task: reverseDelegateTask, Step: protocol.DelegateFrame, N: n, ID: id, Frame: bytes.Repeat([]byte{5}, 90)})
	_ = minion.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, data, err := readTCPFrame(minion); err != nil || len(data) != 90 {
		t.Fatalf("core's frame at the minion: %v", err)
	}
	_ = minion.Close()
	if c := awaitCore(t, r, protocol.DelegateClose); c.N != n || c.Reason != protocol.DelegateClosed {
		t.Fatalf("the minion's close %+v", c)
	}
	// Stopped: no longer listening, ended ok.
	r.stop(reverseDelegateTask, 0)
	deadline := time.Now().Add(5 * time.Second)
	for {
		c, err := net.Dial("tcp", addr)
		if err != nil {
			break
		}
		_ = c.Close()
		if time.Now().After(deadline) {
			t.Fatal("still listening once stopped")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A reverse delegate of reverse-call: each minion's exchange a sync, core's
// reply written back (the id alone: nothing), a refusal no reply.
func TestReverseDelegateCall(t *testing.T) {
	r, addr := startReverseDelegate(t, protocol.KindReverseCall)
	id := bytes.Repeat([]byte{6}, 16)
	for _, c := range []struct {
		name   string
		status int
		frame  []byte
		ok     bool
	}{
		{"a reply", protocol.ReplyOK, bytes.Repeat([]byte{4}, 70), true},
		{"nothing", protocol.ReplyNoContent, nil, true},
		{"refused", 422, nil, false},
	} {
		minion, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		_ = writeTCPFrame(minion, id, bytes.Repeat([]byte{3}, 96))
		sync := awaitCore(t, r, protocol.DelegateSync)
		if sync.Serve != 1 || !bytes.Equal(sync.ID, id) || len(sync.Frame) != 96 || !bytes.Equal(sync.Peer, []byte{127, 0, 0, 1}) {
			t.Fatalf("%s: the sync %+v", c.name, sync)
		}
		reply := protocol.DelegateItem{Task: reverseDelegateTask, Step: protocol.DelegateReply, N: sync.N, Status: c.status}
		if c.frame != nil {
			reply.ID, reply.Frame = id, c.frame
		}
		fromCore(t, r, reply)
		_ = minion.SetReadDeadline(time.Now().Add(5 * time.Second))
		gotID, data, err := readTCPReply(bufio.NewReader(minion))
		_ = minion.Close()
		if (err == nil) != c.ok || (c.ok && (!bytes.Equal(gotID[:], id) || !bytes.Equal(data, c.frame) && len(data)+len(c.frame) > 0)) {
			t.Fatalf("%s: the reply %v %x", c.name, err, data)
		}
	}
}
