package main

import (
	"net"
	"strconv"
	"time"

	"github.com/logoscore/logos-golang-protocol/protocol"
)

// A stream's tunnel_ctl items (docs: contracts/minion-messages/tunnels.md,
// Controls): what it says besides its bytes, in order and none lost. The
// minion numbers its own from 0 and sends each again from core's ack (its
// Ctl) until it covers it: a process's exit, who came to an accept. It
// takes core's at its next, acking with its own acks: a terminal's size, a
// signal. And the two kinds of open that use them: a process (proc.go) and
// an accept, SOCKS5's BIND.

// queueCtl puts a ctl of the minion's on the stream, numbered. Under ts.mu.
func (ts *tunnels) queueCtl(st *tstream, c protocol.TunnelCtlItem) {
	if st.gone {
		return
	}
	c.T, c.Task, c.Stream, c.Seq = protocol.ItemTunnelCtl, st.t.task, st.id, st.ctlBase+uint64(len(st.ctlOut))
	st.ctlOut = append(st.ctlOut, c)
	ts.nudge()
}

// ctlDue: one of the stream's ctls is to go: one not sent, or sent and not
// acked for resend.
func (st *tstream) ctlDue(now time.Time, resend time.Duration) bool {
	if len(st.ctlOut) == 0 {
		return false
	}
	return st.ctlSent < st.ctlBase+uint64(len(st.ctlOut)) || now.Sub(st.ctlSentAt) >= resend
}

// putCtls puts what of the stream's ctls is due, from the ack again once
// they went unacked resend long.
func (st *tstream) putCtls(put func(any, int) bool, now time.Time, resend time.Duration) {
	if !st.ctlDue(now, resend) {
		return
	}
	from := st.ctlSent
	if from > st.ctlBase && now.Sub(st.ctlSentAt) >= resend {
		from = st.ctlBase
	}
	went := false
	for seq := from; seq < st.ctlBase+uint64(len(st.ctlOut)); seq++ {
		c := st.ctlOut[seq-st.ctlBase]
		if !put(c, tunnelItemSize+len(c.Host)) {
			break
		}
		st.ctlSent, went = max(st.ctlSent, seq+1), true
	}
	if went {
		st.ctlSentAt = now
	}
}

// ctlAcked drops the ctls core's ack covers (its Ctl: the next it takes);
// true when that left none.
func (st *tstream) ctlAcked(next uint64) bool {
	if next <= st.ctlBase || next > st.ctlBase+uint64(len(st.ctlOut)) {
		return false
	}
	st.ctlOut = st.ctlOut[next-st.ctlBase:]
	st.ctlBase = next
	st.ctlSent = max(st.ctlSent, next)
	if len(st.ctlOut) == 0 {
		st.ctlOut = nil
		return true
	}
	return false
}

// takeCtl takes core's ctl at the minion's next and acks it (one before is
// acked again; one past waits for core to send again): a process's new
// terminal size, a signal for it.
func (ts *tunnels) takeCtl(c protocol.TunnelCtlItem) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	st, _ := ts.find(c.Task, c.Stream)
	if st == nil {
		ts.closeUnknown(c.Task, c.Stream)
		return
	}
	if c.Seq > st.ctlIn {
		return
	}
	st.ackDue = true
	defer ts.nudge()
	if c.Seq < st.ctlIn {
		return
	}
	st.ctlIn++
	p := st.proc
	if p == nil {
		return
	}
	if c.Rows > 0 && c.Cols > 0 {
		_ = p.resize(int(c.Rows), int(c.Cols))
	}
	if c.Signal > 0 {
		// Off the lock: on Windows, taskkill takes a while.
		go func() { _ = p.signal(c.Signal) }()
	}
}

// startProc runs a stream's process and answers core: open (in a terminal
// or on pipes, as the reply says), or why not. Its exit goes to core as a
// ctl once its output ended, before the stream's eof.
func (ts *tunnels) startProc(st *tstream, it protocol.TunnelItem) {
	p, terminal, err := startProcess(it.Path, it.Args, int(it.Rows), int(it.Cols))
	ts.mu.Lock()
	defer ts.mu.Unlock()
	if st.gone {
		if p != nil {
			_ = p.Close()
		}
		return
	}
	r := protocol.TunnelReplyItem{T: protocol.ItemTunnelReply, Task: st.t.task, Stream: st.id}
	if err != nil {
		r.Result, r.Msg = procResult(err), clip(err.Error(), protocol.MaxTunnelMsg)
		st.t.replies = append(st.t.replies, r)
		ts.drop(st, false)
		ts.nudge()
		return
	}
	p.onExit = func(code int64) {
		ts.mu.Lock()
		defer ts.mu.Unlock()
		ts.queueCtl(st, protocol.TunnelCtlItem{Exit: &code})
	}
	r.Terminal = terminal
	st.conn, st.proc, st.reply = p, p, &r
	st.t.replies = append(st.t.replies, r)
	go ts.readHost(st)
	go ts.writeHost(st)
	ts.nudge()
}

// startAccept listens where the open says (every address of the host, any
// free port, as core asks) for one connection, and answers core with where
// it listens; who came goes in a ctl, then the stream is that connection's.
// No one within acceptWait, and the stream is reset.
func (ts *tunnels) startAccept(st *tstream, it protocol.TunnelItem) {
	ln, err := net.Listen("tcp", net.JoinHostPort(it.Host, strconv.FormatUint(it.Port, 10)))
	ts.mu.Lock()
	if st.gone {
		ts.mu.Unlock()
		if ln != nil {
			_ = ln.Close()
		}
		return
	}
	r := protocol.TunnelReplyItem{T: protocol.ItemTunnelReply, Task: st.t.task, Stream: st.id}
	if err != nil {
		r.Result, r.Msg = dialResult(err), clip(err.Error(), protocol.MaxTunnelMsg)
		if inUseErr(err) {
			r.Result = protocol.TunnelInUse
		}
		st.t.replies = append(st.t.replies, r)
		ts.drop(st, false)
		ts.nudge()
		ts.mu.Unlock()
		return
	}
	r.Bound = clip(ln.Addr().String(), protocol.MaxTunnelBound)
	st.ln, st.reply = ln, &r
	st.t.replies = append(st.t.replies, r)
	ts.nudge()
	wait := ts.acceptWait
	ts.mu.Unlock()
	if tl, ok := ln.(*net.TCPListener); ok {
		_ = tl.SetDeadline(time.Now().Add(wait))
	}
	c, err := ln.Accept()
	_ = ln.Close()
	ts.mu.Lock()
	defer ts.mu.Unlock()
	if st.gone {
		if c != nil {
			_ = c.Close()
		}
		return
	}
	st.ln = nil
	if err != nil {
		// No one came in time.
		ts.reset(st)
		return
	}
	host, port := peer(c)
	st.conn = c
	ts.queueCtl(st, protocol.TunnelCtlItem{Host: host, Port: port})
	go ts.readHost(st)
	go ts.writeHost(st)
}
