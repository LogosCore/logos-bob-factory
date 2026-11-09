package main

import (
	"bufio"
	"context"
	"encoding/hex"
	"fmt"
	"log"
	"net"
	"strconv"
	"time"

	"github.com/logoscore/logos-golang-protocol/protocol"
)

// A reverse delegate (docs: contracts/minion-messages/delegation/): while a
// task of the reverse_delegate command runs (opReverseDelegate), the minion
// listens as the TCP channel's reverse kinds do, and carries the minions
// that dial it to core: each connection a reverse-persistent one (its first
// frame opens it in core: a frame item naming serve 1 and its address),
// or, with the task's kind reverse-call, each a single exchange (a sync,
// core's reply written back under the same id, the id alone when core had
// nothing; no reply, the connection closed, when core refused it). The
// minions that dial it are built for the TCP channel with its address: it
// is the channel to them. It numbers what it takes, from 1.

// reverseCallWait bounds how long a reverse call waits for core's reply:
// below the TCP channel's request_timeout default (40s), so the minion
// hears the failure and sends again rather than giving up first.
var reverseCallWait = 35 * time.Second

// reverseFirstWait bounds how long a minion that dialed may take to send
// its first frame.
var reverseFirstWait = 30 * time.Second

// runReverseDelegate listens where its args say (bind, 0.0.0.0 unless they
// say; listen; kind: reverse-persistent unless reverse-call;
// max_connections) until core stops it, when it ends ok, its listener and
// connections closed. A listener that does not open ends it failed.
func (r *runner) runReverseDelegate(ctx context.Context, t *task, args protocol.Args) {
	bind, ok := protocol.ArgString(args, reverseDelegateBind)
	if !ok || bind == "" {
		bind = "0.0.0.0"
	}
	listen, _ := protocol.ArgInt(args, reverseDelegateListen)
	kind, ok := protocol.ArgString(args, reverseDelegateKind)
	if !ok || kind == "" {
		kind = protocol.KindReversePersistent
	}
	most, ok := protocol.ArgInt(args, reverseDelegateMaxConnections)
	if !ok || most < 1 {
		most = 16
	}
	if kind != protocol.KindReversePersistent && kind != protocol.KindReverseCall {
		r.end(t, false, nil, fmt.Sprintf("kind %q is not reverse-persistent or reverse-call", kind))
		return
	}
	if listen < 1 || listen > 65535 {
		r.end(t, false, nil, "a listen port, 1 to 65535, is required")
		return
	}
	address := net.JoinHostPort(bind, strconv.FormatInt(listen, 10))
	ln, err := net.Listen("tcp", address)
	if err != nil {
		r.end(t, false, nil, fmt.Sprintf("listen on %s: %v", address, err))
		return
	}
	if !r.dlg.listening(t.id, ln) {
		_ = ln.Close()
		r.end(t, false, nil, "the delegate task ended before it listened")
		return
	}
	r.write(t, []byte(fmt.Sprintf("listening on %s for minions that dial it (%s)\n", ln.Addr(), kind)), nil)
	r.nudge()
	log.Printf("task %s: reverse delegate listening on %s (%s)", hex.EncodeToString(t.id)[:8], ln.Addr(), kind)
	go r.dlg.serve(t.id, ln, kind, int(most))
	<-ctx.Done()
	n := r.dlg.close(t.id)
	log.Printf("task %s: the reverse delegate stops, with %d connection(s)", hex.EncodeToString(t.id)[:8], n)
	r.end(t, true, nil, "")
}

// listening keeps the task's listener: false when the task ended first.
func (ds *delegates) listening(task []byte, ln net.Listener) bool {
	ds.mu.Lock()
	defer ds.mu.Unlock()
	d := ds.byTask[string(task)]
	if d == nil || d.closed {
		return false
	}
	d.ln = ln
	return true
}

