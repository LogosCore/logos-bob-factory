package protocol

import (
	"errors"
	"fmt"

	"github.com/fxamacker/cbor/v2"
)

// File transfers (docs: contracts/minion-messages.md, File transfers): a
// file moves in chunks, each acked with how far the receiver has it, so a
// transfer resumes after a restart of either side. A minion sends one for a
// download_file task; core sending one to a minion comes later.

// ItemChunk is one piece of a file transfer.
const ItemChunk = "chunk"

// MaxChunkData is the most a chunk carries.
const MaxChunkData = 512 << 10

// ChunkItem is one piece of a file in transfer xfer, from off. The first
// (off 0) says the whole's size, the task it is for and the file's name;
// the last (off+len(Data) == Total) says the whole's SHA-256, which the
// receiver checks.
type ChunkItem struct {
	T    string `cbor:"t"`
	Xfer []byte `cbor:"xfer"`
	Off  uint64 `cbor:"off"`
	Data []byte `cbor:"data"`
	// Total is the whole's size: in the first chunk.
	Total *uint64 `cbor:"total,omitempty"`
	// SHA256 is the whole's: in the last chunk.
	SHA256 []byte `cbor:"sha256,omitempty"`
	// Task is the task the transfer is for: in the first chunk.
	Task []byte `cbor:"task,omitempty"`
	// Name is the file's, as the sender has it (a path): in the first.
	Name string `cbor:"name,omitempty"`
}

// Validate checks the ids and sizes, and that the first chunk says what a
// receiver needs to start (total, task).
func (c ChunkItem) Validate() error {
	switch {
	case len(c.Xfer) != 16:
		return errors.New("chunk: xfer is 16 bytes")
	case len(c.Data) > MaxChunkData:
		return fmt.Errorf("chunk: data of %d bytes is over %d", len(c.Data), MaxChunkData)
	case c.SHA256 != nil && len(c.SHA256) != 32:
		return errors.New("chunk: sha256 is 32 bytes")
	case c.Task != nil && len(c.Task) != 16:
		return errors.New("chunk: task is 16 bytes")
	case c.Off == 0 && (c.Total == nil || c.Task == nil):
		return errors.New("chunk: the first says total and task")
	case c.Off != 0 && (c.Total != nil || c.Task != nil || c.Name != ""):
		return errors.New("chunk: only the first says total, task and name")
	case c.Total != nil && uint64(len(c.Data)) > *c.Total:
		return errors.New("chunk: more data than total")
	}
	return nil
}

// FileDownload is a file_download task's data: the file the minion sent in
// transfer Xfer, its path on the host, size and SHA-256. Core keeps the file
// itself; this says which one, and what it should be.
type FileDownload struct {
	Xfer   []byte `cbor:"xfer" json:"xfer"`
	Path   string `cbor:"path" json:"path"`
	Size   uint64 `cbor:"size" json:"size"`
	SHA256 []byte `cbor:"sha256" json:"sha256"`
	// X holds a factory's own fields.
	X map[string]any `cbor:"x,omitempty" json:"x,omitempty"`
}

// ReadFileDownload decodes a file_download part's data and checks it. A
// file_download task gives its data in one part.
func ReadFileDownload(seq uint64, data cbor.RawMessage) (*FileDownload, error) {
	var f FileDownload
	if err := cborDec.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("file_download: %w", err)
	}
	switch {
	case len(f.Xfer) != 16:
		return nil, errors.New("file_download: xfer is 16 bytes")
	case f.Path == "":
		return nil, errors.New("file_download: a path")
	case len(f.SHA256) != 32:
		return nil, errors.New("file_download: sha256 is 32 bytes")
	}
	return &f, nil
}
