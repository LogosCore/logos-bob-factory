package main

import (
	"bytes"
	"io"
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/fxamacker/cbor/v2"
	"github.com/logoscore/logos-golang-protocol/protocol"
)

var delegateTask = bytes.Repeat([]byte{0xde}, 16)

// startDelegate is a runner running a delegate task.
func startDelegate(t *testing.T) *runner {
	t.Helper()
	r := newRunner(description{Ops: []uint64{opDelegate}}, &schedule{interval: time.Second})
	r.start(protocol.TaskItem{T: protocol.ItemTask, Task: delegateTask, Cmd: opDelegate})
	t.Cleanup(func() { r.dlg.closeAll() })
	return r
}

// fromCore gives the runner core's delegate items, as core sends them: each
// read back as core reads it first.
func fromCore(t *testing.T, r *runner, items ...protocol.DelegateItem) {
	t.Helper()
	var raws []cbor.RawMessage
	for _, it := range items {
		it.T = protocol.ItemDelegate
		if err := it.Validate(false); err != nil {
			t.Fatalf("core would not send %+v: %v", it, err)
		}
		b, err := protocol.MarshalCBOR(it)
		if err != nil {
			t.Fatal(err)
		}
		raws = append(raws, b)
	}
	r.take(protocol.CoreMessage{V: protocol.MinionMessageV3, Items: raws}, nil)
}

// toCore is the delegate items of the runner's next message, delivered,
// each checked as core checks it.
func toCore(t *testing.T, r *runner) []protocol.DelegateItem {
	t.Helper()
	items := r.outgoing()
	r.delivered(items)
	var out []protocol.DelegateItem
	for _, it := range items {
		d, ok := it.(protocol.DelegateItem)
		if !ok {
			continue
		}
		b, _ := protocol.MarshalCBOR(d)
		got, err := protocol.ReadDelegate(b, true)
		if err != nil {
			t.Fatalf("core would not take %+v: %v", d, err)
		}
		out = append(out, got)
	}
	return out
}

