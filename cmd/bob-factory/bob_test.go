package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/logoscore/logos-golang-factory-core/pkg/factorytest"
	"github.com/logoscore/logos-golang-protocol/protocol"
)

// TestBuildDummyMinion runs BOB's builder in process, through the same checks
// and job as under factory.Run.
func TestBuildDummyMinion(t *testing.T) {
	st := factorytest.NewStore(t)
	badge := st.File("badge-1", "badge.png", "image/png", []byte("\x89PNG fake"))
	req := coreRequest(t)
	req.Artifact = protocol.ArtifactUpload{} // filled in by Build
	req.Files = map[string]protocol.BuildFile{badge.ID: badge}
	req.Values["badge"] = badge.ID

	out := factorytest.Run(t, buildSchema, &bob{builtBy: "bob bob-test test"}, st, req)
	res, artifact := out.Result, out.Artifact
	if res.Status != protocol.BuildSucceeded || res.Artifact.Filename != "bob-5b0c4f6e.json" ||
		res.Artifact.MediaType != "application/json" {
		t.Fatalf("result %+v (error %+v)", res, res.Error)
	}
	var m dummyMinion
	if err := json.Unmarshal(artifact, &m); err != nil {
		t.Fatalf("artifact %s: %v", artifact, err)
	}
	if m.BuildID != req.BuildID || m.Badge == nil || m.Badge.SHA256 != badge.SHA256 || m.BuiltBy != "bob bob-test test" ||
		m.Connections[0].Values["path_prefix"] != "/cdn/assets/" || strings.Join(m.Commands, " ") != "sleep exit hello ls" {
		t.Fatalf("minion %+v", m)
	}
	if strings.Contains(string(artifact), badge.URL) || !strings.Contains(res.Log, "dummy minion of build 5b0c4f6e") {
		t.Fatalf("artifact or log: %s\nlog: %s", artifact, res.Log)
	}
	// The plan core got, and the step the build reported last.
	if ids := stepIDs(out.Steps); ids != "badge render upload" {
		t.Fatalf("plan %s", ids)
	}
	if n := len(out.Progress); n == 0 || out.Progress[n-1].Step != "upload" {
		t.Fatalf("progress %+v", out.Progress)
	}
}

func TestPlanFollowsTheBuild(t *testing.T) {
	b := &bob{}
	if ids := stepIDs(b.Steps(noBadgeRequest(t))); ids != "render upload" {
		t.Errorf("no badge, no delay: %s", ids)
	}
	b.settings.Store(&settings{delay: time.Second})
	withBadge := noBadgeRequest(t)
	withBadge.Values["badge"] = "badge-1"
	if ids := stepIDs(b.Steps(withBadge)); ids != "badge punches render upload" {
		t.Errorf("a badge and a delay: %s", ids)
	}
	if err := protocol.ValidateBuildSteps(b.Steps(withBadge)); err != nil {
		t.Errorf("an invalid plan: %v", err)
	}
}

func stepIDs(steps []protocol.BuildStep) string {
	ids := make([]string, len(steps))
	for i, s := range steps {
		ids[i] = s.ID
	}
	return strings.Join(ids, " ")
}

func TestBuildRefusesAWrongBadge(t *testing.T) {
	st := factorytest.NewStore(t)
	badge := st.File("badge-1", "badge.png", "image/png", []byte("\x89PNG fake"))
	badge.Size++ // the download will not check out
	req := coreRequest(t)
	req.Artifact = protocol.ArtifactUpload{}
	req.Files = map[string]protocol.BuildFile{badge.ID: badge}
	req.Values["badge"] = badge.ID

	res, artifact := factorytest.Build(t, buildSchema, &bob{}, st, req)
	if res.Status != protocol.BuildFailed || res.Error.Code != "badge_unreadable" || artifact != nil {
		t.Fatalf("result %+v (error %+v)", res, res.Error)
	}
}

// noBadgeRequest is a request without a badge, for builds that need no files.
func noBadgeRequest(t *testing.T) protocol.BuildRequest {
	t.Helper()
	req := coreRequest(t)
	req.Artifact = protocol.ArtifactUpload{}
	req.Files = nil
	delete(req.Values, "badge")
	return req
}

func TestBuildTakesPunchesFirst(t *testing.T) {
	b := &bob{}
	b.settings.Store(&settings{delay: 30 * time.Millisecond})
	start := time.Now()
	res, artifact := factorytest.Build(t, buildSchema, b, factorytest.NewStore(t), noBadgeRequest(t))
	if res.Status != protocol.BuildSucceeded || artifact == nil {
		t.Fatalf("result %+v (error %+v)", res, res.Error)
	}
	if took := time.Since(start); took < 30*time.Millisecond || !strings.Contains(res.Log, "taking punches for 30ms") {
		t.Fatalf("took %s, log: %s", took, res.Log)
	}
}

func TestBuildFailsWhenTold(t *testing.T) {
	b := &bob{}
	b.settings.Store(&settings{fail: true})
	res, artifact := factorytest.Build(t, buildSchema, b, factorytest.NewStore(t), noBadgeRequest(t))
	if res.Status != protocol.BuildFailed || res.Error.Code != "knocked_out" || artifact != nil {
		t.Fatalf("result %+v (error %+v)", res, res.Error)
	}
}

func TestSleepEndsWithItsContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	if err := sleep(ctx, time.Hour); !errors.Is(err, context.Canceled) || time.Since(start) > time.Second {
		t.Fatalf("sleep on a cancelled context: %v after %s", err, time.Since(start))
	}
	if err := sleep(context.Background(), time.Millisecond); err != nil {
		t.Fatalf("sleep: %v", err)
	}
}
