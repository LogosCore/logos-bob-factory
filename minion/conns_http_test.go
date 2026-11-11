package main

import (
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/logoscore/logos-golang-protocol/protocol"
)

// Check-ins reuse the connection to the channel: one, however many
// exchanges (a new client a request left each connection open behind it).
func TestCheckInsReuseTheConnection(t *testing.T) {
	var opened atomic.Int32
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	srv.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			opened.Add(1)
		}
	}
	srv.Start()
	defer srv.Close()
	c := connection{testConn("http", protocol.KindReverseCall, protocol.Args{httpBaseURL: srv.URL})}
	for range 20 {
		if _, status, _, err := c.send([]byte("check-in")); err != nil || status != http.StatusNoContent {
			t.Fatalf("send: %d %v", status, err)
		}
	}
	if n := opened.Load(); n != 1 {
		t.Fatalf("%d connections for 20 check-ins", n)
	}
}
