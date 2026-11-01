package main

import (
	"bytes"
	"context"
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
	if tr.place() != 2 || tr.ws != nil {
		t.Fatalf("starts on %d (ws %v), want 2, a request each exchange", tr.place(), tr.ws != nil)
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
	if _, err := tr.try(2, id, keys, update); err == nil || tr.place() != 1 || tr.ws != nil {
		t.Fatalf("an unreachable connection: %v, now on %d", err, tr.place())
	}
	update.Conn = 3
	if _, err := tr.try(3, id, keys, update); err != nil || tr.place() != 3 || tr.ws == nil {
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

// A connect asks the session and ends with what came of it.
func TestConnectTask(t *testing.T) {
	r := newRunner(description{Ops: []uint64{opConnect}}, &schedule{interval: time.Minute})
	go func() {
		req := <-r.switches
		if req.place != 2 || req.dur != 10*time.Minute {
			req.done <- errUnexpected
			return
		}
		req.done <- nil
	}()
	tk := &task{id: bytes.Repeat([]byte{1}, 16), cmd: opConnect}
	r.connect(context.Background(), tk, protocol.Args{connectConn: int64(2), connectFor: int64(600_000)})
	if !tk.ended || !tk.ok || string(tk.out) != "switched to connection 2 for 10m0s\n" {
		t.Fatalf("task %+v out %q", tk, tk.out)
	}

	go func() { (<-r.switches).done <- errUnexpected }()
	tk = &task{id: bytes.Repeat([]byte{2}, 16), cmd: opConnect}
	r.connect(context.Background(), tk, protocol.Args{connectConn: int64(1)})
	if !tk.ended || tk.ok || tk.err != errUnexpected.Error() {
		t.Fatalf("a refused switch: %+v", tk)
	}

	tk = &task{id: bytes.Repeat([]byte{3}, 16), cmd: opConnect}
	r.connect(context.Background(), tk, protocol.Args{})
	if !tk.ended || tk.ok {
		t.Fatalf("no connection named: %+v", tk)
	}
}

var errUnexpected = stringErr("unexpected")

type stringErr string

func (e stringErr) Error() string { return string(e) }
