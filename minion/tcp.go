package main

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/logoscore/logos-golang-protocol/protocol"
)

// A bind-persistent connection (docs: contracts/channel-core-connections/,
// bind-persistent connections) through the TCP channel: the minion listens,
// and core asks the channel to dial it. Once a connection is in, the
// minion speaks first, as over a WebSocket (its stage, its hello, or a
// check-in after a reconnect), and core pushes over it. On the connection
// each message is one frame, both ways:
//
//	[length: 4 bytes, big-endian][id: 16 bytes][sealed message]
//
// length counting the id and the message. When the connection closes, the
// next exchange waits for the channel's next dial: the minion cannot reach
// out, it is reached.

// maxTCPFrame bounds a frame's length.
const maxTCPFrame = 16 + protocol.MaxMinionMessageSize + 4096

// tcpLink is the minion's listener and the connection the channel dialed.
type tcpLink struct {
	conn connection

	mu sync.Mutex
	// ln is the listener while it listens; stopped once closed for good.
	ln      net.Listener
	stopped bool
	// cur is the connection in use; nil while none is in.
	cur net.Conn
	// open counts the connections in, the one in use and those not closed
	// yet; arrived says one came in.
	open    int
	arrived chan struct{}
	keys    map[[16]byte]protocol.AES256CBCHMACSHA256Material
	waiting map[string]chan protocol.CoreMessage
	pushes  chan protocol.CoreMessage
	lost    chan struct{}
}

// newTCPLink starts listening as the connection says, and keeps listening
// until closed: a listen that fails (the port taken) is tried again.
func newTCPLink(conn connection) *tcpLink {
	l := &tcpLink{conn: conn, arrived: make(chan struct{}, 1), keys: map[[16]byte]protocol.AES256CBCHMACSHA256Material{},
		waiting: map[string]chan protocol.CoreMessage{}, pushes: make(chan protocol.CoreMessage, 16), lost: make(chan struct{}, 1)}
	go l.listen()
	return l
}

// listenAddr is where the minion listens: the endpoint's address and port.
func (c connection) listenAddr() string {
	addr := c.param("listen_address")
	if addr == "" {
		addr = "0.0.0.0"
	}
	return net.JoinHostPort(addr, strconv.Itoa(c.intParam("listen_port", 0)))
}

// intParam is an endpoint's integer parameter (JSON numbers come as
// float64), def when it has none.
func (c connection) intParam(key string, def int) int {
	switch n := c.Endpoint.Params[key].(type) {
	case float64:
		return int(n)
	case int:
		return n
	case int64:
		return int(n)
	}
	return def
}

// keepalive is the endpoint's TCP keepalive.
func (c connection) keepalive() time.Duration {
	if d, err := time.ParseDuration(c.param("keepalive")); err == nil && d > 0 {
		return d
	}
	return 30 * time.Second
}

func (l *tcpLink) listen() {
	for !l.isStopped() {
		ln, err := net.Listen("tcp", l.conn.listenAddr())
		if err != nil {
			log.Printf("listen on %s: %v; trying again in 30s", l.conn.listenAddr(), err)
			time.Sleep(30 * time.Second)
			continue
		}
		l.mu.Lock()
		if l.stopped {
			l.mu.Unlock()
			_ = ln.Close()
			return
		}
		l.ln = ln
		l.mu.Unlock()
		log.Printf("listening on %s for the channel's dial", ln.Addr())
		for {
			c, err := ln.Accept()
			if err != nil {
				if !l.isStopped() {
					log.Printf("accept: %v", err)
				}
				_ = ln.Close()
				break
			}
			l.take(c)
		}
	}
	log.Printf("no longer listening on %s", l.conn.listenAddr())
}

func (l *tcpLink) isStopped() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.stopped
}

// dialed says a connection came in (a switch waits on it); a receive
// consumes it, which wait does not need: it looks at the one in use.
func (l *tcpLink) dialed() <-chan struct{} { return l.arrived }

// take makes a connection that came in the one in use, while fewer than
// max_connections are in: the newest wins (the one before is likely stale,
// its peer gone without a word). One past the limit is closed at once.
func (l *tcpLink) take(c net.Conn) {
	if tc, ok := c.(*net.TCPConn); ok {
		_ = tc.SetKeepAlive(true)
		_ = tc.SetKeepAlivePeriod(l.conn.keepalive())
	}
	l.mu.Lock()
	if l.open >= max(1, l.conn.intParam("max_connections", 1)) {
		l.mu.Unlock()
		report(protocol.FeedbackWarning, "tcp_refused", nil, "a connection from %s came past max_connections (%d): closed",
			c.RemoteAddr(), max(1, l.conn.intParam("max_connections", 1)))
		_ = c.Close()
		return
	}
	l.open++
	old := l.cur
	l.cur = c
	l.mu.Unlock()
	if old != nil {
		_ = old.Close()
	}
	log.Printf("connection in from %s", c.RemoteAddr())
	select {
	case l.arrived <- struct{}{}:
	default:
	}
	go l.read(c)
}

// wait is the connection in use, waiting for one to come in when none is.
func (l *tcpLink) wait() net.Conn {
	for {
		l.mu.Lock()
		c := l.cur
		l.mu.Unlock()
		if c != nil {
			return c
		}
		<-l.arrived
	}
}

