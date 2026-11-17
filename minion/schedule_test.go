package main

import (
	"testing"
	"time"

	"github.com/logoscore/logos-golang-protocol/protocol"
)

// An interval of 0 is no wait between check-ins: the next check-in at
// once. A retry after a failure still waits a second.
func TestScheduleNoWait(t *testing.T) {
	s := schedule{interval: 0, jitter: 20}
	if s.next() != 0 {
		t.Fatalf("next: %v", s.next())
	}
	if s.retry() != time.Second {
		t.Fatalf("retry: %v", s.retry())
	}
	if (schedule{interval: 5 * time.Second}).retry() < time.Second {
		t.Fatal("a retry is under a second")
	}
}

// The sleep task takes an interval of 0 and says it to core as 0.
func TestSleepTaskZero(t *testing.T) {
	sched := &schedule{interval: time.Minute, jitter: 20}
	r := newRunner(description{Ops: []uint64{opSleep}}, sched)
	r.sleep(&task{id: make([]byte, 16), cmd: opSleep}, protocol.Args{sleepInterval: 0, sleepJitter: 0})
	if sched.interval != 0 || r.sleepUpd == nil || r.sleepUpd.Interval != 0 {
		t.Fatalf("interval %v, update %+v", sched.interval, r.sleepUpd)
	}
}
