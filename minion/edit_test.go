package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/logoscore/logos-golang-protocol/protocol"
)

// editArgs are an edit task's args as they come: a task item core sent,
// decoded.
func editArgs(t *testing.T, place int64, hc protocol.ConnectionPatch) protocol.Args {
	t.Helper()
	b, err := protocol.MarshalCBOR(protocol.TaskItem{T: protocol.ItemTask, Task: make([]byte, 16), Cmd: opEditConnection,
		Args: protocol.Args{editConnectionConn: place, editConnectionConnection: hc}})
	if err != nil {
		t.Fatal(err)
	}
	var ti protocol.TaskItem
	if err := protocol.UnmarshalCBOR(b, &ti); err != nil {
		t.Fatal(err)
	}
	return ti.Args
}

// A connection core sends is one the minion holds when it speaks its
// channel and uses its kind, and says back as it came.
func TestConnectionOf(t *testing.T) {
	auto := true
	hc := protocol.HelloConnection{Channel: "http", Kind: protocol.KindReversePersistent, Name: "ws", Version: 3,
		Params: map[uint64]any{httpBaseURL: "http://127.0.0.1:1"}, Values: map[uint64]any{1: "/cdn/"},
		Failover: &protocol.HelloConnectionFailover{Auto: &auto, FailAfter: 3}, MaxMessage: 4096}
	c, err := connectionOf(hc)
	if err != nil || c.Channel != chanHTTP || c.Kind != protocol.KindNumReversePersistent || c.param(httpBaseURL) != "http://127.0.0.1:1" ||
		c.Failover == nil || c.Failover.FailAfter != 3 || c.MaxMessage != 4096 {
		t.Fatalf("%+v %v", c, err)
	}
	back := helloConnection(c, "ws")
	if back.Channel != "http" || back.Kind != protocol.KindReversePersistent || back.Name != "ws" || back.Version != 3 ||
		back.Params[httpBaseURL] != "http://127.0.0.1:1" || back.Failover.FailAfter != 3 || back.Validate() != nil {
		t.Fatalf("said back: %+v", back)
	}
	for _, bad := range []struct {
		hc   protocol.HelloConnection
		says string
	}{
		{protocol.HelloConnection{Channel: "dns", Kind: protocol.KindReverseCall}, "this minion speaks no dns"},
		{protocol.HelloConnection{Channel: "http", Kind: protocol.KindBindCall}, "does not use bind-call through http"},
	} {
		if _, err := connectionOf(bad.hc); err == nil || !strings.Contains(err.Error(), bad.says) {
			t.Errorf("%+v: %v", bad.hc, err)
		}
	}
}

// An edit replaces the connection at its place, keeping the others; the
// one in use takes the new settings, its link the new kind's; the next
// message says the connections, each named as the minion knows it.
func TestEditConnection(t *testing.T) {
	tr, err := newTransport([]protocol.MinionConnection{
		buildConn("http", protocol.KindReverseCall, "http://127.0.0.1:1"),
		buildConn("http", protocol.KindReverseCall, "http://127.0.0.1:2"),
	})
	if err != nil {
		t.Fatal(err)
	}
	r := newRunner(description{Ops: []uint64{opEditConnection}}, &schedule{interval: time.Minute})
	n := byte(0)
	edit := func(place int64, p protocol.ConnectionPatch) *task {
		t.Helper()
		go func() { r.applyEdit(tr, <-r.edits) }()
		n++
		tk := &task{id: bytes.Repeat([]byte{n}, 16), cmd: opEditConnection}
		r.editConnection(context.Background(), tk, editArgs(t, place, p))
		return tk
	}

	// Another one's path alone: the rest of it kept.
	tk := edit(2, protocol.ConnectionPatch{Values: map[uint64]any{paramPathPrefix: "/new/2"}})
	if !tk.ok || string(tk.out) != "connection 2: values 2 changed\n" || tr.conns[1].param(httpBaseURL) != "http://127.0.0.1:2" ||
		tr.conns[1].Values[paramPathPrefix] != "/new/2" || tr.place() != 1 {
		t.Fatalf("a path: %+v out %q, %v", tk, tk.out, tr.conns[1].Values)
	}
	// A param gone (null).
	if tk = edit(2, protocol.ConnectionPatch{Params: map[uint64]any{httpRequestTimeout: nil}}); !tk.ok {
		t.Fatalf("a param gone: %+v", tk)
	}
	if _, ok := tr.conns[1].Params[httpRequestTimeout]; ok || tr.conns[1].param(httpBaseURL) != "http://127.0.0.1:2" {
		t.Fatalf("after a param gone: %v", tr.conns[1].Params)
	}

	// The one in use, to a WebSocket at another address: whole, as its
	// kind changes, and a link of its own.
	ws := protocol.HelloConnection{Channel: "http", Kind: protocol.KindReversePersistent, Name: "ws", Version: 3,
		Params: map[uint64]any{httpBaseURL: "http://127.0.0.1:9", httpRequestTimeout: int64(2000)}}
	tk = edit(1, protocol.DiffConnection(helloConnection(tr.conns[0], ""), ws))
	if !tk.ended || !tk.ok || string(tk.out) != "connection 1: kind, name, version, params 1 changed\n" {
		t.Fatalf("task %+v out %q", tk, tk.out)
	}
	if tr.place() != 1 || !tr.conn().persists() || tr.conn().param(httpBaseURL) != "http://127.0.0.1:9" || !tr.persistent() || !tr.edited {
		t.Fatalf("in use: %s, persistent %v", tr.describe(), tr.persistent())
	}
	if tr.conns[1].param(httpBaseURL) != "http://127.0.0.1:2" {
		t.Fatalf("the other changed: %s", tr.conns[1].param(httpBaseURL))
	}
	items := r.outgoing()
	var upd *protocol.ConnectionUpdateItem
	for _, it := range items {
		if u, ok := it.(protocol.ConnectionUpdateItem); ok {
			upd = &u
		}
	}
	if upd == nil || len(upd.Connections) != 2 || upd.Connections[0].Name != "ws" || upd.Connections[1].Name != "" ||
		upd.Connections[0].Kind != protocol.KindReversePersistent || upd.Validate() != nil {
		t.Fatalf("the update: %+v", upd)
	}
	tr.closeLink()

	// Refused, nothing changed: a place past them, a channel it does not
	// speak.
	if tk := edit(3, protocol.ConnectionPatch{Name: "x"}); tk.ok || !strings.Contains(tk.err, "there is no connection 3") {
		t.Fatalf("past them: %+v", tk)
	}
	tk = edit(2, protocol.ConnectionPatch{Channel: "dns", Kind: protocol.KindReverseCall})
	if tk.ok || tk.err != "this minion speaks no dns" || tr.conns[1].param(httpBaseURL) != "http://127.0.0.1:2" {
		t.Fatalf("another channel: %+v", tk)
	}
	// Not a patch: a channel without a kind.
	tk = &task{id: bytes.Repeat([]byte{99}, 16), cmd: opEditConnection}
	r.editConnection(context.Background(), tk, editArgs(t, 2, protocol.ConnectionPatch{Channel: "http"}))
	if tk.ok || !strings.Contains(tk.err, "a channel and a kind together") {
		t.Fatalf("half a connection: %+v", tk)
	}
}
