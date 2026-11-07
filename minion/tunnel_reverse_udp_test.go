package main

import (
	"net"
	"strings"
	"testing"
	"time"

	"github.com/logoscore/logos-golang-protocol/protocol"
)

// startReverseUDP is a runner running a UDP reverse forward on 127.0.0.1,
// at a free port unless args say, its resends quick.
func startReverseUDP(t *testing.T, args protocol.Args) *runner {
	t.Helper()
	if args == nil {
		args = protocol.Args{}
	}
	args[reversePortForwardUDP] = true
	return startReverse(t, args)
}

// answerUDP answers each stream the minion opens with result, and no
// stream of bytes: a UDP stream's go as datagrams.
func answerUDP(result int) func(protocol.TunnelItem) (protocol.TunnelReplyItem, *coreStream) {
	return func(it protocol.TunnelItem) (protocol.TunnelReplyItem, *coreStream) {
		return protocol.TunnelReplyItem{T: protocol.ItemTunnelReply, Task: it.Task, Stream: it.Stream, Result: result}, nil
	}
}

// udpClient is a UDP socket on 127.0.0.1 that sends to the minion's.
func udpClient(t *testing.T, core *tunnelCore) (net.PacketConn, *net.UDPAddr) {
	t.Helper()
	core.until("it listens", func() bool { return len(core.listens) > 0 })
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pc.Close() })
	to, err := net.ResolveUDPAddr("udp", core.listens[0].Bound)
	if err != nil {
		t.Fatal(err)
	}
	return pc, to
}

func send(t *testing.T, pc net.PacketConn, to *net.UDPAddr, data string) {
	t.Helper()
	if _, err := pc.WriteTo([]byte(data), to); err != nil {
		t.Fatal(err)
	}
}

// recv is what pc gets within a second: "" for nothing.
func recv(pc net.PacketConn) string {
	_ = pc.SetReadDeadline(time.Now().Add(time.Second))
	buf := make([]byte, 2048)
	n, _, err := pc.ReadFrom(buf)
	if err != nil {
		return ""
	}
	return string(buf[:n])
}

// dgramsOf are the minion's datagrams of a stream, as core got them.
func dgramsOf(core *tunnelCore, stream uint64) []protocol.TunnelDatagramItem {
	var out []protocol.TunnelDatagramItem
	for _, d := range core.datagrams {
		if d.Stream == stream {
			out = append(out, d)
		}
	}
	return out
}

// A UDP reverse forward listens where its args say and tells core where.
// A datagram from an address opens a stream naming it, held until core
// answers; then it and those after go as datagrams numbered from 0, naming
// their source, and core's go back to the source, once each.
func TestReverseUDPRelays(t *testing.T) {
	r := startReverseUDP(t, nil)
	core := newTunnelCore(t, r)
	client, to := udpClient(t, core)
	if b := core.listens[0].Bound; !strings.HasPrefix(b, "127.0.0.1:") || strings.HasSuffix(b, ":0") {
		t.Errorf("listening at %q", b)
	}
	local := client.LocalAddr().(*net.UDPAddr)
	send(t, client, to, "early")
	core.until("the stream opens", func() bool { return len(core.opens) > 0 })
	for range 5 {
		core.exchange()
	}
	if len(core.datagrams) != 0 {
		t.Fatalf("datagrams before core's answer: %+v", core.datagrams)
	}
	open := core.opens[0]
	if open.Stream != 1 || !open.UDP || open.Host != "127.0.0.1" || open.Port != uint64(local.Port) {
		t.Fatalf("open %+v, the client at %s", open, local)
	}
	core.answer = answerUDP(protocol.TunnelOpen)
	core.until("the held datagram", func() bool { return len(dgramsOf(core, 1)) > 0 })
	send(t, client, to, "later")
	core.until("the next datagram", func() bool { return len(dgramsOf(core, 1)) > 1 })
	got := dgramsOf(core, 1)
	for i, want := range []string{"early", "later"} {
		d := got[i]
		if d.Seq != uint64(i) || string(d.Data) != want || d.Host != "127.0.0.1" || d.Port != uint64(local.Port) {
			t.Errorf("datagram %d: %+v", i, d)
		}
	}
	back := func(seq uint64, data string) protocol.TunnelDatagramItem {
		// Whatever address core's datagram names, it goes to the source.
		return protocol.TunnelDatagramItem{T: protocol.ItemTunnelDatagram, Task: socksTask, Stream: 1, Seq: seq, Host: "10.9.9.9", Port: 9,
			Data: []byte(data)}
	}
	r.take(coreSays(t, back(0, "one"), back(0, "one"), back(1, "two")), nil)
	if a, b, c := recv(client), recv(client), recv(client); a != "one" || b != "two" || c != "" {
		t.Errorf("the source got %q %q %q", a, b, c)
	}
}

