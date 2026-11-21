package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"maps"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/logoscore/logos-golang-protocol/protocol"
)

// Editing a connection (docs: contracts/task-results-and-actions.md,
// edit_connection): the edit_connection command puts a connection core sends, whole,
// at a place in the minion's connections, keeping the others and their
// order, and the minion says its connections again with a
// connection_update. The connection in use goes on with the new settings
// from the next exchange, which carries the update: a WebSocket or a TCP
// connection it keeps open closes, and the next exchange opens one by the
// new settings (a listener listens where the new ones say, and waits for
// core's dial there). An edit lives as long as the process: a restarted
// minion has its build's connections again.

// editReq is an edit task asking the session to apply patch to the
// connection at place. done gets what came of it.
type editReq struct {
	place int
	patch protocol.ConnectionPatch
	done  chan editDone
}

// editDone is what an edit came to: what it changed, or why not.
type editDone struct {
	what string
	err  error
}

// channelNames are the channels BOB's schema numbers (fields.go), by the
// module name core gives a connection's channel.
var channelNames = map[string]uint64{"http": chanHTTP, "tcp": chanTCP}

// channelName is a channel's module name; "" for one BOB does not number.
func channelName(n uint64) string {
	for name, num := range channelNames {
		if num == n {
			return name
		}
	}
	return ""
}

// connectionOf is a connection core sent in its numbered form (a
// HelloConnection: protocol.ConnectionArg) as the minion holds it; an
// error for one it cannot use.
func connectionOf(hc protocol.HelloConnection) (connection, error) {
	ch, ok := channelNames[hc.Channel]
	if !ok {
		return connection{}, fmt.Errorf("this minion speaks no %s", hc.Channel)
	}
	c := connection{protocol.MinionConnection{Channel: ch, Version: hc.Version, Kind: protocol.KindNumber(hc.Kind),
		Params: protocol.Args(hc.Params), Values: protocol.Args(hc.Values), MaxMessage: hc.MaxMessage}}
	if hc.Failover != nil {
		c.Failover = &protocol.MinionConnectionFailover{Auto: hc.Failover.Auto, FailAfter: hc.Failover.FailAfter}
	}
	if !usable(c) {
		return connection{}, fmt.Errorf("this minion does not use %s through %s", hc.Kind, hc.Channel)
	}
	return c, nil
}

// helloConnection is c as a hello or a connection_update says it, named
// name ("": core keeps the name it has at the place).
func helloConnection(c connection, name string) protocol.HelloConnection {
	kind, _ := protocol.KindOfNumber(c.Kind)
	hc := protocol.HelloConnection{Channel: channelName(c.Channel), Kind: kind, Name: name, Version: c.Version,
		Params: c.Params, Values: c.Values, MaxMessage: c.MaxMessage}
	if c.Failover != nil {
		hc.Failover = &protocol.HelloConnectionFailover{Auto: c.Failover.Auto, FailAfter: c.Failover.FailAfter}
	}
	return hc
}

// ownConnections are the minion's connections as it says them.
func (t *transport) ownConnections() []protocol.HelloConnection {
	out := make([]protocol.HelloConnection, len(t.conns))
	for i, c := range t.conns {
		out[i] = helloConnection(c, t.names[i])
	}
	return out
}

// edit puts c, named name, at place; the connection in use takes the new
// settings at its next exchange. It changes nothing when place is past
// the connections.
func (t *transport) edit(place int, c connection, name string) error {
	if place < 1 || place > len(t.conns) {
		return fmt.Errorf("the minion has %d connection(s): there is no connection %d", len(t.conns), place)
	}
	t.conns[place-1], t.names[place-1], t.edited = c, name, true
	if t.listening == place {
		// A switch's port, opened by the old settings.
		t.stopListening()
	}
	if place == t.place() {
		// Its link, kept open by the old settings, closes: the next
		// exchange opens one by the new.
		t.use(place-1, nil)
	}
	return nil
}

// editChanges says what an edit changed of was, for its task's output:
// "connection 2: kind, params 1 and 3 changed".
func editChanges(place int, was, now connection, wasName, name string) string {
	var changed []string
	if was.Channel != now.Channel {
		changed = append(changed, "channel")
	}
	if was.Kind != now.Kind {
		changed = append(changed, "kind")
	}
	if name != "" && name != wasName {
		changed = append(changed, "name")
	}
	if was.Version != now.Version {
		changed = append(changed, "version")
	}
	for _, f := range []struct {
		what     string
		was, now protocol.Args
	}{{"params", was.Params, now.Params}, {"values", was.Values, now.Values}} {
		if nums := changedNums(f.was, f.now); len(nums) > 0 {
			changed = append(changed, f.what+" "+joinNums(nums))
		}
	}
	if !reflect.DeepEqual(was.Failover, now.Failover) {
		changed = append(changed, "failover")
	}
	if was.MaxMessage != now.MaxMessage {
		changed = append(changed, "max_message")
	}
	if len(changed) == 0 {
		return fmt.Sprintf("connection %d: nothing changed", place)
	}
	return fmt.Sprintf("connection %d: %s changed", place, strings.Join(changed, ", "))
}

