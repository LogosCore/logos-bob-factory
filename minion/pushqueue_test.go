package main

import (
	"bufio"
	"bytes"
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/logoscore/logos-golang-protocol/protocol"
)

// A link's reader never waits on the loop: a burst of pushes nobody takes
// (the loop busy exchanging, behind a slow round trip) leaves the next
// reply readable, and the connection's close seen. Session 138 hung on a
// buffer of 16: the reply sat behind a push the reader could not hand over,
// the peer's close was never read, and the minion never dialed again.

const pushBurst = 100

// cancelPush is a push of core's, sealed under id.
func cancelPush(t *testing.T, id [16]byte) []byte {
	t.Helper()
	return coreMessage(t, id, nil, protocol.NewMessageID(), protocol.CancelItem{T: protocol.ItemCancel, Task: bytes.Repeat([]byte{1}, 16)})
}

// waitLost waits for a link's lost signal.
func waitLost(t *testing.T, lost chan struct{}, what string) {
	t.Helper()
	select {
	case <-lost:
	case <-time.After(5 * time.Second):
		t.Fatalf("%s: not lost", what)
	}
}

// exchangeAsync runs an exchange over l in the background.
func exchangeAsync(l persistentLink, id [16]byte) chan error {
	done := make(chan error, 1)
	go func() { _, err := l.exchange(id, tcpKeys); done <- err }()
	return done
}

func TestTCPDialLinkPushesNeverBlock(t *testing.T) {
	id := [16]byte{8}
	conns := make(chan net.Conn, 4)
	l := newTCPDialLink(tcpEndpoint(t, protocol.KindReversePersistent, fakeTCPChannel(t, func(c net.Conn) { conns <- c })))
	defer l.close()
	done := exchangeAsync(l, id)
	channel := <-conns
	r := bufio.NewReader(channel)
	// Once the minion's frame is in, its keys are: the pushes, then the reply.
	_, frame, err := readTCPFrame(r)
	if err != nil {
		t.Fatal(err)
	}
	for range pushBurst {
		_ = writeTCPFrame(channel, id[:], cancelPush(t, id))
	}
	_ = writeTCPFrame(channel, id[:], coreAnswer(t, id, frame))
	if err := <-done; err != nil {
		t.Fatalf("the reply behind %d pushes: %v", pushBurst, err)
	}
	if got := len(l.pushed().take()); got != pushBurst {
		t.Fatalf("%d pushes queued, not %d", got, pushBurst)
	}
	// The other end closes: seen, and the next exchange dials again.
	_ = channel.Close()
	waitLost(t, l.lostCh(), "the close")
	exchangeAsync(l, id)
	select {
	case <-conns:
	case <-time.After(5 * time.Second):
		t.Fatal("not dialed again")
	}
}

// A connection that does not answer in time, or fails a write, is dropped:
// the next exchange dials again.
func TestTCPDialLinkDropsADeadConnection(t *testing.T) {
	id := [16]byte{8}
	conns := make(chan net.Conn, 4)
	conn := tcpEndpoint(t, protocol.KindReversePersistent, fakeTCPChannel(t, func(c net.Conn) { conns <- c }))
	conn.Endpoint.Params["request_timeout"] = "300ms"
	l := newTCPDialLink(conn)
	defer l.close()
	if _, err := l.exchange(id, tcpKeys); err == nil {
		t.Fatal("an exchange nobody answered worked")
	}
	first := <-conns
	defer func() { _ = first.Close() }()
	waitLost(t, l.lostCh(), "a reply not in time")
	exchangeAsync(l, id)
	select {
	case <-conns:
	case <-time.After(5 * time.Second):
		t.Fatal("not dialed again after a reply not in time")
	}
}

