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

// The TCP channel's reverse kinds (its README): the minion dials the
// channel, with the frames a bind-persistent connection carries,
// [len u32 BE][id 16][sealed] (tcp.go's readTCPFrame and writeTCPFrame).
//
//   - reverse-persistent (tcpDialLink): the connection stays open, as the
//     WebSocket to the HTTP channel does: every exchange goes over it, core
//     pushes over it, and it is dialed again when it closes, backing off up
//     to the endpoint's reconnect_backoff. TCP keepalive finds a channel
//     gone without a word.
//   - reverse-call (tcpCall): a connection for each exchange: the message,
//     core's reply under the same id (the id alone: core had nothing), then
//     the channel closes. One closed with no reply is a failure, as the HTTP
//     channel's 404.

// dialAddr is where the minion dials the channel: the endpoint's host and
// port.
func (c connection) dialAddr() string {
	return net.JoinHostPort(c.param(tcpHost), strconv.Itoa(c.intParam(tcpPort, 0)))
}

// connectTimeout is how long a dial of the channel may take.
func (c connection) connectTimeout() time.Duration {
	return c.duration(tcpConnectTimeout, 10*time.Second)
}

// dialChannel dials the TCP channel as the connection says, with its
// keepalive.
func (c connection) dialChannel() (net.Conn, error) {
	d := net.Dialer{Timeout: c.connectTimeout(), KeepAlive: c.keepalive()}
	conn, err := d.Dial("tcp", c.dialAddr())
	if err != nil {
		return nil, fmt.Errorf("reach the channel: %w", err)
	}
	return conn, nil
}

// readTCPReply reads a reverse call's reply: a frame whose sealed message
// may be empty.
func readTCPReply(r io.Reader) ([16]byte, []byte, error) {
	var id [16]byte
	var head [4]byte
	if _, err := io.ReadFull(r, head[:]); err != nil {
		return id, nil, err
	}
	n := binary.BigEndian.Uint32(head[:])
	if n < 16 || n > maxTCPFrame {
		return id, nil, fmt.Errorf("a reply of %d bytes", n)
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(r, b); err != nil {
		return id, nil, err
	}
	copy(id[:], b[:16])
	return id, b[16:], nil
}

// tcpCall is one reverse-call exchange through the TCP channel: frame,
// sealed under id, and core's sealed reply (empty: nothing for the minion).
func (c connection) tcpCall(id [16]byte, frame []byte) ([]byte, time.Duration, error) {
	start := time.Now()
	conn, err := c.dialChannel()
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(c.timeout()))
	if err := writeTCPFrame(conn, id[:], frame); err != nil {
		return nil, 0, fmt.Errorf("write: %w", err)
	}
	replyID, sealed, err := readTCPReply(bufio.NewReader(conn))
	if err != nil {
		if errors.Is(err, io.EOF) {
			return nil, 0, errors.New("the channel closed without a reply: core refused the message, or could not be reached")
		}
		return nil, 0, fmt.Errorf("read the reply: %w", err)
	}
	if replyID != id {
		return nil, 0, fmt.Errorf("the reply is for %x, not %x", replyID, id)
	}
	return sealed, time.Since(start), nil
}

// openReply opens core's sealed reply to the message msgID under id.
func openReply(keys protocol.AES256CBCHMACSHA256Material, id [16]byte, msgID, sealed []byte) (protocol.CoreMessage, error) {
	var answer protocol.CoreMessage
	plain, err := protocol.OpenFrame(keys, id, protocol.FrameCoreToMinion, sealed)
	if err != nil {
		return answer, fmt.Errorf("core's reply: %w", err)
	}
	if answer, err = readCore(plain); err != nil {
		return answer, fmt.Errorf("core's reply: %w", err)
	}
	if !bytes.Equal(answer.Re, msgID) {
		return answer, fmt.Errorf("core's reply answers %x, not our message", answer.Re)
	}
	return answer, nil
}

// tcpDialLink is a reverse-persistent connection through the TCP channel.
type tcpDialLink struct {
	conn connection

	mu sync.Mutex
	c  net.Conn
	// closing is the connection the minion closes itself (a switch, a
	// session's end).
	closing net.Conn
	keys    map[[16]byte]protocol.AES256CBCHMACSHA256Material
	waiting map[string]chan protocol.CoreMessage
	pushes  *pushQueue
	lost    chan struct{}
}

