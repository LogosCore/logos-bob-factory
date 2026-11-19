package main

import "time"

// Timing that follows the link (docs: contracts/minion-messages/tunnels.md,
// change 9, the minion's half): the minion sends again what core did not
// ack once the session's round trip has passed, as TCP does (RFC 6298),
// never sooner than tunnelResend (ts.resend) nor later than a minute; and
// a stream's window, the room it gives core, grows while core fills it,
// round trip after round trip, and the host keeps up, to maxWindow
// (maxTunnelExtra over a tunnel's streams).
// No item changes: each side's timing is its own.

// maxResend bounds how long the minion waits for an ack, however slow the
// link.
const maxResend = time.Minute

// rttState is the session's round trip, as core's acks measure it.
type rttState struct {
	srtt, rttvar time.Duration
	have         bool
}

func (r *rttState) sample(d time.Duration) {
	if !r.have {
		r.srtt, r.rttvar, r.have = d, d/2, true
		return
	}
	diff := r.srtt - d
	if diff < 0 {
		diff = -diff
	}
	r.rttvar = (3*r.rttvar + diff) / 4
	r.srtt = (7*r.srtt + d) / 8
}

// resendAfter is how long the minion waits for core's ack before it sends
// again: the round trip and four times its spread, within ts.resend and
// maxResend (ts.mu held).
func (ts *tunnels) resendAfter() time.Duration {
	if !ts.rtt.have {
		return ts.resend
	}
	return min(max(ts.resend, ts.rtt.srtt+4*ts.rtt.rttvar), maxResend)
}

// measure starts a round trip on what the stream sends, unless one runs:
// its ack ends it. A send again ends any, as it would mislead (Karn's
// rule).
func (st *tstream) measure(end uint64, resent bool, now time.Time) {
	switch {
	case resent:
		st.rttAt = time.Time{}
	case st.rttAt.IsZero():
		st.rttEnd, st.rttAt = end, now
	}
}

// measured takes the stream's round trip once core's ack covers it (ts.mu
// held).
func (ts *tunnels) measured(st *tstream, next uint64) {
	if st.rttAt.IsZero() || next < st.rttEnd {
		return
	}
	if d := time.Since(st.rttAt); d >= 0 {
		ts.rtt.sample(d)
	}
	st.rttAt = time.Time{}
}

// win is the room the minion gives core on the stream now.
func (st *tstream) win() int {
	if st.window == 0 {
		return tunnelWindow
	}
	return st.window
}

// growShare is the share of its window a stream takes per round trip that
// says the window holds it back.
const growShare = 0.75

// grow doubles the stream's window when the window, not the host, held the
// stream back over the last round trip: core delivered at least growShare
// of it per round trip (as TCP's receive-window autotuning measures it),
// and the host took all of it. Up to maxWindow, and the tunnel's
// maxTunnelExtra; true when it grew (an ack says so). Each look covers a
// round trip at least, from the one before; none before the session's
// round trip is known (core's acks of the minion's own bytes measure it).
// ts.mu held.
//
// What core delivers says it, not how near its bytes come to the end the
// minion's last ack gave: the minion acks every piece core sends, each ack
// moving that end a full window past core's bytes, so in a steady flow
// they never near it, however full the window runs.
func (ts *tunnels) grow(st *tstream, now time.Time) bool {
	if !ts.rtt.have || ts.rtt.srtt <= 0 {
		return false
	}
	if st.growAt.IsZero() {
		st.growAt, st.growFrom = now, st.next
		return false
	}
	took := now.Sub(st.growAt)
	if took < ts.rtt.srtt {
		return false
	}
	delivered := st.next - st.growFrom
	st.growAt, st.growFrom = now, st.next
	w := st.win()
	switch {
	case w >= maxWindow, len(st.in) > 0, st.eofIn:
		return false
	case float64(delivered)*float64(ts.rtt.srtt) < growShare*float64(w)*float64(took):
		return false
	case st.t.extra+w > maxTunnelExtra:
		return false
	}
	st.t.extra += w
	st.window = 2 * w
	st.ackDue = true
	return true
}

// shrink gives back what the stream's window grew, as it goes.
func (st *tstream) shrink() {
	st.t.extra -= st.win() - tunnelWindow
	st.window = tunnelWindow
}
