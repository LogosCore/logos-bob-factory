package main

import (
	"sync"
	"sync/atomic"

	"github.com/logoscore/logos-golang-protocol/protocol"
)

// pushQueue holds the messages core starts over a connection kept open
// until the session's loop takes them. It takes as many as come: the
// connection's reader, which fills it, must never wait on the loop, which
// may itself be waiting on the reader for a reply (with a bounded buffer,
// a burst of pushes behind a slow round trip, as through delegates, left
// the reply unread and the connection's close unseen). What core pushes is
// bounded by core: tasks within the minion's allowance, tunnel data within
// each stream's window. It counts what came, so that the pushes that came
// before a reply are taken before it (takeBefore): in the order core sent
// them.
type pushQueue struct {
	mu    sync.Mutex
	queue []protocol.CoreMessage
	// came is how many messages were ever queued, taken how many the loop
	// took.
	came, taken uint64
	// ready says something was queued since the loop last took; it may
	// fire for a queue taken meanwhile.
	ready chan struct{}
}

func newPushQueue() *pushQueue { return &pushQueue{ready: make(chan struct{}, 1)} }

// streamTaker, while a session runs, takes the tunnel and delegate items of
// what core pushes as it comes, in the connection's reader: a stream's
// bytes and acks reach its host, and its window, without waiting for the
// loop, which may be waiting on a reply of its own. The loop takes the rest
// (runner.takePush). One session runs at a time.
var streamTaker atomic.Pointer[func(protocol.CoreMessage)]

// setStreamTaker sets it; nil, when the session ends, sets none.
func setStreamTaker(take func(protocol.CoreMessage)) {
	if take == nil {
		streamTaker.Store(nil)
		return
	}
	streamTaker.Store(&take)
}

// put queues m, never waiting, its stream items taken first.
func (q *pushQueue) put(m protocol.CoreMessage) {
	if take := streamTaker.Load(); take != nil {
		(*take)(m)
	}
	q.mu.Lock()
	q.queue = append(q.queue, m)
	q.came++
	q.mu.Unlock()
	select {
	case q.ready <- struct{}{}:
	default:
	}
}

// count is how many messages came so far: a reply read now came after
// them.
func (q *pushQueue) count() uint64 {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.came
}

// take is everything queued, oldest first, and empties the queue; nil
// for a nil queue (no connection kept open).
func (q *pushQueue) take() []protocol.CoreMessage {
	if q == nil {
		return nil
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	out := q.queue
	q.queue = nil
	q.taken += uint64(len(out))
	return out
}

// takeBefore is what is queued of the first n messages that came, oldest
// first: those that came before a reply that counted n (count).
func (q *pushQueue) takeBefore(n uint64) []protocol.CoreMessage {
	if q == nil {
		return nil
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	k := 0
	if n > q.taken {
		k = int(min(n-q.taken, uint64(len(q.queue))))
	}
	out := q.queue[:k:k]
	q.queue = q.queue[k:]
	q.taken += uint64(k)
	return out
}

// readyCh fires once something was queued; nil, which never fires, for a
// nil queue.
func (q *pushQueue) readyCh() <-chan struct{} {
	if q == nil {
		return nil
	}
	return q.ready
}
