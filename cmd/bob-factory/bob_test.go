package main

import (
	"bytes"
	"debug/elf"
	"encoding/json"
	"runtime"
	"slices"
	"strings"
	"testing"

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

	needGo(t)
	out := factorytest.Run(t, buildSchema, &bob{builtBy: "bob bob-test test", minionSrc: testMinionSrc}, st, req)
	res := out.Result
	if res.Status != protocol.BuildSucceeded || res.Artifact.Filename != "bob-5b0c4f6e.bin" ||
		res.Artifact.MediaType != "application/x-executable" {
		t.Fatalf("result %+v (error %+v, log %s)", res, res.Error, res.Log)
	}
	if _, err := elf.NewFile(bytes.NewReader(out.Artifact)); err != nil {
		t.Fatalf("artifact is no ELF: %v", err)
	}
	if runtime.GOOS != "linux" || runtime.GOARCH != "amd64" {
		return // cannot run it here
	}
	artifact := run(t, out.Artifact)
	var m dummyMinion
	if err := json.Unmarshal(artifact, &m); err != nil {
		t.Fatalf("the binary printed %s: %v", artifact, err)
	}
	if m.BuildID != req.BuildID || m.Badge == nil || m.Badge.SHA256 != badge.SHA256 || m.BuiltBy != "bob bob-test test" ||
		m.Connections[0].Values["path_prefix"] != "/cdn/assets/" || !slices.Equal(m.Ops, []int{1, 2, 3, 4, 12}) {
		t.Fatalf("minion %+v", m)
	}
	if strings.Contains(string(artifact), badge.URL) || !strings.Contains(res.Log, "dummy minion of build 5b0c4f6e") {
		t.Fatalf("artifact or log: %s\nlog: %s", artifact, res.Log)
	}
	// The plan core got, and the step the build reported last.
	if ids := stepIDs(out.Steps); ids != "badge compile upload" {
		t.Fatalf("plan %s", ids)
	}
	if n := len(out.Progress); n == 0 || out.Progress[n-1].Step != "upload" {
		t.Fatalf("progress %+v", out.Progress)
	}
}

func TestPlanFollowsTheBuild(t *testing.T) {
	b := &bob{}
	if ids := stepIDs(b.Steps(noBadgeRequest(t))); ids != "compile upload" {
		t.Errorf("no badge: %s", ids)
	}
	withBadge := noBadgeRequest(t)
	withBadge.Values["badge"] = "badge-1"
	if ids := stepIDs(b.Steps(withBadge)); ids != "badge compile upload" {
		t.Errorf("a badge: %s", ids)
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

func TestBuildFailsWhenTold(t *testing.T) {
	b := &bob{}
	b.settings.Store(&settings{fail: true})
	res, artifact := factorytest.Build(t, buildSchema, b, factorytest.NewStore(t), noBadgeRequest(t))
	if res.Status != protocol.BuildFailed || res.Error.Code != "knocked_out" || artifact != nil {
		t.Fatalf("result %+v (error %+v)", res, res.Error)
	}
}
