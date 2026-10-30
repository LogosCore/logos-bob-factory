package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"time"

	"github.com/logoscore/logos-golang-factory-core/pkg/factory"
	"github.com/logoscore/logos-golang-protocol/protocol"
)

// buildSchema is what BOB builds: dummy minions, Linux binaries that print a
// JSON description of the minion and do nothing else. It declares one field of
// each kind a build schema adds (datetime, enum_list, file), HTTP connection
// parameters, and commands of each kind (required, picked by default, picked
// by the operator), so the build flow can be worked out end to end before a
// real factory exists. Version 2: the JSON-only targets dummy and dummy-lite
// are gone.
var buildSchema = protocol.BuildSchema{
	SchemaVersion: protocol.BuildSchemaV1,
	Version:       2,
	Channels: []protocol.BuildChannel{{
		ChannelVersions: protocol.ChannelVersions{ModuleName: "http", Versions: []int{2}},
		Fields: []protocol.FieldSpec{
			{
				Key: "method", Type: protocol.FieldEnum, Label: "Method", Default: "POST", Group: "HTTP", Order: 1,
				Description: "The HTTP method the minion sends its messages with.",
				Options:     []protocol.EnumOption{{Value: "GET"}, {Value: "POST"}, {Value: "PUT"}},
			},
			{
				Key: "path_prefix", Type: protocol.FieldString, Format: protocol.FormatPath, Label: "Path prefix", Default: "/",
				Group: "HTTP", Order: 2,
				Description: "Every request path starts with it, e.g. /cdn/assets/ when the front forwards only that path.",
				Pattern:     `^/[A-Za-z0-9._~!$&'()*+,;=:@%/-]*$`, MaxLength: 256,
			},
		},
	}},
	// Its minions encrypt with AES-256-CBC and HMAC-SHA256; core generates
	// each one's keys and BOB writes them into the artifact.
	Crypto: protocol.CryptoAES256CBCHMACSHA256,
	// Its minions claim a persisted session when built with one.
	PersistedSessions: true,
	Fields: []protocol.FieldSpec{
		{
			Key: "greeting", Type: protocol.FieldString, Label: "Greeting", Default: "hello from BOB", MaxLength: 200,
			Order: 1, Description: "What the dummy minion says.",
		},
		{
			Key: "kill_date", Type: protocol.FieldDatetime, Label: "Kill date", Group: "Limits", Order: 2,
			Description: "When the minion stops for good.",
		},
		{
			Key: "punches", Type: protocol.FieldEnumList, Label: "Punches it takes", Order: 3,
			Options: []protocol.EnumOption{{Value: "jab"}, {Value: "cross"}, {Value: "hook"}, {Value: "uppercut"}},
		},
		{
			Key: "badge", Type: protocol.FieldFile, Label: "Badge", Order: 4, Accept: []string{"image/*"}, MaxSize: 256 << 10,
			Description: "An image the minion would wear. BOB checks it and records its SHA-256.",
		},
	},
	Targets: []protocol.BuildTarget{
		{
			ID: "linux-elf", Label: "Linux binary", Format: "elf", Platforms: []string{"linux/amd64", "linux/386"},
			Description: "A static Linux binary that prints the dummy minion's JSON description, baked in when it is compiled.",
		},
	},
	Commands: []protocol.BuildCommand{
		{
			ID: "sleep", Label: "Sleep", Required: true,
			Description: "Changes how long the minion waits between check-ins.",
			Params: []protocol.FieldSpec{
				{Key: "interval", Type: protocol.FieldDuration, Label: "Interval", Required: true},
				{
					Key: "jitter", Type: protocol.FieldInt, Label: "Jitter", Unit: "%", Min: 0, Max: 100, Default: 10,
					Description: "How far each wait may stray from the interval.",
				},
			},
		},
		{ID: "exit", Label: "Exit", Required: true, Description: "Stops the minion for good."},
		{ID: "hello", Label: "Say hello", Default: true, Description: "Answers with the build's greeting."},
		{
			ID: "ls", Label: "List a directory", Default: true,
			Params: []protocol.FieldSpec{{Key: "path", Type: protocol.FieldString, Label: "Path", Default: "."}},
		},
		{
			ID: "download", Label: "Download a file",
			Description: "Sends a file from the minion's host to core.",
			Params:      []protocol.FieldSpec{{Key: "path", Type: protocol.FieldString, Label: "Path", Required: true}},
		},
		{
			ID: "shell", Label: "Run a shell command",
			Description: "Runs a command in the system shell and answers with its output.",
			Params: []protocol.FieldSpec{
				{Key: "command", Type: protocol.FieldString, Label: "Command", Required: true},
				{Key: "timeout", Type: protocol.FieldDuration, Label: "Timeout", Default: "30s"},
			},
		},
	},
}

