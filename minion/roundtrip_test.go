package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/logoscore/logos-golang-protocol/protocol"
)

// A round trip is none until an exchange notes one; 0 ms (under a
// millisecond) is one.
func TestRoundtripNoted(t *testing.T) {
	var r roundtrip
	if r.said() != nil {
		t.Fatal("a round trip before any exchange")
	}
	for _, d := range []time.Duration{300 * time.Microsecond, 142 * time.Millisecond} {
		r.note(d)
		if got := r.said(); got == nil || *got != uint64(d.Milliseconds()) {
			t.Fatalf("after %s: %v", d, got)
		}
	}
	r.note(-time.Second)
	if got := r.said(); got == nil || *got != 142 {
		t.Fatalf("a negative duration changed it: %v", got)
	}
}

// An exchange through a request notes its round trip, the channel's empty
// answer too; a failed one notes nothing; the hello says the latest.
func TestExchangeNotesRoundtrip(t *testing.T) {
	defer lastRoundtrip.ms.Store(lastRoundtrip.ms.Load())
	lastRoundtrip.ms.Store(0)
	if describeHost().RoundtripTime != nil {
		t.Fatal("a hello before any exchange says a round trip")
	}
	status := http.StatusNoContent
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(40 * time.Millisecond)
		w.WriteHeader(status)
	}))
	defer srv.Close()
	c := connection{buildConn("http", protocol.KindReverseCall, srv.URL)}
	keys := protocol.AES256CBCHMACSHA256Material{EncKey: bytes.Repeat([]byte{1}, 32), MACKey: bytes.Repeat([]byte{2}, 32)}
	id := [16]byte{9}
	if _, err := exchange(c, id, keys, protocol.GetTasksItem{T: protocol.ItemGetTasks}); err != nil {
		t.Fatal(err)
	}
	got := describeHost().RoundtripTime
	if got == nil || *got < 40 || *got > 2000 {
		t.Fatalf("after a 40 ms exchange the hello says %v", got)
	}
	// Refused: nothing noted, the last stays.
	lastRoundtrip.note(7 * time.Millisecond)
	status = http.StatusNotFound
	if _, err := exchange(c, id, keys, protocol.GetTasksItem{T: protocol.ItemGetTasks}); err == nil {
		t.Fatal("a 404 passed")
	}
	if got := describeHost().RoundtripTime; got == nil || *got != 7 {
		t.Fatalf("after a failed exchange: %v", got)
	}
}
