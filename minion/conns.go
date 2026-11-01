package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/logoscore/logos-golang-protocol/protocol"
)

// Connections (docs: contracts/channel-core-connections/, Connection
// management): the minion is built with an ordered list of them and uses
// one at a time, the first it can use when it starts. Its hello says which,
// by its place in the build's (from 1); an operator's connect moves it to
// another, which it takes only once a check-in through it worked, saying so
// in a conn_update over the new one; with a duration it moves back after it.
// The session, its keys and its schedule stay the same whichever connection
// carries it.

// transport is the build's connections and the one in use.
type transport struct {
	conns []connection
	cur   int
	// ws is the WebSocket of a reverse-persistent connection in use; nil
	// for a reverse-call one.
	ws *wsLink
}

// usable reports whether this minion can use c: an http connection of a
// reverse kind.
func usable(c connection) bool {
	k := c.ConnectionKind()
	return c.Endpoint.Channel.ModuleName == "http" && (k == protocol.KindReverseCall || k == protocol.KindReversePersistent)
}

// newTransport uses the first connection the minion can use.
func newTransport(all []protocol.BuildConnection) (*transport, error) {
	t := &transport{cur: -1}
	for _, c := range all {
		t.conns = append(t.conns, connection{c})
	}
	for i, c := range t.conns {
		if usable(c) {
			t.use(i, nil)
			return t, nil
		}
	}
	return nil, errors.New("built without an http connection")
}

// use makes connection i the one in use, with its WebSocket when it has
// one (ws: one opened already; nil opens one when needed), closing the
// previous one's.
func (t *transport) use(i int, ws *wsLink) {
	old := t.ws
	t.cur, t.ws = i, nil
	if t.conn().ConnectionKind() == protocol.KindReversePersistent {
		if ws == nil {
			ws = newWSLink(t.conn())
		}
		t.ws = ws
	}
	if old != nil && old != t.ws {
		old.close()
	}
}

// conn is the connection in use; place its place in the build's, from 1.
func (t *transport) conn() connection { return t.conns[t.cur] }
func (t *transport) place() int       { return t.cur + 1 }

// describe is the connection in use, for the log.
func (t *transport) describe() string {
	c := t.conn()
	if t.ws != nil {
		return fmt.Sprintf("connection %d, http %q (WebSocket %s)", t.place(), c.Endpoint.Name, c.wsURL())
	}
	return fmt.Sprintf("connection %d, http %q (%s %s)", t.place(), c.Endpoint.Name, c.method(), c.url())
}

// exchange sends a message through the connection in use.
func (t *transport) exchange(id [16]byte, keys protocol.AES256CBCHMACSHA256Material, items ...any) (protocol.CoreMessage, error) {
	if t.ws != nil {
		return t.ws.exchange(id, keys, items...)
	}
	return exchange(t.conn(), id, keys, items...)
}

// pushes and lost are the WebSocket's, when one is in use; nil channels,
// which never deliver, otherwise.
func (t *transport) pushes() chan protocol.CoreMessage {
	if t.ws == nil {
		return nil
	}
	return t.ws.pushes
}

func (t *transport) lost() chan struct{} {
	if t.ws == nil {
		return nil
	}
	return t.ws.lost
}

// closeLink closes the WebSocket in use, if any: the next exchange opens
// another.
func (t *transport) closeLink() {
	if t.ws != nil {
		t.ws.close()
	}
}

// check is why the minion cannot move to the connection at place: nil when
// it can.
func (t *transport) check(place int) error {
	switch {
	case place < 1 || place > len(t.conns):
		return fmt.Errorf("the build has %d connection(s): there is no connection %d", len(t.conns), place)
	case place == t.place():
		return fmt.Errorf("already on connection %d", place)
	case !usable(t.conns[place-1]):
		c := t.conns[place-1]
		return fmt.Errorf("connection %d is %s through %s, which this minion cannot use", place, c.ConnectionKind(),
			c.Endpoint.Channel.ModuleName)
	}
	return nil
}

// try moves to the connection at place once a message with items, under
// id, went through it: core's answer, and the connection in use from then
// on. When it fails nothing changes.
func (t *transport) try(place int, id [16]byte, keys protocol.AES256CBCHMACSHA256Material, items ...any) (protocol.CoreMessage, error) {
	c := t.conns[place-1]
	var ws *wsLink
	var answer protocol.CoreMessage
	var err error
	if c.ConnectionKind() == protocol.KindReversePersistent {
		ws = newWSLink(c)
		answer, err = ws.exchange(id, keys, items...)
	} else {
		answer, err = exchange(c, id, keys, items...)
	}
	if err != nil {
		if ws != nil {
			ws.close()
		}
		return answer, err
	}
	t.use(place-1, ws)
	return answer, nil
}

// switchReq is a connect task asking the session to move: to the
// connection at place, for dur (0: for good). done gets what came of it:
// nil once moved.
type switchReq struct {
	place int
	dur   time.Duration
	done  chan error
}

// revert is a move back the session owes: to the connection at place, at
// at.
type revert struct {
	place int
	at    time.Time
	after time.Duration
}

// connect is the connect command: it asks the session to move, and ends
// with what came of it.
func (r *runner) connect(ctx context.Context, t *task, args protocol.Args) {
	place, ok := protocol.ArgInt(args, connectConn)
	if !ok || place < 1 {
		r.end(t, false, nil, "a connection, from 1, is required")
		return
	}
	dur, ok := protocol.ArgDuration(args, connectFor)
	if !ok || dur < 0 {
		dur = 0
	}
	req := switchReq{place: int(place), dur: dur, done: make(chan error, 1)}
	// The session moves between check-ins; a session that ends first (it
	// starts over) never answers.
	var err error
	select {
	case r.switches <- req:
		r.nudge()
		select {
		case err = <-req.done:
		case <-ctx.Done():
			err = errors.New("cancelled")
		case <-time.After(switchWait):
			err = errors.New("the session did not get to it")
		}
	case <-ctx.Done():
		err = errors.New("cancelled")
	case <-time.After(switchWait):
		err = errors.New("another switch is under way")
	}
	if err != nil {
		r.end(t, false, nil, err.Error())
		r.nudge()
		return
	}
	text := fmt.Sprintf("switched to connection %d\n", place)
	if dur > 0 {
		text = fmt.Sprintf("switched to connection %d for %s\n", place, dur)
	}
	r.write(t, []byte(text), nil)
	r.end(t, true, nil, "")
	r.nudge()
}

// switchWait bounds how long a connect waits for the session.
const switchWait = 5 * time.Minute

// logSwitch logs a move.
func logSwitch(from, to int, why string) {
	log.Printf("moved from connection %d to %d (%s)", from, to, why)
}