// changedNums are the numbers whose values differ between a and b, in
// order: by their CBOR, as they travel (an int and a uint64 of one value
// are the same).
func changedNums(a, b protocol.Args) []uint64 {
	same := func(x, y any) bool {
		bx, ex := protocol.MarshalCBOR(x)
		by, ey := protocol.MarshalCBOR(y)
		return ex == nil && ey == nil && bytes.Equal(bx, by)
	}
	var nums []uint64
	for _, n := range slices.Sorted(maps.Keys(a)) {
		if v, ok := b[n]; !ok || !same(v, a[n]) {
			nums = append(nums, n)
		}
	}
	for _, n := range slices.Sorted(maps.Keys(b)) {
		if _, ok := a[n]; !ok {
			nums = append(nums, n)
		}
	}
	slices.Sort(nums)
	return nums
}

// joinNums is "1", "1 and 3", "1, 2 and 3".
func joinNums(nums []uint64) string {
	s := make([]string, len(nums))
	for i, n := range nums {
		s[i] = fmt.Sprint(n)
	}
	if len(s) == 1 {
		return s[0]
	}
	return strings.Join(s[:len(s)-1], ", ") + " and " + s[len(s)-1]
}

// editConnection is the edit_connection command: it hands the session what
// core sent of a place's connection, what changed of it (a
// protocol.ConnectionPatch), and ends with what that was.
func (r *runner) editConnection(ctx context.Context, t *task, args protocol.Args) {
	place, ok := protocol.ArgInt(args, editConnectionConn)
	if !ok || place < 1 {
		r.end(t, false, nil, "a connection, from 1, is required")
		return
	}
	var patch protocol.ConnectionPatch
	if ok, err := protocol.ArgStructured(args, editConnectionConnection, &patch); !ok || err != nil {
		r.end(t, false, nil, fmt.Sprintf("the connection does not read: %v", err))
		return
	}
	if err := patch.Validate(); err != nil {
		r.end(t, false, nil, err.Error())
		return
	}
	req := editReq{place: int(place), patch: patch, done: make(chan editDone, 1)}
	var err error
	// The session edits between check-ins; a session that ends first (it
	// starts over) never answers.
	var what string
	select {
	case r.edits <- req:
		r.nudge()
		select {
		case d := <-req.done:
			what, err = d.what, d.err
		case <-ctx.Done():
			err = errors.New("cancelled")
		case <-time.After(switchWait):
			err = errors.New("the session did not get to it")
		}
	case <-ctx.Done():
		err = errors.New("cancelled")
	case <-time.After(switchWait):
		err = errors.New("another edit is under way")
	}
	if err != nil {
		r.end(t, false, nil, err.Error())
		r.nudge()
		return
	}
	r.write(t, []byte(what+"\n"), nil)
	r.end(t, true, nil, "")
	r.nudge()
}

// applyEdit makes an edit in the session loop: the patch applied to the
// connection at its place (whole in its place when it is one), and the next
// message saying the connections again, whole. A connection this minion
// cannot use changes nothing.
func (r *runner) applyEdit(tr *transport, req editReq) {
	if req.place < 1 || req.place > len(tr.conns) {
		req.done <- editDone{err: fmt.Errorf("the minion has %d connection(s): there is no connection %d", len(tr.conns), req.place)}
		return
	}
	was, wasName := tr.conns[req.place-1], tr.names[req.place-1]
	made := req.patch.Apply(helloConnection(was, wasName))
	now, err := connectionOf(made)
	if err == nil {
		err = tr.edit(req.place, now, made.Name)
	}
	if err != nil {
		req.done <- editDone{err: err}
		return
	}
	what := editChanges(req.place, was, now, wasName, made.Name)
	r.mu.Lock()
	r.connUpd = &protocol.ConnectionUpdateItem{T: protocol.ItemConnectionUpdate, Connections: tr.ownConnections()}
	r.mu.Unlock()
	log.Printf("edit: %s; now on %s", what, tr.describe())
	req.done <- editDone{what: what}
}
