package main

import (
	"testing"
	"time"

	"github.com/logoscore/logos-golang-protocol/protocol"
)

// getTasksIn is the max a message's get_tasks asks for; nil without one.
func getTasksIn(items []any) *uint64 {
	for _, it := range items {
		if g, ok := it.(protocol.GetTasksItem); ok {
			return g.Max
		}
	}
	return nil
}

// maxTasksSaid is the limit a message says back, if it says one.
func maxTasksSaid(items []any) *uint64 {
	for _, it := range items {
		if m, ok := it.(protocol.MaxTasksItem); ok {
			return &m.Max
		}
	}
	return nil
}

// next is the runner's next message, as if it went through.
func next(r *runner) []any {
	items := r.outgoing()
	r.mu.Lock()
	r.inflight = nil
	r.mu.Unlock()
	return items
}

// Several tasks run at once, up to the limit: through a call the minion
// asks for the room it has left, none when it is full; on a persistent
// connection its get_tasks is the limit itself, which core counts its
// tasks under way against.
func TestMaxTasksIntake(t *testing.T) {
	two := uint64(2)
	r := newRunner(description{Ops: []uint64{opShell}, MaxTasks: &two}, &schedule{interval: time.Second})
	if m := getTasksIn(next(r)); m == nil || *m != 2 {
		t.Fatalf("free: %v", m)
	}
	ids := [][]byte{make([]byte, 16), make([]byte, 16)}
	ids[1][0] = 1
	r.start(protocol.TaskItem{T: protocol.ItemTask, Task: ids[0], Cmd: opShell, Args: protocol.Args{shellCommand: sleep30()}})
	if m := getTasksIn(next(r)); m == nil || *m != 1 {
		t.Fatalf("one running: %v", m)
	}
	r.start(protocol.TaskItem{T: protocol.ItemTask, Task: ids[1], Cmd: opShell, Args: protocol.Args{shellCommand: sleep30()}})
	if m := getTasksIn(next(r)); m != nil {
		t.Fatalf("full: asks for %d", *m)
	}
	// Both run at once.
	time.Sleep(200 * time.Millisecond)
	r.mu.Lock()
	running := 0
	for _, task := range r.tasks {
		if !task.ended {
			running++
		}
	}
	r.mu.Unlock()
	if running != 2 {
		t.Fatalf("%d running", running)
	}
	r.setPersistent(true)
	if m := getTasksIn(next(r)); m == nil || *m != 2 {
		t.Fatalf("persistent: %v", m)
	}
	r.setPersistent(false)
	for _, id := range ids {
		r.stop(id, 0)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		drain(r)
		if m := getTasksIn(next(r)); m != nil && *m == 2 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the room did not come back once both ended")
}

// Core's max_tasks changes the limit at once, whatever runs, and the
// minion says it back in its next message (sent at once: pending); 0
// takes no tasks; a limit past the most is refused.
func TestMaxTasksFromCore(t *testing.T) {
	r := newRunner(description{}, &schedule{interval: time.Second})
	if r.maxTasksNow() != protocol.DefaultMaxTasks {
		t.Fatalf("the default: %d", r.maxTasksNow())
	}
	r.take(coreSays(t, protocol.MaxTasksItem{T: protocol.ItemMaxTasks, Max: 0}), nil)
	if !r.pending() {
		t.Fatal("the answer waits for the schedule")
	}
	items := next(r)
	if m := maxTasksSaid(items); m == nil || *m != 0 || getTasksIn(items) != nil {
		t.Fatalf("paused: said %v, asks %v", m, getTasksIn(items))
	}
	if maxTasksSaid(next(r)) != nil {
		t.Fatal("said twice")
	}
	r.setPersistent(true)
	if m := getTasksIn(next(r)); m == nil || *m != 0 {
		t.Fatalf("paused on a persistent connection: %v", m)
	}
	r.take(coreSays(t, protocol.MaxTasksItem{T: protocol.ItemMaxTasks, Max: protocol.MaxTasksLimit + 1}), nil)
	items = next(r)
	if maxTasksSaid(items) != nil || r.maxTasksNow() != 0 {
		t.Fatalf("a limit past the most taken: %d", r.maxTasksNow())
	}
	bad := false
	for _, it := range items {
		if e, ok := it.(protocol.ErrItem); ok && e.Code == protocol.ErrItemBadMessage {
			bad = true
		}
	}
	if !bad {
		t.Fatalf("no err for it: %+v", items)
	}
	// A rehello says the limit it has now.
	r.hello = func() protocol.HelloItem {
		return protocol.HelloItem{T: protocol.ItemHello, PID: 1, OS: "linux", Arch: "amd64"}
	}
	r.take(coreSays(t, protocol.MaxTasksItem{T: protocol.ItemMaxTasks, Max: 7}, protocol.RehelloItem{T: protocol.ItemRehello}), nil)
	said := false
	for _, it := range next(r) {
		if h, ok := it.(protocol.HelloItem); ok {
			said = h.MaxTasks != nil && *h.MaxTasks == 7
		}
	}
	if !said {
		t.Fatal("the rehello did not say the limit it has")
	}
}

// exit stops the tasks running beside it first: each ends cancelled, its
// end before exit's, so core hears how every one went.
func TestExitStopsTheOthersFirst(t *testing.T) {
	r := newRunner(description{Ops: []uint64{opShell, opExit}}, &schedule{interval: time.Second})
	shell, exit := make([]byte, 16), make([]byte, 16)
	exit[0] = 9
	r.start(protocol.TaskItem{T: protocol.ItemTask, Task: shell, Cmd: opShell, Args: protocol.Args{shellCommand: sleep30()}})
	time.Sleep(200 * time.Millisecond)
	r.start(protocol.TaskItem{T: protocol.ItemTask, Task: exit, Cmd: opExit})
	var ends [][]byte
	var shellEnd *protocol.ResultItem
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) && len(ends) < 2 {
		for _, p := range drain(r) {
			if p.Done {
				ends = append(ends, p.Task)
				if p.Task[0] == 0 {
					shellEnd = &p
				}
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(ends) != 2 || ends[0][0] != 0 || ends[1][0] != 9 {
		t.Fatalf("ends %v", ends)
	}
	if shellEnd == nil || *shellEnd.OK || shellEnd.Err != "cancelled" {
		t.Fatalf("the shell's end: %+v", shellEnd)
	}
}
