package main

import (
	"errors"
	"strconv"
	"sync"

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

// A tunnel's bytes are mostly encrypted already (TLS, SSH, RDP), and do not
// shrink: deflating them only costs time, nearly a millisecond for each
// 64 KiB. So a stream whose data did not shrink packMisses items in a row
// (of protocol.PackMinSize bytes or more) sends the next packSkip bytes of
// it unpacked, then tries packing again. Core does the same.
const (
	packMisses = 3
	packSkip   = 1 << 20
)

// packing is each stream's: its data's misses in a row, and how many bytes
// more go unpacked; by task and stream.
var packing = struct {
	sync.Mutex
	streams map[string]*packState
}{streams: map[string]*packState{}}

type packState struct {
	misses int
	skip   int
}

func packKey(task []byte, stream uint64) string {
	return string(task) + "/" + strconv.FormatUint(stream, 10)
}

// packData is a tunnel_data item, raw, as the minion sends it: packed when
// the build packs and it pays, unless its stream's data did not shrink of
// late.
func packData(d protocol.TunnelDataItem, raw cbor.RawMessage) cbor.RawMessage {
	if !packs || len(raw) < protocol.PackMinSize {
		return packItem(raw)
	}
	key := packKey(d.Task, d.Stream)
	packing.Lock()
	st := packing.streams[key]
	if st == nil {
		st = &packState{}
		packing.streams[key] = st
	}
	if st.skip > 0 {
		st.skip -= len(d.Data)
		if st.skip <= 0 {
			st.skip, st.misses = 0, 0
		}
		packing.Unlock()
		return raw
	}
	packing.Unlock()
	out, packed, err := protocol.Pack(raw)
	packing.Lock()
	if err != nil || !packed {
		if st.misses++; st.misses >= packMisses {
			st.skip = packSkip
		}
	} else {
		st.misses = 0
	}
	packing.Unlock()
	if err != nil || !packed {
		return raw
	}
	return out
}

// forgetPacking lets go of a stream's packing: it is gone.
func forgetPacking(task []byte, stream uint64) {
	packing.Lock()
	delete(packing.streams, packKey(task, stream))
	packing.Unlock()
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
