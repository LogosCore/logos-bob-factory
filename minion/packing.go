package main

import (
	"errors"

	"github.com/fxamacker/cbor/v2"
	"github.com/logoscore/logos-golang-protocol/protocol"
)

// Compression (docs: contracts/minion-messages/wire-format/, Compression):
// what core sends packed is taken in its place, whatever the build; with
// compression on (the build's "compression", deflate) the minion packs each
// item it sends where that pays (protocol.Pack: 256 bytes or more, an
// eighth smaller), each in a context of its own. Its messages keep within
// the connection by their items' own sizes: packing only makes them
// smaller.

// packs says the build has compression on: set once, as the minion starts.
var packs bool

// packItem is raw as the minion sends it: packed when the build packs and
// packing pays.
func packItem(raw cbor.RawMessage) cbor.RawMessage {
	if !packs {
		return raw
	}
	if out, _, err := protocol.Pack(raw); err == nil {
		return out
	}
	return raw
}

// readCore reads core's message from its plaintext, its packed items taken
// in their places; one that does not unpack stays, for the runner to
// answer (packedErr).
func readCore(plain []byte) (protocol.CoreMessage, error) {
	var msg protocol.CoreMessage
	if err := protocol.UnmarshalCBOR(plain, &msg); err != nil {
		return msg, err
	}
	if err := protocol.ValidateMessageHead(msg.V, msg.ID, msg.Re, len(msg.Items)); err != nil {
		return msg, err
	}
	protocol.UnpackItems(msg.Items)
	return msg, nil
}

// packedErr is the err for item i of items, a packed item that did not
// unpack: too_large past the room its message had left, bad_message
// otherwise.
func packedErr(items []cbor.RawMessage, i int) (int, string) {
	_, err := protocol.Unpack(items[i], protocol.UnpackRoom(items, i))
	switch {
	case errors.Is(err, protocol.ErrPackedTooLarge):
		return protocol.ErrItemTooLarge, err.Error()
	case err != nil:
		return protocol.ErrItemBadMessage, err.Error()
	}
	return protocol.ErrItemBadMessage, protocol.ErrPackedBad.Error()
}
