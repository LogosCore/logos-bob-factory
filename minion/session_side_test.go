package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/logoscore/logos-golang-protocol/protocol"
)

// slowCore is wsCore over a slow link: each reply to a message of the
// session goes a round trip later (in order on each socket, as a channel's
// delay line keeps them). It counts how many of the minion's messages wait for their
// replies at once, numbers the sockets, and can put items in a reply.
type slowCore struct {
	*wsCore
	delay time.Duration
	// slower are paths whose sockets take longer.
	slower map[string]time.Duration

	smu         sync.Mutex
	waiting     int
	mostWaiting int
	sockets     int
	socketOf    []int // of each message in sent
	arrived     []time.Time
	firstReply  map[int]time.Time // when each socket's first reply went
	inReply     []any             // what the next reply carries
}

func newSlowCore(t *testing.T, delay time.Duration) *slowCore {
	t.Helper()
	c := &slowCore{wsCore: &wsCore{t: t, buildID: [16]byte{1}, tempID: [16]byte{2}, sessionID: [16]byte{3},
		keys:    protocol.AES256CBCHMACSHA256Material{EncKey: bytes.Repeat([]byte{1}, 32), MACKey: bytes.Repeat([]byte{2}, 32)},
		newKeys: protocol.AES256CBCHMACSHA256Material{EncKey: bytes.Repeat([]byte{3}, 32), MACKey: bytes.Repeat([]byte{4}, 32)},
		came:    make(chan struct{}, 1)}, delay: delay, firstReply: map[int]time.Time{}}
	c.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = ws.CloseNow() }()
		c.mu.Lock()
		c.ws = ws
		c.mu.Unlock()
		c.smu.Lock()
		c.sockets++
		socket := c.sockets
		c.smu.Unlock()
		// The socket's replies, in order.
		replies := make(chan func(), 1024)
		defer close(replies)
		go func() {
			for reply := range replies {
				reply()
			}
		}()
		for {
			_, data, err := ws.Read(r.Context())
			if err != nil {
				return
			}
			c.answerSlowly(ws, socket, replies, r.URL.Path, data)
		}
	}))
	t.Cleanup(c.srv.Close)
	return c
}

// answerSlowly answers as wsCore does; a message of the session's a delay
// later.
func (c *slowCore) answerSlowly(ws *websocket.Conn, socket int, replies chan<- func(), path string, data []byte) {
	id, sealed, err := unpack(data)
	if err != nil || id != c.sessionID {
		c.answer(ws, path, data)
		return
	}
	plain, err := protocol.OpenFrame(c.newKeys, id, protocol.FrameMinionToCore, sealed)
	if err != nil {
		c.t.Errorf("open: %v", err)
		return
	}
	var msg protocol.MinionMessage
	if err := protocol.UnmarshalCBOR(plain, &msg); err != nil {
		c.t.Errorf("message: %v", err)
		return
	}
	at := time.Now()
	c.mu.Lock()
	c.sent = append(c.sent, wsSent{path: path, items: msg.Items})
	c.mu.Unlock()
	c.smu.Lock()
	c.waiting++
	c.mostWaiting = max(c.mostWaiting, c.waiting)
	c.socketOf = append(c.socketOf, socket)
	c.arrived = append(c.arrived, at)
	c.smu.Unlock()
	select {
	case c.came <- struct{}{}:
	default:
	}
	re := msg.ID
	delay := c.delay
	if d, ok := c.slower[path]; ok {
		delay = d
	}
	replies <- func() {
		time.Sleep(time.Until(at.Add(delay)))
		c.smu.Lock()
		c.waiting--
		items := c.inReply
		c.inReply = nil
		c.smu.Unlock()
		c.write(ws, id, c.newKeys, protocol.CoreMessage{V: protocol.MinionMessageV3, Re: re}, items...)
		c.smu.Lock()
		if _, ok := c.firstReply[socket]; !ok {
			c.firstReply[socket] = time.Now()
		}
		c.smu.Unlock()
	}
}

