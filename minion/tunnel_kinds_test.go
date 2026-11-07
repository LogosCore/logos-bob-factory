package main

import (
	"bytes"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/logoscore/logos-golang-protocol/protocol"
)

// openItem asks the minion for a stream as it says (its kind, its path,
// its source, TLS), whose tool sends send and then ends.
func (c *tunnelCore) openItem(it protocol.TunnelItem, send []byte) *coreStream {
	st := &coreStream{send: send, eof: true}
	c.streams[it.Stream] = st
	it.T, it.Task = protocol.ItemTunnel, socksTask
	c.r.take(coreSays(c.t, it), nil)
	return st
}

// unixEcho is a unix socket that sends back what it gets: its path.
func unixEcho(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "echo.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = c.Close() }()
				_, _ = io.Copy(c, c)
				_ = c.(*net.UnixConn).CloseWrite()
			}()
		}
	}()
	return path
}

// A stream to a unix socket is a stream as a TCP one is: its bytes both
// ways, its end done; one to a socket that is not there is not found.
func TestTunnelUnixSocket(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the test's socket is a unix one")
	}
	r := startSocks(t, nil)
	core := newTunnelCore(t, r)
	st := core.openItem(protocol.TunnelItem{Stream: 1, Kind: protocol.TunnelKindUnix, Path: unixEcho(t)}, randomBytes(96<<10))
	core.until("the stream ends done", func() bool { return st.closed != nil })
	if st.reply == nil || st.reply.Result != protocol.TunnelOpen || !bytes.Equal(st.got, st.send) {
		t.Fatalf("reply %+v, %d bytes back of %d", st.reply, len(st.got), len(st.send))
	}
	gone := core.openItem(protocol.TunnelItem{Stream: 2, Kind: protocol.TunnelKindUnix, Path: filepath.Join(t.TempDir(), "none.sock")}, nil)
	core.until("answered", func() bool { return gone.reply != nil })
	if gone.reply.Result != protocol.TunnelNotFound || gone.reply.Msg == "" {
		t.Errorf("a socket not there: %+v", gone.reply)
	}
}

// A named pipe is Windows': elsewhere an open of one is unsupported.
func TestTunnelPipeElsewhere(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("pipes are there")
	}
	r := startSocks(t, nil)
	core := newTunnelCore(t, r)
	st := core.openItem(protocol.TunnelItem{Stream: 1, Kind: protocol.TunnelKindPipe, Path: `\\.\pipe\spoolss`}, nil)
	core.until("answered", func() bool { return st.reply != nil })
	if st.reply.Result != protocol.TunnelUnsupported {
		t.Errorf("a pipe off Windows: %+v", st.reply)
	}
}

// A TCP stream goes from its source address when it says one: an address
// of the host's; one the host does not have fails, saying why.
func TestTunnelSource(t *testing.T) {
	r := startSocks(t, nil)
	core := newTunnelCore(t, r)
	host, port := echoHost(t)
	st := core.openItem(protocol.TunnelItem{Stream: 1, Host: host, Port: uint64(port), Source: "127.0.0.1"}, []byte("from here"))
	core.until("the stream ends done", func() bool { return st.closed != nil })
	if st.reply.Result != protocol.TunnelOpen || !strings.HasPrefix(st.reply.Bound, "127.0.0.1:") || string(st.got) != "from here" {
		t.Fatalf("reply %+v, back %q", st.reply, st.got)
	}
	// 192.0.2.1 (TEST-NET-1) is no address of this host's.
	far := core.openItem(protocol.TunnelItem{Stream: 2, Host: host, Port: uint64(port), Source: "192.0.2.1"}, nil)
	core.until("answered", func() bool { return far.reply != nil })
	if far.reply.Result == protocol.TunnelOpen || far.reply.Msg == "" {
		t.Errorf("a source not the host's: %+v", far.reply)
	}
}

// A TCP stream with TLS speaks TLS to its destination, its tool's bytes
// plain: what the tool sends is the request the server gets, what the
// server answers the tool reads. No certificate is checked.
func TestTunnelTLS(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		_, _ = io.WriteString(w, "over tls: "+req.URL.Path)
	}))
	defer srv.Close()
	host, port := splitHostPort(t, srv.Listener.Addr().String())
	r := startSocks(t, nil)
	core := newTunnelCore(t, r)
	st := core.openItem(protocol.TunnelItem{Stream: 1, Host: host, Port: uint64(port), TLS: true},
		[]byte("GET /path HTTP/1.0\r\nHost: x\r\n\r\n"))
	core.until("the stream ends done", func() bool { return st.closed != nil })
	if st.reply.Result != protocol.TunnelOpen || !strings.HasPrefix(string(st.got), "HTTP/1.0 200") ||
		!strings.HasSuffix(string(st.got), "over tls: /path") {
		t.Fatalf("reply %+v, back %q", st.reply, st.got)
	}
	// Plain to a TLS server: the handshake is the tool's, not the minion's.
	plainSrv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer plainSrv.Close()
	ph, pp := splitHostPort(t, plainSrv.Listener.Addr().String())
	bad := core.openItem(protocol.TunnelItem{Stream: 2, Host: ph, Port: uint64(pp), TLS: true}, nil)
	core.until("answered", func() bool { return bad.reply != nil })
	if bad.reply.Result == protocol.TunnelOpen || !strings.Contains(bad.reply.Msg, "tls") {
		t.Errorf("TLS to a plain server: %+v", bad.reply)
	}
}

func splitHostPort(t *testing.T, addr string) (string, int) {
	t.Helper()
	h, p, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, c := range p {
		n = n*10 + int(c-'0')
	}
	return h, n
}
