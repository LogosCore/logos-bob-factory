package main

import (
	"bytes"
	"log"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/logoscore/logos-golang-protocol/protocol"
)

// A stream counts what it did: the host's bytes core acked, a probe of a
// window shut, a send again, the most in flight and of window, core's bytes
// the host took, read ahead and kept ahead. A notable one says it when it
// goes, and its tunnel's last line adds up every stream.
func TestStreamStats(t *testing.T) {
	var logged bytes.Buffer
	was := log.Writer()
	log.SetOutput(&logged)
	t.Cleanup(func() { log.SetOutput(was) })
	ts := newTunnels(func() {})
	ts.resend = 10 * time.Millisecond
	tun := &tunnel{task: socksTask, streams: map[uint64]*tstream{}}
	ts.byTask[string(socksTask)] = tun
	newStream := func(id uint64, conn net.Conn) *tstream {
		st := &tstream{stats: streamStats{opened: time.Now()}, t: tun, id: id, conn: conn, done: make(chan struct{}),
			inSignal: make(chan struct{}, 1), outSignal: make(chan struct{}, 1)}
		tun.streams[id] = st
		return st
	}

	// The host's bytes toward core: a window shut, so a probe once it
	// waited; core acks it and gives room; the rest goes, and again once
	// unacked past the wait; core acks it all.
	up := newStream(1, &net.TCPConn{})
	up.out, up.sentAt = bytes.Repeat([]byte{'h'}, 100), time.Now().Add(-time.Second)
	ts.items(1<<20, 100)
	room := uint64(500)
	ts.takeAck(protocol.TunnelAckItem{T: protocol.ItemTunnelAck, Task: socksTask, Stream: 1, Next: 1, Room: &room})
	ts.items(1<<20, 100)
	time.Sleep(20 * time.Millisecond)
	ts.items(1<<20, 100)
	ts.takeAck(protocol.TunnelAckItem{T: protocol.ItemTunnelAck, Task: socksTask, Stream: 1, Next: 100, Room: &room})
	s := up.stats
	if s.probes != 1 || s.resends != 1 || s.resent != 99 || s.up != 100 || s.flight != 99 || s.window != 500 {
		t.Fatalf("stats %+v", s)
	}
	ts.mu.Lock()
	ts.drop(up, false)
	ts.mu.Unlock()
	if line := logged.String(); !strings.Contains(line, "stream 1: 100 B up, 0 B down") ||
		!strings.Contains(line, "sent again 1 time(s) (99 B), 1 probe(s)") || !strings.Contains(line, "500 B of window") {
		t.Errorf("its line: %q", line)
	}

	// Core's bytes toward the host, one ahead of the bytes before it; and
	// the host's, read ahead of core's ack.
	host, minion := net.Pipe()
	both := newStream(2, minion)
	both.limit = 1000
	go ts.writeHost(both)
	go ts.readHost(both)
	ts.takeData(protocol.TunnelDataItem{T: protocol.ItemTunnelData, Task: socksTask, Stream: 2, Off: 5, Data: []byte("world")})
	ts.takeData(protocol.TunnelDataItem{T: protocol.ItemTunnelData, Task: socksTask, Stream: 2, Off: 0, Data: []byte("hello")})
	got := make([]byte, 10)
	if _, err := host.Read(got); err != nil {
		t.Fatal(err)
	}
	if _, err := host.Write([]byte("answer")); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		ts.mu.Lock()
		s = both.stats
		ts.mu.Unlock()
		if s.down == 10 && s.read == 6 || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if s.down != 10 || s.read != 6 || s.ahead != 5 {
		t.Fatalf("stats %+v", s)
	}
	n, totals := ts.close(socksTask)
	if n != 1 || totals.streams != 2 || totals.up != 100 || totals.down != 10 || totals.resends != 1 || totals.probes != 1 {
		t.Errorf("closed %d stream(s), totals %+v", n, totals)
	}
	if text := totals.text(); !strings.Contains(text, "2 stream(s) in all: 100 B up, 10 B down, sent again 1 time(s), 1 probe(s)") {
		t.Errorf("the tunnel's line adds %q", text)
	}
	_ = host.Close()
}

func TestSizeText(t *testing.T) {
	for n, want := range map[uint64]string{0: "0 B", 1023: "1023 B", 1536: "1.5 KiB", 3 << 20: "3.0 MiB", 5 << 30: "5.0 GiB"} {
		if got := sizeText(n); got != want {
			t.Errorf("%d: %q, not %q", n, got, want)
		}
	}
}
