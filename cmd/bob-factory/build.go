package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"slices"
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
	SchemaVersion: protocol.BuildSchemaV2,
	Version:       2,
	Channels: []protocol.BuildChannel{{
		// Requests first (a build that names no kind polls), and a WebSocket
		// core pushes tasks over.
		ChannelVersions: protocol.ChannelVersions{ModuleName: "http", Versions: []int{2},
			Kinds: []string{protocol.KindReverseCall, protocol.KindReversePersistent}},
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
			ID: "sleep", Op: 1, Label: "Sleep", Required: true,
			Description: "Changes how long the minion waits between check-ins.",
			Params: []protocol.FieldSpec{
				{Key: "interval", Num: 1, Type: protocol.FieldDuration, Label: "Interval", Required: true},
				{
					Key: "jitter", Num: 2, Type: protocol.FieldInt, Label: "Jitter", Unit: "%", Min: 0, Max: 100, Default: 10,
					Description: "How far each wait may stray from the interval.",
				},
			},
		},
		{ID: "exit", Op: 2, Label: "Exit", Required: true, Description: "Stops the minion for good."},
		{ID: "hello", Op: 3, Label: "Say hello", Default: true, Description: "Answers with the build's greeting."},
		{
			ID: "ls", Op: 4, Label: "List a directory", Default: true,
			Result: protocol.ResultDirListing, Action: protocol.ActionListDir,
			Params: []protocol.FieldSpec{{Key: "path", Num: 1, Type: protocol.FieldString, Label: "Path", Default: "."}},
		},
		{
			ID: "download", Op: 5, Label: "Download a file",
			Description: "Sends a file from the minion's host to core.",
			Result:      protocol.ResultFileDownload, Action: protocol.ActionDownloadFile,
			Params: []protocol.FieldSpec{{Key: "path", Num: 1, Type: protocol.FieldString, Label: "Path", Required: true}},
		},
		{
			ID: "stream_download", Op: 6, Label: "Stream a file",
			Description: "Streams a file of any size from the minion's host to whoever downloads it; core keeps none of it.",
			Result:      protocol.ResultFileStream, Action: protocol.ActionStreamFile,
			Params: []protocol.FieldSpec{{Key: "path", Num: 1, Type: protocol.FieldString, Label: "Path", Required: true}},
		},
		{
			ID: "upload", Op: 7, Label: "Upload a file",
			Description: "Puts a file from core on the minion's host.",
			Result:      protocol.ResultFileUpload, Action: protocol.ActionUploadFile,
			Params: []protocol.FieldSpec{
				{Key: "file", Num: 1, Type: protocol.FieldFile, Label: "File", Required: true, Order: 1},
				{Key: "path", Num: 2, Type: protocol.FieldString, Label: "Path", Required: true, Order: 2,
					Description: "Where it goes on the host; a folder takes it under its own name."},
				{Key: "overwrite", Num: 3, Type: protocol.FieldBool, Label: "Overwrite", Default: false, Order: 3,
					Description: "Replace a file already there."},
			},
		},
		{
			ID: "stream_upload", Op: 8, Label: "Stream a file up",
			Description: "Puts a file of any size on the minion's host, streamed as it is uploaded; core keeps none of it.",
			Result:      protocol.ResultFileUpload, Action: protocol.ActionUploadStream,
			Params: []protocol.FieldSpec{
				{Key: "stream", Num: 1, Type: protocol.FieldStream, Label: "File", Order: 1,
					Description: "Picked in the browser, it goes once the task is queued. Left out, upload it with curl from the task's view."},
				{Key: "path", Num: 2, Type: protocol.FieldString, Label: "Path", Required: true, Order: 2,
					Description: "Where it goes on the host. A folder takes a picked file under its own name; with curl, name the file."},
				{Key: "overwrite", Num: 3, Type: protocol.FieldBool, Label: "Overwrite", Default: false, Order: 3,
					Description: "Replace a file already there."},
			},
		},
		{
			ID: "rm", Op: 9, Label: "Delete a file or folder",
			Description: "Removes a file, a link (not what it points to) or a folder on the minion's host; never a filesystem's root.",
			Action:      protocol.ActionDeletePath,
			Params: []protocol.FieldSpec{
				{Key: "path", Num: 1, Type: protocol.FieldString, Label: "Path", Required: true, Order: 1},
				{Key: "recursive", Num: 2, Type: protocol.FieldBool, Label: "With what is in it", Default: false, Order: 2,
					Description: "A folder that is not empty goes with everything in it."},
			},
		},
		{
			ID: "walk", Op: 10, Label: "Walk a directory",
			Description: "Lists a directory and the folders under it, breadth first, into core's file browser as it goes; links are not followed.",
			Action:      protocol.ActionWalkDir,
			Result:      protocol.ResultDirWalk,
			Params: []protocol.FieldSpec{
				{Key: "path", Num: 1, Type: protocol.FieldString, Label: "Path", Required: true, Order: 1},
				{Key: "depth", Num: 2, Type: protocol.FieldInt, Label: "Depth", Default: 5, Order: 2,
					Description: "How many levels of folders under it to go down; 0 for all."},
				{Key: "max_folders", Num: 3, Type: protocol.FieldInt, Label: "At most", Default: 10000, Order: 3,
					Description: "Folders to list before it stops; 0 for no limit (stop it from its task)."},
				{Key: "hidden", Num: 4, Type: protocol.FieldBool, Label: "Into hidden folders", Default: true, Order: 4},
				{Key: "one_fs", Num: 5, Type: protocol.FieldBool, Label: "Stay on its file system", Default: true, Order: 5,
					Description: "Folders on another file system (a mount, /proc) are not entered."},
			},
		},
		{
			ID: "shell", Op: 11, Label: "Run a shell command",
			Description: "Runs a command in the system shell and answers with its output.",
			Params: []protocol.FieldSpec{
				{Key: "command", Num: 1, Type: protocol.FieldString, Label: "Command", Required: true},
				{Key: "timeout", Num: 2, Type: protocol.FieldDuration, Label: "Timeout", Default: "30s"},
			},
		},
	},
}

// dummyMinion is BOB's artifact.
type dummyMinion struct {
	Format   string `json:"format"`
	BuildID  string `json:"build_id"`
	Target   string `json:"target"`
	Platform string `json:"platform"`
	// Ops are the numbers of the build's commands (buildSchema's Op):
	// what its tasks name them by.
	Ops         []int                      `json:"ops"`
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
		Ops:         opsOf(req.Commands),
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

// opsOf are the numbers of the commands named, in the schema's order.
func opsOf(ids []string) []int {
	var ops []int
	for _, c := range buildSchema.Commands {
		if slices.Contains(ids, c.ID) {
			ops = append(ops, c.Op)
		}
	}
	return ops
}
