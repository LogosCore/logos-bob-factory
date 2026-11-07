package main

import (
	"errors"
	"io"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"time"

	"github.com/logoscore/logos-golang-protocol/protocol"
)

// A process a stream runs (an open of kind process, docs:
// contracts/minion-messages/tunnels.md, Terminal): the program core names
// (none: the host's shell) with its args, in a terminal of the open's size
// (proc_linux.go's PTY, proc_windows.go's ConPTY) or, with no size or no
// terminal to give, on pipes; stderr merged into the bytes either way. Its
// bytes are the stream's; its exit goes to core as a tunnel_ctl before the
// stream's eof; the stream's end hangs it up.

// proc is a running process as a stream's connection.
type proc struct {
	out, in *os.File
	// exited closes once the process ended, code its exit (128 and the
	// signal's number when one ended it).
	exited chan struct{}
	code   int64
	closed atomic.Bool
	// onExit has the exit said, once the output ended.
	onExit func(code int64)
	// The host's ways: a new size, a signal (protocol.TunnelSignal...), an
	// end to the whole of it, and the end of its input (nil: none, as a
	// terminal's input does not end).
	resizeFn   func(rows, cols int) error
	signalFn   func(sig int) error
	killFn     func()
	closeWrite func() error
	once       sync.Once
}

// watch waits for the process in the background: wait is its exit code,
// then after (a pseudo console closed, so the output ends).
func (p *proc) watch(wait func() int64, after func()) {
	go func() {
		p.code = wait()
		if after != nil {
			after()
		}
		close(p.exited)
	}()
}

// Read is the process's output: once it ended, its exit is said (onExit),
// then an eof.
func (p *proc) Read(b []byte) (int, error) {
	n, err := p.out.Read(b)
	if n > 0 || err == nil {
		return n, nil
	}
	if p.closed.Load() {
		return 0, net.ErrClosed
	}
	// A terminal's output ends (EIO) once the process and its children
	// closed it; pipes', at their eof. Either way, its exit.
	<-p.exited
	if p.closed.Load() {
		return 0, net.ErrClosed
	}
	if p.onExit != nil {
		p.onExit(p.code)
	}
	return 0, io.EOF
}

func (p *proc) Write(b []byte) (int, error) { return p.in.Write(b) }

// CloseWrite ends the process's input: its pipe's; a terminal's does not
// end (its process ends it, or the stream's close).
func (p *proc) CloseWrite() error {
	if p.closeWrite == nil {
		return nil
	}
	return p.closeWrite()
}

// Close hangs the process up: all of it ends, its files close.
func (p *proc) Close() error {
	p.once.Do(func() {
		p.closed.Store(true)
		select {
		case <-p.exited:
		default:
			p.killFn()
		}
		_ = p.out.Close()
		if p.in != p.out {
			_ = p.in.Close()
		}
	})
	return nil
}

func (p *proc) resize(rows, cols int) error {
	if p.resizeFn == nil {
		return nil
	}
	return p.resizeFn(rows, cols)
}

func (p *proc) signal(sig int) error { return p.signalFn(sig) }

func (p *proc) LocalAddr() net.Addr              { return procAddr{} }
func (p *proc) RemoteAddr() net.Addr             { return procAddr{} }
func (p *proc) SetDeadline(time.Time) error      { return nil }
func (p *proc) SetReadDeadline(time.Time) error  { return nil }
func (p *proc) SetWriteDeadline(time.Time) error { return nil }

type procAddr struct{}

func (procAddr) Network() string { return "process" }
func (procAddr) String() string  { return "process" }

// startProcess runs program (none: the host's shell) with args, in a
// terminal of rows and cols when they say one (true when it got one).
func startProcess(program string, args []string, rows, cols int) (*proc, bool, error) {
	if program == "" {
		program = hostShell()
	}
	path, err := exec.LookPath(program)
	if err != nil {
		return nil, false, err
	}
	if rows > 0 && cols > 0 {
		if p, err := startTerminal(path, args, rows, cols); !errors.Is(err, errNoTerminal) {
			return p, err == nil, err
		}
	}
	p, err := startPiped(path, args)
	return p, false, err
}

// errNoTerminal: the host has no terminal to give; the process runs on
// pipes.
var errNoTerminal = errors.New("no terminal on this host")

// procResult is the reply's result for a process that did not start.
func procResult(err error) int {
	switch {
	case errors.Is(err, exec.ErrNotFound), errors.Is(err, fs.ErrNotExist):
		return protocol.TunnelNotFound
	case errors.Is(err, fs.ErrPermission):
		return protocol.TunnelDenied
	}
	return protocol.TunnelFailed
}

// newProc is a process's connection over its output and input.
func newProc(out, in *os.File) *proc {
	return &proc{out: out, in: in, exited: make(chan struct{})}
}
