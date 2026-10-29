package main

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/logoscore/logos-golang-module-core/pkg/mgmt"
	"github.com/logoscore/logos-golang-protocol/protocol"
)

// BOB's own management: how its dummy builds behave, its log level, and a
// hello to check actions and the live log tail. factory-core adds the builds
// section (max_concurrent_builds, drain/resume, build counters) next to it.

// logLevels maps the log_level values onto slog levels, most verbose first.
// Each description says what BOB logs at that level.
var logLevels = []struct {
	value, label, description string
	level                     slog.Level
}{
	{"debug", "Debug", "Builds refused while busy or draining, plus everything below.", slog.LevelDebug},
	{"info", "Info", "Builds accepted, finished and reported, hellos and management changes, plus everything below.", slog.LevelInfo},
	{"warn", "Warn", "Build requests that do not fit the schema and results core could not be sent, plus errors.", slog.LevelWarn},
	{"error", "Error", "Errors only.", slog.LevelError},
}

const defaultLogLevel = "info"

func logLevelOptions() []protocol.EnumOption {
	out := make([]protocol.EnumOption, len(logLevels))
	for i, l := range logLevels {
		out[i] = protocol.EnumOption{Value: l.value, Label: l.label, Description: l.description}
	}
	return out
}

// slogLevel returns the slog level for a log_level value; unknown values
// (never valid against the manifest) fall back to info.
func slogLevel(v any) slog.Level {
	for _, l := range logLevels {
		if l.value == v {
			return l.level
		}
	}
	return slog.LevelInfo
}

// manifest is BOB's own management manifest. Its properties come after the
// builds section's max_concurrent_builds (order 1).
func manifest() protocol.ManagementManifest {
	return protocol.ManagementManifest{
		SchemaVersion: protocol.ManagementSchemaV1,
		Properties: []protocol.PropertySpec{
			{FieldSpec: protocol.FieldSpec{
				Key: "build_delay", Type: protocol.FieldDuration, Label: "Build delay",
				Description: "How long each dummy build takes punches before it compiles, so its progress can be watched and it can be cancelled.",
				Group:       "Dummy builds", Order: 2,
				Default: "0s", Min: "0s", Max: "10m",
			}, Apply: protocol.ApplyLive},
			{FieldSpec: protocol.FieldSpec{
				Key: "fail_builds", Type: protocol.FieldBool, Label: "Fail builds",
				Description: "Fail every build with code knocked_out (after the build delay), to see how a failed build looks.",
				Group:       "Dummy builds", Order: 3,
				Default: false,
			}, Apply: protocol.ApplyLive},
			{FieldSpec: protocol.FieldSpec{
				Key: "log_level", Type: protocol.FieldEnum, Label: "Log level",
				Description: "Level of BOB's own output (stderr). The Logs tab has its own level.",
				Group:       "Logging", Order: 4,
				Default: defaultLogLevel,
				Options: logLevelOptions(),
			}, Apply: protocol.ApplyLive},
		},
		Actions: []protocol.ActionSpec{
			{
				Name: "say_hello", Label: "Say hello", Risk: protocol.RiskLow,
				Description: "BOB logs a hello at info level: a quick check of actions and the live log tail.",
			},
		},
	}
}

// newManagement builds BOB's own management server for b. log_level drives
// level, the level of the process's own log output.
func newManagement(b *bob, level *slog.LevelVar) (*mgmt.Server, error) {
	s, err := mgmt.New(manifest())
	if err != nil {
		return nil, err
	}
	s.OnApply(func(ctx context.Context, live map[string]any) error { return b.apply(ctx, level, live) })
	s.Action("say_hello", func(_ context.Context, _ map[string]any, actor *protocol.Actor) (any, error) {
		slog.Info("hello from BOB", "by", actorName(actor))
		return map[string]any{"said": "hello from BOB"}, nil
	})
	return s, nil
}

// apply sets the build settings and the log level from the live values.
// Builds already running keep the settings they started with.
func (b *bob) apply(ctx context.Context, level *slog.LevelVar, live map[string]any) error {
	delay, err := protocol.ParseDuration(live["build_delay"])
	if err != nil {
		return fmt.Errorf("build_delay: %w", err)
	}
	fail, _ := live["fail_builds"].(bool)
	next, lvl := settings{delay: delay, fail: fail}, slogLevel(live["log_level"])
	if b.current() == next && level.Level() == lvl {
		return nil
	}
	b.settings.Store(&next)
	level.Set(lvl)
	slog.Info("management: config applied", "by", actorName(mgmt.ActorFromContext(ctx)),
		"build_delay", delay.String(), "fail_builds", fail, "log_level", lvl.String())
	return nil
}

func actorName(a *protocol.Actor) string {
	switch {
	case a == nil:
		return "core"
	case a.Name != "":
		return fmt.Sprintf("%s %s (%s)", a.Type, a.Name, a.ID)
	default:
		return a.Type + " " + a.ID
	}
}
