//go:build windows

package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"syscall"
	"time"
)

// Named pipes (tunnel.go, openConn): a stream to \\.\pipe\name, or to
// another host's \\host\pipe\name (over SMB, as the minion's user). The
// pipe opens overlapped, so os.NewFile hands its reads and writes to Go's
// poller and both go at once. A message-mode pipe's messages are a byte
// stream here: one past a read's room goes on in the next read.

// errorPipeBusy: every instance of the pipe is taken; the open is tried
// again until the stream's timeout.
const errorPipeBusy = syscall.Errno(231)

// errNoPipes: named pipes are Windows': a minion elsewhere answers an open of
// one unsupported. Never on Windows.
var errNoPipes = errors.New("named pipes are Windows'")

// dialPipe opens the pipe at path for reading and writing.
func dialPipe(ctx context.Context, path string, timeout time.Duration) (net.Conn, error) {
	name, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(timeout)
	for {
		h, err := syscall.CreateFile(name, syscall.GENERIC_READ|syscall.GENERIC_WRITE, 0, nil, syscall.OPEN_EXISTING,
			syscall.FILE_FLAG_OVERLAPPED, 0)
		if err == nil {
			return &pipeConn{f: os.NewFile(uintptr(h), path), path: path}, nil
		}
		if !errors.Is(err, errorPipeBusy) {
			return nil, &os.PathError{Op: "open", Path: path, Err: err}
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("%s: every instance busy: %w", path, context.DeadlineExceeded)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// pipeConn is an open pipe as a stream's connection.
type pipeConn struct {
	f    *os.File
	path string
}

func (c *pipeConn) Read(b []byte) (int, error) {
	n, err := c.f.Read(b)
	if errors.Is(err, syscall.ERROR_MORE_DATA) {
		// A message longer than b: the rest comes with the next read.
		err = nil
	}
	return n, err
}

func (c *pipeConn) Write(b []byte) (int, error)        { return c.f.Write(b) }
func (c *pipeConn) Close() error                       { return c.f.Close() }
func (c *pipeConn) LocalAddr() net.Addr                { return pipeAddr(c.path) }
func (c *pipeConn) RemoteAddr() net.Addr               { return pipeAddr(c.path) }
func (c *pipeConn) SetDeadline(t time.Time) error      { return c.f.SetDeadline(t) }
func (c *pipeConn) SetReadDeadline(t time.Time) error  { return c.f.SetReadDeadline(t) }
func (c *pipeConn) SetWriteDeadline(t time.Time) error { return c.f.SetWriteDeadline(t) }

// pipeAddr is a pipe's name as an address.
type pipeAddr string

func (a pipeAddr) Network() string { return "pipe" }
func (a pipeAddr) String() string  { return string(a) }
