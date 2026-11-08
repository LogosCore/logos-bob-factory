package main

import (
	"testing"
	"time"

	"github.com/logoscore/logos-golang-protocol/protocol"
)

// An icmp stream pings from the host: core's echo request to 127.0.0.1
// comes back as an echo reply from it, its sequence and payload core's;
// one that is no echo request goes nowhere, one sent twice goes once.
func TestTunnelPings(t *testing.T) {
	r := startSocks(t, nil)
	r.take(coreSays(t, protocol.TunnelItem{T: protocol.ItemTunnel, Task: socksTask, Stream: 1, Kind: protocol.TunnelKindICMP}), nil)
	var reply *protocol.TunnelReplyItem
	deadline := time.Now().Add(5 * time.Second)
	for reply == nil && time.Now().Before(deadline) {
		items := r.outgoing()
		r.delivered(items)
		for _, it := range items {
			if rep, ok := it.(protocol.TunnelReplyItem); ok {
				reply = &rep
			}
		}
		time.Sleep(2 * time.Millisecond)
	}
	if reply == nil {
		t.Fatal("no reply")
	}
	if reply.Result == protocol.TunnelDenied {
		t.Skipf("this host gives no ICMP socket: %s", reply.Msg)
	}
	if reply.Result != protocol.TunnelOpen {
		t.Fatalf("reply %+v", reply)
	}
	ping := func(seq uint64, m protocol.ICMPMessage) protocol.TunnelDatagramItem {
		return protocol.TunnelDatagramItem{T: protocol.ItemTunnelDatagram, Task: socksTask, Stream: 1, Seq: seq, Host: "127.0.0.1",
			Data: m.Bytes(false)}
	}
	echo := protocol.ICMPMessage{Type: protocol.ICMPEchoRequest, ID: 1, Seq: 77, Payload: []byte("logos ping")}
	r.take(coreSays(t, ping(0, echo), ping(0, echo), ping(1, protocol.ICMPMessage{Type: protocol.ICMPEchoReply, Seq: 78})), nil)
	var got []protocol.ICMPMessage
	deadline = time.Now().Add(3 * time.Second)
	var settled time.Time
	for time.Now().Before(deadline) && (settled.IsZero() || time.Since(settled) < 200*time.Millisecond) {
		items := r.outgoing()
		r.delivered(items)
		for _, it := range items {
			if d, ok := it.(protocol.TunnelDatagramItem); ok {
				m, err := protocol.ReadICMP(d.Data, false)
				if err != nil || d.Host != "127.0.0.1" || d.Port != 0 {
					t.Fatalf("a datagram %+v: %v", d, err)
				}
				got = append(got, m)
				if settled.IsZero() {
					settled = time.Now()
				}
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	if len(got) != 1 || got[0].Type != protocol.ICMPEchoReply || got[0].Seq != 77 || string(got[0].Payload) != "logos ping" {
		t.Fatalf("answers: %+v", got)
	}
	r.take(coreSays(t, protocol.TunnelCloseItem{T: protocol.ItemTunnelClose, Task: socksTask, Stream: 1, Reason: protocol.TunnelCloseDone}), nil)
}

// What a raw socket gets: an echo reply as it is, an error whose quote is
// the minion's echo (behind its IP header), anything else not.
func TestICMPAnswer(t *testing.T) {
	rep := protocol.ICMPMessage{Type: protocol.ICMPEchoReply, ID: 9, Seq: 3, Payload: []byte("x")}.Bytes(false)
	if m, ok := answer(rep, false); !ok || m.Seq != 3 || m.ID != 9 {
		t.Errorf("a reply: %+v %v", m, ok)
	}
	quote := make([]byte, 20)
	quote[0] = 0x45
	quote = append(quote, protocol.ICMPMessage{Type: protocol.ICMPEchoRequest, ID: 9, Seq: 4}.Bytes(false)[:8]...)
	unreach := append([]byte{protocol.ICMPDestUnreachable, protocol.ICMPHostUnreachable, 0, 0, 0, 0, 0, 0}, quote...)
	if m, ok := answer(unreach, false); !ok || m.Quoted == nil || m.Quoted.Seq != 4 || m.Quoted.ID != 9 || m.Code != protocol.ICMPHostUnreachable {
		t.Errorf("an unreachable: %+v %v", m, ok)
	}
	if _, ok := answer(protocol.ICMPMessage{Type: protocol.ICMPEchoRequest, Seq: 1}.Bytes(false), false); ok {
		t.Error("an echo request taken as an answer")
	}
}
