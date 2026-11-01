package protocol

import (
	"errors"
	"fmt"
	"strings"

	"github.com/fxamacker/cbor/v2"
)

// File transfers (docs: contracts/minion-messages.md, File transfers): a
// file moves in chunks, each acked with how far the receiver has it, so a
// transfer resumes after a restart of either side. A minion sends one for a
// download_file task; core sends one to a minion for an upload_file
// task, the minion asking for each chunk with its ack (FileOffer).

// MaxChunkData is the most a chunk carries.
const MaxChunkData = 512 << 10

// ChunkItem is one piece of a file in transfer xfer, from off. The first
// (off 0) says the whole's size, the task it is for and the file's name;
// the last (off+len(Data) == Total) says the whole's SHA-256, which the
// receiver checks.
type ChunkItem struct {
	T    int    `cbor:"0,keyasint"`
	Xfer []byte `cbor:"1,keyasint"`
	Off  uint64 `cbor:"2,keyasint"`
	Data []byte `cbor:"3,keyasint"`
	// Total is the whole's size: in the first chunk.
	Total *uint64 `cbor:"4,keyasint,omitempty"`
	// SHA256 is the whole's: in the last chunk.
	SHA256 []byte `cbor:"5,keyasint,omitempty"`
	// Task is the task the transfer is for: in the first chunk.
	Task []byte `cbor:"6,keyasint,omitempty"`
	// Name is the file's, as the sender has it (a path): in the first.
	Name string `cbor:"7,keyasint,omitempty"`
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

// FileOffer is a file param's arg: the file core sends the minion for the
// task, in transfer Xfer (core's pick, one a task). The minion gets ready to
// write it, then asks for its chunks with ack {xfer, next}; each reply brings
// the chunk from next.
type FileOffer struct {
	Xfer []byte `cbor:"1,keyasint" json:"xfer"`
	// Name is the file's name as uploaded: no path.
	Name string `cbor:"2,keyasint" json:"name"`
	// Size and SHA256 are the whole's; a stream's are not known when the
	// task goes: its first chunk says the size, its last the SHA-256.
	Size   uint64 `cbor:"3,keyasint,omitempty" json:"size,omitempty"`
	SHA256 []byte `cbor:"4,keyasint,omitempty" json:"sha256,omitempty"`
	// Stream: the file comes as its uploader sends it to core (docs:
	// contracts/minion-messages.md, Streaming a file to a minion).
	Stream bool `cbor:"5,keyasint,omitempty" json:"stream,omitempty"`
}

// Validate checks the ids and the name.
func (o FileOffer) Validate() error {
	switch {
	case len(o.Xfer) != 16:
		return errors.New("file: xfer is 16 bytes")
	case !o.Stream && len(o.SHA256) != 32:
		return errors.New("file: sha256 is 32 bytes")
	case o.Stream && (o.SHA256 != nil || o.Size != 0):
		return errors.New("file: a stream's size and sha256 come with its chunks")
	case o.Name == "" || strings.ContainsAny(o.Name, "/\\\x00"):
		return fmt.Errorf("file: a name without a path (%q)", o.Name)
	}
	return nil
}

// ArgFile is a task's file arg, as core put it (FileOffer), checked.
func ArgFile(args map[string]any, key string) (*FileOffer, error) {
	v, ok := args[key]
	if !ok {
		return nil, fmt.Errorf("no file %q", key)
	}
	// However the args were decoded (a map of any, of strings), read the
	// value as the CBOR it came as.
	b, err := MarshalCBOR(v)
	if err != nil {
		return nil, fmt.Errorf("file %q: %w", key, err)
	}
	var o FileOffer
	if err := cborDec.Unmarshal(b, &o); err != nil {
		return nil, fmt.Errorf("file %q: %w", key, err)
	}
	if err := o.Validate(); err != nil {
		return nil, err
	}
	return &o, nil
}

// FileUpload is a file_upload task's data: the file core sent in transfer
// Xfer, where the minion put it (Path, as the host writes it), and the size
// and SHA-256 of what it wrote, which core checks against the file it sent.
type FileUpload struct {
	Xfer   []byte `cbor:"xfer" json:"xfer"`
	Path   string `cbor:"path" json:"path"`
	Size   uint64 `cbor:"size" json:"size"`
	SHA256 []byte `cbor:"sha256" json:"sha256"`
	// X holds a factory's own fields.
	X map[string]any `cbor:"x,omitempty" json:"x,omitempty"`
}

// ReadFileUpload decodes a file_upload part's data and checks it. A
// file_upload task gives its data in one part.
func ReadFileUpload(seq uint64, data cbor.RawMessage) (*FileUpload, error) {
	var f FileUpload
	if err := cborDec.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("file_upload: %w", err)
	}
	switch {
	case len(f.Xfer) != 16:
		return nil, errors.New("file_upload: xfer is 16 bytes")
	case f.Path == "":
		return nil, errors.New("file_upload: a path")
	case len(f.SHA256) != 32:
		return nil, errors.New("file_upload: sha256 is 32 bytes")
	}
	return &f, nil
}

// FileStream is a file_stream task's data: the file the minion streamed in
// transfer Xfer, its path on the host and size, and its SHA-256 when the
// minion read it whole, in order (a stream resumed from the middle may end
// without one). Core keeps none of it.
type FileStream struct {
	Xfer   []byte `cbor:"xfer" json:"xfer"`
	Path   string `cbor:"path" json:"path"`
	Size   uint64 `cbor:"size" json:"size"`
	SHA256 []byte `cbor:"sha256,omitempty" json:"sha256,omitempty"`
	// X holds a factory's own fields.
	X map[string]any `cbor:"x,omitempty" json:"x,omitempty"`
}

// ReadFileStream decodes a file_stream part's data and checks it.
func ReadFileStream(seq uint64, data cbor.RawMessage) (*FileStream, error) {
	var f FileStream
	if err := cborDec.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("file_stream: %w", err)
	}
	switch {
	case len(f.Xfer) != 16:
		return nil, errors.New("file_stream: xfer is 16 bytes")
	case f.Path == "":
		return nil, errors.New("file_stream: a path")
	case f.SHA256 != nil && len(f.SHA256) != 32:
		return nil, errors.New("file_stream: sha256 is 32 bytes")
	}
	return &f, nil
}