func newTCPDialLink(conn connection) *tcpDialLink {
	return &tcpDialLink{conn: conn, keys: map[[16]byte]protocol.AES256CBCHMACSHA256Material{},
		waiting: map[string]chan protocol.CoreMessage{}, pushes: newPushQueue(), lost: make(chan struct{}, 1)}
}

// open dials the channel when no connection is open.
func (l *tcpDialLink) open() (net.Conn, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.c != nil {
		return l.c, nil
	}
	c, err := l.conn.dialChannel()
	if err != nil {
		return nil, err
	}
	l.c = c
	log.Printf("connected to the channel at %s", l.conn.dialAddr())
	go l.read(c)
	return c, nil
}

// read takes what comes over c until it closes: replies to their waiters,
// what core starts to pushes (never waiting on the loop).
func (l *tcpDialLink) read(c net.Conn) {
	defer l.drop(c)
	r := bufio.NewReaderSize(c, 64<<10)
	for {
		id, sealed, err := readTCPFrame(r)
		if err != nil {
			l.mu.Lock()
			ours := l.closing == c
			l.mu.Unlock()
			if !ours {
				log.Printf("the connection to the channel closed: %v", err)
				report(protocol.FeedbackWarning, "tcp_closed", nil, "the connection to the TCP channel closed: %v; it is dialed again", err)
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
		msg, err := readCore(plain)
		if err != nil {
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
		l.pushes.put(msg)
	}
}

// drop forgets c and closes it: it closed, or failed an exchange (a write,
// a reply not in time), so the next exchange dials again. When it was the
// one in use, what waits for a reply over it fails and the session hears
// it lost; one dropped already changes nothing.
func (l *tcpDialLink) drop(c net.Conn) {
	l.mu.Lock()
	if l.c == c {
		l.c = nil
		select {
		case l.lost <- struct{}{}:
		default:
		}
		for id, ch := range l.waiting {
			close(ch)
			delete(l.waiting, id)
		}
	}
	l.mu.Unlock()
	_ = c.Close()
}

// exchange sends one message with items under id, sealed with keys, and
// returns core's reply to it.
func (l *tcpDialLink) exchange(id [16]byte, keys protocol.AES256CBCHMACSHA256Material, items ...any) (protocol.CoreMessage, error) {
	var answer protocol.CoreMessage
	msgID, frame, plainLen, err := seal(id, keys, items)
	if err != nil {
		return answer, err
	}
	c, err := l.open()
	if err != nil {
		return answer, err
	}
	reply := make(chan protocol.CoreMessage, 1)
	l.mu.Lock()
	l.keys[id] = keys
	l.waiting[string(msgID)] = reply
	l.mu.Unlock()
	log.Printf("message %x under %s over the channel's connection: %d bytes of CBOR, %d sealed", msgID, formatUUID(id[:]), plainLen, len(frame))
	_ = c.SetWriteDeadline(time.Now().Add(l.conn.timeout()))
	if err := writeTCPFrame(c, id[:], frame); err != nil {
		l.forget(msgID)
		l.drop(c)
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
		log.Printf("core's reply over the channel's connection: %d item(s)", len(msg.Items))
		return msg, nil
	case <-timer.C:
		// A connection that does not answer in time is taken for dead.
		l.forget(msgID)
		l.drop(c)
		return answer, errors.New("no reply over the connection in time: it is closed, the next exchange dials again")
	}
}

func (l *tcpDialLink) forget(msgID []byte) {
	l.mu.Lock()
	delete(l.waiting, string(msgID))
	l.mu.Unlock()
}

func (l *tcpDialLink) pushed() *pushQueue    { return l.pushes }
func (l *tcpDialLink) lostCh() chan struct{} { return l.lost }

// close closes the connection, if one is open.
func (l *tcpDialLink) close() {
	l.mu.Lock()
	c := l.c
	l.closing = c
	l.mu.Unlock()
	if c != nil {
		_ = c.Close()
	}
}