// serve takes the connections that come to ln until it closes.
func (ds *delegates) serve(task []byte, ln net.Listener, kind string, most int) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		if tc, ok := c.(*net.TCPConn); ok {
			_ = tc.SetKeepAlive(true)
			_ = tc.SetKeepAlivePeriod(30 * time.Second)
		}
		ds.mu.Lock()
		d := ds.byTask[string(task)]
		full := d == nil || d.closed || len(d.conns)+len(d.syncs) >= most
		var n uint64
		if !full {
			d.nextN++
			n = d.nextN
		}
		ds.mu.Unlock()
		if full {
			if d != nil && !d.closed {
				report(protocol.FeedbackWarning, "delegate_refused", d.task, "a minion from %s came past max_connections (%d): closed",
					c.RemoteAddr(), most)
			}
			_ = c.Close()
			continue
		}
		if kind == protocol.KindReverseCall {
			go ds.call(d, n, c)
		} else {
			go ds.accept(d, n, c)
		}
	}
}

// peerBytes is c's remote address, raw, as a delegate item's peer.
func peerBytes(c net.Conn) []byte {
	tcp, ok := c.RemoteAddr().(*net.TCPAddr)
	if !ok {
		return nil
	}
	if ip := tcp.IP.To4(); ip != nil {
		return ip
	}
	return tcp.IP.To16()
}

// accept carries a reverse-persistent connection: its frames to core, its
// first naming the kind served and its address, and core's back.
func (ds *delegates) accept(d *delegation, n uint64, c net.Conn) {
	dc := &delegateConn{c: c, writes: make(chan delegateFrame, delegateWriteQueue)}
	ds.mu.Lock()
	if d.closed {
		ds.mu.Unlock()
		_ = c.Close()
		return
	}
	d.conns[n] = dc
	ds.mu.Unlock()
	log.Printf("delegate %x: connection %d in from %s", d.task[:4], n, c.RemoteAddr())
	go ds.writer(dc)
	ds.reader(d, n, dc, &protocol.DelegateItem{Serve: 1, Peer: peerBytes(c)})
}

// call carries one reverse-call exchange: the minion's message as a sync,
// core's reply written back, then the connection closes.
func (ds *delegates) call(d *delegation, n uint64, c net.Conn) {
	defer func() { _ = c.Close() }()
	_ = c.SetReadDeadline(time.Now().Add(reverseFirstWait))
	id, data, err := readTCPFrame(bufio.NewReader(c))
	if err != nil {
		return
	}
	reply := make(chan protocol.DelegateItem, 1)
	ds.mu.Lock()
	if d.closed {
		ds.mu.Unlock()
		return
	}
	d.syncs[n] = reply
	ds.queueLocked(protocol.DelegateItem{Task: d.task, Step: protocol.DelegateSync, N: n, Serve: 1, Peer: peerBytes(c), ID: id[:], Frame: data})
	ds.mu.Unlock()
	ds.nudge()
	var r protocol.DelegateItem
	var ok bool
	select {
	case r, ok = <-reply:
	case <-time.After(reverseCallWait):
	}
	ds.mu.Lock()
	delete(d.syncs, n)
	d.join.Drop(n)
	ds.mu.Unlock()
	if !ok {
		log.Printf("delegate %x: call %d: no reply from core in time", d.task[:4], n)
		return
	}
	if r.Status != protocol.ReplyOK && r.Status != protocol.ReplyNoContent {
		// Core refused it: no reply, as the TCP channel closes one.
		log.Printf("delegate %x: call %d: core answered %d", d.task[:4], n, r.Status)
		return
	}
	_ = c.SetWriteDeadline(time.Now().Add(time.Minute))
	_ = writeTCPFrame(c, id[:], r.Frame)
}

// replyLocked takes core's reply to a reverse call: joined from its
// pieces, given to the exchange that waits for it (none: it gave up).
func (ds *delegates) replyLocked(d *delegation, it protocol.DelegateItem) {
	ch := d.syncs[it.N]
	if ch == nil {
		return
	}
	frame, done, err := d.join.Add(it, time.Now())
	if err != nil || !done {
		return
	}
	it.Frame, it.Off, it.Size = frame, 0, 0
	select {
	case ch <- it:
	default:
	}
}
