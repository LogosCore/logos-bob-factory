package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/logoscore/logos-golang-protocol/protocol"
)

// feedbackIn is a message's feedback items.
func feedbackIn(items []any) []protocol.FeedbackItem {
	var out []protocol.FeedbackItem
	for _, it := range items {
		if f, ok := it.(protocol.FeedbackItem); ok {
			out = append(out, f)
		}
	}
	return out
}

// Reported feedback goes with the next message (sent at once: pending);
// one like a queued one is folded into it; past the most kept, the oldest
// go and a warning says so; a message that fails sends them again.
func TestFeedbackQueue(t *testing.T) {
	feedback = &feedbackQueue{}
	r := newRunner(description{}, &schedule{interval: time.Second})
	report(protocol.FeedbackWarning, "x", nil, "the same")
	report(protocol.FeedbackWarning, "x", nil, "the same")
	report(protocol.FeedbackError, "y", nil, "another")
	if !r.pending() {
		t.Fatal("feedback waits for the schedule")
	}
	items := r.outgoing()
	fb := feedbackIn(items)
	if len(fb) != 2 || fb[0].Count != 2 || fb[1].Level != protocol.FeedbackError || len(fb[0].ID) != 8 || fb[0].Validate() != nil {
		t.Fatalf("sent %+v", fb)
	}
	r.failed(items)
	if again := feedbackIn(r.outgoing()); len(again) != 2 || string(again[0].ID) != string(fb[0].ID) {
		t.Fatalf("not sent again as it was: %+v", again)
	}
	r.delivered(nil)
	r.mu.Lock()
	r.inflight = nil
	r.mu.Unlock()

	for i := range maxFeedback + 5 {
		report(protocol.FeedbackWarning, "n", nil, "number %d", i)
	}
	var all []protocol.FeedbackItem
	for range 20 {
		all = append(all, feedbackIn(next(r))...)
	}
	if len(all) != maxFeedback+1 || all[0].Code != "feedback_dropped" || !strings.Contains(all[0].Msg, "5 feedback messages dropped") ||
		all[1].Msg != "number 5" {
		t.Fatalf("%d sent, first %+v", len(all), all[0])
	}
	// A message too small for all of them takes some, at least one.
	feedback = &feedbackQueue{}
	r.setLimit(protocol.MinConnectionMessage)
	for i := range 5 {
		report(protocol.FeedbackWarning, "n", nil, "%s %d", strings.Repeat("x", 300), i)
	}
	if n := len(feedbackIn(next(r))); n < 1 || n >= 5 {
		t.Fatalf("%d in a small message", n)
	}
}

// Check-ins failing in a row are said once one works again; one alone is
// not.
func TestFailStreak(t *testing.T) {
	feedback = &feedbackQueue{}
	var s failStreak
	s.failed(errors.New("connection refused"))
	s.worked()
	if feedback.pending() {
		t.Fatal("one failure reported")
	}
	s.failed(errors.New("connection refused"))
	s.failed(errors.New("timeout"))
	s.failed(errors.New("timeout"))
	s.worked()
	fb, _ := feedback.take(1<<20, true)
	if len(fb) != 1 || fb[0].Code != "checkin_failed" || !strings.Contains(fb[0].Msg, "3 check-ins failed") ||
		!strings.Contains(fb[0].Msg, "connection refused") {
		t.Fatalf("%+v", fb)
	}
}

// The feedback command sends the operator's words at the level asked,
// about its task; a command that crashes fails its task and says so.
func TestFeedbackCommandAndCrash(t *testing.T) {
	feedback = &feedbackQueue{}
	r := newRunner(description{Ops: []uint64{opFeedback, opHello}}, &schedule{interval: time.Second})
	id := make([]byte, 16)
	id[0] = 4
	r.start(protocol.TaskItem{T: protocol.ItemTask, Task: id, Cmd: opFeedback, Args: protocol.Args{feedbackLevel: "error", feedbackMessage: "disk almost full"}})
	if task := waitEnd(t, r, 5*time.Second); !task.ok {
		t.Fatalf("the command: %+v", task)
	}
	fb, _ := feedback.take(1<<20, true)
	if len(fb) != 1 || fb[0].Level != protocol.FeedbackError || fb[0].Msg != "disk almost full" || string(fb[0].Task) != string(id) {
		t.Fatalf("%+v", fb)
	}

	r = newRunner(description{Ops: []uint64{opHello}}, &schedule{interval: time.Second})
	r.exec = func(_ context.Context, _ *task, _ protocol.TaskItem) { panic("boom") }
	crash := make([]byte, 16)
	crash[0] = 5
	r.start(protocol.TaskItem{T: protocol.ItemTask, Task: crash, Cmd: opHello})
	task := waitEnd(t, r, 5*time.Second)
	if task.ok || !strings.Contains(task.err, "crashed: boom") {
		t.Fatalf("the crashed task: %+v", task)
	}
	fb, _ = feedback.take(1<<20, true)
	if len(fb) != 1 || fb[0].Code != "task_panic" || fb[0].Level != protocol.FeedbackError || string(fb[0].Task) != string(crash) {
		t.Fatalf("%+v", fb)
	}
}