// reply puts items in the next reply core writes.
func (c *slowCore) reply(items ...any) {
	c.smu.Lock()
	c.inReply = items
	c.smu.Unlock()
}

func (c *slowCore) most() int {
	c.smu.Lock()
	defer c.smu.Unlock()
	return c.mostWaiting
}

func (c *slowCore) arrivedAt(i int) time.Time {
	c.smu.Lock()
	defer c.smu.Unlock()
	return c.arrived[i]
}

// itemsOf are the types of s's items.
func itemsOf(s wsSent) map[int]bool {
	out := map[int]bool{}
	for _, raw := range s.items {
		out[protocol.ItemType(raw)] = true
	}
	return out
}

// streamData is the bytes s carries of stream 1 of sideTask.
func streamData(s wsSent) []byte {
	var out []byte
	for _, raw := range s.items {
		var d protocol.TunnelDataItem
		if protocol.ItemType(raw) == protocol.ItemTunnelData && protocol.UnmarshalCBOR(raw, &d) == nil && d.Stream == 1 {
			out = append(out, d.Data...)
		}
	}
	return out
}

var sideTask = bytes.Repeat([]byte{9}, 16)

// startSideSession runs a session with tunnels over conns until the test
// ends, and returns once it checked in.
func startSideSession(t *testing.T, c *wsCore, conns ...protocol.MinionConnection) {
	t.Helper()
	feedback.mu.Lock()
	feedback.items, feedback.dropped = nil, 0
	feedback.mu.Unlock()
	tr, err := newTransport(conns)
	if err != nil {
		t.Fatal(err)
	}
	d := description{Build: c.buildID, Keys: c.keys, Connections: conns, Ops: []uint64{opSleep, opExit, opHello, opShell, opSwitchConnection, opTunnel},
		Greeting: "hi"}
	done := make(chan error, 1)
	go func() { done <- session(tr, schedule{interval: time.Second}, d, c.buildID, c.keys, nil) }()
	t.Cleanup(func() {
		c.push(protocol.ErrItem{T: protocol.ItemErr, Code: protocol.ErrItemSessionClosed, Msg: "the test is over"})
		select {
		case <-done:
		case <-time.After(8 * time.Second):
			t.Error("the session did not end")
		}
	})
	c.waitFor(t, 0, 10*time.Second, func(wsSent) bool { return true })
}

// openEchoStream starts a tunnel task and opens its stream 1 to a host that
// sends back what it gets.
func openEchoStream(t *testing.T, c *wsCore) {
	t.Helper()
	host, port := echoHost(t)
	c.push(protocol.TaskItem{T: protocol.ItemTask, Task: sideTask, Cmd: opTunnel})
	c.waitFor(t, 0, 5*time.Second, func(s wsSent) bool { a, _, _ := sentResult(s); return a > 0 })
	c.push(protocol.TunnelItem{T: protocol.ItemTunnel, Task: sideTask, Stream: 1, Host: host, Port: uint64(port)})
	c.waitFor(t, 0, 5*time.Second, func(s wsSent) bool { return itemsOf(s)[protocol.ItemTunnelReply] })
}

// keepAMessageWaiting pushes a shell task whose ack goes alone (it runs a
// second), and returns once that message waits for its reply.
func keepAMessageWaiting(t *testing.T, c *slowCore, task byte) {
	t.Helper()
	from := c.count()
	c.push(protocol.TaskItem{T: protocol.ItemTask, Task: bytes.Repeat([]byte{task}, 16), Cmd: opShell, Args: protocol.Args{1: "sleep 1; echo done"}})
	c.waitFor(t, from, 3*time.Second, func(s wsSent) bool { a, _, _ := sentResult(s); return a > 0 })
}

