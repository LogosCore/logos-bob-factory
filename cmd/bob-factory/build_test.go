package main

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/logoscore/logos-golang-protocol/protocol"
)

// The icons are sent at registration; module-core refuses to start with one
// core would not take.
func TestIconIsValid(t *testing.T) {
	for name, svg := range map[string]string{"icon": icon, "iconLight": iconLight} {
		if err := protocol.ValidateIcon(svg); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestBuildSchemaIsValid(t *testing.T) {
	if err := protocol.ValidateBuildSchema(buildSchema); err != nil {
		t.Fatal(err)
	}
}

// The lite dummy has every command but the shell.
func TestLiteHasNoShell(t *testing.T) {
	ids := func(id string) string {
		target, _ := buildSchema.Target(id)
		var out []string
		for _, c := range buildSchema.TargetCommands(target) {
			out = append(out, c.ID)
		}
		return strings.Join(out, " ")
	}
	if got := ids("dummy"); got != "sleep exit hello ls download shell" {
		t.Errorf("dummy: %s", got)
	}
	if got := ids("dummy-lite"); got != "sleep exit hello ls download" {
		t.Errorf("dummy-lite: %s", got)
	}
	r := coreRequest(t)
	r.Target, r.Commands = "dummy-lite", append(r.Commands, "shell")
	if p := protocol.ValidateBuildRequest(buildSchema, r); p["commands[4]"] == "" || len(p) != 1 {
		t.Fatalf("a lite build with a shell: %v", p)
	}
}

// coreRequest is a request as core would send it: values validated against
// the schema with the defaults filled in.
func coreRequest(t *testing.T) protocol.BuildRequest {
	t.Helper()
	target, _ := buildSchema.Target("dummy")
	http, _ := buildSchema.Channel("http")
	r := protocol.BuildRequest{
		MinionID: "5b0c4f6e-8a1d-4c2b-9e7f-3d2a1b0c9e8f", Target: "dummy", Platform: "any/any",
		SchemaVersion: buildSchema.Version,
		Values: protocol.ResolveFieldValues(buildSchema.TargetFields(target), map[string]any{
			"kill_date": "2027-01-31T00:00:00Z", "punches": []any{"jab", "hook"}, "badge": "badge-1",
		}),
		// Core sends the target's default and required commands when the
		// operator picked none.
		Commands: buildSchema.DefaultCommands(target),
		Connections: []protocol.BuildConnection{{
			Endpoint: protocol.ChannelEndpointExport{
				Format: protocol.ChannelEndpointExportV1, Name: "edge-eu",
				Channel: protocol.ExportChannel{ModuleName: "http", SchemaVersion: 2},
				Params:  map[string]any{"base_url": "https://edge-eu.example.com"},
			},
			Values: protocol.ResolveFieldValues(http.Fields, map[string]any{"path_prefix": "/cdn/assets/"}),
		}},
		Files: map[string]protocol.BuildFile{"badge-1": {
			ID: "badge-1", Name: "badge.png", Size: 3, SHA256: strings.Repeat("ab", 32), MediaType: "image/png",
			URL: "http://logos-seaweedfs-s3:8333/badge-1?X-Amz-Signature=x",
		}},
		Artifact: protocol.ArtifactUpload{UploadURL: "http://logos-seaweedfs-s3:8333/artifact", MaxSize: 1 << 20},
		Deadline: time.Now().Add(time.Minute),
	}
	if p := protocol.ValidateBuildRequest(buildSchema, r); p != nil {
		t.Fatalf("a request core would send does not fit BOB's schema: %v", p)
	}
	return r
}

func TestDescribe(t *testing.T) {
	req := coreRequest(t)
	badge := req.Files["badge-1"]
	at := time.Date(2026, 10, 27, 9, 0, 0, 0, time.UTC)
	m := describe(req, &badge, "bob bob-local dev", at)

	if m.Format != "logos.bob-dummy.v1" || m.MinionID != req.MinionID || m.Platform != "any/any" ||
		m.Greeting != "hello from BOB" || m.KillDate != "2027-01-31T00:00:00Z" ||
		!reflect.DeepEqual(m.Punches, []string{"jab", "hook"}) || !m.BuiltAt.Equal(at) ||
		!reflect.DeepEqual(m.Commands, []string{"sleep", "exit", "hello", "ls"}) {
		t.Fatalf("minion %+v", m)
	}
	if m.Badge == nil || m.Badge.SHA256 != badge.SHA256 || m.Badge.URL != "" || badge.URL == "" {
		t.Fatalf("badge %+v: want it recorded without its download URL, the request's copy untouched", m.Badge)
	}
	conn := m.Connections[0]
	if conn.Endpoint.Name != "edge-eu" || conn.Values["method"] != "POST" || conn.Values["path_prefix"] != "/cdn/assets/" {
		t.Fatalf("connection %+v", conn)
	}

	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "X-Amz-Signature") {
		t.Fatalf("a presigned URL reached the artifact: %s", raw)
	}
}

func TestDescribeWithoutOptionalValues(t *testing.T) {
	req := coreRequest(t)
	delete(req.Values, "kill_date")
	delete(req.Values, "punches")
	m := describe(req, nil, "bob", time.Now())
	if m.KillDate != "" || m.Punches != nil || m.Badge != nil {
		t.Fatalf("minion %+v", m)
	}
}
