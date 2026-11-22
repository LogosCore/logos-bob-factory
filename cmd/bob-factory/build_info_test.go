package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/logoscore/logos-golang-factory-core/pkg/factorytest"
	"github.com/logoscore/logos-golang-protocol/protocol"
)

// Every factory command of the schema has its code, and only those:
// factory.Run refuses to start otherwise.
func TestFactoryCommandsHaveCode(t *testing.T) {
	var ids []string
	for _, c := range buildSchema.FactoryCommands {
		ids = append(ids, c.ID)
	}
	code := slices.Sorted(func(yield func(string) bool) {
		for id := range factoryCommands {
			if !yield(id) {
				return
			}
		}
	})
	if slices.Sort(ids); !slices.Equal(ids, code) {
		t.Fatalf("factory commands %v, code for %v", ids, code)
	}
}

// recordOf is a build BOB made with req, as a factory command's call carries
// it: core's record, the request byte for byte, its badge and artifact on st.
func recordOf(t *testing.T, req protocol.BuildRequest, st *factorytest.Store) protocol.BuildRecord {
	t.Helper()
	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	started := time.Date(2026, 11, 22, 10, 0, 1, 0, time.UTC)
	finished := started.Add(14 * time.Second)
	// Each build's own exit codes, but for Go's (fixed).
	drawn := map[int]int{3: 117, 4: 42, 5: 203}
	var codes []protocol.BuildRecordErrorCode
	for _, c := range buildSchema.ErrorCodes {
		codes = append(codes, protocol.BuildRecordErrorCode{ErrorCode: c, ExitCode: cmpOr(drawn[c.Code], c.Code)})
	}
	return protocol.BuildRecord{
		BuildID: req.BuildID, Description: "the edge in Frankfurt", Target: req.Target, Platform: req.Platform,
		SchemaVersion: req.SchemaVersion, CreatedBy: &protocol.Actor{Type: "user", ID: protocol.NewID(), Name: "alice"},
		CreatedAt: started.Add(-time.Second), StartedAt: &started, FinishedAt: &finished,
		Instance: "bob-1", FactoryVersion: "v0.47.0", Request: raw,
		ConnectionDescriptions: []string{"behind the CDN"}, ErrorCodes: codes,
		Warnings: []string{"a dummy minion, made to try the build flow end to end"},
		Artifact: st.BuildArtifact("bob-5b0c4f6e.bin", "application/x-executable", bytes.Repeat([]byte{0x7f}, 3000)),
		Files:    map[string]protocol.BuildFile{"badge-1": st.File("badge-1", "badge.png", "image/png", []byte{1, 2, 3})},
	}
}

func cmpOr(a, b int) int {
	if a != 0 {
		return a
	}
	return b
}

// build_info prints the session's build by BOB's labels, as core keeps it:
// what it is, its values (the badge by its file), commands, connections,
// exit codes and what the minion was built with; and none of the secrets its
// call carries, nor its links.
func TestBuildInfo(t *testing.T) {
	st := factorytest.NewStore(t)
	req := coreRequest(t)
	out := factorytest.Command(t, buildSchema, buildInfo{}, st, protocol.FactoryCommandRequest{
		Command: buildInfoCommand.ID, Build: recordOf(t, req, st),
	})
	text := string(out.Output)
	if out.Result.State != protocol.EndDone || !out.Result.OK {
		t.Fatalf("result %+v, output:\n%s", out.Result, text)
	}
	for _, want := range []string{
		"Build 5b0c4f6e-8a1d-4c2b-9e7f-3d2a1b0c9e8f (session #1)",
		"Target            Linux binary (linux-elf)",
		"Platform          linux/amd64",
		"Description       the edge in Frankfurt",
		"Created           2026.11.22 10:00:00 UTC by alice",
		"Finished          2026.11.22 10:00:15 UTC, took 14s",
		"Built by          bob-1, BOB v0.47.0",
		"Artifact          bob-5b0c4f6e.bin, 2.9 KiB, application/x-executable",
		"Warning           a dummy minion",
		"Greeting          hello from BOB",
		"Kill date         2027-01-31T00:00:00Z",
		"Punches it takes  jab, hook",
		"Badge             badge.png, 3 B, image/png",
		"  sleep, exit, hello, ls,",
		"  1. edge-eu: http v3, reverse-call",
		"     Note         behind the CDN",
		"     base_url     https://edge-eu.example.com",
		"     Method       POST",
		"     Path prefix  /cdn/assets/",
		"  117  Its config does not read (declared 3)",
		"  2    Crashed (in every build)",
		"Keys           aes-256-cbc-hmac-sha256 (not shown)",
		"Minion config  ", "baked in (not shown: it holds the keys)",
		"Compression    off",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("no %q in:\n%s", want, text)
		}
	}
	// The keys (core's two, as base64), the minion config that holds them,
	// and the links.
	config := base64.StdEncoding.EncodeToString(req.MinionConfig)
	for _, secret := range []string{"AQEBAQEB", "AgICAgIC", config[:min(len(config), 24)], "sig=", st.UploadURL("")} {
		if strings.Contains(text, secret) {
			t.Errorf("%q printed:\n%s", secret, text)
		}
	}
}

// A credential is shown by its type, name and revision, never its values;
// failover in a line; a build of an older schema says so.
func TestBuildInfoCredentialsAndFailover(t *testing.T) {
	st := factorytest.NewStore(t)
	req := coreRequest(t)
	id := protocol.NewID()
	req.Credentials = map[string]protocol.Credential{id: {ID: id, Name: "edge proxy", Type: "password", Username: "edge", Password: "hunter2"}}
	failover := protocol.DefaultBuildFailover()
	req.Failover = &failover
	rec := recordOf(t, req, st)
	rec.SchemaVersion = buildSchema.Version - 1
	rec.CredentialRevs = map[string]int64{id: 3}

	var b bytes.Buffer
	writeBuildInfo(&b, buildSchema, factorytest.Session(buildSchema), rec, req)
	text := b.String()
	for _, want := range []string{
		"Schema version    5 (BOB's is 6 now)",
		"Credentials (1): their values are not shown",
		id + "  password, edge proxy, revision 3",
		"on: the minion moves after 5 failures in a row; after the last connection, the first again; " +
			"5m pause after a round that all failed; back to the first after 1h",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("no %q in:\n%s", want, text)
		}
	}
	if strings.Contains(text, "hunter2") {
		t.Errorf("a credential's password printed:\n%s", text)
	}
}
