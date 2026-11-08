package main

import (
	"bytes"
	"context"
	"errors"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/logoscore/logos-golang-protocol/protocol"
)

// UDP associations (tunnel.go): a socket of the minion's per association,
// core's datagrams sent from it, what comes back to it sent to core.
//
// A UDP reverse port forward: one socket, the task's, listening where its
// args say. Each address datagrams come from is a stream the minion opens
// (a udp tunnel naming it), its datagrams held until core answers, then
// sent as tunnel_datagram items with that address; core's for it go to it
// from the socket, whatever address they name. A stream with no datagram
// either way for udpIdle ends done; its address opens a new one next time.

// datagrams: the stream carries datagrams, as an association or a UDP
// reverse forward's stream core opened.
func (st *tstream) datagrams() bool { return st.pc != nil || st.sourceOpen || st.icmp != nil }

// listenUDP opens a UDP reverse forward's socket at address, and has core
// told where it listens. An error says why it did not open.
func (ts *tunnels) listenUDP(task []byte, address string) (net.PacketConn, error) {
	pc, err := net.ListenPacket("udp", address)
	if err != nil {
		return nil, err
	}
	ts.mu.Lock()
	defer ts.mu.Unlock()
	t := ts.byTask[string(task)]
	if t == nil {
		_ = pc.Close()
		return nil, errors.New("the tunnel closed as it opened")
	}
	t.pc, t.bySource = pc, map[string]*tstream{}
	t.bound, t.sayListen = clip(pc.LocalAddr().String(), protocol.MaxTunnelBound), true
	ts.nudge()
	return pc, nil
}

// sources reads the socket's datagrams, each its source's stream's, until
// the socket closes (the task's end): why it stopped. Any other error (a
// host short of buffers, a reset Windows reports for an earlier send) is
// waited out; streams that idle end meanwhile.
func (ts *tunnels) sources(task []byte, pc net.PacketConn) error {
	ts.mu.Lock()
	t := ts.byTask[string(task)]
	ts.mu.Unlock()
	if t == nil {
		return errors.New("the tunnel is closed")
	}
	stop := make(chan struct{})
	defer close(stop)
	go ts.endIdle(t, stop)
	buf := make([]byte, udpRead)
	var wait time.Duration
	for {
		n, from, err := pc.ReadFrom(buf)
		if err != nil {
			if errors.Is(err, net.ErrClosed) || t.ctx.Err() != nil {
				return err
			}
			wait = min(max(2*wait, 5*time.Millisecond), time.Second)
			select {
			case <-t.ctx.Done():
				return t.ctx.Err()
			case <-time.After(wait):
			}
			continue
		}
		wait = 0
		if ua, ok := from.(*net.UDPAddr); ok {
			ts.fromSource(t, ua, buf[:n])
		}
	}
}

// fromSource takes a datagram from src: its stream's, opened when src has
// none (past max_streams, dropped). It waits for core's answer with the
// stream's, as a UDP datagram goes: one that finds udpQueue full is
// dropped.
func (ts *tunnels) fromSource(t *tunnel, src *net.UDPAddr, data []byte) {
	host := src.IP.String()
	if v4 := src.IP.To4(); v4 != nil {
		host = v4.String()
	}
	if src.Port <= 0 || host == "" || len(host) > protocol.MaxTunnelHost {
		return
	}
	port := uint64(src.Port)
	key := net.JoinHostPort(host, strconv.FormatUint(port, 10))
	ts.mu.Lock()
	defer ts.mu.Unlock()
	if ts.byTask[string(t.task)] != t {
		return
	}
	st := t.bySource[key]
	if st == nil {
		if len(t.streams) >= t.maxStreams {
			return
		}
		t.last++
		st = &tstream{t: t, id: t.last, source: &net.UDPAddr{IP: src.IP, Port: src.Port, Zone: src.Zone}, sourceKey: key,
			done: make(chan struct{}), inSignal: make(chan struct{}, 1), outSignal: make(chan struct{}, 1),
			opening: &protocol.TunnelItem{T: protocol.ItemTunnel, Task: t.task, Stream: t.last, Host: host, Port: port, UDP: true}}
		t.streams[st.id] = st
		t.bySource[key] = st
		time.AfterFunc(t.timeout+ts.slack, func() { ts.gaveUp(st) })
	}
	st.active = time.Now()
	if len(data) > protocol.MaxTunnelDatagram || st.dgramBytes+len(data) > udpQueue {
		return
	}
	st.dgrams = append(st.dgrams, protocol.TunnelDatagramItem{T: protocol.ItemTunnelDatagram, Task: t.task, Stream: st.id, Seq: st.dseq,
		Host: host, Port: port, Data: bytes.Clone(data)})
	st.dseq++
	st.dgramBytes += len(data)
	ts.nudge()
}