// awaitCore gathers the runner's delegate items until one of step comes.
func awaitCore(t *testing.T, r *runner, step int) protocol.DelegateItem {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, d := range toCore(t, r) {
			if d.Step == step {
				return d
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no %s came", protocol.DelegateStepName(step))
	return protocol.DelegateItem{}
}

// listening is a minion listening for the TCP channel: its port, and the
// connections that come.
func listening(t *testing.T) (int, chan net.Conn) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	conns := make(chan net.Conn, 4)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			conns <- c
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port, conns
}

func openItem(n uint64, port int) protocol.DelegateItem {
	return protocol.DelegateItem{Task: delegateTask, Step: protocol.DelegateOpen, N: n, Serve: 1, Deadline: 5000,
		Values: map[string]any{"host": "127.0.0.1", "port": uint64(port), "listen_port": uint64(1), "connect_timeout": "2s"}}
}

// A delegate dials what core's open names, as the TCP channel would, and
// carries the frames both ways, in pieces where a message has no room for
// one, until core closes the connection.
func TestDelegateCarries(t *testing.T) {
	r := startDelegate(t)
	port, conns := listening(t)
	fromCore(t, r, openItem(1, port))
	a := awaitCore(t, r, protocol.DelegateAnswer)
	if a.N != 1 || a.Failure != nil || !bytes.Equal(a.Peer, []byte{127, 0, 0, 1}) {
		t.Fatalf("the answer %+v", a)
	}
	var minion net.Conn
	select {
	case minion = <-conns:
	case <-time.After(5 * time.Second):
		t.Fatal("not dialed")
	}
	defer func() { _ = minion.Close() }()
	// The open again (core's answer lost) gets the same answer, no dial.
	fromCore(t, r, openItem(1, port))
	if again := awaitCore(t, r, protocol.DelegateAnswer); again.Failure != nil {
		t.Fatalf("again %+v", again)
	}
	select {
	case <-conns:
		t.Fatal("dialed twice")
	case <-time.After(100 * time.Millisecond):
	}
	// The minion speaks first: its frame to core.
	id := bytes.Repeat([]byte{7}, 16)
	if err := writeTCPFrame(minion, id, bytes.Repeat([]byte{1}, 80)); err != nil {
		t.Fatal(err)
	}
	f := awaitCore(t, r, protocol.DelegateFrame)
	if f.N != 1 || f.Seq != 0 || !bytes.Equal(f.ID, id) || len(f.Frame) != 80 {
		t.Fatalf("the minion's frame %+v", f)
	}
	// Core's, in pieces: joined, written as one frame.
	big := bytes.Repeat([]byte{9}, 5000)
	pieces := protocol.DelegateItem{Task: delegateTask, Step: protocol.DelegateFrame, N: 1, ID: id, Frame: big}.Pieces(1500)
	fromCore(t, r, pieces[2], pieces[0], pieces[3], pieces[1])
	_ = minion.SetReadDeadline(time.Now().Add(5 * time.Second))
	gotID, data, err := readTCPFrame(minion)
	if err != nil || !bytes.Equal(gotID[:], id) || !bytes.Equal(data, big) {
		t.Fatalf("core's frame at the minion: %v", err)
	}
	// The minion's large frame through small messages: pieces, joined by
	// core.
	r.setLimit(4096)
	large := make([]byte, 30000)
	for i := range large {
		large[i] = byte(i)
	}
	if err := writeTCPFrame(minion, id, large); err != nil {
		t.Fatal(err)
	}
	var j protocol.DelegateJoiner
	var whole []byte
	for deadline := time.Now().Add(5 * time.Second); whole == nil && time.Now().Before(deadline); {
		for _, d := range toCore(t, r) {
			if d.Step != protocol.DelegateFrame {
				continue
			}
			if d.Seq != 1 {
				t.Fatalf("seq %d", d.Seq)
			}
			if got, done, _ := j.Add(d, time.Now()); done {
				whole = got
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !bytes.Equal(whole, large) {
		t.Fatal("the large frame did not join")
	}
	// Core closes it: answered, and the minion's end closes.
	fromCore(t, r, protocol.DelegateItem{Task: delegateTask, Step: protocol.DelegateClose, N: 1, Reason: protocol.DelegateClosed})
	if c := awaitCore(t, r, protocol.DelegateClose); c.N != 1 || c.Reason != protocol.DelegateClosed {
		t.Fatalf("the close's answer %+v", c)
	}
	_ = minion.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.ReadAll(minion); err != nil {
		t.Fatalf("the minion's end did not close: %v", err)
	}
	// A frame for it after: a close back.
	fromCore(t, r, protocol.DelegateItem{Task: delegateTask, Step: protocol.DelegateFrame, N: 1, ID: id, Frame: []byte{1}})
	if c := awaitCore(t, r, protocol.DelegateClose); c.Reason != protocol.DelegateNoConnection {
		t.Fatalf("a frame of a closed connection: %+v", c)
	}
}

// The dialed minion closing ends the connection, core told; a dial that
// fails answers so; an item of a task the minion does not run gets a close;
// the task stopped closes what it carries, and ends ok.
func TestDelegateEnds(t *testing.T) {
	r := startDelegate(t)
	port, conns := listening(t)
	fromCore(t, r, openItem(1, port))
	awaitCore(t, r, protocol.DelegateAnswer)
	_ = (<-conns).Close()
	if c := awaitCore(t, r, protocol.DelegateClose); c.N != 1 || c.Reason != protocol.DelegateClosed {
		t.Fatalf("the minion's close %+v", c)
	}
	// Nothing listens there.
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	dead := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	fromCore(t, r, openItem(2, dead))
	if a := awaitCore(t, r, protocol.DelegateAnswer); a.Failure == nil || a.Failure.Kind != protocol.LinkFailUnreachable {
		t.Fatalf("a dial that fails %+v", a)
	}
	// A proxy is the TCP channel's.
	proxied := openItem(3, port)
	proxied.Values["proxy"] = "socks5://127.0.0.1:1080"
	fromCore(t, r, proxied)
	if a := awaitCore(t, r, protocol.DelegateAnswer); a.Failure == nil || a.Failure.Kind != protocol.LinkFailOther {
		t.Fatalf("a proxied target %+v", a)
	}
	other := openItem(1, port)
	other.Task = bytes.Repeat([]byte{1}, 16)
	fromCore(t, r, other)
	if c := awaitCore(t, r, protocol.DelegateClose); c.Reason != protocol.DelegateTaskEnded {
		t.Fatalf("another task's %+v", c)
	}
	// One more open, then the task stops.
	fromCore(t, r, openItem(4, port))
	awaitCore(t, r, protocol.DelegateAnswer)
	minion := <-conns
	defer func() { _ = minion.Close() }()
	r.stop(delegateTask, 0)
	_ = minion.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.ReadAll(minion); err != nil {
		t.Fatalf("not closed with its task: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		r.mu.Lock()
		ended := len(r.tasks) == 1 && r.tasks[0].ended && r.tasks[0].ok
		r.mu.Unlock()
		if ended {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the task did not end ok")
		}
		time.Sleep(10 * time.Millisecond)
	}
	// It held no place in the minion's max tasks.
	r.mu.Lock()
	room := r.roomLocked()
	r.mu.Unlock()
	if room != r.maxTasks {
		t.Fatalf("room %d of %d", room, r.maxTasks)
	}
}

func TestTCPTarget(t *testing.T) {
	addr, timeout, err := tcpTarget(map[string]any{"host": "10.0.0.5", "listen_port": int64(4444)}, 3*time.Second)
	if err != nil || addr != "10.0.0.5:4444" || timeout != 3*time.Second {
		t.Fatalf("%s %s %v", addr, timeout, err)
	}
	addr, timeout, err = tcpTarget(map[string]any{"host": "h", "port": uint64(80), "connect_timeout": "1s"}, 0)
	if err != nil || addr != "h:"+strconv.Itoa(80) || timeout != time.Second {
		t.Fatalf("%s %s %v", addr, timeout, err)
	}
	if _, _, err := tcpTarget(map[string]any{"port": uint64(80)}, 0); err == nil {
		t.Fatal("no host")
	}
}
