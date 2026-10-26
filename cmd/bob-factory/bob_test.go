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
	req.BuildID, req.Artifact = "", protocol.ArtifactUpload{} // filled in by Build
	req.Files = map[string]protocol.BuildFile{badge.ID: badge}
	req.Values["badge"] = badge.ID

	res, artifact := factorytest.Build(t, buildSchema, &bob{builtBy: "bob bob-test test"}, st, req)
	if res.Status != protocol.BuildSucceeded || res.Artifact.Filename != "bob-5b0c4f6e.json" ||
		res.Artifact.MediaType != "application/json" || res.Capabilities[0] != "hello" {
		t.Fatalf("result %+v (error %+v)", res, res.Error)
	}
	var m dummyMinion
	if err := json.Unmarshal(artifact, &m); err != nil {
		t.Fatalf("artifact %s: %v", artifact, err)
	}
	if m.MinionID != req.MinionID || m.Badge == nil || m.Badge.SHA256 != badge.SHA256 || m.BuiltBy != "bob bob-test test" ||
		m.Connections[0].Values["path_prefix"] != "/cdn/assets/" {
		t.Fatalf("minion %+v", m)
	}
	if strings.Contains(string(artifact), badge.URL) || !strings.Contains(res.Log, "dummy minion 5b0c4f6e") {
		t.Fatalf("artifact or log: %s\nlog: %s", artifact, res.Log)
	}
}

func TestBuildRefusesAWrongBadge(t *testing.T) {
	st := factorytest.NewStore(t)
	badge := st.File("badge-1", "badge.png", "image/png", []byte("\x89PNG fake"))
	badge.Size++ // the download will not check out
	req := coreRequest(t)
	req.BuildID, req.Artifact = "", protocol.ArtifactUpload{}
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
	req.BuildID, req.Artifact = "", protocol.ArtifactUpload{}
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
