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

// An add puts the connection after the others, the one in use kept, and
// the next message says them all; one the minion cannot use is refused;
// with switch, the task then moves there as a switch does.
func TestAddConnection(t *testing.T) {
	tr, err := newTransport([]protocol.MinionConnection{buildConn("http", protocol.KindReverseCall, "http://127.0.0.1:1")})
	if err != nil {
		t.Fatal(err)
	}
	r := newRunner(description{Ops: []uint64{opAddConnection}}, &schedule{interval: time.Minute})
	n := byte(0)
	add := func(hc protocol.HelloConnection, move bool) *task {
		t.Helper()
		go func() { r.applyAdd(tr, <-r.edits) }()
		n++
		b, err := protocol.MarshalCBOR(protocol.TaskItem{T: protocol.ItemTask, Task: make([]byte, 16), Cmd: opAddConnection,
			Args: protocol.Args{addConnectionConnection: hc, addConnectionSwitch: move}})
		if err != nil {
			t.Fatal(err)
		}
		var ti protocol.TaskItem
		if err := protocol.UnmarshalCBOR(b, &ti); err != nil {
			t.Fatal(err)
		}
		tk := &task{id: bytes.Repeat([]byte{n}, 16), cmd: opAddConnection}
		r.addConnection(context.Background(), tk, ti.Args)
		return tk
	}
	ws := protocol.HelloConnection{Channel: "http", Kind: protocol.KindReversePersistent, Name: "ws",
		Params: map[uint64]any{httpBaseURL: "http://127.0.0.1:9"}}

	tk := add(ws, false)
	if !tk.ok || string(tk.out) != "connection 2: ws, http reverse-persistent\n" || len(tr.conns) != 2 || tr.place() != 1 ||
		tr.conns[1].param(httpBaseURL) != "http://127.0.0.1:9" || !tr.edited {
		t.Fatalf("task %+v out %q, %d connections, on %d", tk, tk.out, len(tr.conns), tr.place())
	}
	var upd *protocol.ConnectionUpdateItem
	for _, it := range r.outgoing() {
		if u, ok := it.(protocol.ConnectionUpdateItem); ok {
			upd = &u
		}
	}
	if upd == nil || len(upd.Connections) != 2 || upd.Connections[1].Name != "ws" || upd.Validate() != nil {
		t.Fatalf("the update: %+v", upd)
	}

	if tk := add(protocol.HelloConnection{Channel: "dns", Kind: protocol.KindReverseCall}, false); tk.ok || tk.err != "this minion speaks no dns" || len(tr.conns) != 2 {
		t.Fatalf("another channel: %+v, %d connections", tk, len(tr.conns))
	}

	// With switch: the move asked for the new place.
	go func() {
		req := <-r.switches
		if req.place != 3 {
			req.done <- errUnexpected
			return
		}
		req.done <- nil
	}()
	ws.Name = "ws2"
	if tk := add(ws, true); !tk.ok || !strings.Contains(string(tk.out), "connection 3: ws2") || !strings.Contains(string(tk.out), "switched to connection 3") {
		t.Fatalf("with switch: %+v out %q", tk, tk.out)
	}
}

// A removal takes the connection out, the ones after it moving up (the one
// in use with them), and the next message says the list with the place it
// took out; never the one in use, the only one, or a place past them.
func TestRemoveConnection(t *testing.T) {
	tr, err := newTransport([]protocol.MinionConnection{
		buildConn("http", protocol.KindReverseCall, "http://127.0.0.1:1"),
		buildConn("http", protocol.KindReverseCall, "http://127.0.0.1:2"),
		buildConn("http", protocol.KindReverseCall, "http://127.0.0.1:3"),
	})
	if err != nil {
		t.Fatal(err)
	}
	tr.use(2, nil) // on the third
	r := newRunner(description{Ops: []uint64{opRemoveConnection}}, &schedule{interval: time.Minute})
	n := byte(0)
	remove := func(place int64) *task {
		t.Helper()
		go func() { r.applyEdit(tr, <-r.edits) }()
		n++
		tk := &task{id: bytes.Repeat([]byte{n}, 16), cmd: opRemoveConnection}
		r.removeConnection(context.Background(), tk, protocol.Args{removeConnectionConn: place})
		return tk
	}

	for place, want := range map[int64]string{3: "is in use: switch first", 4: "there is no connection 4"} {
		if tk := remove(place); tk.ok || !strings.Contains(tk.err, want) || len(tr.conns) != 3 {
			t.Fatalf("connection %d: %+v", place, tk)
		}
	}
	tk := remove(1)
	if !tk.ok || string(tk.out) != "connection 1: unnamed, http reverse-call removed\n" || len(tr.conns) != 2 || tr.place() != 2 ||
		tr.conn().param(httpBaseURL) != "http://127.0.0.1:3" || tr.conns[0].param(httpBaseURL) != "http://127.0.0.1:2" {
		t.Fatalf("task %+v out %q, %d connections, on %d", tk, tk.out, len(tr.conns), tr.place())
	}
	var upd *protocol.ConnectionUpdateItem
	for _, it := range r.outgoing() {
		if u, ok := it.(protocol.ConnectionUpdateItem); ok {
			upd = &u
		}
	}
	if upd == nil || len(upd.Connections) != 2 || upd.Removed != 1 || upd.Validate() != nil {
		t.Fatalf("the update: %+v", upd)
	}
	if tk := remove(1); !tk.ok || len(tr.conns) != 1 || tr.place() != 1 {
		t.Fatalf("the other: %+v, %d connections, on %d", tk, len(tr.conns), tr.place())
	}
	tr.use(0, nil)
	if tk := remove(1); tk.ok || !strings.Contains(tk.err, "is in use") {
		t.Fatalf("the only one, in use: %+v", tk)
	}
}

// Failover's last connection tried moves with a removal.
func TestFailoverFollowsARemoval(t *testing.T) {
	f := &failover{tried: 3}
	f.removed(1)
	if f.tried != 2 {
		t.Fatalf("tried %d after one before it went", f.tried)
	}
	f.removed(2)
	if f.tried != 0 {
		t.Fatalf("tried %d after it went", f.tried)
	}
}