// read takes what comes over c until it closes: replies to their waiters,
// what core starts to pushes.
func (l *tcpLink) read(c net.Conn) {
	defer l.dropped(c)
	r := bufio.NewReaderSize(c, 64<<10)
	for {
		id, sealed, err := readTCPFrame(r)
		if err != nil {
			if !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
				log.Printf("connection closed: %v", err)
			}
			return
		}
		l.mu.Lock()
		keys, ok := l.keys[id]
		l.mu.Unlock()
		if !ok {
			log.Printf("a frame under %s, which this minion has no keys for", formatUUID(id[:]))
			continue
		}
		plain, err := protocol.OpenFrame(keys, id, protocol.FrameCoreToMinion, sealed)
		if err != nil {
			log.Printf("a frame that does not open: %v", err)
			continue
		}
		var msg protocol.CoreMessage
		if err := protocol.UnmarshalCBOR(plain, &msg); err != nil ||
			protocol.ValidateMessageHead(msg.V, msg.ID, msg.Re, len(msg.Items)) != nil {
			log.Printf("a message from core that does not read")
			continue
		}
		if msg.Re != nil {
			l.mu.Lock()
			ch := l.waiting[string(msg.Re)]
			delete(l.waiting, string(msg.Re))
			l.mu.Unlock()
			if ch != nil {
				ch <- msg
			}
			continue
		}
		log.Printf("core sent a message: %d item(s)", len(msg.Items))
		l.pushes <- msg
	}
}

// dropped forgets c once it closed: what waits for a reply over it fails,
// and the session hears it lost when it was the one in use.
func (l *tcpLink) dropped(c net.Conn) {
	_ = c.Close()
	l.mu.Lock()
	defer l.mu.Unlock()
	l.open--
	if l.cur != c {
		return
	}
	l.cur = nil
	select {
	case l.lost <- struct{}{}:
	default:
	}
	for id, ch := range l.waiting {
		close(ch)
		delete(l.waiting, id)
	}
}

// exchange sends one message with items under id, sealed with keys, over
// the connection in use (waiting for the channel's dial when none is in),
// and returns core's reply to it.
func (l *tcpLink) exchange(id [16]byte, keys protocol.AES256CBCHMACSHA256Material, items ...any) (protocol.CoreMessage, error) {
	var answer protocol.CoreMessage
	msgID, frame, plainLen, err := seal(id, keys, items)
	if err != nil {
		return answer, err
	}
	c := l.wait()
	reply := make(chan protocol.CoreMessage, 1)
	l.mu.Lock()
	l.keys[id] = keys
	l.waiting[string(msgID)] = reply
	l.mu.Unlock()
	log.Printf("message %x under %s over the connection: %d bytes of CBOR, %d sealed", msgID, formatUUID(id[:]), plainLen, len(frame))
	_ = c.SetWriteDeadline(time.Now().Add(l.conn.timeout()))
	if err := writeTCPFrame(c, id[:], frame); err != nil {
		l.forget(msgID)
		_ = c.Close()
		return answer, fmt.Errorf("write: %w", err)
	}
	timer := time.NewTimer(l.conn.timeout())
	defer timer.Stop()
	select {
	case msg, ok := <-reply:
		if !ok {
			return answer, errors.New("the connection closed before the reply")
		}
		if !bytes.Equal(msg.Re, msgID) {
			return answer, fmt.Errorf("the reply answers %x, not our message", msg.Re)
		}
		log.Printf("core's reply over the connection: %d item(s)", len(msg.Items))
		return msg, nil
	case <-timer.C:
		l.forget(msgID)
		return answer, errors.New("no reply over the connection in time")
	}
}

func (l *tcpLink) forget(msgID []byte) {
	l.mu.Lock()
	delete(l.waiting, string(msgID))
	l.mu.Unlock()
}

// close stops listening and closes the connection in use, if any: the
// minion moved off the connection, or its session ended (one that starts
// over listens again, and finds the port free).
func (l *tcpLink) close() {
	l.mu.Lock()
	l.stopped = true
	c, ln := l.cur, l.ln
	l.mu.Unlock()
	if ln != nil {
		_ = ln.Close()
	}
	if c != nil {
		_ = c.Close()
	}
}

func (l *tcpLink) pushesCh() chan protocol.CoreMessage { return l.pushes }
func (l *tcpLink) lostCh() chan struct{}               { return l.lost }

// readTCPFrame reads one frame: the id it is sent under and the sealed
// message.
func readTCPFrame(r io.Reader) ([16]byte, []byte, error) {
	var id [16]byte
	var head [4]byte
	if _, err := io.ReadFull(r, head[:]); err != nil {
		return id, nil, err
	}
	n := binary.BigEndian.Uint32(head[:])
	if n < 16+protocol.MinFrameSize || n > maxTCPFrame {
		return id, nil, fmt.Errorf("a frame of %d bytes", n)
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(r, b); err != nil {
		return id, nil, err
	}
	copy(id[:], b[:16])
	return id, b[16:], nil
}

// writeTCPFrame writes one frame.
func writeTCPFrame(w io.Writer, id, data []byte) error {
	b := make([]byte, 4, 4+16+len(data))
	binary.BigEndian.PutUint32(b, uint32(16+len(data)))
	b = append(append(b, id...), data...)
	_, err := w.Write(b)
	return err
}
