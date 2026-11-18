package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/fxamacker/cbor/v2"
	"github.com/logoscore/logos-golang-protocol/protocol"
)

// wsCore is an HTTP channel's WebSockets and core in one: it stages the
// minion, answers its hello, answers each of its messages with nothing,
// pushes what a test gives it over the socket opened last, and keeps every
// message the minion sent under its session, with the path of the socket
// it came over.
type wsCore struct {
	t                          *testing.T
	srv                        *httptest.Server
	buildID, tempID, sessionID [16]byte
	keys, newKeys              protocol.AES256CBCHMACSHA256Material

	mu   sync.Mutex
	ws   *websocket.Conn
	sent []wsSent
	came chan struct{}
}

// wsSent is a message the minion sent under its session.
type wsSent struct {
	path  string
	items []cbor.RawMessage
}

func newWSCore(t *testing.T) *wsCore {
	t.Helper()
	c := &wsCore{t: t, buildID: [16]byte{1}, tempID: [16]byte{2}, sessionID: [16]byte{3},
		keys:    protocol.AES256CBCHMACSHA256Material{EncKey: bytes.Repeat([]byte{1}, 32), MACKey: bytes.Repeat([]byte{2}, 32)},
		newKeys: protocol.AES256CBCHMACSHA256Material{EncKey: bytes.Repeat([]byte{3}, 32), MACKey: bytes.Repeat([]byte{4}, 32)},
		came:    make(chan struct{}, 1)}
	c.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = ws.CloseNow() }()
		c.mu.Lock()
		c.ws = ws
		c.mu.Unlock()
		for {
			_, data, err := ws.Read(r.Context())
			if err != nil {
				return
			}
			c.answer(ws, r.URL.Path, data)
		}
	}))
	t.Cleanup(c.srv.Close)
	return c
}

// answer answers a frame of the minion's as core does.
func (c *wsCore) answer(ws *websocket.Conn, path string, data []byte) {
	id, sealed, err := unpack(data)
	if err != nil {
		c.t.Errorf("unpack: %v", err)
		return
	}
	keys := c.newKeys
	if id == c.buildID {
		keys = c.keys
	}
	plain, err := protocol.OpenFrame(keys, id, protocol.FrameMinionToCore, sealed)
	if err != nil {
		c.t.Errorf("open under %x: %v", id, err)
		return
	}
	var msg protocol.MinionMessage
	if err := protocol.UnmarshalCBOR(plain, &msg); err != nil {
		c.t.Errorf("message: %v", err)
		return
	}
	var item []any
	switch id {
	case c.buildID:
		var stage protocol.StageItem
		if len(msg.Items) != 1 || protocol.UnmarshalCBOR(msg.Items[0], &stage) != nil {
			c.t.Errorf("stage %+v", msg)
			return
		}
		pub, err := protocol.ParseStageKey(stage.Key)
		if err != nil {
			c.t.Error(err)
			return
		}
		material, _ := protocol.MarshalCBOR(c.newKeys)
		sealedKeys, _ := protocol.EncryptStageKeys(pub, material)
		item = append(item, protocol.StageResponseItem{T: protocol.ItemStageResponse, TID: c.tempID[:], Keys: sealedKeys, Corr: stage.Corr})
	case c.tempID:
		item = append(item, protocol.HelloResponseItem{T: protocol.ItemHelloResponse, SID: c.sessionID[:]})
	case c.sessionID:
		c.mu.Lock()
		c.sent = append(c.sent, wsSent{path: path, items: msg.Items})
		c.mu.Unlock()
		select {
		case c.came <- struct{}{}:
		default:
		}
	default:
		c.t.Errorf("a message under %x", id)
		return
	}
	c.write(ws, id, keys, protocol.CoreMessage{V: protocol.MinionMessageV3, Re: msg.ID}, item...)
}

func (c *wsCore) write(ws *websocket.Conn, id [16]byte, keys protocol.AES256CBCHMACSHA256Material, m protocol.CoreMessage, items ...any) {
	for _, it := range items {
		raw, err := protocol.MarshalCBOR(it)
		if err != nil {
			c.t.Errorf("item: %v", err)
			return
		}
		m.Items = append(m.Items, raw)
	}
	plain, _ := protocol.MarshalCBOR(m)
	frame, _ := protocol.SealFrame(keys, id, protocol.FrameCoreToMinion, plain)
	b, _ := pack(id, frame)
	if err := ws.Write(context.Background(), websocket.MessageBinary, b); err != nil {
		c.t.Logf("write: %v", err)
	}
}

// push sends items as a message core starts, over the socket opened last.
func (c *wsCore) push(items ...any) {
	c.mu.Lock()
	ws := c.ws
	c.mu.Unlock()
	c.write(ws, c.sessionID, c.newKeys, protocol.CoreMessage{V: protocol.MinionMessageV3, ID: protocol.NewMessageID()}, items...)
}

// count is how many messages the minion sent under its session.
func (c *wsCore) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.sent)
}

// since is the messages the minion sent from the from-th on.
func (c *wsCore) since(from int) []wsSent {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.sent[from:])
}

// waitFor waits for a message from the from-th on that ok takes, and
// returns its place.
func (c *wsCore) waitFor(t *testing.T, from int, within time.Duration, ok func(wsSent) bool) int {
	t.Helper()
	deadline := time.After(within)
	for {
		for i, s := range c.since(from) {
			if ok(s) {
				return from + i
			}
		}
		select {
		case <-c.came:
		case <-time.After(10 * time.Millisecond):
		case <-deadline:
			t.Fatalf("no such message within %s; the minion sent %s", within, describeSent(c.since(from)))
		}
	}
}