// While the loop's message waits for its reply, what core pushes to a
// stream reaches its host at once, and the host's answer goes in a side
// message: a round trip sooner.
func TestSideMessageWhileTheLoopsWaits(t *testing.T) {
	c := newSlowCore(t, 400*time.Millisecond)
	startSideSession(t, c.wsCore, sessionWSConn(c.srv.URL, ""))
	openEchoStream(t, c.wsCore)
	keepAMessageWaiting(t, c, 8)
	from := c.count()
	pushed := time.Now()
	c.push(protocol.TunnelDataItem{T: protocol.ItemTunnelData, Task: sideTask, Stream: 1, Off: 0, Data: []byte("ping")})
	i := c.waitFor(t, from, 3*time.Second, func(s wsSent) bool { return bytes.Equal(streamData(s), []byte("ping")) })
	if took := c.arrivedAt(i).Sub(pushed); took > 200*time.Millisecond {
		t.Errorf("the host's answer came %s after the push: it waited for the loop's reply", took)
	}
	if c.most() < 2 {
		t.Errorf("at most %d message(s) waited at once", c.most())
	}
	if s := c.since(i)[0]; itemsOf(s)[protocol.ItemGetTasks] || itemsOf(s)[protocol.ItemAck] {
		t.Errorf("a side message carries the loop's items: %s", describeSent([]wsSent{s}))
	}
}

// What a stream's ack and its host's answer, a moment later, need goes in
// one side message.
func TestSideMessagesBatch(t *testing.T) {
	was := sideBatch
	sideBatch = 50 * time.Millisecond
	t.Cleanup(func() { sideBatch = was })
	c := newSlowCore(t, 400*time.Millisecond)
	startSideSession(t, c.wsCore, sessionWSConn(c.srv.URL, ""))
	openEchoStream(t, c.wsCore)
	keepAMessageWaiting(t, c, 8)
	from := c.count()
	c.push(protocol.TunnelDataItem{T: protocol.ItemTunnelData, Task: sideTask, Stream: 1, Off: 0, Data: []byte("ping")})
	i := c.waitFor(t, from, 3*time.Second, func(s wsSent) bool {
		return itemsOf(s)[protocol.ItemTunnelAck] || itemsOf(s)[protocol.ItemTunnelData]
	})
	if s := c.since(i)[0]; !itemsOf(s)[protocol.ItemTunnelAck] || !bytes.Equal(streamData(s), []byte("ping")) {
		t.Errorf("the ack and the answer went apart: %s", describeSent(c.since(from)))
	}
}

// Over a slow link a busy stream keeps as many side messages waiting as it
// has answers to send: no count holds its host's bytes back for a reply,
// and every byte gets across once, in order.
func TestSideMessagesAsManyAsThereIs(t *testing.T) {
	c := newSlowCore(t, 400*time.Millisecond)
	startSideSession(t, c.wsCore, sessionWSConn(c.srv.URL, ""))
	openEchoStream(t, c.wsCore)
	keepAMessageWaiting(t, c, 8)
	from := c.count()
	var want []byte
	for k := range 10 {
		data := []byte{'a' + byte(k), 'a' + byte(k)}
		c.push(protocol.TunnelDataItem{T: protocol.ItemTunnelData, Task: sideTask, Stream: 1, Off: uint64(len(want)), Data: data})
		want = append(want, data...)
		time.Sleep(20 * time.Millisecond) // past sideBatch: an answer each
	}
	var got []byte
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		got = nil
		for _, s := range c.since(from) {
			got = append(got, streamData(s)...)
		}
		if len(got) >= len(want) {
			break
		}
	}
	if !bytes.Equal(got, want) {
		t.Errorf("the host's answers: %q, not %q", got, want)
	}
	if c.most() < 7 {
		t.Errorf("at most %d message(s) waited at once: a count held the answers back", c.most())
	}
}

