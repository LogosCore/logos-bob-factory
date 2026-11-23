package main

import (
	"bytes"
	"cmp"
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
	// add, when set, is a connection to put after the others
	// (add_connection) instead of a patch to a place.
	add *protocol.HelloConnection
	// remove, when set, is the place of a connection to take out
	// (remove_connection).
	remove int
	done   chan editDone
}

// editDone is what an edit came to: what it changed, or why not.
type editDone struct {
	what string
	err  error
	// place is where an add put the connection.
	place int
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

// httpFactoryBased is the HTTP channel's mode (its version 4, httpMode) in
// which the factory decides a minion's addresses: BOB decides none.
const httpFactoryBased = "factory-based"

// connectionOf is a connection core sent in its numbered form (a
// HelloConnection: protocol.ConnectionArg) as the minion holds it; an
// error for one it cannot use: through a channel or a kind it does not
// use, or an HTTP connection without a base URL (a factory-based one).
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
	if ch == chanHTTP && (c.param(httpMode) == httpFactoryBased || c.param(httpBaseURL) == "") {
		return connection{}, errors.New("this minion connects to an HTTP connection's base URL: a factory-based one, without it, is not supported")
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
func (r *runner) applyEdit(tr *transport, req editReq) (ok bool) {
	switch {
	case req.add != nil:
		return r.applyAdd(tr, req)
	case req.remove > 0:
		return r.applyRemove(tr, req)
	}
	if req.place < 1 || req.place > len(tr.conns) {
		req.done <- editDone{err: fmt.Errorf("the minion has %d connection(s): there is no connection %d", len(tr.conns), req.place)}
		return false
	}
	was, wasName := tr.conns[req.place-1], tr.names[req.place-1]
	made := req.patch.Apply(helloConnection(was, wasName))
	now, err := connectionOf(made)
	if err == nil {
		err = tr.edit(req.place, now, made.Name)
	}
	if err != nil {
		req.done <- editDone{err: err}
		return false
	}
	what := editChanges(req.place, was, now, wasName, made.Name)
	r.mu.Lock()
	r.connUpd = &protocol.ConnectionUpdateItem{T: protocol.ItemConnectionUpdate, Connections: tr.ownConnections()}
	r.mu.Unlock()
	log.Printf("edit: %s; now on %s", what, tr.describe())
	req.done <- editDone{what: what}
	return true
}

// maxConnections bounds the minion's connections: what an add may grow
// them to.
const maxConnections = 32

// add puts c, named name ("" for none), after the connections: its place.
func (t *transport) add(c connection, name string) int {
	t.conns = append(t.conns, c)
	t.names = append(t.names, name)
	t.edited = true
	return len(t.conns)
}

// applyAdd makes an add in the session loop: the connection after the
// others, and the next message saying them again. A connection this minion
// cannot use, or one past maxConnections, changes nothing.
func (r *runner) applyAdd(tr *transport, req editReq) bool {
	c, err := connectionOf(*req.add)
	if err == nil && len(tr.conns) >= maxConnections {
		err = fmt.Errorf("no room for another: this minion holds %d connections at most", maxConnections)
	}
	if err != nil {
		req.done <- editDone{err: err}
		return false
	}
	place := tr.add(c, req.add.Name)
	what := fmt.Sprintf("connection %d: %s, %s %s", place, cmp.Or(req.add.Name, "unnamed"), req.add.Channel, req.add.Kind)
	r.mu.Lock()
	r.connUpd = &protocol.ConnectionUpdateItem{T: protocol.ItemConnectionUpdate, Connections: tr.ownConnections()}
	r.mu.Unlock()
	log.Printf("add: %s", what)
	req.done <- editDone{what: what, place: place}
	return true
}

// addConnection is the add_connection command: it hands the session the
// connection core sent, whole, and ends with what it added; with switch,
// it then moves there as switch_connection does.
func (r *runner) addConnection(ctx context.Context, t *task, args protocol.Args) {
	var hc protocol.HelloConnection
	if ok, err := protocol.ArgStructured(args, addConnectionConnection, &hc); !ok || err != nil {
		r.end(t, false, nil, fmt.Sprintf("the connection does not read: %v", err))
		return
	}
	if err := hc.Validate(); err != nil {
		r.end(t, false, nil, err.Error())
		return
	}
	req := editReq{add: &hc, done: make(chan editDone, 1)}
	var d editDone
	select {
	case r.edits <- req:
		r.nudge()
		select {
		case d = <-req.done:
		case <-ctx.Done():
			d.err = errors.New("cancelled")
		case <-time.After(switchWait):
			d.err = errors.New("the session did not get to it")
		}
	case <-ctx.Done():
		d.err = errors.New("cancelled")
	case <-time.After(switchWait):
		d.err = errors.New("another edit is under way")
	}
	if d.err != nil {
		r.end(t, false, nil, d.err.Error())
		r.nudge()
		return
	}
	r.write(t, []byte(d.what+"\n"), nil)
	if move, _ := protocol.ArgBool(args, addConnectionSwitch); move {
		// The move, as a switch: it ends the task with how it went.
		r.switchConnection(ctx, t, protocol.Args{switchConnectionConn: int64(d.place)})
		return
	}
	r.end(t, true, nil, "")
	r.nudge()
}

// remove takes out the connection at place, the ones after it moving up
// a place: the one in use (cur), and a switch's listener's, with them.
func (t *transport) remove(place int) {
	i := place - 1
	t.conns = slices.Delete(t.conns, i, i+1)
	t.names = slices.Delete(t.names, i, i+1)
	t.edited = true
	if t.cur > i {
		t.cur--
	}
	switch {
	case t.listening == place:
		t.stopListening()
	case t.listening > place:
		t.listening--
	}
}

// applyRemove makes a removal in the session loop: never the connection in
// use or the only one; the next message says the connections again, with
// the place it took out (removed), for core to move what it keeps.
func (r *runner) applyRemove(tr *transport, req editReq) bool {
	place := req.remove
	var err error
	switch {
	case place > len(tr.conns):
		err = fmt.Errorf("the minion has %d connection(s): there is no connection %d", len(tr.conns), place)
	case place == tr.place():
		err = fmt.Errorf("connection %d is in use: switch first", place)
	case len(tr.conns) == 1:
		err = fmt.Errorf("connection %d is the minion's only connection", place)
	}
	if err != nil {
		req.done <- editDone{err: err}
		return false
	}
	gone := helloConnection(tr.conns[place-1], tr.names[place-1])
	tr.remove(place)
	what := fmt.Sprintf("connection %d: %s, %s %s removed", place, cmp.Or(gone.Name, "unnamed"), gone.Channel, gone.Kind)
	r.mu.Lock()
	r.connUpd = &protocol.ConnectionUpdateItem{T: protocol.ItemConnectionUpdate, Connections: tr.ownConnections(), Removed: uint64(place)} //nolint:gosec // a small place
	r.mu.Unlock()
	log.Printf("remove: %s; now on %s", what, tr.describe())
	req.done <- editDone{what: what, place: place}
	return true
}

// removeConnection is the remove_connection command: it hands the session
// the place of a connection to take out, and ends with what it removed.
func (r *runner) removeConnection(ctx context.Context, t *task, args protocol.Args) {
	place, ok := protocol.ArgInt(args, removeConnectionConn)
	if !ok || place < 1 {
		r.end(t, false, nil, "a connection, from 1, is required")
		return
	}
	req := editReq{remove: int(place), done: make(chan editDone, 1)}
	var d editDone
	select {
	case r.edits <- req:
		r.nudge()
		select {
		case d = <-req.done:
		case <-ctx.Done():
			d.err = errors.New("cancelled")
		case <-time.After(switchWait):
			d.err = errors.New("the session did not get to it")
		}
	case <-ctx.Done():
		d.err = errors.New("cancelled")
	case <-time.After(switchWait):
		d.err = errors.New("another edit is under way")
	}
	if d.err != nil {
		r.end(t, false, nil, d.err.Error())
		r.nudge()
		return
	}
	r.write(t, []byte(d.what+"\n"), nil)
	r.end(t, true, nil, "")
	r.nudge()
}