// endIdle ends the tunnel's open UDP streams that had no datagram either
// way for udpIdle, telling core (done), until stop closes or the task ends.
func (ts *tunnels) endIdle(t *tunnel, stop <-chan struct{}) {
	tick := time.NewTicker(max(ts.udpIdle/8, 10*time.Millisecond))
	defer tick.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.ctx.Done():
			return
		case <-tick.C:
		}
		ts.mu.Lock()
		now, ended := time.Now(), false
		for _, st := range t.streams {
			if st.sourceOpen && now.Sub(st.active) >= ts.udpIdle {
				t.closes = append(t.closes, protocol.TunnelCloseItem{T: protocol.ItemTunnelClose, Task: t.task, Stream: st.id,
					Reason: protocol.TunnelCloseDone})
				ts.drop(st, false)
				ended = true
			}
		}
		if ended {
			ts.nudge()
		}
		ts.mu.Unlock()
	}
}

// openUDP opens an association's socket, on every interface, and answers
// core with its address; refuse says why it could not.
func (ts *tunnels) openUDP(t *tunnel, id uint64, refuse func(result int, msg string)) {
	pc, err := net.ListenPacket("udp", ":0")
	if err != nil {
		refuse(protocol.TunnelFailed, clip(err.Error(), protocol.MaxTunnelMsg))
		return
	}
	r := protocol.TunnelReplyItem{T: protocol.ItemTunnelReply, Task: t.task, Stream: id, Result: protocol.TunnelOpen,
		Bound: clip(pc.LocalAddr().String(), protocol.MaxTunnelBound)}
	st := &tstream{t: t, id: id, pc: pc, reply: &r, done: make(chan struct{}), inSignal: make(chan struct{}, 1), outSignal: make(chan struct{}, 1)}
	t.streams[id] = st
	t.replies = append(t.replies, r)
	go ts.readUDP(st)
	ts.nudge()
}

// readUDP queues what comes to an association's socket for core, with its
// source; past udpQueue, a datagram is dropped.
func (ts *tunnels) readUDP(st *tstream) {
	buf := make([]byte, udpRead)
	for {
		n, from, err := st.pc.ReadFrom(buf)
		if err != nil {
			return
		}
		ua, ok := from.(*net.UDPAddr)
		if !ok {
			continue
		}
		host := ua.IP.String()
		if v4 := ua.IP.To4(); v4 != nil {
			host = v4.String()
		}
		d := protocol.TunnelDatagramItem{T: protocol.ItemTunnelDatagram, Task: st.t.task, Stream: st.id, Host: host, Port: uint64(ua.Port),
			Data: bytes.Clone(buf[:n])}
		ts.mu.Lock()
		if st.gone {
			ts.mu.Unlock()
			return
		}
		queued := st.dgramBytes+n <= udpQueue && n <= protocol.MaxTunnelDatagram
		if queued {
			d.Seq = st.dseq
			st.dseq++
			st.dgrams = append(st.dgrams, d)
			st.dgramBytes += n
		}
		ts.mu.Unlock()
		if queued {
			ts.nudge()
		}
	}
}

