package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/logoscore/logos-golang-factory-core/pkg/factory"
	"github.com/logoscore/logos-golang-module-core/pkg/lifecycle"
	"github.com/logoscore/logos-golang-module-core/pkg/module"
	"github.com/logoscore/logos-golang-protocol/protocol"
)

// TestManagementJoinsTheBuildsSection checks BOB's own manifest next to the
// section factory-core adds: Run refuses a manifest that takes one of its
// keys before it connects, so a run without a broker URL fails only on that.
func TestManagementJoinsTheBuildsSection(t *testing.T) {
	var level slog.LevelVar
	s, err := newManagement(&bob{}, &level)
	if err != nil {
		t.Fatal(err)
	}
	schema := buildSchema
	err = factory.Run(context.Background(), factory.Config{
		Config: module.Config{Identity: lifecycle.Identity{ModuleName: "bob", Instance: "bob-test"}, Management: s},
		Build:  &schema, Builder: &bob{},
	})
	if err == nil || !strings.Contains(err.Error(), "AMQPURL") {
		t.Fatalf("Run: %v", err)
	}
	m := s.Manifest()
	keys := make([]string, len(m.Properties))
	for i, p := range m.Properties {
		keys[i] = p.Key
	}
	if strings.Join(keys, ",") != "fail_builds,log_level,"+factory.PropertyMaxConcurrentBuilds ||
		len(m.Actions) != 3 || len(m.Status) == 0 {
		t.Fatalf("manifest = %+v", m)
	}
}

func TestApply(t *testing.T) {
	var level slog.LevelVar
	b := &bob{}
	s, err := newManagement(b, &level)
	if err != nil {
		t.Fatal(err)
	}
	rep := s.ApplyConfig(context.Background(), protocol.ManagementConfig{Revision: 1, Values: map[string]any{
		"fail_builds": true, "log_level": "debug",
	}})
	if len(rep.Rejected) != 0 {
		t.Fatalf("rejected %v", rep.Rejected)
	}
	if got := b.current(); got != (settings{fail: true}) || level.Level() != slog.LevelDebug {
		t.Fatalf("settings %+v, level %v", got, level.Level())
	}

	// Values core no longer holds return to the defaults.
	s.ApplyConfig(context.Background(), protocol.ManagementConfig{Revision: 2})
	if got := b.current(); got != (settings{}) || level.Level() != slog.LevelInfo {
		t.Fatalf("settings %+v, level %v", got, level.Level())
	}

}

func TestSayHello(t *testing.T) {
	var out bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&out, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	var level slog.LevelVar
	s, err := newManagement(&bob{}, &level)
	if err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(protocol.ActionInvokeRequest{
		InvocationID: "i1", Action: "say_hello", Actor: &protocol.Actor{Type: "user", ID: "u1", Name: "ana"},
	})
	rep := s.Handle(context.Background(), protocol.Envelope{
		MessageID: "m1", CorrelationID: "m1", Type: protocol.TypeMgmtActionInvoke, Version: protocol.EnvelopeVersionV1,
		Source: protocol.Source{Service: protocol.ServiceCore, Instance: "core-1"}, Payload: payload,
	})
	if rep.Status != protocol.StatusOK {
		t.Fatalf("say_hello: %+v", rep.Error)
	}
	if !strings.Contains(out.String(), `"msg":"hello from BOB","by":"user ana (u1)"`) {
		t.Fatalf("log: %s", out.String())
	}
}
