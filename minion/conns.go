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
// by its place in the build's (from 1); an operator's switch moves it to
// another, which it takes only once a check-in through it worked, saying so
// in a conn_update over the new one; with a duration it moves back after it.
// A connection it listens on (bind-persistent) cannot be checked in through
// until core dials it: a switch to one opens its port (listen), the session
// goes on through the connection in use meanwhile, and the move goes through
// the dial once it comes; no dial in listenWait closes the port again.
// The session, its keys and its schedule stay the same whichever connection
// carries it.

// transport is the build's connections and the one in use.
type transport struct {
	conns []connection
	cur   int
	// link is the persistent connection in use: a reverse-persistent one's
	// WebSocket, a bind-persistent one's listener; nil for a reverse-call
	// one.
	link persistentLink
	// listener is a bind-persistent connection's listener a switch opened,
	// waiting for core's dial (at place listening); nil when none.
	listener  *tcpLink
	listening int
}

// persistentLink is a connection kept open, which core pushes over.
type persistentLink interface {
	exchange(id [16]byte, keys protocol.AES256CBCHMACSHA256Material, items ...any) (protocol.CoreMessage, error)
	pushesCh() chan protocol.CoreMessage
	lostCh() chan struct{}
	close()
}

// usable reports whether this minion can use c: an http connection of a
// reverse kind, or a tcp one it listens on (bind-persistent).
func usable(c connection) bool {
	k := c.ConnectionKind()
	switch c.Endpoint.Channel.ModuleName {
	case "http":
		return k == protocol.KindReverseCall || k == protocol.KindReversePersistent
	case "tcp":
		return k == protocol.KindBindPersistent
	}
	return false
}

// bound reports whether c is one the minion is reached through (it
// listens) rather than one it reaches out through.
func (c connection) bound() bool { return c.ConnectionKind() == protocol.KindBindPersistent }

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
	return nil, errors.New("built without a connection this minion can use (http, or tcp to listen on)")
}

// use makes connection i the one in use, with its WebSocket when it has
// one (ws: one opened already; nil opens one when needed), closing the
// previous one's.
func (t *transport) use(i int, ws *wsLink) {
	old := t.link
	t.cur, t.link = i, nil
	switch t.conn().ConnectionKind() {
	case protocol.KindReversePersistent:
		if ws == nil {
			ws = newWSLink(t.conn())
		}
		t.link = ws
	case protocol.KindBindPersistent:
		if t.listener != nil && t.listening == i+1 {
			// The one a switch opened, which core dialed.
			t.link, t.listener, t.listening = t.listener, nil, 0
		} else {
			t.link = newTCPLink(t.conn())
		}
	}
	if old != nil && old != t.link {
		old.close()
	}
}

// conn is the connection in use; place its place in the build's, from 1.
func (t *transport) conn() connection { return t.conns[t.cur] }
func (t *transport) place() int       { return t.cur + 1 }

// describe is the connection in use, for the log.
func (t *transport) describe() string {
	c := t.conn()
	if c.bound() {
		return fmt.Sprintf("connection %d, tcp %q (listening on %s)", t.place(), c.Endpoint.Name, c.listenAddr())
	}
	if c.ConnectionKind() == protocol.KindReversePersistent {
		return fmt.Sprintf("connection %d, http %q (WebSocket %s)", t.place(), c.Endpoint.Name, c.wsURL())
	}
	return fmt.Sprintf("connection %d, http %q (%s %s)", t.place(), c.Endpoint.Name, c.method(), c.url())
}

// exchange sends a message through the connection in use.
func (t *transport) exchange(id [16]byte, keys protocol.AES256CBCHMACSHA256Material, items ...any) (protocol.CoreMessage, error) {
	if t.link != nil {
		return t.link.exchange(id, keys, items...)
	}
	return exchange(t.conn(), id, keys, items...)
}

// pushes and lost are the WebSocket's, when one is in use; nil channels,
// which never deliver, otherwise.
func (t *transport) pushes() chan protocol.CoreMessage {
	if t.link == nil {
		return nil
	}
	return t.link.pushesCh()
}

func (t *transport) lost() chan struct{} {
	if t.link == nil {
		return nil
	}
	return t.link.lostCh()
}

// persistent reports whether the connection in use is kept open.
func (t *transport) persistent() bool { return t.link != nil }

// reconnectWait is how long to wait before the next exchange after the
// connection in use closed or failed: backoff for a WebSocket the minion
// opens again; none for a listener, whose next exchange waits for the
// channel's dial.
func (t *transport) reconnectWait(backoff time.Duration) time.Duration {
	if t.conn().bound() {
		return 0
	}
	return backoff
}

// closeLink closes the WebSocket in use, if any: the next exchange opens
// another.
func (t *transport) closeLink() {
	t.stopListening()
	if t.link != nil {
		t.link.close()
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
	if c.bound() {
		// Through core's dial on the listener a switch opened: one that
		// does not answer leaves it listening.
		if t.listener == nil || t.listening != place {
			return answer, fmt.Errorf("connection %d is not listening", place)
		}
		if answer, err = t.listener.exchange(id, keys, items...); err != nil {
			return answer, err
		}
		t.use(place-1, nil)
		return answer, nil
	}
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

// listen opens the port of the connection at place, a bind-persistent one,
// for a switch to it: core's dial comes on dialed. One open already is
// kept.
func (t *transport) listen(place int) *tcpLink {
	if t.listener == nil || t.listening != place {
		t.stopListening()
		t.listener, t.listening = newTCPLink(t.conns[place-1]), place
	}
	return t.listener
}

// stopListening closes a listener a switch opened and core never dialed.
func (t *transport) stopListening() {
	if t.listener != nil {
		t.listener.close()
		t.listener, t.listening = nil, 0
	}
}

// listenWait is how long a switch to a connection the minion listens on
// waits for core's dial.
var listenWait = 10 * time.Minute

// switchReq is a connect task asking the session to move: to the
// connection at place, for dur (0: for good). done gets what came of it:
// nil once moved.
type switchReq struct {
	place int
	dur   time.Duration
	done  chan error
	// listening, when set, is told the port a switch to a connection the
	// minion listens on opened: the task says it, which is core's cue to
	// dial (it then waits for done, up to listenWait more).
	listening func(addr string)
	// ctx is the task's: a switch it stops no longer waits for a dial.
	ctx context.Context
}

// revert is a move back the session owes: to the connection at place, at
// at.
type revert struct {
	place int
	at    time.Time
	after time.Duration
}

// switchConnection is the switch command: it asks the session to move, and ends
// with what came of it.
func (r *runner) switchConnection(ctx context.Context, t *task, args protocol.Args) {
	place, ok := protocol.ArgInt(args, switchConn)
	if !ok || place < 1 {
		r.end(t, false, nil, "a connection, from 1, is required")
		return
	}
	dur, ok := protocol.ArgDuration(args, switchFor)
	if !ok || dur < 0 {
		dur = 0
	}
	req := switchReq{place: int(place), dur: dur, done: make(chan error, 1), ctx: ctx, listening: func(addr string) {
		r.write(t, []byte(fmt.Sprintf("listening on %s: waiting for core to dial connection %d (up to %s)\n", addr, place, listenWait)), nil)
		r.nudge()
	}}
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
		case <-time.After(switchWait + listenWait):
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
