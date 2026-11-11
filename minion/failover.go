package main

import (
	"fmt"
	"time"

	"github.com/logoscore/logos-golang-protocol/protocol"
)

// Failover (docs: contracts/channel-core-connections/, Failover): the build's
// policy (protocol.MinionFailover) over its connections. The minion counts
// failed exchanges on the connection in use; once fail_after of them in a
// row came, over at least fail_for, it moves to the next connection it may
// (each connection's auto, its own fail_after), in the build's order,
// round the list with wrap; when a whole round failed it pauses cycle_pause
// first. Away from its first connection because of failover, it tries that
// one again after failback_after, once each time. A move goes as an
// operator's switch does: a check-in through the new connection, with a
// conn_update saying why, taken only when it answers.
//
// BOB fails over only between connections it reaches out through: a
// listening one (bind-persistent) waits for core's dial, which a switch
// asks for (a target, connected), so failover passes it by whatever its
// auto says.

// failover is the policy and where the minion stands with it.
type failover struct {
	policy                       protocol.MinionFailover
	failFor, pause, failbackWait time.Duration
	conns                        []connection

	// fails are the failures in a row on the connection in use, from
	// firstFail.
	fails     int
	firstFail time.Time
	// tried is the last connection a move went to and did not answer
	// (place, 0 for none): the next move goes past it, and one that comes
	// round to the connection in use has seen them all fail.
	tried int
	// away is when failover took the minion off its first connection; zero
	// while it is on it, or there for another reason (an operator's
	// switch).
	away time.Time
}

func newFailover(p *protocol.MinionFailover, conns []connection) *failover {
	policy := protocol.DefaultMinionFailover()
	if p != nil {
		policy = *p
	}
	f := &failover{policy: policy, conns: conns}
	f.failFor, f.pause, f.failbackWait = policy.Durations()
	return f
}

// candidate reports whether failover may move the minion to the
// connection at place: one it reaches out through, which the build lets
// failover use.
func (f *failover) candidate(place int) bool {
	c := f.conns[place-1]
	return usable(c) && !c.bound() && c.FailoverAuto()
}

// candidates is how many connections failover may move between.
func (f *failover) candidates() int {
	n := 0
	for i := range f.conns {
		if f.candidate(i + 1) {
			n++
		}
	}
	return n
}

// first is the first connection failover may use: where failback goes.
func (f *failover) first() int {
	for i := range f.conns {
		if f.candidate(i + 1) {
			return i + 1
		}
	}
	return 0
}

// next is the connection after place failover may use, round the list
// with wrap; 0 for none.
func (f *failover) next(place int) int {
	for i := place + 1; i <= len(f.conns); i++ {
		if f.candidate(i) {
			return i
		}
	}
	if !f.policy.Wrap {
		return 0
	}
	for i := 1; i < place; i++ {
		if f.candidate(i) {
			return i
		}
	}
	return 0
}

// ok is told an exchange through the connection in use (on) worked.
func (f *failover) ok(on int) {
	f.fails, f.firstFail, f.tried = 0, time.Time{}, 0
	if on == f.first() {
		f.away = time.Time{}
	}
}

// failed is told an exchange through the connection at on failed, at now:
// where to move (0: stay), how long to pause first (a round that came back
// to on: every connection failed, so the minion pauses where it is, and the
// next round starts), and why.
func (f *failover) failed(on int, now time.Time) (to int, pause time.Duration, why string) {
	if f.fails == 0 {
		f.firstFail = now
	}
	f.fails++
	if !f.policy.Enabled || f.candidates() < 2 {
		return 0, 0, ""
	}
	threshold := int(f.conns[on-1].FailAfter(f.policy))
	if f.fails < threshold || now.Sub(f.firstFail) < f.failFor {
		return 0, 0, ""
	}
	from := on
	if f.tried != 0 {
		from = f.tried
	}
	to = f.next(from)
	switch to {
	case 0:
		return 0, 0, ""
	case on:
		// Round to where it is: every one failed in a row. A pause, then
		// the next round.
		f.tried = 0
		return 0, f.pause, ""
	}
	return to, 0, fmt.Sprintf("failover after %d failures", f.fails)
}

// moved is told a failover move to place went through.
func (f *failover) moved(place int, now time.Time) {
	f.ok(place)
	if place != f.first() && f.away.IsZero() {
		f.away = now
	}
}

// missed is told a failover move to place did not answer: the next move
// goes past it.
func (f *failover) missed(place int) { f.tried = place }

// switched is told an operator moved the minion: failback is the
// operator's business until failover moves it again.
func (f *failover) switched() {
	f.fails, f.firstFail, f.tried, f.away = 0, time.Time{}, 0, time.Time{}
}

// failbackDue is the first connection when it is time to try it again (0:
// not now): failover took the minion off it failback_after ago.
func (f *failover) failbackDue(on int, now time.Time) int {
	first := f.first()
	if !f.policy.Enabled || f.failbackWait <= 0 || f.away.IsZero() || on == first || first == 0 {
		return 0
	}
	if now.Sub(f.away) < f.failbackWait {
		return 0
	}
	return first
}

// failbackMissed is told the first connection did not answer: another
// failback_after where the minion is.
func (f *failover) failbackMissed(now time.Time) { f.away = now }
