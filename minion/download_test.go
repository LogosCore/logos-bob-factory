package main

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fxamacker/cbor/v2"
	"github.com/logoscore/logos-golang-protocol/protocol"
)

// coreSays is core's answer with these items.
func coreSays(t *testing.T, items ...any) protocol.CoreMessage {
	t.Helper()
	var raw []cbor.RawMessage
	for _, it := range items {
		b, err := protocol.MarshalCBOR(it)
		if err != nil {
			t.Fatal(err)
		}
		raw = append(raw, b)
	}
	return protocol.CoreMessage{Items: raw}
}

// chunkIn is the chunk among a message's items, and its index.
func chunkIn(items []any) (protocol.ChunkItem, int, bool) {
	for i, it := range items {
		if c, ok := it.(protocol.ChunkItem); ok {
			return c, i, true
		}
	}
	return protocol.ChunkItem{}, -1, false
}

func startDownload(t *testing.T, path string) *runner {
	t.Helper()
	r := newRunner(description{Commands: []string{"download"}}, &schedule{interval: time.Second})
	r.start(protocol.TaskItem{T: protocol.ItemTask, Task: bytes.Repeat([]byte{5}, 16), Cmd: "download", Args: map[string]any{"path": path}})
	return r
}

// nextMessage is the runner's next message once it has one.
func nextMessage(t *testing.T, r *runner) []any {
	t.Helper()
	for i := 0; i < 200; i++ {
		if r.pending() {
			if items := r.outgoing(); len(items) > 0 {
				return items
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("nothing to send")
	return nil
}

// A file goes in chunks, each from where core's ack says: a message whose
// ack is lost sends the same chunk again. The task ends with the file's
// data once core has it all, its SHA-256 the file's.
func TestDownload(t *testing.T) {
	body := make([]byte, protocol.MaxChunkData*2+1234)
	_, _ = rand.Read(body)
	path := filepath.Join(t.TempDir(), "loot.bin")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	r := startDownload(t, path)

	var got []byte
	lost := false
	for len(got) < len(body) {
		items := nextMessage(t, r)
		r.delivered(items)
		c, _, ok := chunkIn(items)
		if !ok {
			continue
		}
		if c.Off != uint64(len(got)) {
			t.Fatalf("a chunk from %d, core has %d", c.Off, len(got))
		}
		if c.Off == 0 && (c.Total == nil || *c.Total != uint64(len(body)) || c.Name != path) {
			t.Fatalf("the first chunk: %+v", c)
		}
		if !lost && c.Off > 0 {
			// The ack is lost: the same chunk comes again.
			lost = true
			r.take(coreSays(t), items)
			continue
		}
		got = append(got, c.Data...)
		next := uint64(len(got))
		r.take(coreSays(t, protocol.AckItem{T: protocol.ItemAck, Xfer: c.Xfer, Next: &next}), items)
		if next == uint64(len(body)) {
			sum := sha256.Sum256(body)
			if !bytes.Equal(c.SHA256, sum[:]) {
				t.Fatalf("the last chunk's sha256: %x", c.SHA256)
			}
		}
	}
	if !lost || !bytes.Equal(got, body) {
		t.Fatalf("resent %v; sent the file: %v", lost, bytes.Equal(got, body))
	}
	task := waitEnd(t, r, 2*time.Second)
	if !task.ok || len(task.chunks) != 1 {
		t.Fatalf("ended %+v", task)
	}
	f, err := protocol.ReadFileDownload(0, task.chunks[0])
	sum := sha256.Sum256(body)
	if err != nil || f.Path != path || f.Size != uint64(len(body)) || !bytes.Equal(f.SHA256, sum[:]) {
		t.Fatalf("its data: %+v %v", f, err)
	}
}

// Core refusing a chunk (too large, say) ends the task failed, with why.
func TestDownloadRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "big.bin")
	if err := os.WriteFile(path, []byte("abc"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := startDownload(t, path)
	// The task's ack may go first, before the file is open.
	var items []any
	i, ok := -1, false
	for tries := 0; !ok && tries < 5; tries++ {
		items = nextMessage(t, r)
		r.delivered(items)
		_, i, ok = chunkIn(items)
	}
	if !ok {
		t.Fatalf("no chunk in %v", items)
	}
	r.take(coreSays(t, protocol.ErrItem{T: protocol.ItemErr, Code: protocol.ErrItemTooLarge, Msg: "over core's 2", Item: &i}), items)
	task := waitEnd(t, r, 2*time.Second)
	if task.ok || !strings.Contains(task.err, "too_large: over core's 2") {
		t.Fatalf("ended %+v", task)
	}
}

// Not a file: the task fails at once, nothing sent.
func TestDownloadNotAFile(t *testing.T) {
	r := startDownload(t, t.TempDir())
	task := waitEnd(t, r, 2*time.Second)
	if task.ok || !strings.Contains(task.err, "is not a file") {
		t.Fatalf("ended %+v", task)
	}
}