// quiet: the minion sends nothing after the i-th message for a while.
func (c *wsCore) quiet(t *testing.T, i int, d time.Duration) {
	t.Helper()
	time.Sleep(d)
	if after := c.since(i + 1); len(after) > 0 {
		t.Errorf("after it the minion sent %s", describeSent(after))
	}
}

// describeSent names each message's items.
func describeSent(sent []wsSent) string {
	var out []string
	for _, s := range sent {
		var names []string
		for _, raw := range s.items {
			names = append(names, protocol.ItemName(protocol.ItemType(raw)))
		}
		out = append(out, s.path+"["+strings.Join(names, " ")+"]")
	}
	return strings.Join(out, ", ")
}

// getTasksOnly: a message that carries nothing but its get_tasks.
func getTasksOnly(s wsSent) bool {
	for _, raw := range s.items {
		if protocol.ItemType(raw) != protocol.ItemGetTasks {
			return false
		}
	}
	return len(s.items) > 0
}

// sentResult is what a message says: the task ids its acks name, the
// output, and whether a task's end came.
func sentResult(s wsSent) (acks int, out string, done bool) {
	for _, raw := range s.items {
		switch protocol.ItemType(raw) {
		case protocol.ItemAck:
			var a protocol.AckItem
			if protocol.UnmarshalCBOR(raw, &a) == nil {
				acks += len(a.Tasks)
			}
		case protocol.ItemResult:
			var r protocol.ResultItem
			if protocol.UnmarshalCBOR(raw, &r) == nil {
				out += string(r.Out)
				done = done || r.Done
			}
		}
	}
	return acks, out, done
}

// sessionWSConn is an http reverse-persistent connection to url, on path
// ("": the channel's root).
func sessionWSConn(url, path string) protocol.MinionConnection {
	c := testConn("http", protocol.KindReversePersistent, protocol.Args{httpBaseURL: url, httpRequestTimeout: 5000})
	if path != "" {
		c.Values = protocol.Args{paramPathPrefix: path}
	}
	return c
}

// startWSSession runs a session over conns until the test ends, when core
// closes it, and returns once it is up: staged, its hello answered, its
// first check-in made.
func startWSSession(t *testing.T, c *wsCore, conns ...protocol.MinionConnection) {
	t.Helper()
	feedback.mu.Lock()
	feedback.items, feedback.dropped = nil, 0
	feedback.mu.Unlock()
	tr, err := newTransport(conns)
	if err != nil {
		t.Fatal(err)
	}
	d := description{Build: c.buildID, Keys: c.keys, Connections: conns, Ops: []uint64{opSleep, opExit, opHello, opShell, opSwitch},
		Greeting: "hi"}
	done := make(chan error, 1)
	go func() { done <- session(tr, schedule{interval: time.Second}, d, c.buildID, c.keys, nil) }()
	t.Cleanup(func() {
		c.push(protocol.ErrItem{T: protocol.ItemErr, Code: protocol.ErrItemSessionClosed, Msg: "the test is over"})
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("the session did not end")
		}
	})
	c.waitFor(t, 0, 10*time.Second, func(wsSent) bool { return true })
}

// Over a WebSocket a quick task core pushes costs the minion one message:
// the task's ack, its output and its end; no message with nothing in it
// follows.
func TestSessionPushedTaskInOneMessage(t *testing.T) {
	c := newWSCore(t)
	startWSSession(t, c, sessionWSConn(c.srv.URL, ""))
	from := c.count()
	c.push(protocol.TaskItem{T: protocol.ItemTask, Task: bytes.Repeat([]byte{5}, 16), Cmd: opHello})
	i := c.waitFor(t, from, 5*time.Second, func(wsSent) bool { return true })
	c.quiet(t, i, 300*time.Millisecond)
	if acks, out, done := sentResult(c.since(i)[0]); acks != 1 || out != "hi\n" || !done {
		t.Fatalf("the message: acks %d out %q done %v (%s)", acks, out, done, describeSent(c.since(i)))
	}
}

// A switch from one WebSocket to another: its ack, the move, and its end
// over the new one; never a message with nothing in it.
func TestSessionSwitchSendsNoEmptyMessage(t *testing.T) {
	c := newWSCore(t)
	startWSSession(t, c, sessionWSConn(c.srv.URL, "/one/"), sessionWSConn(c.srv.URL, "/two/"))
	from := c.count()
	c.push(protocol.TaskItem{T: protocol.ItemTask, Task: bytes.Repeat([]byte{6}, 16), Cmd: opSwitch, Args: protocol.Args{switchConn: uint64(2)}})
	end := c.waitFor(t, from, 5*time.Second, func(s wsSent) bool { _, _, done := sentResult(s); return done })
	c.quiet(t, end, 300*time.Millisecond)
	sent := c.since(from)
	for _, s := range sent {
		if getTasksOnly(s) {
			t.Errorf("a message with nothing in it: %s", describeSent(sent))
			break
		}
	}
	if path := c.since(end)[0].path; path != "/two/" {
		t.Errorf("the switch's end came over %s: %s", path, describeSent(sent))
	}
}