// The bind-persistent link: the same, with the channel dialing the minion.
func TestTCPLinkPushesNeverBlock(t *testing.T) {
	id := [16]byte{7}
	port := freePort(t)
	conn := connection{protocol.BuildConnection{Endpoint: protocol.ChannelEndpointExport{Name: "tcp-b", Kind: protocol.KindBindPersistent,
		Channel: protocol.ExportChannel{ModuleName: "tcp", SchemaVersion: 1},
		Params:  map[string]any{"listen_address": "127.0.0.1", "listen_port": float64(port), "request_timeout": "1s"}}}}
	l := newTCPLink(conn)
	defer l.close()
	dial := func() net.Conn {
		t.Helper()
		for range 50 {
			c, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
			if err == nil {
				return c
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatal("the minion does not listen")
		return nil
	}
	channel := dial()
	r := bufio.NewReader(channel)
	done := exchangeAsync(l, id)
	_, frame, err := readTCPFrame(r)
	if err != nil {
		t.Fatal(err)
	}
	for range pushBurst {
		_ = writeTCPFrame(channel, id[:], cancelPush(t, id))
	}
	_ = writeTCPFrame(channel, id[:], coreAnswer(t, id, frame))
	if err := <-done; err != nil {
		t.Fatalf("the reply behind %d pushes: %v", pushBurst, err)
	}
	if got := len(l.pushed().take()); got != pushBurst {
		t.Fatalf("%d pushes queued, not %d", got, pushBurst)
	}
	// No reply in time: the connection is dropped and closed, and the next
	// exchange goes over the channel's next dial.
	if _, err := l.exchange(id, tcpKeys); err == nil {
		t.Fatal("an exchange nobody answered worked")
	}
	waitLost(t, l.lostCh(), "a reply not in time")
	if _, _, err := readTCPFrame(r); err != nil {
		t.Fatalf("the unanswered frame: %v", err)
	}
	_ = channel.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, _, err := readTCPFrame(r); err == nil {
		t.Fatal("the dropped connection still open")
	}
	again := dial()
	defer func() { _ = again.Close() }()
	done = exchangeAsync(l, id)
	_, frame, err = readTCPFrame(bufio.NewReader(again))
	if err != nil {
		t.Fatal(err)
	}
	_ = writeTCPFrame(again, id[:], coreAnswer(t, id, frame))
	if err := <-done; err != nil {
		t.Fatalf("over the next dial: %v", err)
	}
}

// The WebSocket link: a burst of pushes before each reply, none taken.
func TestWSLinkPushesNeverBlock(t *testing.T) {
	keys := tcpKeys
	ch := newWSChannel(t, keys)
	ch.burst = pushBurst
	link := newWSLink(wsTestConnection(ch.srv.URL))
	defer link.close()
	id := [16]byte{9}
	for i := range 3 {
		if _, err := link.exchange(id, keys); err != nil {
			t.Fatalf("exchange %d behind %d pushes: %v", i+1, pushBurst, err)
		}
	}
	// Each burst, and the task after the first message.
	deadline := time.Now().Add(5 * time.Second)
	got := 0
	for got < 3*pushBurst+1 && time.Now().Before(deadline) {
		got += len(link.pushes.take())
		time.Sleep(10 * time.Millisecond)
	}
	if got != 3*pushBurst+1 {
		t.Fatalf("%d pushes, not %d", got, 3*pushBurst+1)
	}
}

// A WebSocket that does not answer in time is dropped: the next exchange
// opens another.
func TestWSLinkDropsADeadSocket(t *testing.T) {
	keys := tcpKeys
	ch := newWSChannel(t, keys)
	ch.silentAt = 2
	conn := wsTestConnection(ch.srv.URL)
	conn.Endpoint.Params["request_timeout"] = "300ms"
	link := newWSLink(conn)
	defer link.close()
	id := [16]byte{9}
	if _, err := link.exchange(id, keys); err != nil {
		t.Fatal(err)
	}
	if _, err := link.exchange(id, keys); err == nil {
		t.Fatal("an exchange nobody answered worked")
	}
	waitLost(t, link.lost, "a reply not in time")
	if _, err := link.exchange(id, keys); err != nil {
		t.Fatalf("over another WebSocket: %v", err)
	}
	ch.mu.Lock()
	defer ch.mu.Unlock()
	if ch.conns != 2 {
		t.Fatalf("%d WebSockets, not 2", ch.conns)
	}
}
