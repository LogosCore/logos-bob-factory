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
	"sync"
	"testing"
	"time"

	"github.com/fxamacker/cbor/v2"
	"github.com/logoscore/logos-golang-protocol/protocol"
)

// fakeCore is an HTTP channel and core in one: it unpacks a request and
// answers it as core does. A stage under the build id gets a temporary id
// and new keys, a hello under the temporary id a session id, and a check-in
// under the session id, a get_tasks, nothing (an empty reply), unless
// script says otherwise for that check-in: "404", the channel's answer to
// any failure, or "closed", core's sealed session_closed.
type fakeCore struct {
	t         *testing.T
	buildID   [16]byte
	keys      protocol.AES256CBCHMACSHA256Material
	tempID    [16]byte
	newKeys   protocol.AES256CBCHMACSHA256Material
	sessionID [16]byte
	path      string
	hello     protocol.HelloItem
	// checkIns gets each check-in's number (1, 2, … across sessions): a
	// get_tasks for one task.
	checkIns chan int
	script   map[int]string

	mu      sync.Mutex
	stages  int
	checkIn int
}

func (f *fakeCore) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.path = r.URL.Path
	body, _ := io.ReadAll(r.Body)
	off := int(body[0])
	var id [16]byte
	copy(id[:], body[1+off:1+off+16])
	frame := append(append([]byte{}, body[1:1+off]...), body[1+off+16:]...)
	keys := f.keys
	if id != f.buildID {
		keys = f.newKeys
	}
	plain, err := protocol.OpenFrame(keys, id, protocol.FrameMinionToCore, frame)
	if err != nil {
		f.t.Errorf("open under %x: %v", id, err)
		http.Error(w, "", http.StatusUnprocessableEntity)
		return
	}
	var msg protocol.MinionMessage
	if err := protocol.UnmarshalCBOR(plain, &msg); err != nil {
		f.t.Errorf("message: %v", err)
		return
	}
	var item any
	switch id {
	case f.buildID:
		var stage protocol.StageItem
		if len(msg.Items) != 1 || protocol.UnmarshalCBOR(msg.Items[0], &stage) != nil {
			f.t.Errorf("stage %+v", msg)
			return
		}
		pub, err := protocol.ParseStageKey(stage.Key)
		if err != nil {
			f.t.Error(err)
			return
		}
		f.mu.Lock()
		f.stages++
		f.mu.Unlock()
		material, _ := protocol.MarshalCBOR(f.newKeys)
		sealed, _ := protocol.EncryptStageKeys(pub, material)
		item = protocol.StageResponseItem{T: protocol.ItemStageResponse, TID: f.tempID[:], Keys: sealed, Corr: stage.Corr}
	case f.tempID:
		if len(msg.Items) != 1 || protocol.UnmarshalCBOR(msg.Items[0], &f.hello) != nil || f.hello.Validate() != nil {
			f.t.Errorf("hello %+v", msg)
			return
		}
		item = protocol.HelloResponseItem{T: protocol.ItemHelloResponse, SID: f.sessionID[:]}
	case f.sessionID:
		var get protocol.GetTasksItem
		if len(msg.Items) != 1 || protocol.UnmarshalCBOR(msg.Items[0], &get) != nil || get.T != protocol.ItemGetTasks ||
			get.Max == nil || *get.Max != 1 {
			f.t.Errorf("check-in %+v", msg)
		}
		f.mu.Lock()
		f.checkIn++
		n := f.checkIn
		f.mu.Unlock()
		f.checkIns <- n
		switch f.script[n] {
		case "404":
			http.NotFound(w, r)
			return
		case "closed":
			item = protocol.ErrItem{T: protocol.ItemErr, Code: protocol.ErrItemSessionClosed, Msg: "start over"}
		default:
			// Core's 204, as the HTTP channel passes it on: the id, no frame.
			_, _ = w.Write(append([]byte{0}, id[:]...))
			return
		}
	default:
		f.t.Errorf("sent under %x", id)
		return
	}
	raw, _ := protocol.MarshalCBOR(item)
	answer, _ := protocol.MarshalCBOR(protocol.CoreMessage{V: protocol.MinionMessageV1, Re: msg.ID, Items: []cbor.RawMessage{raw}})
	replyFrame, _ := protocol.SealFrame(keys, id, protocol.FrameCoreToMinion, answer)
	out := append([]byte{0}, id[:]...)
	_, _ = w.Write(append(out, replyFrame...))
}

// The compiled minion stages through its http connection, says hello,
// checks in every interval of the connection, and logs it. It keeps its
// session through a failed check-in, and starts over when core closes the
// session.
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
	_, _ = rand.Read(core.sessionID[:])
	core.checkIns = make(chan int, 16)
	core.script = map[int]string{1: "404", 3: "closed"}
	core.newKeys = protocol.AES256CBCHMACSHA256Material{EncKey: make([]byte, 32), MACKey: make([]byte, 32)}
	_, _ = rand.Read(core.newKeys.EncKey)
	_, _ = rand.Read(core.newKeys.MACKey)
	srv := httptest.NewServer(core)
	defer srv.Close()

	req := coreRequest(t)
	material, _ := json.Marshal(core.keys)
	req.Crypto = &protocol.BuildCrypto{Suite: protocol.CryptoAES256CBCHMACSHA256, Material: material}
	req.Connections[0].Endpoint.Params["base_url"] = srv.URL
	req.Connections[0].Endpoint.Params["checkin_interval"] = "100ms"
	req.Connections[0].Endpoint.Params["checkin_jitter"] = 20.0
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
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	// The minion checks in until it is stopped: a failed check-in, a good
	// one, session_closed, and one more under the new session.
	for i := range 4 {
		select {
		case <-core.checkIns:
		case <-time.After(10 * time.Second):
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			t.Fatalf("no check-in %d:\n%s", i+1, logs.String())
		}
	}
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	out := logs.String()
	for _, want := range []string{
		"build 5b0c4f6e-8a1d-4c2b-9e7f-3d2a1b0c9e8f",
		"generated an RSA-2048 key pair",
		"channel answered 200",
		"stage_response: correlation id matches",
		"temporary id " + formatUUID(core.tempID),
		"staged",
		"session id " + formatUUID(core.sessionID),
		"check-in every 100ms ± 20%",
		"check-in 1 failed, keeping the session",
		"check-in 2: get_tasks max 1",
		"core closed the session; starting over",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("log lacks %q:\n%s", want, out)
		}
	}
	if core.hello.PID != uint64(cmd.Process.Pid) || core.hello.OS != "linux" || core.hello.Arch != "amd64" {
		t.Errorf("hello %+v", core.hello)
	}
	core.mu.Lock()
	stages := core.stages
	core.mu.Unlock()
	if stages != 2 {
		t.Errorf("%d stages, want 2: the first, and one after session_closed alone", stages)
	}
	if core.path != "/cdn/assets/" {
		t.Errorf("posted to %q, want the path prefix", core.path)
	}
}

func formatUUID(b [16]byte) string {
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