// Each address is a stream of its own; past max_streams, a new address's
// datagrams are dropped, with no stream for it.
func TestReverseUDPSources(t *testing.T) {
	r := startReverseUDP(t, protocol.Args{reversePortForwardMaxStreams: int64(2)})
	core := newTunnelCore(t, r)
	core.answer = answerUDP(protocol.TunnelOpen)
	a, to := udpClient(t, core)
	b, _ := udpClient(t, core)
	c, _ := udpClient(t, core)
	send(t, a, to, "a")
	core.until("a's stream", func() bool { return len(dgramsOf(core, 1)) > 0 })
	send(t, b, to, "b")
	core.until("b's stream", func() bool { return len(dgramsOf(core, 2)) > 0 })
	send(t, c, to, "c")
	for range 20 {
		core.exchange()
		time.Sleep(2 * time.Millisecond)
	}
	if len(core.opens) != 2 || core.opens[0].Port == core.opens[1].Port {
		t.Errorf("opens %+v", core.opens)
	}
	for _, d := range core.datagrams {
		if string(d.Data) == "c" {
			t.Errorf("a datagram past max_streams went: %+v", d)
		}
	}
	if string(dgramsOf(core, 1)[0].Data) != "a" || string(dgramsOf(core, 2)[0].Data) != "b" {
		t.Errorf("datagrams %+v", core.datagrams)
	}
}

// Core's refusal drops the stream with what it held; the address's next
// datagram opens a new one.
func TestReverseUDPRefused(t *testing.T) {
	r := startReverseUDP(t, nil)
	core := newTunnelCore(t, r)
	core.answer = answerUDP(protocol.TunnelRefused)
	client, to := udpClient(t, core)
	send(t, client, to, "held")
	core.until("core refuses", func() bool { return len(core.opens) > 0 })
	core.until("the stream goes", func() bool {
		r.tun.mu.Lock()
		defer r.tun.mu.Unlock()
		return len(r.tun.byTask[string(socksTask)].streams) == 0
	})
	core.answer = answerUDP(protocol.TunnelOpen)
	send(t, client, to, "again")
	core.until("a new stream", func() bool { return len(dgramsOf(core, 2)) > 0 })
	if len(dgramsOf(core, 1)) != 0 || string(dgramsOf(core, 2)[0].Data) != "again" {
		t.Errorf("datagrams %+v", core.datagrams)
	}
}

// A stream with no datagram either way for udpIdle ends done, and core is
// told; the address's next datagram opens a new stream.
func TestReverseUDPIdle(t *testing.T) {
	r := newRunner(description{Ops: []uint64{opReversePortForward}}, &schedule{interval: time.Second})
	r.tun.resend, r.tun.udpIdle = 100*time.Millisecond, 200*time.Millisecond
	r.start(protocol.TaskItem{T: protocol.ItemTask, Task: socksTask, Cmd: opReversePortForward,
		Args: protocol.Args{reversePortForwardBind: "127.0.0.1", reversePortForwardListen: int64(0), reversePortForwardUDP: true}})
	t.Cleanup(func() { r.tun.closeAll() })
	core := newTunnelCore(t, r)
	core.answer = answerUDP(protocol.TunnelOpen)
	client, to := udpClient(t, core)
	send(t, client, to, "one")
	core.until("the stream", func() bool { return len(dgramsOf(core, 1)) > 0 })
	idled := func() bool {
		for _, it := range core.other {
			if c, ok := it.(protocol.TunnelCloseItem); ok && c.Stream == 1 && c.Reason == protocol.TunnelCloseDone {
				return true
			}
		}
		return false
	}
	core.until("it idles", idled)
	send(t, client, to, "two")
	core.until("a new stream", func() bool { return len(dgramsOf(core, 2)) > 0 })
	if core.opens[len(core.opens)-1].Stream != 2 {
		t.Errorf("opens %+v", core.opens)
	}
}

// Core's cancel ends the task ok, its socket closed (the port free
// again); a UDP socket that does not open ends it failed, saying why, and
// tells core nothing.
func TestReverseUDPCancelAndListenFails(t *testing.T) {
	r := startReverseUDP(t, nil)
	core := newTunnelCore(t, r)
	core.answer = answerUDP(protocol.TunnelOpen)
	client, to := udpClient(t, core)
	send(t, client, to, "one")
	core.until("the stream", func() bool { return len(dgramsOf(core, 1)) > 0 })
	r.take(coreSays(t, protocol.CancelItem{T: protocol.ItemCancel, Task: socksTask}), nil)
	if end := waitEnd(t, r, 5*time.Second); !end.ok {
		t.Errorf("the task ended %+v", end)
	}
	again, err := net.ListenPacket("udp", core.listens[0].Bound)
	if err != nil {
		t.Fatalf("the socket stays open: %v", err)
	}
	defer func() { _ = again.Close() }()

	taken := again.LocalAddr().(*net.UDPAddr)
	failed := startReverseUDP(t, protocol.Args{reversePortForwardListen: int64(taken.Port)})
	end := waitEnd(t, failed, 5*time.Second)
	if end.ok || !strings.Contains(end.err, "listen udp") {
		t.Errorf("ended %+v", end)
	}
	for _, it := range failed.outgoing() {
		if _, ok := it.(protocol.TunnelListenItem); ok {
			t.Error("a tunnel_listen all the same")
		}
	}
}
