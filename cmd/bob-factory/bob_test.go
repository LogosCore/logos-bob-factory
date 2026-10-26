package main

import (
	"encoding/json"
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
	req.BuildID, req.Artifact = "", protocol.ArtifactUpload{} // filled in by Build
	req.Files = map[string]protocol.BuildFile{badge.ID: badge}
	req.Values["badge"] = badge.ID

	res, artifact := factorytest.Build(t, buildSchema, bob{builtBy: "bob bob-test test"}, st, req)
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

	res, artifact := factorytest.Build(t, buildSchema, bob{}, st, req)
	if res.Status != protocol.BuildFailed || res.Error.Code != "badge_unreadable" || artifact != nil {
		t.Fatalf("result %+v (error %+v)", res, res.Error)
	}
}
