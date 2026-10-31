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
	"slices"
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
// under the session id nothing (an empty reply), unless script says
// otherwise for that check-in: "404", the channel's answer to any failure,
// or "closed", core's sealed session_closed; or, when it asks for tasks
// (get_tasks, max 1) and tasks are waiting, the next of them. It keeps what
// the check-ins carried.
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
	// tasks wait for a get_tasks; received is what check-ins carried.
	tasks    []protocol.TaskItem
	received []cbor.RawMessage

	mu      sync.Mutex
	stages  int
	checkIn int
	hellos  []protocol.HelloItem
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
		var h protocol.HelloItem // fresh: a key a hello leaves out stays unset
		if len(msg.Items) != 1 || protocol.UnmarshalCBOR(msg.Items[0], &h) != nil || h.Validate() != nil {
			f.t.Errorf("hello %+v", msg)
			return
		}
		f.mu.Lock()
		f.hello = h
		f.hellos = append(f.hellos, h)
		f.mu.Unlock()
		item = protocol.HelloResponseItem{T: protocol.ItemHelloResponse, SID: f.sessionID[:]}
	case f.sessionID:
		asked := false
		for _, raw := range msg.Items {
			if protocol.ItemType(raw) != protocol.ItemGetTasks {
				continue
			}
			var get protocol.GetTasksItem
			if asked || protocol.UnmarshalCBOR(raw, &get) != nil || get.Max == nil || *get.Max != 1 {
				f.t.Errorf("check-in %+v", msg)
			}
			asked = true
		}
		f.mu.Lock()
		f.checkIn++
		n := f.checkIn
		f.received = append(f.received, msg.Items...)
		if asked && len(f.tasks) > 0 && f.script[n] == "" {
			item, f.tasks = f.tasks[0], f.tasks[1:]
			f.script[n] = "task"
		}
		f.mu.Unlock()
		f.checkIns <- n
		switch f.script[n] {
		case "404":
			http.NotFound(w, r)
			return
		case "closed":
			item = protocol.ErrItem{T: protocol.ItemErr, Code: protocol.ErrItemSessionClosed, Msg: "start over"}
		case "replaced":
			item = protocol.ErrItem{T: protocol.ItemErr, Code: protocol.ErrItemSessionReplaced, Msg: "start over without claiming"}
		case "task":
			// item is the task.
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
	core := newFakeCore(t, map[int]string{1: "404", 3: "closed"})
	cmd, logs := startMinion(t, core, nil)
	// The minion checks in until it is stopped: a failed check-in, a good
	// one, session_closed, and one more under the new session.
	waitCheckIns(t, core, cmd, logs, 4)
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

func newFakeCore(t *testing.T, script map[int]string) *fakeCore {
	core := &fakeCore{t: t, buildID: [16]byte{0x5b, 0x0c, 0x4f, 0x6e, 0x8a, 0x1d, 0x4c, 0x2b, 0x9e, 0x7f, 0x3d, 0x2a, 0x1b, 0x0c, 0x9e, 0x8f}}
	core.keys = protocol.AES256CBCHMACSHA256Material{EncKey: make([]byte, 32), MACKey: make([]byte, 32)}
	_, _ = rand.Read(core.keys.EncKey)
	_, _ = rand.Read(core.keys.MACKey)
	_, _ = rand.Read(core.tempID[:])
	_, _ = rand.Read(core.sessionID[:])
	core.checkIns = make(chan int, 16)
	core.script = script
	core.newKeys = protocol.AES256CBCHMACSHA256Material{EncKey: make([]byte, 32), MACKey: make([]byte, 32)}
	_, _ = rand.Read(core.newKeys.EncKey)
	_, _ = rand.Read(core.newKeys.MACKey)
	return core
}

// startMinion compiles a minion for core's fake (mutate edits its build
// request) and starts it; the test stops it.
func startMinion(t *testing.T, core *fakeCore, mutate func(req *protocol.BuildRequest)) (*exec.Cmd, *bytes.Buffer) {
	t.Helper()
	srv := httptest.NewServer(core)
	t.Cleanup(srv.Close)
	req := coreRequest(t)
	material, _ := json.Marshal(core.keys)
	req.Crypto = &protocol.BuildCrypto{Suite: protocol.CryptoAES256CBCHMACSHA256, Material: material}
	req.Connections[0].Endpoint.Params["base_url"] = srv.URL
	req.Connections[0].Endpoint.Params["checkin_interval"] = "100ms"
	req.Connections[0].Endpoint.Params["checkin_jitter"] = 20.0
	if mutate != nil {
		mutate(&req)
	}
	desc, _ := json.Marshal(describe(req, nil, "bob", req.Deadline))
	bin, err := compile(context.Background(), testMinionSrc, "linux-elf", "linux/amd64", desc, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "minion")
	if err := os.WriteFile(path, bin, 0o700); err != nil {
		t.Fatal(err)
	}
	logs := &bytes.Buffer{}
	cmd := exec.Command(path)
	cmd.Stderr = logs
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	return cmd, logs
}

// waitCheckIns waits for n check-ins at core's fake, then stops the minion.
func waitCheckIns(t *testing.T, core *fakeCore, cmd *exec.Cmd, logs *bytes.Buffer, n int) {
	t.Helper()
	for i := range n {
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
}

// A minion built with a persisted session claims it in its hello; told
// another process took it, it starts over without claiming.
func TestMinionClaimsPersistedSession(t *testing.T) {
	needGo(t)
	if runtime.GOOS != "linux" || runtime.GOARCH != "amd64" {
		t.Skip("runs a linux/amd64 minion")
	}
	core := newFakeCore(t, map[int]string{2: "replaced"})
	cmd, logs := startMinion(t, core, func(req *protocol.BuildRequest) { req.SessionID = formatUUID(core.sessionID) })
	waitCheckIns(t, core, cmd, logs, 3)
	core.mu.Lock()
	hellos := core.hellos
	core.mu.Unlock()
	if len(hellos) != 2 || !bytes.Equal(hellos[0].SID, core.sessionID[:]) || hellos[1].SID != nil ||
		hellos[0].Sleep == nil || *hellos[0].Sleep != (protocol.Sleep{Interval: 100, Jitter: 20}) {
		t.Fatalf("hellos %+v\n%s", hellos, logs.String())
	}
	for _, want := range []string{"the persisted session, claimed", "goes on in a session of its own"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("log lacks %q:\n%s", want, logs.String())
		}
	}
}

func formatUUID(b [16]byte) string {
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// The minion runs the tasks core gives it, one at a time: it acks each and
// sends its output as results, the structured output of ls as data, a shell
// command's exit code, and stops once the exit task's end is delivered.
func TestMinionRunsTasks(t *testing.T) {
	needGo(t)
	if runtime.GOOS != "linux" || runtime.GOARCH != "amd64" {
		t.Skip("runs a linux/amd64 minion")
	}
	core := newFakeCore(t, map[int]string{})
	id := func(b byte) []byte { return bytes.Repeat([]byte{b}, 16) }
	// A directory past a message's 256 items, and past one part.
	big := t.TempDir()
	for i := range 1200 {
		if err := os.WriteFile(filepath.Join(big, fmt.Sprintf("f%04d", i)), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	core.tasks = []protocol.TaskItem{
		{T: protocol.ItemTask, Task: id(1), Cmd: "hello"},
		{T: protocol.ItemTask, Task: id(2), Cmd: "shell", Args: map[string]any{"command": "echo hi; exit 3", "timeout": uint64(5000)}},
		{T: protocol.ItemTask, Task: id(3), Cmd: "ls", Args: map[string]any{"path": "/"}},
		{T: protocol.ItemTask, Task: id(6), Cmd: "ls", Args: map[string]any{"path": big}},
		{T: protocol.ItemTask, Task: id(4), Cmd: "sleep", Args: map[string]any{"interval": uint64(50), "jitter": int64(0)}},
		{T: protocol.ItemTask, Task: id(5), Cmd: "exit"},
	}
	cmd, logs := startMinion(t, core, func(req *protocol.BuildRequest) { req.Commands = append(req.Commands, "shell") })
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	go func() {
		for range core.checkIns {
		}
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("the minion exited with %v:\n%s", err, logs.String())
		}
	case <-time.After(20 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatalf("the minion did not exit:\n%s", logs.String())
	}

	core.mu.Lock()
	received := core.received
	core.mu.Unlock()
	acked := map[byte]bool{}
	results := map[byte][]protocol.ResultItem{}
	var sleepUpdate *protocol.SleepUpdateItem
	for _, raw := range received {
		switch protocol.ItemType(raw) {
		case protocol.ItemAck:
			var a protocol.AckItem
			_ = protocol.UnmarshalCBOR(raw, &a)
			for _, task := range a.Tasks {
				acked[task[0]] = true
			}
		case protocol.ItemResult:
			var r protocol.ResultItem
			if err := protocol.UnmarshalCBOR(raw, &r); err != nil || r.Validate() != nil {
				t.Fatalf("result %v %v", r, err)
			}
			results[r.Task[0]] = append(results[r.Task[0]], r)
		case protocol.ItemSleepUpdate:
			var u protocol.SleepUpdateItem
			_ = protocol.UnmarshalCBOR(raw, &u)
			sleepUpdate = &u
		}
	}
	end := func(task byte) (protocol.ResultItem, string) {
		t.Helper()
		parts := results[task]
		if !acked[task] || len(parts) == 0 || !parts[len(parts)-1].Done {
			t.Fatalf("task %d: acked %v, %d parts:\n%s", task, acked[task], len(parts), logs.String())
		}
		var out []byte
		for i, p := range parts {
			if p.Seq != uint64(i) {
				t.Fatalf("task %d: part %d has seq %d", task, i, p.Seq)
			}
			out = append(out, p.Out...)
		}
		return parts[len(parts)-1], string(out)
	}
	if last, out := end(1); !*last.OK || out != "hello from BOB\n" {
		t.Errorf("hello: %q", out)
	}
	if last, out := end(2); !*last.OK || last.Exit == nil || *last.Exit != 3 || out != "hi\n" {
		t.Errorf("shell: ok %v exit %v out %q", *last.OK, last.Exit, out)
	}
	// An ls's parts are a dir_listing: the first names the directory, every
	// one reads as the handler says.
	listing := func(task byte) (*protocol.DirListing, int) {
		var first *protocol.DirListing
		n := 0
		for _, p := range results[task] {
			if len(p.Data) == 0 {
				continue
			}
			l, err := protocol.ReadDirListing(p.Seq, p.Data)
			if err != nil {
				t.Fatalf("task %d part %d: %v", task, p.Seq, err)
			}
			if p.Seq == 0 {
				first = l
			}
			n += len(l.Entries)
		}
		return first, n
	}
	if last, _ := end(3); !*last.OK {
		t.Errorf("ls failed: %s", last.Err)
	} else if l, n := listing(3); l == nil || l.Path != "/" || l.Sep != "/" || l.Parent != "" || n == 0 {
		t.Errorf("ls of /: %+v, %d entries", l, n)
	} else if i := slices.IndexFunc(l.Entries, func(e protocol.DirEntry) bool { return e.Name == "etc" }); i < 0 ||
		l.Entries[i].Type != protocol.DirEntryDir || l.Entries[i].Modified == nil || l.Entries[i].Owner == "" {
		t.Errorf("ls of / has no etc folder with its time and owner: %+v", l.Entries)
	}
	if last, _ := end(6); !*last.OK || len(results[6]) < 3 {
		t.Errorf("a big ls in %d parts, want 3 or more", len(results[6]))
	} else if l, n := listing(6); l == nil || n != 1200 {
		t.Errorf("a big ls joined to %d entries, want 1200", n)
	}
	if last, _ := end(4); !*last.OK || sleepUpdate == nil || sleepUpdate.Interval != 50 || sleepUpdate.Jitter != 0 {
		t.Errorf("sleep: %+v %+v", last, sleepUpdate)
	}
	if last, out := end(5); !*last.OK || out != "bye\n" {
		t.Errorf("exit: %q", out)
	}
}
