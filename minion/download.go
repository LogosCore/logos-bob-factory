package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"log"
	"os"

	"github.com/logoscore/logos-golang-protocol/protocol"
)

// download sends a file core asked for (docs: contracts/minion-messages.md,
// File transfers): in chunks of at most protocol.MaxChunkData, one in each
// message, each the next from where core's last ack said it has the file,
// so a lost message or a restart of core only sends a chunk again. The file
// is read as it goes and hashed on its first read; the last chunk says the
// whole's SHA-256. The task ends, with the file_download data, once core
// has every byte; core's refusal (too large, say) or a stop ends it failed.

// transfer is a file being sent: one of the host's, read as it goes, or a
// screenshot's picture, held in memory (screenshot.go).
type transfer struct {
	id []byte
	// path is the file's path on the host, or a name alone for a file on
	// no file system: what the first chunk names.
	path  string
	file  io.ReaderAt
	total uint64
	// acked is how far core has the file: the next chunk starts there.
	acked uint64
	// waiting: a chunk went in the last message, its ack not taken yet.
	waiting bool
	// hashed is how far the hash has read: a chunk sent again is not hashed
	// twice.
	hashed uint64
	sum    hash.Hash
	// end is closed when core has the file, or it failed (err).
	end  chan struct{}
	err  string
	over bool

	// A stream (stream.go): announced once core acked the empty first
	// chunk; held while core's last ack said hold; started once an ack
	// without hold came. gap: a chunk was read past what is hashed (a
	// download resumed ahead), so the whole's SHA-256 is not known.
	stream    bool
	announced bool
	held      bool
	started   bool
	gap       bool
}

func (r *runner) download(ctx context.Context, t *task, args protocol.Args) {
	r.sendFile(ctx, t, args, false)
}

// sendFile sends a file core asked for: as a download, or as a stream
// (stream.go), which ends with other data.
func (r *runner) sendFile(ctx context.Context, t *task, args protocol.Args, stream bool) {
	key := uint64(downloadPath)
	if stream {
		key = streamDownloadPath
	}
	path, _ := protocol.ArgString(args, key)
	if path == "" {
		r.end(t, false, nil, "no path")
		return
	}
	abs, err := hostPath(path)
	if err != nil {
		r.end(t, false, nil, err.Error())
		return
	}
	f, err := os.Open(abs)
	if err != nil {
		r.end(t, false, nil, err.Error())
		return
	}
	defer func() { _ = f.Close() }() // read only: nothing to lose on close
	info, err := f.Stat()
	if err != nil {
		r.end(t, false, nil, err.Error())
		return
	}
	if !info.Mode().IsRegular() {
		r.end(t, false, nil, abs+" is not a file")
		return
	}
	x := newTransfer(abs, f, uint64(info.Size()), stream)
	id := x.id
	if failed := r.sendTransfer(ctx, t, x); failed != "" {
		r.end(t, false, nil, failed)
		return
	}
	if stream {
		r.mu.Lock()
		var sum []byte
		if !x.gap && x.hashed == x.total {
			sum = x.sum.Sum(nil)
		}
		r.mu.Unlock()
		r.write(t, []byte(fmt.Sprintf("%s: %d bytes streamed\n", abs, x.total)),
			protocol.FileStream{Xfer: id, Path: abs, Size: x.total, SHA256: sum})
	} else {
		r.write(t, nil, protocol.FileDownload{Xfer: id, Path: abs, Size: x.total, SHA256: x.sum.Sum(nil)})
	}
	r.end(t, true, nil, "")
}

// newTransfer is a transfer of total bytes read from body, its first chunk
// naming it path, under an id of its own.
func newTransfer(path string, body io.ReaderAt, total uint64, stream bool) *transfer {
	id := make([]byte, 16)
	_, _ = rand.Read(id)
	return &transfer{id: id, path: path, file: body, total: total, sum: sha256.New(), end: make(chan struct{}), stream: stream}
}

// sendTransfer sends x for t, and waits until core has all of it, it fails,
// or the task is stopped: why it did not go, "" when core has it.
func (r *runner) sendTransfer(ctx context.Context, t *task, x *transfer) string {
	r.mu.Lock()
	t.xfer = x
	r.mu.Unlock()
	log.Printf("task %s: sending %s (%d bytes) as transfer %s", hex.EncodeToString(t.id)[:8], x.path, x.total, hex.EncodeToString(x.id)[:8])
	r.nudge()
	select {
	case <-x.end:
	case <-ctx.Done():
		r.mu.Lock()
		x.finish("stopped")
		r.mu.Unlock()
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return x.err
}

// finish ends the transfer: err empty when core has the file. Under r.mu.
func (x *transfer) finish(err string) {
	if x.over {
		return
	}
	x.over, x.err = true, err
	close(x.end)
}

// sending: the transfer has a chunk to put in the next message.
func (x *transfer) sending() bool {
	return x != nil && !x.over && !x.waiting && !x.held
}

// chunk is the next chunk, of at most room bytes, read from the file and
// hashed on its first read. Under r.mu.
func (x *transfer) chunk(t *task, room int) (protocol.ChunkItem, error) {
	if x.stream && !x.announced {
		// A stream announces the file first, without data, until core
		// acks it.
		total := x.total
		x.waiting = true
		return protocol.ChunkItem{T: protocol.ItemChunk, Xfer: x.id, Total: &total, Task: t.id, Name: x.path, Data: []byte{}}, nil
	}
	n := min(uint64(min(room, protocol.MaxChunkData)), x.total-x.acked)
	data := make([]byte, n)
	if _, err := x.file.ReadAt(data, int64(x.acked)); err != nil && !errors.Is(err, io.EOF) {
		return protocol.ChunkItem{}, err
	} else if errors.Is(err, io.EOF) {
		return protocol.ChunkItem{}, fmt.Errorf("%s got shorter while sent", x.path)
	}
	if x.acked > x.hashed {
		// Read ahead of the hash (a stream resumed from further on): the
		// whole's SHA-256 is not known.
		x.gap = true
	}
	if end := x.acked + n; !x.gap && end > x.hashed {
		x.sum.Write(data[x.hashed-x.acked:])
		x.hashed = end
	}
	c := protocol.ChunkItem{T: protocol.ItemChunk, Xfer: x.id, Off: x.acked, Data: data}
	if x.acked == 0 {
		total := x.total
		c.Total, c.Task, c.Name = &total, t.id, x.path
	}
	if x.acked+n == x.total && !x.gap && x.hashed == x.total {
		c.SHA256 = x.sum.Sum(nil)
	}
	x.waiting = true
	return c, nil
}
