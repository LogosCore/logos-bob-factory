package main

import (
	"strings"
	"testing"

	"github.com/fxamacker/cbor/v2"
	"github.com/logoscore/logos-golang-protocol/protocol"
)

// Core items the minion does not take get unknown_item, naming the item; a
// rehello gets a hello in the next message.
func TestCoreItemsTheMinionDoesNotTake(t *testing.T) {
	r := newTestRunner()
	r.hello = func() protocol.HelloItem { return protocol.HelloItem{T: protocol.ItemHello, PID: 7, Conn: 2} }
	rehello, _ := protocol.MarshalCBOR(protocol.RehelloItem{T: protocol.ItemRehello})
	resume, _ := protocol.MarshalCBOR(protocol.ResumeItem{T: protocol.ItemResume, Task: make([]byte, 16), Ctx: []byte("x")})
	future, _ := protocol.MarshalCBOR(map[int]any{0: 99})
	r.take(protocol.CoreMessage{V: protocol.MinionMessageV3, Items: []cbor.RawMessage{rehello, resume, future}}, nil)
	if !r.pending() {
		t.Fatal("nothing to say after a rehello")
	}
	items := r.outgoing()
	if h, ok := items[0].(protocol.HelloItem); !ok || h.PID != 7 || h.Conn != 2 {
		t.Fatalf("first item %#v, not the hello", items[0])
	}
	var errs []protocol.ErrItem
	for _, it := range items {
		if e, ok := it.(protocol.ErrItem); ok {
			errs = append(errs, e)
		}
	}
	if len(errs) != 2 || errs[0].Code != protocol.ErrItemUnknownItem || *errs[0].Item != 1 ||
		errs[1].Code != protocol.ErrItemUnknownItem || *errs[1].Item != 2 || !strings.Contains(errs[1].Msg, "item 99") {
		t.Fatalf("errs %+v", errs)
	}
	if next := r.outgoing(); len(next) != 1 {
		t.Fatalf("the hello asked again: %#v", next)
	}
}

// A connection that carries little (its channel's max_message) gets small
// listing pieces and small result parts.
func TestListingWithinTheConnection(t *testing.T) {
	r := newTestRunner()
	r.setLimit(3000)
	tk := &task{id: make([]byte, 16)}
	var entries []entry
	for i := range 200 {
		entries = append(entries, entry{LsEntry: protocol.LsEntry{Name: "file-" + strings.Repeat("x", 20) + itoa(i), Type: protocol.EntryFile},
			owner: "root", group: "root"})
	}
	r.list(tk, "/srv", "/", "/", entries, false, 0)
	if len(tk.pieces) < 5 {
		t.Fatalf("%d pieces for 200 entries over a 3000-byte connection", len(tk.pieces))
	}
	n := 0
	for i, p := range tk.pieces {
		if tk.sizes[i] > 1200 {
			t.Fatalf("piece %d is %d bytes", i, tk.sizes[i])
		}
		n += len(p.Entries)
		if p.Last != (i == len(tk.pieces)-1) || p.Part != uint64(i) {
			t.Fatalf("piece %d: part %d last %v", i, p.Part, p.Last)
		}
	}
	if n != len(entries) {
		t.Fatalf("%d entries in the pieces, not %d", n, len(entries))
	}
	r.tasks = append(r.tasks, tk)
	tk.ended, tk.ok = true, true
	for range 50 {
		items := r.outgoing()
		size := 0
		for _, it := range items {
			b, _ := protocol.MarshalCBOR(it)
			size += len(b)
		}
		if size > 3000 {
			t.Fatalf("a message of %d bytes over a 3000-byte connection", size)
		}
		r.delivered(items)
		if !r.pending() {
			return
		}
	}
	t.Fatal("the listing never all went")
}

// An empty directory is one piece, the last.
func TestListingEmpty(t *testing.T) {
	r := newTestRunner()
	tk := &task{id: make([]byte, 16)}
	r.list(tk, "/empty", "/", "/", nil, false, 0)
	if len(tk.pieces) != 1 || !tk.pieces[0].Last || tk.pieces[0].Path != "/empty" {
		t.Fatalf("pieces %+v", tk.pieces)
	}
}
