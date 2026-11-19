package main

import (
	"bytes"
	"testing"
	"time"

	"github.com/logoscore/logos-golang-protocol/protocol"
)

// The wait for an ack follows the round trip: no sooner than ts.resend,
// longer on a slow link, a minute at most.
func TestResendFollowsTheRoundTrip(t *testing.T) {
	ts := newTunnels(func() {})
	if got := ts.resendAfter(); got != tunnelResend {
		t.Errorf("unmeasured: %v", got)
	}
	ts.rtt.sample(50 * time.Millisecond)
	if got := ts.resendAfter(); got != tunnelResend {
		t.Errorf("a fast link: %v, want %v", got, tunnelResend)
	}
	slow := newTunnels(func() {})
	for range 8 {
		slow.rtt.sample(4 * time.Second)
	}
	if got := slow.resendAfter(); got <= tunnelResend || got > maxResend {
		t.Errorf("a slow link: %v", got)
	}
	slower := newTunnels(func() {})
	slower.rtt.sample(10 * time.Minute)
	if got := slower.resendAfter(); got != maxResend {
		t.Errorf("a very slow link: %v", got)
	}
	// A send again measures nothing (Karn's rule).
	st := &tstream{}
	st.measure(100, false, time.Now())
	st.measure(200, true, time.Now())
	if !st.rttAt.IsZero() {
		t.Error("a resend kept its measure")
	}
}

// A stream core fills, to a host that keeps up, carries every byte both
// ways, and gives back what its window grew as it goes (a link as fast as
// this one's needs none grown: TestWindowGrows says when it grows).
func TestWindowGivenBack(t *testing.T) {
	r := startSocks(t, nil)
	core := newTunnelCore(t, r)
	host, port := echoHost(t)
	st := core.open(1, host, port, randomBytes(8<<20))
	core.until("the stream ends done", func() bool { return st.closed != nil })
	if st.closed.Reason != protocol.TunnelCloseDone || !bytes.Equal(st.got, st.send) {
		t.Fatalf("close %+v, %d bytes back of %d", st.closed, len(st.got), len(st.send))
	}
	r.tun.mu.Lock()
	extra := r.tun.byTask[string(socksTask)].extra
	r.tun.mu.Unlock()
	if extra != 0 {
		t.Errorf("the tunnel holds %d grown bytes after its stream went", extra)
	}
}

// windowRig is a stream of a session whose round trip is rtt (0: none
// known), its window the default, on a clock the test moves.
func windowRig(rtt time.Duration) (*tunnels, *tstream, *time.Time) {
	ts := newTunnels(func() {})
	if rtt > 0 {
		ts.rtt.sample(rtt)
	}
	now := time.Unix(1000, 0)
	return ts, &tstream{t: &tunnel{}}, &now
}

// A stream's window grows while core fills it, round trip after round
// trip, and the host keeps up: to maxWindow and within the tunnel's
// maxTunnelExtra, giving what it grew back as it goes. Not while the host
// is behind, nor for core sending less than growShare of it, nor before a
// round trip passed, nor with none known.
func TestWindowGrows(t *testing.T) {
	ts, st, now := windowRig(time.Second)
	// A round trip delivers share of the window's room.
	full := func(share float64) {
		*now = now.Add(time.Second)
		st.next += uint64(share * float64(st.win()))
	}
	if ts.grow(st, *now) {
		t.Fatal("grown at its first look")
	}
	full(1)
	if !ts.grow(st, *now) || st.win() != 2*tunnelWindow || st.t.extra != tunnelWindow || !st.ackDue {
		t.Fatalf("held back by its window: %d, extra %d", st.win(), st.t.extra)
	}
	st.in = []byte{1}
	full(1)
	if ts.grow(st, *now) {
		t.Error("grown with the host behind")
	}
	st.in = nil
	full(0.5)
	if ts.grow(st, *now) {
		t.Error("grown with room to spare")
	}
	// Half a round trip is no look: the next, at a full one, takes both.
	*now = now.Add(time.Second / 2)
	st.next += uint64(st.win())
	if ts.grow(st, *now) {
		t.Error("grown before a round trip passed")
	}
	*now = now.Add(time.Second / 2)
	if !ts.grow(st, *now) || st.win() != 4*tunnelWindow {
		t.Errorf("a round trip of a full window: %d", st.win())
	}
	// A long quiet stretch: the window's room in it is no full one.
	*now = now.Add(10 * time.Second)
	st.next += uint64(st.win())
	if ts.grow(st, *now) {
		t.Error("grown on a window spread over ten round trips")
	}
	for full(1); ts.grow(st, *now); full(1) {
	}
	if st.win() != maxWindow {
		t.Errorf("grew to %d", st.win())
	}
	other := &tstream{t: st.t}
	st.t.extra = maxTunnelExtra
	_ = ts.grow(other, *now)
	*now = now.Add(time.Second)
	other.next += uint64(other.win())
	if ts.grow(other, *now) {
		t.Error("grown past the tunnel's extra")
	}
	st.t.extra = maxWindow - tunnelWindow
	st.shrink()
	if st.win() != tunnelWindow || st.t.extra != 0 {
		t.Errorf("gave back: %d, extra %d", st.win(), st.t.extra)
	}
	unknown, alone, at := windowRig(0)
	_ = unknown.grow(alone, *at)
	*at = at.Add(time.Second)
	alone.next += uint64(alone.win())
	if unknown.grow(alone, *at) {
		t.Error("grown with no round trip known")
	}
}

// A steady flow its window holds back grows the window, as an upload
// through a tunnel over a slow link did not: core sends as each of the
// minion's acks gives it room, MaxTunnelData at a time; the minion acks
// every piece as it takes it, the host taking each at once; each way takes
// half a 1.24 s round trip. Core's bytes never come within an eighth of the
// end the minion's last ack gave (the rule before), however full the window
// runs: what core delivers per round trip says the window is full.
func TestWindowGrowsUnderSteadyFlow(t *testing.T) {
	const rtt = 1240 * time.Millisecond
	ts, st, now := windowRig(rtt)
	st.ackedEnd = uint64(st.win()) // the reply's room
	type mark struct {
		at  time.Time
		end uint64
	}
	var acks, pieces []mark // the minion's acks on their way, core's bytes on theirs
	limit, sent := st.ackedEnd, uint64(0)
	for range 3000 { // 30 s, 10 ms apart
		*now = now.Add(10 * time.Millisecond)
		for len(acks) > 0 && !acks[0].at.After(*now) {
			limit, acks = acks[0].end, acks[1:]
		}
		for sent < limit {
			sent += min(limit-sent, protocol.MaxTunnelData)
			pieces = append(pieces, mark{now.Add(rtt / 2), sent})
		}
		for len(pieces) > 0 && !pieces[0].at.After(*now) {
			// Taken (an ack due, as takeData has it), and written to the
			// host at once (writeHost).
			st.next, pieces = pieces[0].end, pieces[1:]
			st.ackDue = true
			_ = ts.grow(st, *now)
			a := st.ackItem()
			st.ackDue, st.ackedEnd = false, a.Next+*a.Room
			acks = append(acks, mark{now.Add(rtt / 2), st.ackedEnd})
		}
	}
	if st.win() != maxWindow {
		t.Errorf("a full window for 30 s grew it to %d, not %d", st.win(), maxWindow)
	}
}