// takeDatagram takes the first datagram off the queue: sent, or dropped.
func (st *tstream) takeDatagram() {
	st.dgramBytes -= len(st.dgrams[0].Data)
	st.dgrams[0] = protocol.TunnelDatagramItem{}
	st.dgrams = st.dgrams[1:]
}

// sendDatagram sends core's datagram from the association's socket to the
// destination it names (a name resolved here, as a connect's is); a UDP
// reverse forward's, from its socket to the stream's source, whatever it
// names. One sent twice goes once.
func (ts *tunnels) sendDatagram(d protocol.TunnelDatagramItem) {
	ts.mu.Lock()
	st, _ := ts.find(d.Task, d.Stream)
	if st == nil {
		ts.closeUnknown(d.Task, d.Stream)
		ts.mu.Unlock()
		return
	}
	if st.icmp != nil {
		ts.ping(st, d)
		return
	}
	if st.source != nil {
		if !st.sourceOpen || st.t.pc == nil || !st.seen.take(d.Seq) {
			ts.mu.Unlock()
			return
		}
		pc, to := st.t.pc, st.source
		st.active = time.Now()
		ts.mu.Unlock()
		_, _ = pc.WriteTo(d.Data, to)
		return
	}
	if st.pc == nil || !st.seen.take(d.Seq) {
		ts.mu.Unlock()
		return
	}
	pc := st.pc
	ts.mu.Unlock()
	address := net.JoinHostPort(d.Host, strconv.FormatUint(d.Port, 10))
	if ip := net.ParseIP(d.Host); ip != nil {
		_, _ = pc.WriteTo(d.Data, &net.UDPAddr{IP: ip, Port: int(d.Port)})
		return
	}
	// A name: resolved apart, so a slow lookup holds nothing up.
	go func() {
		if to, err := udpLookups.lookup(address); err == nil {
			_, _ = pc.WriteTo(d.Data, to)
		}
	}()
}

// udpNames keeps the names datagrams went to, resolved, for a minute: a
// burst of datagrams to one name looks it up once.
type udpNames struct {
	mu   sync.Mutex
	byAt map[string]resolved
}

type resolved struct {
	addr *net.UDPAddr
	at   time.Time
}

var udpLookups = &udpNames{byAt: map[string]resolved{}}

func (n *udpNames) lookup(address string) (*net.UDPAddr, error) {
	n.mu.Lock()
	r, ok := n.byAt[address]
	n.mu.Unlock()
	if ok && time.Since(r.at) < time.Minute {
		return r.addr, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}
	if len(ips) == 0 {
		return nil, errors.New("no address")
	}
	p, _ := strconv.Atoi(port)
	addr := &net.UDPAddr{IP: ips[0].IP, Port: p}
	n.mu.Lock()
	if len(n.byAt) > 1024 {
		n.byAt = map[string]resolved{}
	}
	n.byAt[address] = resolved{addr: addr, at: time.Now()}
	n.mu.Unlock()
	return addr, nil
}

// replayWindow takes each of a sequence's numbers once, as a message sent
// again repeats them: the latest, and the 64 before it.
type replayWindow struct {
	started bool
	last    uint64
	seen    uint64
}

func (w *replayWindow) take(seq uint64) bool {
	switch {
	case !w.started:
		w.started, w.last, w.seen = true, seq, 1
		return true
	case seq > w.last:
		if shift := seq - w.last; shift < 64 {
			w.seen <<= shift
		} else {
			w.seen = 0
		}
		w.last, w.seen = seq, w.seen|1
		return true
	case w.last-seq >= 64:
		return false
	}
	bit := uint64(1) << (w.last - seq)
	if w.seen&bit != 0 {
		return false
	}
	w.seen |= bit
	return true
}
