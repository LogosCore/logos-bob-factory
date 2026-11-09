package main

import (
	"bytes"
	"compress/flate"
	"strings"
	"testing"

	"github.com/fxamacker/cbor/v2"
	"github.com/logoscore/logos-golang-protocol/protocol"
)

// withPacking runs a test with the build's compression on or off.
func withPacking(t *testing.T, on bool) {
	t.Helper()
	was := packs
	packs = on
	t.Cleanup(func() { packs = was })
}

// bigResult is a result with text output that deflates well.
func bigResult() protocol.ResultItem {
	ok := true
	return protocol.ResultItem{T: protocol.ItemResult, Task: bytes.Repeat([]byte{7}, 16), Seq: 1, Done: true, OK: &ok,
		Out: []byte(strings.Repeat("-rw-r--r-- 1 root root 4096 libc.so.6\n", 200))}
}

// sealedItems are the items of a message seal made, as core reads them.
func sealedItems(t *testing.T, items ...any) []cbor.RawMessage {
	t.Helper()
	id := [16]byte{3}
	_, frame, _, err := seal(id, tcpKeys, items)
	if err != nil {
		t.Fatal(err)
	}
	plain, err := protocol.OpenFrame(tcpKeys, id, protocol.FrameMinionToCore, frame)
	if err != nil {
		t.Fatal(err)
	}
	var msg protocol.MinionMessage
	if err := protocol.UnmarshalCBOR(plain, &msg); err != nil {
		t.Fatal(err)
	}
	return msg.Items
}

// With compression on the minion packs what pays; off, nothing.
func TestSealPacksWhereTheBuildPacks(t *testing.T) {
	small := protocol.GetTasksItem{T: protocol.ItemGetTasks}
	withPacking(t, true)
	items := sealedItems(t, small, bigResult())
	if protocol.ItemType(items[0]) != protocol.ItemGetTasks || protocol.ItemType(items[1]) != protocol.ItemPacked {
		t.Fatalf("types %d %d", protocol.ItemType(items[0]), protocol.ItemType(items[1]))
	}
	want, _ := protocol.MarshalCBOR(bigResult())
	if got, err := protocol.Unpack(items[1], protocol.MaxMinionMessageSize); err != nil || !bytes.Equal(got, want) {
		t.Fatalf("unpacked: %v", err)
	}
	packs = false
	if items := sealedItems(t, bigResult()); protocol.ItemType(items[0]) != protocol.ItemResult {
		t.Fatal("packed with compression off")
	}
}

// deflatedRaw is a packed item of data, whatever it holds.
func deflatedRaw(t *testing.T, alg uint64, data []byte) cbor.RawMessage {
	t.Helper()
	var buf bytes.Buffer
	w, _ := flate.NewWriter(&buf, flate.BestSpeed)
	_, _ = w.Write(data)
	_ = w.Close()
	b, err := protocol.MarshalCBOR(protocol.PackedItem{T: protocol.ItemPacked, Alg: alg, Data: buf.Bytes()})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// What core sends packed is read in its place, whatever the build; one
// that does not unpack is answered by its index.
func TestReadCoreTakesPackedItems(t *testing.T) {
	withPacking(t, false)
	poll, _ := protocol.MarshalCBOR(protocol.PollItem{T: protocol.ItemPoll, Within: 7})
	msg := protocol.CoreMessage{V: protocol.MinionMessageV3, ID: protocol.NewMessageID(), At: 1,
		Items: []cbor.RawMessage{deflatedRaw(t, protocol.PackDeflate, poll), deflatedRaw(t, 9, poll),
			deflatedRaw(t, protocol.PackDeflate, make([]byte, protocol.MaxMinionMessageSize))}}
	plain, _ := protocol.MarshalCBOR(msg)
	got, err := readCore(plain)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got.Items[0], poll) || protocol.ItemType(got.Items[1]) != protocol.ItemPacked {
		t.Fatalf("items %v", got.Items)
	}
	r := newTestRunner()
	r.take(got, nil)
	codes := map[int]int{}
	for _, e := range r.errs {
		codes[*e.Item] = e.Code
	}
	if codes[1] != protocol.ErrItemBadMessage || codes[2] != protocol.ErrItemTooLarge || len(codes) != 2 {
		t.Fatalf("errs %+v", r.errs)
	}
	if p, ok := r.pollEvery(); !ok || p.Milliseconds() != 7 {
		t.Fatalf("the packed poll was not taken: %v %v", p, ok)
	}
}
