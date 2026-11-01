package protocol

import (
	"errors"
	"fmt"
)

// Connections (docs: contracts/channel-core-connections/): beside the sync
// call, a channel opens a WebSocket to core for each persistent minion
// connection, a connection socket, and the connection's messages go over it
// both ways, core sending when it has something.

// Connection kinds: who dials (reverse: the minion; bind: the channel), and
// whether the connection carries one exchange (call) or stays open
// (persistent).
const (
	KindReverseCall       = "reverse-call"
	KindReversePersistent = "reverse-persistent"
	KindBindCall          = "bind-call"
	KindBindPersistent    = "bind-persistent"
)

// ConnectionPath is core's endpoint for connection sockets, under core's
// base URL: a WebSocket handshake.
const ConnectionPath = "/api/channel/connection"

// Headers of a connection socket's handshake, besides HeaderChannelInstance
// and, for a reverse-persistent connection, HeaderPeerAddress (the minion's
// address, as on the sync call).
const (
	HeaderConnectionKind = "Logos-Connection-Kind"
	// HeaderOpenRequest names the open request a bind-persistent
	// connection's socket answers.
	HeaderOpenRequest = "Logos-Open-Request"
)

// ConnFrame is one binary frame of a connection socket, either way: a minion
// message's sealed frame and the id it is sent under (build id, temporary
// id or session id, 16 raw bytes).
type ConnFrame struct {
	ID   []byte `cbor:"1,keyasint"`
	Data []byte `cbor:"2,keyasint"`
}

// EncodeConnFrame encodes a connection socket's frame.
func EncodeConnFrame(f ConnFrame) ([]byte, error) {
	if err := f.Validate(); err != nil {
		return nil, err
	}
	return cborEnc.Marshal(f)
}

// DecodeConnFrame decodes and checks a connection socket's frame.
func DecodeConnFrame(b []byte) (ConnFrame, error) {
	var f ConnFrame
	if err := UnmarshalCBOR(b, &f); err != nil {
		return ConnFrame{}, fmt.Errorf("connection frame: %w", err)
	}
	return f, f.Validate()
}

// Validate checks the id's length and that there is a message.
func (f ConnFrame) Validate() error {
	switch {
	case len(f.ID) != 16:
		return errors.New("connection frame: the id is 16 bytes")
	case len(f.Data) == 0:
		return errors.New("connection frame: no message")
	}
	return nil
}
