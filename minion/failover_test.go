package main

import (
	"testing"
	"time"

	"github.com/logoscore/logos-golang-protocol/protocol"
)

// conns are three http connections (reverse-call), and a listening one
// last, as a build lists them.
func failoverConns(over ...*protocol.ConnectionFailover) []connection {
	out := []connection{}
	for i := range 3 {
		c := connection{buildConn("http", protocol.KindReverseCall, "http://127.0.0.1:1")}
		if i < len(over) {
			c.Failover = over[i]
		}
		out = append(out, c)
	}
	return append(out, connection{protocol.BuildConnection{Endpoint: protocol.ChannelEndpointExport{Kind: protocol.KindBindPersistent,
		Channel: protocol.ExportChannel{ModuleName: "tcp"}}}})
}

// fail_after failures in a row move the minion to the next connection,
// fail_for holding it until they span long enough; one that answers
// starts the count over.
func TestFailoverMoves(t *testing.T) {
	p := protocol.DefaultBuildFailover()
	p.FailAfter, p.FailFor = 3, "1m"
	f := newFailover(&p, failoverConns())
	now := time.Now()
	for i := range 2 {
		if to, _, _ := f.failed(1, now.Add(time.Duration(i)*time.Minute)); to != 0 {
			t.Fatalf("moved after %d failures", i+1)
		}
	}
	// Three, but over less than a minute: not yet.
	f.ok(1)
	for i := range 3 {
		if to, _, _ := f.failed(1, now.Add(time.Duration(i)*time.Second)); to != 0 {
			t.Fatalf("moved over %ds", i)
		}
	}
	to, pause, why := f.failed(1, now.Add(2*time.Minute))
	if to != 2 || pause != 0 || why != "failover after 4 failures" {
		t.Fatalf("to %d, pause %s, %q", to, pause, why)
	}
}

// Past a connection that did not answer, round the list with wrap (the
// listening one never), pausing after a round where all failed; without
// wrap the last one stays.
func TestFailoverRound(t *testing.T) {
	p := protocol.DefaultBuildFailover()
	p.FailAfter = 1
	f := newFailover(&p, failoverConns())
	now := time.Now()
	to, _, _ := f.failed(1, now)
	f.missed(to)
	to2, _, _ := f.failed(1, now)
	if to != 2 || to2 != 3 {
		t.Fatalf("moves %d then %d", to, to2)
	}
	f.missed(to2)
	// Past 3 comes 1 again (the listening 4th is never one): a whole round
	// failed, so a pause where it is; then the next round starts at 2.
	to3, pause, _ := f.failed(1, now)
	if to3 != 0 || pause != 5*time.Minute {
		t.Fatalf("a whole round failed: to %d, pause %s", to3, pause)
	}
	if to4, _, _ := f.failed(1, now); to4 != 2 {
		t.Fatalf("the next round starts at %d", to4)
	}
	p.Wrap = false
	f = newFailover(&p, failoverConns())
	if to, _, _ := f.failed(3, now); to != 0 {
		t.Fatalf("without wrap the last moved to %d", to)
	}
}

// A connection's own settings: one failover passes by, another's count.
func TestFailoverPerConnection(t *testing.T) {
	off := false
	p := protocol.DefaultBuildFailover()
	f := newFailover(&p, failoverConns(&protocol.ConnectionFailover{FailAfter: 1}, &protocol.ConnectionFailover{Auto: &off}))
	if to, _, _ := f.failed(1, time.Now()); to != 3 {
		t.Fatalf("connection 1 (fail_after 1) moved to %d, past the one left out", to)
	}
	p.Enabled = false
	f = newFailover(&p, failoverConns())
	for range 10 {
		if to, _, _ := f.failed(1, time.Now()); to != 0 {
			t.Fatal("moved with failover off")
		}
	}
}

// Away from the first connection by failover, it is tried again after
// failback_after; one that does not answer, after another; an operator's
// switch is not failover's to undo.
func TestFailback(t *testing.T) {
	p := protocol.DefaultBuildFailover()
	f := newFailover(&p, failoverConns())
	now := time.Now()
	f.moved(2, now)
	if f.failbackDue(2, now.Add(59*time.Minute)) != 0 || f.failbackDue(2, now.Add(time.Hour)) != 1 {
		t.Fatal("failback after 1h")
	}
	f.failbackMissed(now.Add(time.Hour))
	if f.failbackDue(2, now.Add(90*time.Minute)) != 0 || f.failbackDue(2, now.Add(2*time.Hour)) != 1 {
		t.Fatal("another 1h after a miss")
	}
	f.switched()
	if f.failbackDue(2, now.Add(10*time.Hour)) != 0 {
		t.Fatal("failback after an operator's switch")
	}
	p.FailbackAfter = "0s"
	f = newFailover(&p, failoverConns())
	f.moved(2, now)
	if f.failbackDue(2, now.Add(100*time.Hour)) != 0 {
		t.Fatal("failback with 0s")
	}
}