// The socket closes with side messages waiting: their items go again over
// the next socket, after its first message (the loop's, with its
// get_tasks), and the session goes on.
func TestSideMessagesAgainAfterALostSocket(t *testing.T) {
	c := newSlowCore(t, 400*time.Millisecond)
	startSideSession(t, c.wsCore, sessionWSConn(c.srv.URL, ""))
	openEchoStream(t, c.wsCore)
	keepAMessageWaiting(t, c, 7)
	from := c.count()
	c.push(protocol.TunnelDataItem{T: protocol.ItemTunnelData, Task: sideTask, Stream: 1, Off: 0, Data: []byte("one")})
	c.waitFor(t, from, 3*time.Second, func(s wsSent) bool { return bytes.Equal(streamData(s), []byte("one")) })
	if c.most() < 2 {
		t.Fatalf("at most %d message(s) waited at once", c.most())
	}
	c.mu.Lock()
	ws := c.ws
	c.mu.Unlock()
	_ = ws.CloseNow()
	from = c.count()
	// The next socket's first message waits for its reply: a stream's bytes
	// now go to the host, but its answer waits for that reply.
	deadline := time.Now().Add(6 * time.Second)
	for {
		c.smu.Lock()
		on2 := len(c.socketOf) > 0 && c.socketOf[len(c.socketOf)-1] == 2
		c.smu.Unlock()
		if on2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no message over a second socket")
		}
		time.Sleep(5 * time.Millisecond)
	}
	c.push(protocol.TunnelDataItem{T: protocol.ItemTunnelData, Task: sideTask, Stream: 1, Off: 3, Data: []byte("two")})
	// The shell task's end still comes, and so does the stream's answer.
	c.waitFor(t, from, 6*time.Second, func(s wsSent) bool { _, _, done := sentResult(s); return done })
	c.waitFor(t, from, 6*time.Second, func(s wsSent) bool { return bytes.Contains(streamData(s), []byte("two")) })
	// On each socket the first message is the loop's, and nothing more goes
	// before its reply: no side message over a socket core has not tied to
	// the session yet.
	c.smu.Lock()
	sockets := append([]int(nil), c.socketOf...)
	arrived := append([]time.Time(nil), c.arrived...)
	first := map[int]time.Time{}
	for k, v := range c.firstReply {
		first[k] = v
	}
	c.smu.Unlock()
	sent := c.since(0)
	seen := map[int]bool{}
	for i, s := range sent {
		if !seen[sockets[i]] {
			seen[sockets[i]] = true
			if !itemsOf(s)[protocol.ItemGetTasks] {
				t.Errorf("socket %d's first message is not the loop's: %s", sockets[i], describeSent([]wsSent{s}))
			}
			continue
		}
		if at, ok := first[sockets[i]]; !ok || arrived[i].Before(at) {
			t.Errorf("over socket %d, %s before core answered its first message", sockets[i], describeSent([]wsSent{s}))
		}
	}
	if len(seen) < 2 {
		t.Errorf("no second socket: %v", sockets)
	}
}

