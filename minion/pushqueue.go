package main

import (
	"sync"

	"github.com/logoscore/logos-golang-protocol/protocol"
)

// pushQueue holds the messages core starts over a connection kept open
// until the session's loop takes them. It takes as many as come: the
// connection's reader, which fills it, must never wait on the loop, which
// may itself be waiting on the reader for a reply (with a bounded buffer,
// a burst of pushes behind a slow round trip, as through delegates, left
// the reply unread and the connection's close unseen). What core pushes is
// bounded by core: tasks within the minion's allowance, tunnel data within
// each stream's window.
type pushQueue struct {
	mu    sync.Mutex
	queue []protocol.CoreMessage
	// ready says something was queued since the loop last took; it may
	// fire for a queue taken meanwhile.
	ready chan struct{}
}

func newPushQueue() *pushQueue { return &pushQueue{ready: make(chan struct{}, 1)} }

// put queues m, never waiting.
func (q *pushQueue) put(m protocol.CoreMessage) {
	q.mu.Lock()
	q.queue = append(q.queue, m)
	q.mu.Unlock()
	select {
	case q.ready <- struct{}{}:
	default:
	}
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
