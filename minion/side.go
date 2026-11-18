package main

import (
	"fmt"
	"log"
	"time"

	"github.com/logoscore/logos-golang-protocol/protocol"
)

// Side messages (docs: contracts/minion-messages/tunnels/, On each
// connection kind: over a connection kept open, a tunnel's items go as they
// come). The session's loop sends one message at a time and waits for its
// reply: over a slow link every byte a host wrote meanwhile waited a round
// trip for it. Over a connection kept open, while the loop's message waits,
// the tunnels' and the delegates' items go in side messages of their own:
//
//   - as many waiting at once as there is to send, each within a message's
//     room, and what came over sideBatch in one: the streams' windows bound
//     the bytes on their way, and sideBatch how often one goes (a count of
//     them would only hold a slow link's bytes back for a reply);
//   - only over a socket whose first message core answered (it ties the
//     socket to the session, and its get_tasks sets the allowance);
//   - one whose reply does not come (the socket closed) goes again as it
//     was, under a new id, once the next socket's first message is
//     answered: every item in it may go again (offsets, windows, numbers);
//   - a move to another connection waits for their replies first;
//   - the pushes that came before a reply are taken before it (reply).
//
// Nothing else changes: acks, results, files and tasks go in the loop's own
// messages, one at a time, as before; and through a request (reverse-call)
// one message goes at a time.

// sideBatch is how long a side message waits for more to carry: a stream's
// ack and its host's answer, a moment later, in one (longer in tests).
var sideBatch = 5 * time.Millisecond

// sideResult is how a side message went: its items, and core's reply or
// why none came.
type sideResult struct {
	items []any
	r     reply
	err   error
}

// sideLane is a session's side messages.
type sideLane struct {
	tasks *runner
	id    [16]byte
	keys  protocol.AES256CBCHMACSHA256Material
	// done brings each side message's result; waiting is how many have not
	// come, n numbers them for the log. quit, closed when the session ends,
	// lets go of those whose results nobody takes any more.
	done    chan sideResult
	quit    chan struct{}
	waiting int
	n       int
	// retry are side messages whose replies did not come, to go again.
	retry [][]any
	// link and gen are the socket core answered the loop's last message
	// over: side messages go only over it.
	link persistentLink
	gen  uint64
	// batch, while it runs (due), gathers what goes in the next one.
	batch *time.Timer
	due   <-chan time.Time
}

func newSideLane(tasks *runner, id [16]byte, keys protocol.AES256CBCHMACSHA256Material) *sideLane {
	return &sideLane{tasks: tasks, id: id, keys: keys, done: make(chan sideResult), quit: make(chan struct{})}
}

// close ends the lane with its session: side messages still waiting give
// up their results.
func (s *sideLane) close() {
	s.stop()
	close(s.quit)
}

// linked: core answered the loop's message over the socket tr uses now
// (none, through a request).
func (s *sideLane) linked(tr *transport) {
	s.link = nil
	if tr.link == nil {
		return
	}
	if gen, open := tr.link.state(); open {
		s.link, s.gen = tr.link, gen
	}
}

// ready: a side message may go now.
func (s *sideLane) ready(tr *transport) bool {
	if s.link == nil || tr.link != s.link {
		return false
	}
	gen, open := s.link.state()
	return open && gen == s.gen
}

// poke: there may be something for a side message, which goes a batch
// later (one that runs already takes it).
func (s *sideLane) poke(tr *transport) {
	if s.due != nil || !s.ready(tr) || (len(s.retry) == 0 && !s.tasks.streamsPending()) {
		return
	}
	s.batch = time.NewTimer(sideBatch)
	s.due = s.batch.C
}

// stop drops the batch under way: the loop's own message takes what it
// gathered.
func (s *sideLane) stop() {
	if s.batch != nil {
		s.batch.Stop()
	}
	s.batch, s.due = nil, nil
}

// send sends what waits in side messages, while there is room for them:
// those to go again first.
func (s *sideLane) send(tr *transport) {
	s.stop()
	for s.ready(tr) {
		var items []any
		if len(s.retry) > 0 {
			items, s.retry = s.retry[0], s.retry[1:]
		} else if s.tasks.streamsPending() {
			items = s.tasks.streamItems()
		}
		if len(items) == 0 {
			return
		}
		s.waiting++
		link, gen := s.link, s.gen
		go func() {
			r, err := link.exchangeOn(true, gen, s.id, s.keys, items...)
			select {
			case s.done <- sideResult{items: items, r: r, err: err}:
			case <-s.quit:
			}
		}()
	}
}

// took takes a side message's result: its items to go again when no reply
// came; else the pushes that came before the reply (takeBefore), then the
// reply. An error ends the session (core closed it, or another process has
// it).
func (s *sideLane) took(res sideResult, takeBefore func(uint64) error) error {
	s.waiting--
	s.n++
	if res.err != nil {
		log.Printf("side message %d: %v; its items go again", s.n, res.err)
		s.retry = append(s.retry, res.items)
		return nil
	}
	if err := takeBefore(res.r.pushed); err != nil {
		return err
	}
	if e, ok := coreErrItem(res.r.msg); ok {
		switch e.Code {
		case protocol.ErrItemSessionClosed:
			return fmt.Errorf("side message %d: core closed the session", s.n)
		case protocol.ErrItemSessionReplaced:
			return fmt.Errorf("side message %d: %w", s.n, errReplaced)
		}
	}
	s.tasks.take(res.r.msg, res.items)
	log.Printf("side message %d: %d item(s) sent, %d back", s.n, len(res.items), len(res.r.msg.Items))
	return nil
}

// drain waits for every side message's result, and takes it.
func (s *sideLane) drain(takeBefore func(uint64) error) error {
	s.stop()
	var first error
	for s.waiting > 0 {
		if err := s.took(<-s.done, takeBefore); err != nil && first == nil {
			first = err
		}
	}
	return first
}
