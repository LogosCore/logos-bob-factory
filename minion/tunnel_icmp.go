package main

import (
	"net"

	"github.com/logoscore/logos-golang-protocol/protocol"
)

// Pings (docs: contracts/vpn-front, ICMP): an open of kind icmp is a
// stream of echo requests to any host, as a UDP association is of
// datagrams. Core's tunnel_datagrams each name the host to ping and carry
// an echo request (its sequence and payload; the identifier is the
// minion's socket's); what answers goes back to core as datagrams from the
// address that answered: an echo reply, or an error for an echo the minion
// sent (unreachable, time exceeded) where the host's socket delivers one.
// The pinger is the host's own (icmp_unix.go: an unprivileged ICMP
// socket, else a raw one; icmp_windows.go: IcmpSendEcho): with none, the
// open is answered denied.

// pinger sends a stream's echo requests; answers come through its deliver.
type pinger interface {
	echo(dst net.IP, seq uint16, payload []byte) error
	close()
}

// openICMP opens a stream of pings and answers core; refuse says why the
// host gives none (ts.mu held).
func (ts *tunnels) openICMP(t *tunnel, id uint64, refuse func(result int, msg string)) {
	st := &tstream{t: t, id: id, done: make(chan struct{}), inSignal: make(chan struct{}, 1), outSignal: make(chan struct{}, 1)}
	p, err := newPinger(func(from net.IP, m protocol.ICMPMessage) { ts.answered(st, from, m) })
	if err != nil {
		refuse(protocol.TunnelDenied, clip(err.Error(), protocol.MaxTunnelMsg))
		return
	}
	r := protocol.TunnelReplyItem{T: protocol.ItemTunnelReply, Task: t.task, Stream: id, Result: protocol.TunnelOpen}
	st.reply, st.icmp = &r, p
	t.streams[id] = st
	t.replies = append(t.replies, r)
	ts.nudge()
}

// answered queues for core what answered one of the stream's echoes; past
// udpQueue it is dropped, as a datagram is.
func (ts *tunnels) answered(st *tstream, from net.IP, m protocol.ICMPMessage) {
	host := from.String()
	v6 := true
	if v4 := from.To4(); v4 != nil {
		host, v6 = v4.String(), false
	}
	data := m.Bytes(v6)
	ts.mu.Lock()
	if st.gone || st.dgramBytes+len(data) > udpQueue {
		ts.mu.Unlock()
		return
	}
	st.dgrams = append(st.dgrams, protocol.TunnelDatagramItem{T: protocol.ItemTunnelDatagram, Task: st.t.task, Stream: st.id, Seq: st.dseq,
		Host: host, Data: data})
	st.dseq++
	st.dgramBytes += len(data)
	ts.mu.Unlock()
	ts.nudge()
}

// ping sends core's echo request (ts.mu held, released): to an address,
// once each.
func (ts *tunnels) ping(st *tstream, d protocol.TunnelDatagramItem) {
	if !st.seen.take(d.Seq) {
		ts.mu.Unlock()
		return
	}
	p := st.icmp
	ts.mu.Unlock()
	ip := net.ParseIP(d.Host)
	if ip == nil {
		return
	}
	v6 := ip.To4() == nil
	m, err := protocol.ReadICMP(d.Data, v6)
	req, _ := protocol.ICMPEchoTypes(v6)
	if err != nil || m.Type != req {
		return
	}
	_ = p.echo(ip, m.Seq, m.Payload)
}

// answer reads what an ICMP socket got: an echo reply, or an error that
// quotes an echo request (its IP header, then the echo's header), as the
// protocol has them; ok false for anything else. raw: an error's quote is
// the socket's own IP packet.
func answer(b []byte, v6 bool) (protocol.ICMPMessage, bool) {
	if len(b) < 8 {
		return protocol.ICMPMessage{}, false
	}
	req, rep := protocol.ICMPEchoTypes(v6)
	m := protocol.ICMPMessage{Type: int(b[0]), Code: int(b[1])}
	switch {
	case m.Type == rep:
		e, err := protocol.ReadICMP(b, v6)
		return e, err == nil
	case m.IsError(v6):
		q := b[8:]
		ihl := 40
		if !v6 {
			if len(q) < 1 || q[0]>>4 != 4 {
				return protocol.ICMPMessage{}, false
			}
			ihl = int(q[0]&0x0f) * 4
		}
		if len(q) < ihl+8 || int(q[ihl]) != req {
			return protocol.ICMPMessage{}, false
		}
		e := q[ihl:]
		m.Quoted = &protocol.ICMPMessage{Type: req, ID: uint16(e[4])<<8 | uint16(e[5]), Seq: uint16(e[6])<<8 | uint16(e[7])}
		return m, true
	}
	return protocol.ICMPMessage{}, false
}
