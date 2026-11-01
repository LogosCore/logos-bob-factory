package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"

	"github.com/coder/websocket"
	"github.com/logoscore/logos-golang-protocol/protocol"
)

// A reverse-persistent connection (docs: contracts/channel-core-connections/):
// the minion keeps a WebSocket open to the HTTP channel. Its messages go as
// binary frames packed like a request body; core's come back packed like a
// response: the reply to each of the minion's (always one, empty when core
// has nothing to say), and the messages core starts, tasks pushed within the
// minion's get_tasks allowance and cancels. The session goes on over it as
// it would through requests; when it drops, the next exchange opens another.

// wsLink is the minion's WebSocket to the channel.
type wsLink struct {
	conn connection

	mu sync.Mutex
	ws *websocket.Conn
	// keys open what comes under each id: the build's, the stage's, the
	// session's.
	keys map[[16]byte]protocol.AES256CBCHMACSHA256Material
	// waiting are the minion's messages waiting for their replies, by id.
	waiting map[string]chan protocol.CoreMessage
	// pushes are the messages core starts.
	pushes chan protocol.CoreMessage
	// lost says the WebSocket closed: the session connects again, backing
	// off.
	lost chan struct{}
}

func newWSLink(conn connection) *wsLink {
	return &wsLink{conn: conn, keys: map[[16]byte]protocol.AES256CBCHMACSHA256Material{},
		waiting: map[string]chan protocol.CoreMessage{}, pushes: make(chan protocol.CoreMessage, 16), lost: make(chan struct{}, 1)}
}

// wsURL is the connection's URL as a WebSocket's.
func (c connection) wsURL() string {
	u := c.url()
	switch {
	case strings.HasPrefix(u, "https://"):
		return "wss://" + strings.TrimPrefix(u, "https://")
	case strings.HasPrefix(u, "http://"):
		return "ws://" + strings.TrimPrefix(u, "http://")
	}
	return u
}

// open dials the channel when no WebSocket is open.
func (l *wsLink) open() (*websocket.Conn, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.ws != nil {
		return l.ws, nil
	}
	verify, _ := l.conn.Endpoint.Params["tls_verify"].(bool)
	client := &http.Client{Transport: &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: !verify, ServerName: l.conn.param("sni")}, //nolint:gosec // the endpoint's own setting
	}}
	h := http.Header{}
	if ua := l.conn.param("user_agent"); ua != "" {
		h.Set("User-Agent", ua)
	}
	if headers, ok := l.conn.Endpoint.Params["headers"].(map[string]any); ok {
		for k, v := range headers {
			if s, ok := v.(string); ok {
				h.Set(k, s)
			}
		}
	}
	opts := &websocket.DialOptions{HTTPClient: client, HTTPHeader: h, CompressionMode: websocket.CompressionDisabled}
	if host := l.conn.param("host_header"); host != "" {
		opts.Host = host
	}
	ctx, cancel := context.WithTimeout(context.Background(), l.conn.timeout())
	defer cancel()
	ws, resp, err := websocket.Dial(ctx, l.conn.wsURL(), opts)
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		return nil, fmt.Errorf("open the WebSocket: %w", err)
	}
	ws.SetReadLimit(protocol.MaxMinionMessageSize + 1024)
	l.ws = ws
	log.Printf("WebSocket open to %s", l.conn.wsURL())
	go l.read(ws)
	return ws, nil
}

// read takes what comes over ws until it closes: replies to their waiters,
// what core starts to pushes.
func (l *wsLink) read(ws *websocket.Conn) {
	defer l.dropped(ws)
	for {
		typ, data, err := ws.Read(context.Background())
		if err != nil {
			log.Printf("WebSocket closed: %v", err)
			return
		}
		if typ != websocket.MessageBinary {
			continue
		}
		id, sealed, err := unpack(data)
		if err != nil || len(sealed) == 0 {
			continue
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

// dropped forgets ws once it closed: what waits for a reply over it fails.
func (l *wsLink) dropped(ws *websocket.Conn) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.ws == ws {
		l.ws = nil
		select {
		case l.lost <- struct{}{}:
		default:
		}
	}
	for id, ch := range l.waiting {
		close(ch)
		delete(l.waiting, id)
	}
}

// exchange sends one message with items under id, sealed with keys, and
// returns core's reply to it, as exchange does through a request.
func (l *wsLink) exchange(id [16]byte, keys protocol.AES256CBCHMACSHA256Material, items ...any) (protocol.CoreMessage, error) {
	var answer protocol.CoreMessage
	msgID, frame, plainLen, err := seal(id, keys, items)
	if err != nil {
		return answer, err
	}
	body, err := pack(id, frame)
	if err != nil {
		return answer, err
	}
	ws, err := l.open()
	if err != nil {
		return answer, err
	}
	reply := make(chan protocol.CoreMessage, 1)
	l.mu.Lock()
	l.keys[id] = keys
	l.waiting[string(msgID)] = reply
	l.mu.Unlock()
	log.Printf("message %x under %s over the WebSocket: %d bytes of CBOR, %d packed", msgID, formatUUID(id[:]), plainLen, len(body))
	ctx, cancel := context.WithTimeout(context.Background(), l.conn.timeout())
	defer cancel()
	if err := ws.Write(ctx, websocket.MessageBinary, body); err != nil {
		l.forget(msgID)
		_ = ws.CloseNow()
		return answer, fmt.Errorf("write: %w", err)
	}
	select {
	case msg, ok := <-reply:
		if !ok {
			return answer, errors.New("the WebSocket closed before the reply")
		}
		if !bytes.Equal(msg.Re, msgID) {
			return answer, fmt.Errorf("the reply answers %x, not our message", msg.Re)
		}
		log.Printf("core's reply over the WebSocket: %d item(s)", len(msg.Items))
		return msg, nil
	case <-ctx.Done():
		l.forget(msgID)
		return answer, errors.New("no reply over the WebSocket in time")
	}
}

func (l *wsLink) forget(msgID []byte) {
	l.mu.Lock()
	delete(l.waiting, string(msgID))
	l.mu.Unlock()
}

// close closes the WebSocket, if one is open.
func (l *wsLink) close() {
	l.mu.Lock()
	ws := l.ws
	l.mu.Unlock()
	if ws != nil {
		_ = ws.Close(websocket.StatusNormalClosure, "")
	}
}
