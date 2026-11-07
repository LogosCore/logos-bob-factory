//go:build unix

package main

import (
	"bytes"
	"net"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/logoscore/logos-golang-protocol/protocol"
)

// openProc asks the minion for a process: program with args, in a
// terminal of rows and cols (none: on pipes).
func (c *tunnelCore) openProc(id uint64, program string, args []string, rows, cols uint64, send []byte, eof bool) *coreStream {
	return c.openItemEOF(protocol.TunnelItem{Stream: id, Kind: protocol.TunnelKindProcess, Path: program, Args: args, Rows: rows, Cols: cols}, send, eof)
}

func (c *tunnelCore) openItemEOF(it protocol.TunnelItem, send []byte, eof bool) *coreStream {
	st := c.openItem(it, send)
	st.eof = eof
	return st
}

// ctl sends the minion one of core's ctls.
func (c *tunnelCore) ctl(id uint64, ctl protocol.TunnelCtlItem) {
	ctl.T, ctl.Task, ctl.Stream = protocol.ItemTunnelCtl, socksTask, id
	c.r.take(coreSays(c.t, ctl), nil)
}

func exitOf(st *coreStream) (int64, bool) {
	for _, c := range st.ctls {
		if c.Exit != nil {
			return *c.Exit, true
		}
	}
	return 0, false
}

// A process on pipes: what core sends is its input (its eof the input's
// end), its output comes back with stderr in it, its exit goes in a ctl
// before its eof, and the stream ends done.
func TestTunnelProcessOnPipes(t *testing.T) {
	r := startSocks(t, nil)
	core := newTunnelCore(t, r)
	st := core.openProc(1, "/bin/sh", []string{"-c", "read x; echo got $x; echo oops >&2; exit 3"}, 0, 0, []byte("hi\n"), true)
	core.until("the stream ends", func() bool { return st.closed != nil })
	if st.reply == nil || st.reply.Result != protocol.TunnelOpen || st.reply.Terminal {
		t.Fatalf("reply %+v", st.reply)
	}
	if code, ok := exitOf(st); !ok || code != 3 {
		t.Errorf("exit %d %v (ctls %+v)", code, ok, st.ctls)
	}
	if st.closed.Reason != protocol.TunnelCloseDone || !st.eofGot || string(st.got) != "got hi\noops\n" {
		t.Errorf("close %+v, got %q", st.closed, st.got)
	}
}

// A process in a terminal: the reply says it got one, the terminal is the
// open's size and then core's ctl's, what core types is echoed as a
// terminal does, and its exit comes before the end.
func TestTunnelProcessInATerminal(t *testing.T) {
	if _, err := os.Stat("/dev/ptmx"); err != nil {
		t.Skip("no PTYs here")
	}
	r := startSocks(t, nil)
	core := newTunnelCore(t, r)
	st := core.openProc(1, "/bin/sh", []string{"-c", "stty size; read x; stty size; echo got $x; exit 4"}, 24, 80, nil, false)
	core.until("it opens and says its size", func() bool { return bytes.Contains(st.got, []byte("24 80")) })
	if st.reply == nil || st.reply.Result != protocol.TunnelOpen || !st.reply.Terminal {
		t.Fatalf("reply %+v", st.reply)
	}
	core.ctl(1, protocol.TunnelCtlItem{Seq: 0, Rows: 30, Cols: 100})
	// Again: taken once, acked again.
	core.ctl(1, protocol.TunnelCtlItem{Seq: 0, Rows: 99, Cols: 99})
	st.send = []byte("hi\r")
	core.until("it ends", func() bool { _, ok := exitOf(st); return ok && st.eofGot })
	got := string(st.got)
	if !strings.Contains(got, "30 100") || !strings.Contains(got, "got hi") || strings.Contains(got, "99 99") {
		t.Errorf("got %q", got)
	}
	if code, _ := exitOf(st); code != 4 {
		t.Errorf("exit %d", code)
	}
	st.eof = true
	core.until("the stream ends done", func() bool { return st.closed != nil })
	if st.closed.Reason != protocol.TunnelCloseDone {
		t.Errorf("close %+v", st.closed)
	}
}