// A switch while a side message waits: the move waits for its reply, then
// goes over the new socket; nothing goes over the old one after, and
// nothing goes twice.
func TestSideMessagesBeforeAMove(t *testing.T) {
	c := newSlowCore(t, 400*time.Millisecond)
	// The new socket answers at once: a side message over the old one is
	// still waiting once the move's own message is answered.
	c.slower = map[string]time.Duration{"/one/": 800 * time.Millisecond, "/two/": 20 * time.Millisecond}
	startSideSession(t, c.wsCore, sessionWSConn(c.srv.URL, "/one/"), sessionWSConn(c.srv.URL, "/two/"))
	openEchoStream(t, c.wsCore)
	from := c.count()
	switchTask := bytes.Repeat([]byte{5}, 16)
	c.push(protocol.TaskItem{T: protocol.ItemTask, Task: switchTask, Cmd: opSwitchConnection, Args: protocol.Args{switchConnectionConn: uint64(2)}})
	// The switch's ack waits for its reply; a stream's answer goes
	// meanwhile, in a side message.
	c.waitFor(t, from, 3*time.Second, func(s wsSent) bool { a, _, _ := sentResult(s); return a > 0 })
	time.Sleep(100 * time.Millisecond)
	c.push(protocol.TunnelDataItem{T: protocol.ItemTunnelData, Task: sideTask, Stream: 1, Off: 0, Data: []byte("abc")})
	end := c.waitFor(t, from, 8*time.Second, func(s wsSent) bool {
		for _, raw := range s.items {
			var r protocol.ResultItem
			if protocol.ItemType(raw) == protocol.ItemResult && protocol.UnmarshalCBOR(raw, &r) == nil && r.Done && r.Task[0] == 5 {
				return true
			}
		}
		return false
	})
	time.Sleep(300 * time.Millisecond)
	sent := c.since(from)
	if p := c.since(end)[0].path; p != "/two/" {
		t.Errorf("the switch's end came over %s: %s", p, describeSent(sent))
	}
	moved, echoes := false, 0
	for _, s := range sent {
		if bytes.Equal(streamData(s), []byte("abc")) {
			echoes++
		}
		if s.path == "/two/" {
			moved = true
		} else if moved {
			t.Errorf("a message over /one/ after the move: %s", describeSent(sent))
		}
	}
	if echoes != 1 {
		t.Errorf("the host's answer went %d times (the move did not wait for its side message): %s", echoes, describeSent(sent))
	}
}

// A task core pushed, then cancelled in the reply to a message that waited
// meanwhile: the task is taken first, as core sent it, and so cancelled;
// it never runs as one the minion did not have.
func TestCancelAfterItsPushedTask(t *testing.T) {
	c := newSlowCore(t, 400*time.Millisecond)
	startSideSession(t, c.wsCore, sessionWSConn(c.srv.URL, ""))
	from := c.count()
	c.push(protocol.TaskItem{T: protocol.ItemTask, Task: bytes.Repeat([]byte{4}, 16), Cmd: opHello})
	c.waitFor(t, from, 3*time.Second, func(s wsSent) bool { a, _, _ := sentResult(s); return a > 0 })
	// That message waits for its reply: the task comes now, its cancel in
	// the reply.
	x := bytes.Repeat([]byte{3}, 16)
	c.push(protocol.TaskItem{T: protocol.ItemTask, Task: x, Cmd: opShell, Args: protocol.Args{1: "sleep 2; echo late"}})
	c.reply(protocol.CancelItem{T: protocol.ItemCancel, Task: x})
	from = c.count()
	end := c.waitFor(t, from, 5*time.Second, func(s wsSent) bool {
		for _, raw := range s.items {
			var r protocol.ResultItem
			if protocol.ItemType(raw) == protocol.ItemResult && protocol.UnmarshalCBOR(raw, &r) == nil && r.Done && r.Task[0] == 3 {
				return true
			}
		}
		return false
	})
	for _, s := range c.since(from)[:end-from+1] {
		for _, raw := range s.items {
			var e protocol.ErrItem
			if protocol.ItemType(raw) == protocol.ItemErr && protocol.UnmarshalCBOR(raw, &e) == nil && e.Code == protocol.ErrItemUnknownTask {
				t.Errorf("the cancel came before its task: %s", describeSent(c.since(from)))
			}
			var r protocol.ResultItem
			if protocol.ItemType(raw) == protocol.ItemResult && protocol.UnmarshalCBOR(raw, &r) == nil && bytes.Contains(r.Out, []byte("late")) {
				t.Errorf("the cancelled task ran to its end: %s", describeSent(c.since(from)))
			}
			if protocol.ItemType(raw) == protocol.ItemResult && protocol.UnmarshalCBOR(raw, &r) == nil && r.Done && r.Task[0] == 3 && r.OK != nil && *r.OK {
				t.Errorf("the cancelled task ended ok")
			}
		}
	}
}
