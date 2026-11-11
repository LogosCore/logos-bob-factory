package main

import (
	"encoding/json"
	"reflect"
	"slices"
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

// The binary has every command; shell, the tunnels (tunnel,
// reverse_tunnel) and the delegates (delegate, reverse_delegate) are ones
// only the operator picks.
func TestCommands(t *testing.T) {
	target, _ := buildSchema.Target("linux-elf")
	var ids []string
	for _, c := range buildSchema.TargetCommands(target) {
		ids = append(ids, c.ID)
	}
	if got := strings.Join(ids, " "); got != "sleep exit hello ls download stream_download upload stream_upload rm walk shell switch volumes feedback tunnel reverse_tunnel delegate reverse_delegate" {
		t.Errorf("linux-elf: %s", got)
	}
}

// coreRequest is a request as core would send it: values validated against
// the schema with the defaults filled in.
func coreRequest(t *testing.T) protocol.BuildRequest {
	t.Helper()
	target, _ := buildSchema.Target("linux-elf")
	http, _ := buildSchema.Channel("http")
	r := protocol.BuildRequest{
		BuildID: "5b0c4f6e-8a1d-4c2b-9e7f-3d2a1b0c9e8f", Target: "linux-elf", Platform: "linux/amd64",
		SchemaVersion: buildSchema.Version,
		Values: protocol.ResolveFieldValues(buildSchema.TargetFields(target), map[string]any{
			"kill_date": "2027-01-31T00:00:00Z", "punches": []any{"jab", "hook"}, "badge": "badge-1",
		}),
		// Core sends the target's default and required commands when the
		// operator picked none.
		Commands: buildSchema.DefaultCommands(target),
		Connections: []protocol.BuildConnection{{
			Endpoint: protocol.ChannelEndpointExport{
				Format: protocol.ChannelEndpointExportV2, Name: "edge-eu", Kind: protocol.KindReverseCall,
				Channel: protocol.ExportChannel{ModuleName: "http", SchemaVersion: 3},
				Params:  map[string]any{"base_url": "https://edge-eu.example.com"},
			},
			Values: protocol.ResolveFieldValues(http.KindFields(protocol.KindReverseCall), map[string]any{"path_prefix": "/cdn/assets/"}),
		}},
		Files: map[string]protocol.BuildFile{"badge-1": {
			ID: "badge-1", Name: "badge.png", Size: 3, SHA256: strings.Repeat("ab", 32), MediaType: "image/png",
			URL: "/api/factory/builds/b/files/badge-1?expires=1&sig=badge-link",
		}},
		Crypto: &protocol.BuildCrypto{
			Suite: protocol.CryptoAES256CBCHMACSHA256,
			// Two 32-byte keys, as core makes them.
			Material: json.RawMessage(`{"enc_key":"AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE=","mac_key":"AgICAgICAgICAgICAgICAgICAgICAgICAgICAgICAgI="}`),
		},
		Artifact: protocol.ArtifactUpload{UploadURL: "/api/factory/builds/b/artifact?expires=1&sig=artifact-link", MaxSize: 1 << 20},
		Deadline: time.Now().Add(time.Minute),
	}
	r, err := withConfig(r)
	if err != nil {
		t.Fatalf("the request's minion config: %v", err)
	}
	if p := protocol.ValidateBuildRequest(buildSchema, r); p != nil {
		t.Fatalf("a request core would send does not fit BOB's schema: %v", p)
	}
	return r
}

// What the minion reads of a build: core's config, every value by number,
// the badge (a file) and the build's links left out.
func TestMinionConfigOfABuild(t *testing.T) {
	req := coreRequest(t)
	c, err := protocol.ReadMinionConfig(req.MinionConfig)
	if err != nil {
		t.Fatal(err)
	}
	num := func(key string) uint64 {
		i := slices.IndexFunc(buildSchema.Fields, func(f protocol.FieldSpec) bool { return f.Key == key })
		return uint64(buildSchema.Fields[i].Num)
	}
	if v, _ := protocol.ArgString(c.Values, num("greeting")); v != "hello from BOB" {
		t.Errorf("greeting %q", v)
	}
	if v, _ := protocol.ArgInt(c.Values, num("kill_date")); v != time.Date(2027, 1, 31, 0, 0, 0, 0, time.UTC).Unix() {
		t.Errorf("kill date %d", v)
	}
	if v, _ := protocol.ArgStrings(c.Values, num("punches")); !reflect.DeepEqual(v, []string{"jab", "hook"}) {
		t.Errorf("punches %v", v)
	}
	if len(c.Values) != 3 {
		t.Errorf("values %v: the badge is BOB's alone", c.Values)
	}
	if !reflect.DeepEqual(c.Ops, []uint64{1, 2, 3, 4, 12, 13}) || c.Crypto == nil || c.Crypto.Suite != protocol.CryptoAES256CBCHMACSHA256 {
		t.Errorf("ops %v, crypto %+v", c.Ops, c.Crypto)
	}
	http, _ := buildSchema.Channel("http")
	conn := c.Connections[0]
	if conn.Channel != uint64(http.Num) || conn.Kind != protocol.KindNumReverseCall || conn.Version != 3 {
		t.Errorf("connection %+v", conn)
	}
	if v, _ := protocol.ArgString(conn.Values, 2); v != "/cdn/assets/" { // path_prefix
		t.Errorf("path prefix %q", v)
	}
	// Nor names: the connection's endpoint has none in it at all (its base
	// URL, a value, says edge-eu).
	for _, s := range []string{"badge", "link", "path_prefix", "greeting", "base_url"} {
		if strings.Contains(string(req.MinionConfig), s) {
			t.Errorf("the config carries %q", s)
		}
	}
}

// The build's compression goes into the minion as the request says it: the
// algorithm, or none when the operator turned it off.
func TestMinionConfigCompression(t *testing.T) {
	if !slices.Contains(buildSchema.Compression, protocol.CompressionDeflate) {
		t.Fatalf("the schema declares %v", buildSchema.Compression)
	}
	req := coreRequest(t)
	if c, _ := protocol.ReadMinionConfig(req.MinionConfig); c.Compression != 0 {
		t.Fatalf("compression %d from a request without it", c.Compression)
	}
	req.Compression = protocol.CompressionDeflate
	req, err := withConfig(req)
	if err != nil {
		t.Fatal(err)
	}
	if c, _ := protocol.ReadMinionConfig(req.MinionConfig); c.Compression != protocol.PackDeflate {
		t.Fatalf("compression %d", c.Compression)
	}
}

// windows-exe builds for its three platforms, and refuses any other.
func TestWindowsEXE(t *testing.T) {
	r := coreRequest(t)
	r.Target = "windows-exe"
	for _, platform := range []string{"windows/amd64", "windows/386", "windows/arm64"} {
		r.Platform = platform
		if p := protocol.ValidateBuildRequest(buildSchema, r); p != nil {
			t.Fatalf("windows-exe for %s: %v", platform, p)
		}
	}
	r.Platform = "linux/amd64"
	if p := protocol.ValidateBuildRequest(buildSchema, r); p["platform"] == "" {
		t.Fatalf("windows-exe for linux/amd64: %v", p)
	}
	// Every command is the target's: the shell is cmd.exe there.
	target, _ := buildSchema.Target("windows-exe")
	if got, all := len(buildSchema.TargetCommands(target)), len(buildSchema.Commands); got != all {
		t.Fatalf("windows-exe has %d commands of %d", got, all)
	}
}

// linux-elf builds for its two platforms, and refuses any other.
func TestLinuxELF(t *testing.T) {
	r := coreRequest(t)
	r.Target, r.Platform = "linux-elf", "linux/386"
	if p := protocol.ValidateBuildRequest(buildSchema, r); p != nil {
		t.Fatalf("linux-elf for linux/386: %v", p)
	}
	r.Platform = "any/any"
	if p := protocol.ValidateBuildRequest(buildSchema, r); p["platform"] == "" {
		t.Fatalf("linux-elf for any/any: %v", p)
	}
}