// A signal reaches the process, its exit says it as a shell does (128 and
// its number); a program that is not there is not found.
func TestTunnelProcessSignalAndNotFound(t *testing.T) {
	r := startSocks(t, nil)
	core := newTunnelCore(t, r)
	st := core.openProc(1, "sleep", []string{"30"}, 0, 0, nil, false)
	core.until("it opens", func() bool { return st.reply != nil })
	core.ctl(1, protocol.TunnelCtlItem{Seq: 0, Signal: protocol.TunnelSignalTerminate})
	core.until("it ended", func() bool { _, ok := exitOf(st); return ok && st.eofGot })
	if code, _ := exitOf(st); code != 128+15 {
		t.Errorf("exit %d", code)
	}
	missing := core.openProc(2, "/nonexistent/program", nil, 0, 0, nil, true)
	core.until("it is answered", func() bool { return missing.reply != nil })
	if missing.reply.Result != protocol.TunnelNotFound {
		t.Errorf("reply %+v", missing.reply)
	}
}

// An exit core's acks lost goes again until one comes, and is taken once.
func TestTunnelExitGoesAgain(t *testing.T) {
	r := startSocks(t, nil)
	core := newTunnelCore(t, r)
	lost := 0
	core.lose = func(int) bool { lost++; return lost%2 == 0 }
	st := core.openProc(1, "/bin/sh", []string{"-c", "exit 7"}, 0, 0, nil, true)
	core.until("the stream ends done", func() bool { return st.closed != nil })
	if code, ok := exitOf(st); !ok || code != 7 || len(st.ctls) != 1 || st.closed.Reason != protocol.TunnelCloseDone {
		t.Errorf("ctls %+v, close %+v", st.ctls, st.closed)
	}
}

// An accept (SOCKS5 BIND): the minion listens, says where, then who came
// in a ctl, and the stream is theirs both ways. One whose address is taken
// is in use; one no one comes to is reset.
func TestTunnelAccept(t *testing.T) {
	r := startSocks(t, nil)
	core := newTunnelCore(t, r)
	st := core.openItemEOF(protocol.TunnelItem{Stream: 1, Kind: protocol.TunnelKindAccept, Host: "127.0.0.1"}, []byte("hello"), true)
	core.until("it listens", func() bool { return st.reply != nil })
	if st.reply.Result != protocol.TunnelOpen || st.reply.Bound == "" {
		t.Fatalf("reply %+v", st.reply)
	}
	c, err := net.Dial("tcp", st.reply.Bound)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	core.until("who came", func() bool { return len(st.ctls) > 0 })
	from := c.LocalAddr().(*net.TCPAddr)
	if came := st.ctls[0]; came.Host != "127.0.0.1" || came.Port != uint64(from.Port) || came.Exit != nil {
		t.Errorf("came %+v, from %v", came, from)
	}
	if _, err := c.Write([]byte("back")); err != nil {
		t.Fatal(err)
	}
	_ = c.(*net.TCPConn).CloseWrite()
	buf := make([]byte, 16)
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	var got []byte
	core.until("both ways", func() bool {
		_ = c.SetReadDeadline(time.Now().Add(time.Millisecond))
		if n, _ := c.Read(buf); n > 0 {
			got = append(got, buf[:n]...)
		}
		return string(got) == "hello" && st.eofGot
	})
	core.until("the stream ends done", func() bool { return st.closed != nil })
	if string(st.got) != "back" || st.closed.Reason != protocol.TunnelCloseDone {
		t.Errorf("got %q, close %+v", st.got, st.closed)
	}

	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = taken.Close() }()
	tp := uint64(taken.Addr().(*net.TCPAddr).Port)
	inUse := core.openItemEOF(protocol.TunnelItem{Stream: 2, Kind: protocol.TunnelKindAccept, Host: "127.0.0.1", Port: tp}, nil, true)
	core.until("in use", func() bool { return inUse.reply != nil })
	if inUse.reply.Result != protocol.TunnelInUse {
		t.Errorf("reply %+v (port %s)", inUse.reply, strconv.FormatUint(tp, 10))
	}

	r.tun.mu.Lock()
	r.tun.acceptWait = 100 * time.Millisecond
	r.tun.mu.Unlock()
	alone := core.openItemEOF(protocol.TunnelItem{Stream: 3, Kind: protocol.TunnelKindAccept, Host: "127.0.0.1"}, nil, true)
	core.until("no one came", func() bool { return alone.closed != nil })
	if alone.closed.Reason != protocol.TunnelCloseReset {
		t.Errorf("close %+v", alone.closed)
	}
}
