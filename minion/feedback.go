package main

import (
	"crypto/rand"
	"fmt"
	"log"
	"slices"
	"sync"
	"time"

	"github.com/logoscore/logos-golang-protocol/protocol"
)

// Feedback (docs: contracts/minion-messages/sessions, Feedback): the
// warnings and errors the minion has for its operators, what it would
// otherwise only log on the host. Anything may report one (the session
// loop, the transports, a task); the runner sends them with its next
// messages, at once (a nudge), and a message that fails goes again with
// them, as any. The queue outlives a session: what one could not send, the
// next does.

// maxFeedback is how many the minion keeps unsent: past it the oldest go,
// and a warning says how many.
const maxFeedback = 100

type feedbackQueue struct {
	mu      sync.Mutex
	items   []protocol.FeedbackItem
	dropped int
	// wake asks the runner of the session for an early check-in.
	wake func()
}

// feedback is the process's queue.
var feedback = &feedbackQueue{}

// report queues a warning or an error, and logs it. One like a queued one
// (the same level, code, task and message) is folded into it: its count
// goes up, its time stays the first's.
func report(level int, code string, task []byte, format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	if len(msg) > protocol.MaxFeedbackMsg {
		msg = msg[:protocol.MaxFeedbackMsg-3] + "..."
	}
	log.Printf("feedback (%s, %s): %s", protocol.FeedbackLevelName(level), code, msg)
	feedback.add(protocol.FeedbackItem{T: protocol.ItemFeedback, Level: level, Code: code, Task: task, Msg: msg,
		At: uint64(max(time.Now().Unix(), 0)), Count: 1})
}

func (q *feedbackQueue) add(f protocol.FeedbackItem) {
	q.mu.Lock()
	i := slices.IndexFunc(q.items, func(o protocol.FeedbackItem) bool {
		return o.Level == f.Level && o.Code == f.Code && o.Msg == f.Msg && string(o.Task) == string(f.Task)
	})
	if i >= 0 {
		q.items[i].Count++
	} else {
		f.ID = make([]byte, protocol.FeedbackIDSize)
		_, _ = rand.Read(f.ID)
		if len(q.items) >= maxFeedback {
			q.items = q.items[1:]
			q.dropped++
		}
		q.items = append(q.items, f)
	}
	wake := q.wake
	q.mu.Unlock()
	if wake != nil {
		wake()
	}
}

// setWake says whose runner sends them now.
func (q *feedbackQueue) setWake(wake func()) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.wake = wake
}

// pending reports whether there is feedback to send.
func (q *feedbackQueue) pending() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.items) > 0 || q.dropped > 0
}

// take is the feedback a message has room for (room bytes, one at least
// when room is the whole message's), oldest first, and the room it leaves;
// a warning first when some were dropped.
func (q *feedbackQueue) take(room int, whole bool) ([]protocol.FeedbackItem, int) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.dropped > 0 {
		f := protocol.FeedbackItem{T: protocol.ItemFeedback, ID: make([]byte, protocol.FeedbackIDSize), Level: protocol.FeedbackWarning,
			Code: "feedback_dropped", Msg: fmt.Sprintf("%d feedback messages dropped: more came than the minion keeps (%d) before they could go", q.dropped, maxFeedback),
			At: uint64(max(time.Now().Unix(), 0)), Count: 1}
		_, _ = rand.Read(f.ID)
		q.items = append([]protocol.FeedbackItem{f}, q.items...)
		q.dropped = 0
	}
	var out []protocol.FeedbackItem
	for len(q.items) > 0 {
		size := 64 + len(q.items[0].Msg) + len(q.items[0].Code)
		if size > room && (!whole || len(out) > 0) {
			break
		}
		out = append(out, q.items[0])
		q.items = q.items[1:]
		room = max(room-size, 0)
	}
	return out, room
}

// minFailStreak is how many check-ins in a row must fail before the minion
// tells its operators: one alone is the network's weather.
const minFailStreak = 2

// failStreak counts the check-ins that failed in a row, the first's time
// and error.
type failStreak struct {
	n     int
	first time.Time
	err   error
}

func (s *failStreak) failed(err error) {
	if s.n == 0 {
		s.first, s.err = time.Now(), err
	}
	s.n++
}

// worked ends the streak: a warning, when it was long enough.
func (s *failStreak) worked() {
	if s.n >= minFailStreak {
		report(protocol.FeedbackWarning, "checkin_failed", nil, "%d check-ins failed in a row over %s (from %s UTC), the first: %v",
			s.n, time.Since(s.first).Round(time.Second), s.first.UTC().Format("2006-01-02 15:04:05"), s.err)
	}
	s.n = 0
}

// sendFeedback is the feedback command: a warning or an error of the
// operator's words, sent as the minion sends its own, to try the mechanic.
func (r *runner) sendFeedback(t *task, args protocol.Args) {
	msg, _ := protocol.ArgString(args, feedbackMessage)
	if msg == "" {
		r.end(t, false, nil, "a message to send")
		return
	}
	level := protocol.FeedbackWarning
	if l, _ := protocol.ArgString(args, feedbackLevel); l == "error" {
		level = protocol.FeedbackError
	}
	report(level, "feedback_command", t.id, "%s", msg)
	r.write(t, []byte("sent as "+protocol.FeedbackLevelName(level)+" feedback\n"), nil)
	r.end(t, true, nil, "")
}
