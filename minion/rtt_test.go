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

// A stream core fills, to a host that keeps up, has its window grow past
// the default room, and gives it back as it goes.
func TestWindowGrows(t *testing.T) {
	r := startSocks(t, nil)
	core := newTunnelCore(t, r)
	host, port := echoHost(t)
	st := core.open(1, host, port, randomBytes(8<<20))
	grew := 0
	core.until("the stream ends done", func() bool {
		r.tun.mu.Lock()
		if tn := r.tun.byTask[string(socksTask)]; tn != nil {
			if s := tn.streams[1]; s != nil {
				grew = max(grew, s.win())
			}
		}
		r.tun.mu.Unlock()
		return st.closed != nil
	})
	if grew <= tunnelWindow {
		t.Errorf("the window stayed %d", grew)
	}
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
