package main

import (
	"fmt"
	"strings"
	"time"
)

// What a tunnel's streams did, for the minion's log: neither side logs a
// stream's sends again, probes or windows otherwise, so a slow or lossy
// link's stalls could not be seen. A notable stream says it when it goes;
// a tunnel's last line adds up all of its streams. Nothing goes to core.

// streamStats is what a stream did: its bytes each way (the host's that
// core acked, core's that the host took), what went again (sends again from
// core's ack, probes of a window shut), and the most it had: in flight to
// core, as the window core gave, read from the host ahead of core's ack,
// and kept from core ahead of the bytes before it.
type streamStats struct {
	opened                      time.Time
	up, down                    uint64
	resends, probes             int
	resent                      uint64
	flight, window, read, ahead int
}

// notable: a stream worth a line when it goes: it sent again or probed,
// ran a minute or more, or moved a mebibyte either way.
func (s streamStats) notable(now time.Time) bool {
	return s.resends > 0 || s.probes > 0 || now.Sub(s.opened) >= time.Minute || s.up >= 1<<20 || s.down >= 1<<20
}

// text is s for the log line of a stream that goes now, with the session's
// round trip as the tunnels measured it (0: none yet).
func (s streamStats) text(now time.Time, rtt time.Duration) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s up, %s down in %s", sizeText(s.up), sizeText(s.down), now.Sub(s.opened).Round(time.Second))
	fmt.Fprintf(&b, "; sent again %d time(s) (%s), %d probe(s)", s.resends, sizeText(s.resent), s.probes)
	fmt.Fprintf(&b, "; at most %s in flight, %s of window, %s read ahead, %s ahead", sizeText(uint64(s.flight)), sizeText(uint64(s.window)),
		sizeText(uint64(s.read)), sizeText(uint64(s.ahead)))
	if rtt > 0 {
		fmt.Fprintf(&b, "; round trip %s", rtt.Round(time.Millisecond))
	}
	return b.String()
}

// tunnelTotals add up a tunnel's streams, for its last line.
type tunnelTotals struct {
	streams         int
	up, down        uint64
	resends, probes int
}

func (t *tunnelTotals) add(s streamStats) {
	t.streams++
	t.up += s.up
	t.down += s.down
	t.resends += s.resends
	t.probes += s.probes
}

// text is what the tunnel's last line adds: nothing when it had no stream.
func (t tunnelTotals) text() string {
	if t.streams == 0 {
		return ""
	}
	return fmt.Sprintf("; %d stream(s) in all: %s up, %s down, sent again %d time(s), %d probe(s)", t.streams, sizeText(t.up),
		sizeText(t.down), t.resends, t.probes)
}

// sizeText is n bytes, for the log: 512 B, 1.5 KiB, 3.2 MiB.
func sizeText(n uint64) string {
	switch {
	case n < 1<<10:
		return fmt.Sprintf("%d B", n)
	case n < 1<<20:
		return fmt.Sprintf("%.1f KiB", float64(n)/(1<<10))
	case n < 1<<30:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	}
	return fmt.Sprintf("%.1f GiB", float64(n)/(1<<30))
}