// dummyMinion is BOB's artifact.
type dummyMinion struct {
	Format      string                     `json:"format"`
	BuildID     string                     `json:"build_id"`
	Target      string                     `json:"target"`
	Platform    string                     `json:"platform"`
	Commands    []string                   `json:"commands"`
	Greeting    string                     `json:"greeting"`
	KillDate    string                     `json:"kill_date,omitempty"`
	Punches     []string                   `json:"punches,omitempty"`
	Badge       *protocol.BuildFile        `json:"badge,omitempty"`
	Connections []protocol.BuildConnection `json:"connections"`
	Crypto      *protocol.BuildCrypto      `json:"crypto,omitempty"`
	// SessionID is the persisted session the minion claims, when the build
	// has one.
	SessionID string    `json:"session_id,omitempty"`
	BuiltBy   string    `json:"built_by"`
	BuiltAt   time.Time `json:"built_at"`
}

// bob builds dummy minions, the way its management says (management.go).
type bob struct {
	builtBy string // "bob <instance> <version>"
	// minionSrc is the minion module's directory, compiled for linux-elf.
	minionSrc string
	settings  atomic.Pointer[settings]
}

// settings are BOB's own properties that shape its builds.
type settings struct {
	fail bool // fail_builds
}

// current is the settings a build starts with: the defaults until management
// applies any.
func (b *bob) current() settings {
	if s := b.settings.Load(); s != nil {
		return *s
	}
	return settings{}
}

// The steps of BOB's builds.
var (
	stepBadge   = protocol.BuildStep{ID: "badge", Label: "Check the badge"}
	stepCompile = protocol.BuildStep{ID: "compile", Label: "Compile the binary"}
	stepUpload  = protocol.BuildStep{ID: "upload", Label: "Upload the artifact"}
)

// Steps is the plan for a build of req: the badge is checked only when there
// is one.
func (b *bob) Steps(req protocol.BuildRequest) []protocol.BuildStep {
	var steps []protocol.BuildStep
	if id, _ := req.Values["badge"].(string); id != "" {
		steps = append(steps, stepBadge)
	}
	return append(steps, stepCompile, stepUpload)
}

func (b *bob) Build(ctx context.Context, job *factory.Job) (factory.Result, error) {
	s := b.current()
	var badge *protocol.BuildFile
	if f, ok := job.File("badge"); ok {
		job.Step(stepBadge.ID, "checking the badge")
		if _, err := job.ReadFile(ctx, f); err != nil {
			return factory.Result{}, factory.Fail("badge_unreadable", err.Error())
		}
		badge = &f
	}
	job.Step(stepCompile.ID, "go build for "+job.Request.Platform)
	if s.fail {
		return factory.Result{}, factory.Fail("knocked_out", "BOB is set to fail every build (fail_builds)")
	}
	m := describe(job.Request, badge, b.builtBy, time.Now().UTC())
	body, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return factory.Result{}, err
	}
	job.Logf("dummy minion of build %s for %s with %d connection(s)", m.BuildID, m.Platform, len(m.Connections))

	out := &lineLog{logf: job.Logf}
	bin, err := compile(ctx, b.minionSrc, m.Target, m.Platform, body, out)
	out.Flush()
	switch {
	case errors.Is(err, errNoToolchain):
		return factory.Result{}, factory.Fail("no_toolchain", err.Error())
	case err != nil && ctx.Err() == nil:
		return factory.Result{}, factory.Fail("compile_failed", "the dummy minion did not compile; see the build log")
	case err != nil:
		return factory.Result{}, err
	}
	job.Logf("compiled a %d-byte binary for %s", len(bin), m.Platform)

	job.Step(stepUpload.ID, "")
	name := "bob-" + m.BuildID[:8] + ".bin"
	if err := job.UploadArtifact(ctx, name, "application/x-executable", bytes.NewReader(bin)); err != nil {
		return factory.Result{}, err
	}
	return factory.Result{
		Warnings: []string{"a dummy minion: it prints its description and does nothing else"},
	}, nil
}

// describe is the dummy minion a request asks for. A badge is recorded
// without its download URL, which only lives as long as the build.
func describe(req protocol.BuildRequest, badge *protocol.BuildFile, builtBy string, now time.Time) dummyMinion {
	if badge != nil {
		b := *badge
		b.URL = ""
		badge = &b
	}
	text := func(key string) string {
		s, _ := req.Values[key].(string)
		return s
	}
	var punches []string
	if list, ok := req.Values["punches"].([]any); ok {
		for _, p := range list {
			if s, ok := p.(string); ok {
				punches = append(punches, s)
			}
		}
	}
	return dummyMinion{
		Format:      "logos.bob-dummy.v1",
		BuildID:     req.BuildID,
		Target:      req.Target,
		Platform:    req.Platform,
		Commands:    req.Commands,
		Greeting:    text("greeting"),
		KillDate:    text("kill_date"),
		Punches:     punches,
		Badge:       badge,
		Connections: req.Connections,
		Crypto:      req.Crypto,
		SessionID:   req.SessionID,
		BuiltBy:     builtBy,
		BuiltAt:     now,
	}
}
