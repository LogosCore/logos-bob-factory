package main

import (
	"bytes"
	"context"
	"encoding/json"
	"sync/atomic"
	"time"

	"github.com/logoscore/logos-golang-factory-core/pkg/factory"
	"github.com/logoscore/logos-golang-protocol/protocol"
)

// buildSchema is what BOB builds: one dummy target whose artifact describes
// the minion as JSON. It declares one field of each kind a build schema adds
// (datetime, enum_list, file), and HTTP connection parameters, so the build
// flow can be worked out end to end before a real factory exists.
var buildSchema = protocol.BuildSchema{
	SchemaVersion: protocol.BuildSchemaV1,
	Version:       1,
	Channels: []protocol.BuildChannel{{
		ChannelVersions: protocol.ChannelVersions{ModuleName: "http", Versions: []int{2}},
		Fields: []protocol.FieldSpec{
			{
				Key: "method", Type: protocol.FieldEnum, Label: "Method", Default: "POST", Group: "HTTP", Order: 1,
				Description: "The HTTP method the minion sends its messages with.",
				Options:     []protocol.EnumOption{{Value: "GET"}, {Value: "POST"}, {Value: "PUT"}},
			},
			{
				Key: "path_prefix", Type: protocol.FieldString, Label: "Path prefix", Default: "/", Group: "HTTP", Order: 2,
				Description: "Every request path starts with it, e.g. /cdn/assets/ when the front forwards only that path.",
				Pattern:     `^/[A-Za-z0-9._~!$&'()*+,;=:@%/-]*$`, MaxLength: 256,
			},
		},
	}},
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
	Targets: []protocol.BuildTarget{{
		ID: "dummy", Label: "Dummy minion", Format: "json", Platforms: []string{"any/any"},
		Description: "A JSON description of the minion: what it would be built with, and nothing to run.",
	}},
}

// dummyMinion is BOB's artifact.
type dummyMinion struct {
	Format      string                     `json:"format"`
	MinionID    string                     `json:"minion_id"`
	Target      string                     `json:"target"`
	Platform    string                     `json:"platform"`
	Greeting    string                     `json:"greeting"`
	KillDate    string                     `json:"kill_date,omitempty"`
	Punches     []string                   `json:"punches,omitempty"`
	Badge       *protocol.BuildFile        `json:"badge,omitempty"`
	Connections []protocol.BuildConnection `json:"connections"`
	BuiltBy     string                     `json:"built_by"`
	BuiltAt     time.Time                  `json:"built_at"`
}

// bob builds dummy minions, the way its management says (management.go).
type bob struct {
	builtBy  string // "bob <instance> <version>"
	settings atomic.Pointer[settings]
}

// settings are BOB's own properties that shape its builds.
type settings struct {
	delay time.Duration // build_delay
	fail  bool          // fail_builds
}

// current is the settings a build starts with: the defaults until management
// applies any.
func (b *bob) current() settings {
	if s := b.settings.Load(); s != nil {
		return *s
	}
	return settings{}
}

func (b *bob) Build(ctx context.Context, job *factory.Job) (factory.Result, error) {
	s := b.current()
	var badge *protocol.BuildFile
	if f, ok := job.File("badge"); ok {
		job.Progress("badge", "checking the badge", 20)
		if _, err := job.ReadFile(ctx, f); err != nil {
			return factory.Result{}, factory.Fail("badge_unreadable", err.Error())
		}
		badge = &f
	}
	if s.delay > 0 {
		job.Progress("punches", "taking punches for "+s.delay.String(), 30)
		job.Logf("taking punches for %s (build_delay)", s.delay)
		if err := sleep(ctx, s.delay); err != nil {
			return factory.Result{}, err
		}
	}
	if s.fail {
		return factory.Result{}, factory.Fail("knocked_out", "BOB is set to fail every build (fail_builds)")
	}
	job.Progress("render", "describing the minion", 50)
	m := describe(job.Request, badge, b.builtBy, time.Now().UTC())
	body, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return factory.Result{}, err
	}
	job.Logf("dummy minion %s for %s with %d connection(s)", m.MinionID, m.Platform, len(m.Connections))

	job.Progress("upload", "uploading the artifact", 80)
	name := "bob-" + m.MinionID[:8] + ".json"
	if err := job.UploadArtifact(ctx, name, "application/json", bytes.NewReader(body)); err != nil {
		return factory.Result{}, err
	}
	return factory.Result{
		Capabilities: []string{"hello"},
		Warnings:     []string{"a dummy minion: it describes a minion and runs nothing"},
	}, nil
}

// sleep waits for d, or until ctx ends.
func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
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
		MinionID:    req.MinionID,
		Target:      req.Target,
		Platform:    req.Platform,
		Greeting:    text("greeting"),
		KillDate:    text("kill_date"),
		Punches:     punches,
		Badge:       badge,
		Connections: req.Connections,
		BuiltBy:     builtBy,
		BuiltAt:     now,
	}
}
