package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io/fs"
	"log"
	"os"
	"path/filepath"

	"github.com/logoscore/logos-golang-protocol/protocol"
)

// upload puts a file core sends on the host (docs: contracts/minion-messages.md,
// Core sending a file): the task's file arg says the transfer, the file's
// name, size and SHA-256. The minion checks where it goes (a folder that
// exists takes the file under its name; a file there fails the task unless
// overwrite), opens a temporary file beside it, then pulls the file: each
// message asks with ack {xfer, next} for the chunk from what it has, and
// core's reply brings it. With every byte written and checked against the
// size and SHA-256, the temporary file takes the path, and the task ends
// with the file_upload data.

// receiving is a file core sends.
type receiving struct {
	offer protocol.FileOffer
	file  *os.File
	// have is how many bytes are written: the next chunk is from there.
	have uint64
	sum  hash.Hash
	// asked: an ack went in the last message, its answer not taken yet.
	asked bool
	// told: the last ack, saying the minion has all of it, went.
	told bool
	// end is closed when the file is all there, or the transfer failed
	// (err).
	end  chan struct{}
	err  string
	over bool
}

func (r *runner) upload(ctx context.Context, t *task, args map[string]any) {
	offer, err := protocol.ArgFile(args, "file")
	if err != nil {
		r.end(t, false, nil, err.Error())
		return
	}
	path, _ := protocol.ArgString(args, "path")
	overwrite, _ := args["overwrite"].(bool)
	if path == "" {
		r.end(t, false, nil, "no path")
		return
	}
	dest, err := uploadPath(path, offer.Name, overwrite)
	if err != nil {
		r.end(t, false, nil, err.Error())
		return
	}
	tmp, err := os.CreateTemp(filepath.Dir(dest), "."+filepath.Base(dest)+".*.part")
	if err != nil {
		r.end(t, false, nil, err.Error())
		return
	}
	keep := false
	defer func() {
		if !keep {
			_ = tmp.Close()
			_ = os.Remove(tmp.Name())
		}
	}()
	x := &receiving{offer: *offer, file: tmp, sum: sha256.New(), end: make(chan struct{})}
	if offer.Size == 0 {
		x.over = true
		close(x.end)
	}
	r.mu.Lock()
	t.recv = x
	r.mu.Unlock()
	log.Printf("task %s: receiving %s (%d bytes) for %s in transfer %s", hex.EncodeToString(t.id)[:8], offer.Name, offer.Size, dest,
		hex.EncodeToString(offer.Xfer)[:8])
	r.nudge()

	select {
	case <-x.end:
	case <-ctx.Done():
		r.mu.Lock()
		x.finish("stopped")
		r.mu.Unlock()
	}
	r.mu.Lock()
	failed, sum := x.err, x.sum.Sum(nil)
	r.mu.Unlock()
	if failed == "" && !bytes.Equal(sum, offer.SHA256) {
		failed = "the file's SHA-256 is not the one core said"
	}
	if failed == "" {
		failed = place(tmp, dest)
	}
	if failed != "" {
		r.end(t, false, nil, failed)
		return
	}
	keep = true
	r.write(t, []byte(fmt.Sprintf("%s: %d bytes\n", dest, offer.Size)),
		protocol.FileUpload{Xfer: offer.Xfer, Path: dest, Size: offer.Size, SHA256: sum})
	r.end(t, true, nil, "")
}

// uploadPath is where a file goes: path, or name in path when path is a
// folder; an error for a file already there unless overwrite, or a path
// that is not a file.
func uploadPath(path, name string, overwrite bool) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	if info, err := os.Stat(abs); err == nil && info.IsDir() {
		abs = filepath.Join(abs, name)
	}
	info, err := os.Lstat(abs)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return abs, nil
	case err != nil:
		return "", err
	case !info.Mode().IsRegular():
		return "", fmt.Errorf("%s exists and is not a file", abs)
	case !overwrite:
		return "", fmt.Errorf("%s exists", abs)
	}
	return abs, nil
}

// place moves the written file to dest, with dest's mode when it replaces
// one; "" when it went.
func place(tmp *os.File, dest string) string {
	mode := fs.FileMode(0o644)
	if info, err := os.Stat(dest); err == nil {
		mode = info.Mode().Perm()
	}
	if err := tmp.Sync(); err != nil {
		return err.Error()
	}
	if err := tmp.Close(); err != nil {
		return err.Error()
	}
	if err := os.Chmod(tmp.Name(), mode); err != nil {
		return err.Error()
	}
	if err := os.Rename(tmp.Name(), dest); err != nil {
		return err.Error()
	}
	return ""
}

// finish ends the transfer: err empty when the file is all there. Under r.mu.
func (x *receiving) finish(err string) {
	if x.over {
		return
	}
	x.over, x.err = true, err
	close(x.end)
}

// asking: the transfer wants the next chunk asked for.
func (x *receiving) asking() bool {
	return x != nil && !x.over && !x.asked
}

// telling: the file is all there, and core is not told yet: its progress
// ends at the whole.
func (x *receiving) telling() bool {
	return x != nil && x.over && x.err == "" && x.have == x.offer.Size && !x.told
}

// ask is the ack asking for the next chunk, or saying the minion has all
// of it. Under r.mu.
func (x *receiving) ask() protocol.AckItem {
	next := x.have
	x.asked = true
	x.told = x.over
	return protocol.AckItem{T: protocol.ItemAck, Xfer: x.offer.Xfer, Next: &next}
}

// take writes a chunk core sent: one from where the file is, once; the
// first's total and the last's SHA-256 must be the offer's. Under r.mu.
func (x *receiving) take(c protocol.ChunkItem) {
	x.asked = false
	if x.over || c.Off != x.have {
		return
	}
	switch {
	case c.Total != nil && *c.Total != x.offer.Size:
		x.finish(fmt.Sprintf("core said %d bytes, then %d", x.offer.Size, *c.Total))
		return
	case c.Off+uint64(len(c.Data)) > x.offer.Size:
		x.finish("a chunk past the file's size")
		return
	case c.SHA256 != nil && !bytes.Equal(c.SHA256, x.offer.SHA256):
		x.finish("the last chunk's SHA-256 is not the one core said")
		return
	}
	if _, err := x.file.Write(c.Data); err != nil {
		x.finish(err.Error())
		return
	}
	x.sum.Write(c.Data)
	x.have += uint64(len(c.Data))
	if x.have == x.offer.Size {
		x.finish("")
	}
}
