package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/fxamacker/cbor/v2"
	"github.com/logoscore/logos-golang-protocol/protocol"
)

// wsChannel is a fake channel and core on a WebSocket: it answers each
// message with an empty reply naming it, pushes a task after the first, and
// closes the socket after a message it is told to drop at.
type wsChannel struct {
	srv   *httptest.Server
	keys  protocol.AES256CBCHMACSHA256Material
	mu    sync.Mutex
	conns int
	drop  int // closes the socket after this many messages, 0 never
	got   int
}

func newWSChannel(t *testing.T, keys protocol.AES256CBCHMACSHA256Material) *wsChannel {
	t.Helper()
	c := &wsChannel{keys: keys}
	c.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = ws.CloseNow() }()
		c.mu.Lock()
		c.conns++
		c.mu.Unlock()
		ctx := r.Context()
		for {
			_, data, err := ws.Read(ctx)
			if err != nil {
				return
			}
			id, sealed, err := unpack(data)
			if err != nil {
				return
			}
			plain, err := protocol.OpenFrame(c.keys, id, protocol.FrameMinionToCore, sealed)
			if err != nil {
				return
			}
			var msg protocol.MinionMessage
			if protocol.UnmarshalCBOR(plain, &msg) != nil {
				return
			}
			c.mu.Lock()
			c.got++
			n, drop := c.got, c.drop
			c.mu.Unlock()
			out := []protocol.CoreMessage{{V: protocol.MinionMessageV3, Re: msg.ID}}
			if n == 1 {
				task, _ := protocol.MarshalCBOR(protocol.TaskItem{T: protocol.ItemTask, Task: bytes.Repeat([]byte{3}, 16), Cmd: opHello})
				out = append(out, protocol.CoreMessage{V: protocol.MinionMessageV3, ID: protocol.NewMessageID(), Items: []cbor.RawMessage{task}})
			}
			for _, m := range out {
				p, _ := protocol.MarshalCBOR(m)
				f, _ := protocol.SealFrame(c.keys, id, protocol.FrameCoreToMinion, p)
				b, _ := pack(id, f)
				if err := ws.Write(ctx, websocket.MessageBinary, b); err != nil {
					return
				}
			}
			if drop > 0 && n == drop {
				return
			}
		}
	}))
	t.Cleanup(c.srv.Close)
	return c
}

func wsTestConnection(url string) connection {
	return connection{protocol.BuildConnection{
		Endpoint: protocol.ChannelEndpointExport{Kind: protocol.KindReversePersistent, Params: map[string]any{"base_url": url, "request_timeout": "5s"}}}}
}

// Over the WebSocket each message gets the reply naming it, and what core
// starts arrives as a push; a socket that closes is opened again at the next
// exchange.
func TestWSLink(t *testing.T) {
	keys := protocol.AES256CBCHMACSHA256Material{EncKey: bytes.Repeat([]byte{1}, 32), MACKey: bytes.Repeat([]byte{2}, 32)}
	ch := newWSChannel(t, keys)
	ch.drop = 2
	link := newWSLink(wsTestConnection(ch.srv.URL))
	defer link.close()
	id := [16]byte{9}

	reply, err := link.exchange(id, keys, protocol.GetTasksItem{T: protocol.ItemGetTasks})
	if err != nil || reply.Re == nil || len(reply.Items) != 0 {
		t.Fatalf("reply %+v %v", reply, err)
	}
	select {
	case push := <-link.pushes:
		if push.ID == nil || len(push.Items) != 1 || protocol.ItemType(push.Items[0]) != protocol.ItemTask {
			t.Fatalf("push %+v", push)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no push")
	}
	// The second message's reply comes, then the socket closes.
	if _, err := link.exchange(id, keys); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		link.mu.Lock()
		open := link.ws != nil
		link.mu.Unlock()
		if !open {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the socket did not close")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := link.exchange(id, keys); err != nil {
		t.Fatalf("after the drop: %v", err)
	}
	ch.mu.Lock()
	conns := ch.conns
	ch.mu.Unlock()
	if conns != 2 {
		t.Fatalf("%d sockets, want 2", conns)
	}
}

func TestWSURL(t *testing.T) {
	c := connection{protocol.BuildConnection{Endpoint: protocol.ChannelEndpointExport{Params: map[string]any{"base_url": "https://edge.example.com"}},
		Values: map[string]any{"path_prefix": "/cdn/"}}}
	if got := c.wsURL(); got != "wss://edge.example.com/cdn/" {
		t.Fatalf("%s", got)
	}
}
