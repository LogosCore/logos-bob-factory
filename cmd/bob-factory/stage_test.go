package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/fxamacker/cbor/v2"
	"github.com/logoscore/logos-golang-protocol/protocol"
)

// fakeCore is an HTTP channel and core in one: it unpacks a request, opens
// the stage as core does, and answers with a stage_response.
type fakeCore struct {
	t       *testing.T
	buildID [16]byte
	keys    protocol.AES256CBCHMACSHA256Material
	tempID  [16]byte
	path    string
}

func (f *fakeCore) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.path = r.URL.Path
	body, _ := io.ReadAll(r.Body)
	off := int(body[0])
	var id [16]byte
	copy(id[:], body[1+off:1+off+16])
	frame := append(append([]byte{}, body[1:1+off]...), body[1+off+16:]...)
	if id != f.buildID {
		f.t.Errorf("sent under %x", id)
	}
	plain, err := protocol.OpenFrame(f.keys, id, protocol.FrameMinionToCore, frame)
	if err != nil {
		f.t.Errorf("open: %v", err)
		http.NotFound(w, r)
		return
	}
	var msg protocol.MinionMessage
	var stage protocol.StageItem
	if err := protocol.UnmarshalCBOR(plain, &msg); err != nil || len(msg.Items) != 1 || protocol.UnmarshalCBOR(msg.Items[0], &stage) != nil {
		f.t.Errorf("message %+v: %v", msg, err)
		return
	}
	pub, err := protocol.ParseStageKey(stage.Key)
	if err != nil {
		f.t.Error(err)
		return
	}
	newKeys := protocol.AES256CBCHMACSHA256Material{EncKey: make([]byte, 32), MACKey: make([]byte, 32)}
	material, _ := protocol.MarshalCBOR(newKeys)
	sealed, _ := protocol.EncryptStageKeys(pub, material)
	item, _ := protocol.MarshalCBOR(protocol.StageResponseItem{T: protocol.ItemStageResponse, TID: f.tempID[:], Keys: sealed, Corr: stage.Corr})
	answer, _ := protocol.MarshalCBOR(protocol.CoreMessage{V: protocol.MinionMessageV1, Re: msg.ID, Items: []cbor.RawMessage{item}})
	replyFrame, _ := protocol.SealFrame(f.keys, id, protocol.FrameCoreToMinion, answer)
	out := append([]byte{0}, id[:]...)
	_, _ = w.Write(append(out, replyFrame...))
}

// The compiled minion stages through its http connection and logs it.
func TestMinionStages(t *testing.T) {
	needGo(t)
	if runtime.GOOS != "linux" || runtime.GOARCH != "amd64" {
		t.Skip("runs a linux/amd64 minion")
	}
	core := &fakeCore{t: t, buildID: [16]byte{0x5b, 0x0c, 0x4f, 0x6e, 0x8a, 0x1d, 0x4c, 0x2b, 0x9e, 0x7f, 0x3d, 0x2a, 0x1b, 0x0c, 0x9e, 0x8f}}
	core.keys = protocol.AES256CBCHMACSHA256Material{EncKey: make([]byte, 32), MACKey: make([]byte, 32)}
	_, _ = rand.Read(core.keys.EncKey)
	_, _ = rand.Read(core.keys.MACKey)
	_, _ = rand.Read(core.tempID[:])
	srv := httptest.NewServer(core)
	defer srv.Close()

	req := coreRequest(t)
	material, _ := json.Marshal(core.keys)
	req.Crypto = &protocol.BuildCrypto{Suite: protocol.CryptoAES256CBCHMACSHA256, Material: material}
	req.Connections[0].Endpoint.Params["base_url"] = srv.URL
	desc, _ := json.Marshal(describe(req, nil, "bob", req.Deadline))
	bin, err := compile(context.Background(), testMinionSrc, "linux-elf", "linux/amd64", desc, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "minion")
	if err := os.WriteFile(path, bin, 0o700); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	cmd := exec.Command(path)
	cmd.Stderr = &logs
	if err := cmd.Run(); err != nil {
		t.Fatalf("the minion failed: %v\n%s", err, logs.String())
	}
	out := logs.String()
	for _, want := range []string{
		"build 5b0c4f6e-8a1d-4c2b-9e7f-3d2a1b0c9e8f",
		"generated an RSA-2048 key pair",
		"channel answered 200",
		"stage_response: correlation id matches",
		"temporary id " + formatTempID(core.tempID),
		"staged.",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("log lacks %q:\n%s", want, out)
		}
	}
	if core.path != "/cdn/assets/" {
		t.Errorf("posted to %q, want the path prefix", core.path)
	}
}

func formatTempID(b [16]byte) string {
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
