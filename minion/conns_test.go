package main

import (
	"bufio"
	"bytes"
	"context"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/logoscore/logos-golang-protocol/protocol"
)

func buildConn(module, kind, url string) protocol.BuildConnection {
	return protocol.BuildConnection{Endpoint: protocol.ChannelEndpointExport{
		Name: module + "-" + kind, Kind: kind, Channel: protocol.ExportChannel{ModuleName: module},
		Params: map[string]any{"base_url": url, "request_timeout": "2s"},
	}}
}

// The minion starts on the first connection it can use, and says why it
// cannot move where it cannot.
func TestTransportStartAndChecks(t *testing.T) {
	tr, err := newTransport([]protocol.BuildConnection{
		buildConn("dns", protocol.KindReverseCall, "x"),
		buildConn("http", protocol.KindReverseCall, "http://127.0.0.1:1"),
		buildConn("http", protocol.KindReversePersistent, "http://127.0.0.1:1"),
		buildConn("http", protocol.KindBindCall, "http://127.0.0.1:1"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if tr.place() != 2 || tr.link != nil {
		t.Fatalf("starts on %d (ws %v), want 2, a request each exchange", tr.place(), tr.link != nil)
	}
	for place, want := range map[int]string{
		0: "there is no connection 0", 5: "there is no connection 5", 2: "already on connection 2",
		1: "reverse-call through dns", 4: "bind-call through http",
	} {
		if err := tr.check(place); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("check(%d) = %v, want %q", place, err, want)
		}
	}
	if err := tr.check(3); err != nil {
		t.Errorf("check(3): %v", err)
	}
	if _, err := newTransport([]protocol.BuildConnection{buildConn("dns", protocol.KindReverseCall, "x")}); err == nil {
		t.Error("a build with no http connection started")
	}
}

// A move goes once a message went through the new connection, onto its
// WebSocket; one that does not answer changes nothing.
func TestTransportTry(t *testing.T) {
	keys := protocol.AES256CBCHMACSHA256Material{EncKey: bytes.Repeat([]byte{1}, 32), MACKey: bytes.Repeat([]byte{2}, 32)}
	ch := newWSChannel(t, keys)
	tr, err := newTransport([]protocol.BuildConnection{
		buildConn("http", protocol.KindReverseCall, "http://127.0.0.1:1"),
		buildConn("http", protocol.KindReversePersistent, "http://127.0.0.1:1"),
		buildConn("http", protocol.KindReversePersistent, ch.srv.URL),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer tr.closeLink()
	id := [16]byte{9}
	update := protocol.ConnUpdateItem{T: protocol.ItemConnUpdate, Conn: 2}
	if _, err := tr.try(2, id, keys, update); err == nil || tr.place() != 1 || tr.link != nil {
		t.Fatalf("an unreachable connection: %v, now on %d", err, tr.place())
	}
	update.Conn = 3
	if _, err := tr.try(3, id, keys, update); err != nil || tr.place() != 3 || tr.link == nil {
		t.Fatalf("a reachable one: %v, now on %d", err, tr.place())
	}
	// Later messages go over the WebSocket it opened.
	if _, err := tr.exchange(id, keys); err != nil {
		t.Fatal(err)
	}
	ch.mu.Lock()
	conns, got := ch.conns, ch.got
	ch.mu.Unlock()
	if conns != 1 || got != 2 {
		t.Fatalf("channel saw %d socket(s), %d message(s)", conns, got)
	}
}

// A switch asks the session and ends with what came of it.
func TestSwitchTask(t *testing.T) {
	r := newRunner(description{Ops: []uint64{opSwitch}}, &schedule{interval: time.Minute})
	go func() {
		req := <-r.switches
		if req.place != 2 || req.dur != 10*time.Minute {
			req.done <- errUnexpected
			return
		}
		req.done <- nil
	}()
	tk := &task{id: bytes.Repeat([]byte{1}, 16), cmd: opSwitch}
	r.switchConnection(context.Background(), tk, protocol.Args{switchConn: int64(2), switchFor: int64(600_000)})
	if !tk.ended || !tk.ok || string(tk.out) != "switched to connection 2 for 10m0s\n" {
		t.Fatalf("task %+v out %q", tk, tk.out)
	}

	go func() { (<-r.switches).done <- errUnexpected }()
	tk = &task{id: bytes.Repeat([]byte{2}, 16), cmd: opSwitch}
	r.switchConnection(context.Background(), tk, protocol.Args{switchConn: int64(1)})
	if !tk.ended || tk.ok || tk.err != errUnexpected.Error() {
		t.Fatalf("a refused switch: %+v", tk)
	}

	tk = &task{id: bytes.Repeat([]byte{3}, 16), cmd: opSwitch}
	r.switchConnection(context.Background(), tk, protocol.Args{})
	if !tk.ended || tk.ok {
		t.Fatalf("no connection named: %+v", tk)
	}
}

var errUnexpected = stringErr("unexpected")

type stringErr string

func (e stringErr) Error() string { return string(e) }

// A minion built to listen (tcp, bind-persistent) uses its listener: the
// channel dials it, it speaks first over the connection that came in, and
// core's reply comes back framed; it cannot be moved to or from it.
func TestListener(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	tcp := protocol.BuildConnection{Endpoint: protocol.ChannelEndpointExport{Name: "listener", Kind: protocol.KindBindPersistent,
		Channel: protocol.ExportChannel{ModuleName: "tcp", SchemaVersion: 1},
		Params:  map[string]any{"listen_port": float64(port), "listen_address": "127.0.0.1", "request_timeout": "5s"}}}
	tr, err := newTransport([]protocol.BuildConnection{tcp})
	if err != nil {
		t.Fatal(err)
	}
	defer tr.closeLink()
	if !tr.persistent() || tr.reconnectWait(time.Second) != 0 || tr.check(1) == nil {
		t.Fatal("a listener: persistent, no back-off, not switched to")
	}
	keys := protocol.AES256CBCHMACSHA256Material{EncKey: bytes.Repeat([]byte{1}, 32), MACKey: bytes.Repeat([]byte{2}, 32)}
	id := [16]byte{9}
	// The minion waits for the dial: its exchange goes once the channel
	// connected.
	type result struct {
		msg protocol.CoreMessage
		err error
	}
	done := make(chan result, 1)
	go func() {
		msg, err := tr.exchange(id, keys, protocol.GetTasksItem{T: protocol.ItemGetTasks})
		done <- result{msg, err}
	}()
	var c net.Conn
	for i := 0; i < 100; i++ {
		if c, err = net.Dial("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port))); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if c == nil {
		t.Fatal("the minion is not listening")
	}
	defer func() { _ = c.Close() }()
	gotID, sealed, err := readTCPFrame(bufio.NewReader(c))
	if err != nil || gotID != id {
		t.Fatalf("the minion's first frame: %v", err)
	}
	plain, err := protocol.OpenFrame(keys, id, protocol.FrameMinionToCore, sealed)
	if err != nil {
		t.Fatal(err)
	}
	var msg protocol.MinionMessage
	_ = protocol.UnmarshalCBOR(plain, &msg)
	reply, _ := protocol.MarshalCBOR(protocol.CoreMessage{V: protocol.MinionMessageV3, Re: msg.ID})
	frame, _ := protocol.SealFrame(keys, id, protocol.FrameCoreToMinion, reply)
	if err := writeTCPFrame(c, id[:], frame); err != nil {
		t.Fatal(err)
	}
	select {
	case r := <-done:
		if r.err != nil || !bytes.Equal(r.msg.Re, msg.ID) {
			t.Fatalf("the reply: %+v %v", r.msg, r.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no reply")
	}
	// The channel hangs up: the session hears it lost.
	_ = c.Close()
	select {
	case <-tr.lost():
	case <-time.After(5 * time.Second):
		t.Fatal("not lost")
	}
}

// A switch to a connection the minion listens on: its port opens while the
// minion stays where it is, the move goes through core's dial (answered),
// and from there the minion can move off it again; the port closes when it
// does.
func TestSwitchToListener(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	tcp := protocol.BuildConnection{Endpoint: protocol.ChannelEndpointExport{Name: "listener", Kind: protocol.KindBindPersistent,
		Channel: protocol.ExportChannel{ModuleName: "tcp", SchemaVersion: 1},
		Params:  map[string]any{"listen_port": float64(port), "listen_address": "127.0.0.1", "request_timeout": "5s"}}}
	tr, err := newTransport([]protocol.BuildConnection{buildConn("http", protocol.KindReverseCall, "http://127.0.0.1:1"), tcp})
	if err != nil {
		t.Fatal(err)
	}
	defer tr.closeLink()
	if tr.place() != 1 || tr.check(2) != nil {
		t.Fatalf("on %d; check(2): %v", tr.place(), tr.check(2))
	}
	keys := protocol.AES256CBCHMACSHA256Material{EncKey: bytes.Repeat([]byte{1}, 32), MACKey: bytes.Repeat([]byte{2}, 32)}
	id := [16]byte{9}
	if _, err := tr.try(2, id, keys); err == nil {
		t.Fatal("moved to a connection not listening")
	}
	link := tr.listen(2)
	var c net.Conn
	for i := 0; i < 100; i++ {
		if c, err = net.Dial("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port))); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if c == nil {
		t.Fatal("the switch did not open the port")
	}
	defer func() { _ = c.Close() }()
	select {
	case <-link.dialed():
	case <-time.After(5 * time.Second):
		t.Fatal("the dial was not seen")
	}
	// Core's side of the dial answers the minion's check-in.
	go func() {
		r := bufio.NewReader(c)
		gotID, sealed, err := readTCPFrame(r)
		if err != nil {
			return
		}
		plain, _ := protocol.OpenFrame(keys, gotID, protocol.FrameMinionToCore, sealed)
		var msg protocol.MinionMessage
		_ = protocol.UnmarshalCBOR(plain, &msg)
		reply, _ := protocol.MarshalCBOR(protocol.CoreMessage{V: protocol.MinionMessageV3, Re: msg.ID})
		frame, _ := protocol.SealFrame(keys, gotID, protocol.FrameCoreToMinion, reply)
		_ = writeTCPFrame(c, gotID[:], frame)
	}()
	update := protocol.ConnUpdateItem{T: protocol.ItemConnUpdate, Conn: 2}
	if _, err := tr.try(2, id, keys, update); err != nil || tr.place() != 2 || tr.link != link || tr.listener != nil {
		t.Fatalf("the move through the dial: %v, on %d", err, tr.place())
	}
	// Off it again: allowed now, and the port closes.
	if err := tr.check(1); err != nil {
		t.Fatalf("check(1) from the listener: %v", err)
	}
	tr.use(0, nil)
	deadline := time.Now().Add(5 * time.Second)
	for {
		d, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), 200*time.Millisecond)
		if err != nil {
			break
		}
		_ = d.Close()
		if time.Now().After(deadline) {
			t.Fatal("still listening after moving off")
		}
		time.Sleep(50 * time.Millisecond)
	}
}
