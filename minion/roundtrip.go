package main

import (
	"sync/atomic"
	"time"
)

// The round trip a hello says (roundtrip_time, docs:
// contracts/minion-messages/sessions.md, Hello): the latest exchange's, from
// sending a message to core to the reply that answers it (or the channel's
// empty answer), whatever connection carried it. Each exchange that gets
// its answer notes it; one that fails notes nothing. A first hello after
// staging says the stage's; one with no exchange before it (straight under
// the build id) says none.
var lastRoundtrip roundtrip

// roundtrip is the latest round trip, in milliseconds plus one: 0 is none
// yet.
type roundtrip struct{ ms atomic.Int64 }

// note takes the round trip of an exchange that got its answer.
func (r *roundtrip) note(d time.Duration) {
	if d >= 0 {
		r.ms.Store(d.Milliseconds() + 1)
	}
}

// said is the round trip as a hello says it; nil before any.
func (r *roundtrip) said() *uint64 {
	v := r.ms.Load()
	if v == 0 {
		return nil
	}
	ms := uint64(v - 1)
	return &ms
}
