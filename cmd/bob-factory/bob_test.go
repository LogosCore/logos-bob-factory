package main

import (
	"bytes"
	"debug/elf"
	"debug/pe"
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
	// The exit codes core drew for the build: through factory-core's check of
	// the request, into the minion.
	req.ErrorCodes = []protocol.BuildErrorCode{{Code: 3, ExitCode: 201}, {Code: 4, ExitCode: 87}, {Code: 5, ExitCode: 14}}

	needGo(t)
	out := factorytest.Run(t, buildSchema, &bob{minionSrc: testMinionSrc}, st, req)
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
	// The binary carries core's config as it came.
	if printed := run(t, out.Artifact); !bytes.Equal(printed, req.MinionConfig) {
		t.Fatalf("the binary printed %x, core's config is %x", printed, req.MinionConfig)
	}
	if bytes.Contains(out.Artifact, []byte(badge.URL)) || !strings.Contains(res.Log, "dummy minion of build 5b0c4f6e") ||
		!strings.Contains(res.Log, "exit codes of its own: 3→201, 4→87, 5→14") {
		t.Fatalf("a link in the artifact, or the log: %s", res.Log)
	}
	// The plan core got, and the step the build reported last.
	if ids := stepIDs(out.Steps); ids != "badge compile upload" {
		t.Fatalf("plan %s", ids)
	}
	if n := len(out.Progress); n == 0 || out.Progress[n-1].Step != "upload" {
		t.Fatalf("progress %+v", out.Progress)
	}
	// The live log: every line under the step BOB was on.
	for step, want := range map[string][]string{
		"badge":   {"badge badge.png: 9 bytes, image/png", "badge checked"},
		"compile": {"dummy minion of build 5b0c4f6e", "exit codes of its own: 3→201", "$ GOOS=linux GOARCH=amd64", "compiled a "},
		"upload":  {"uploading bob-5b0c4f6e.bin: ", "uploaded bob-5b0c4f6e.bin"},
	} {
		for _, w := range want {
			if !logged(out.Log, step, w) {
				t.Errorf("no %q in step %s; the log:\n%s", w, step, logLines(out.Log))
			}
		}
	}
}

// logged reports whether a line of the live log in step has text.
func logged(lines []protocol.BuildLogLine, step, text string) bool {
	return slices.ContainsFunc(lines, func(l protocol.BuildLogLine) bool { return l.Step == step && strings.Contains(l.Text, text) })
}

func logLines(lines []protocol.BuildLogLine) string {
	var b strings.Builder
	for _, l := range lines {
		b.WriteString(l.Step + ": " + l.Text + "\n")
	}
	return b.String()
}

// A windows-exe build is the same minion compiled for Windows: an .exe.
func TestBuildWindowsMinion(t *testing.T) {
	st := factorytest.NewStore(t)
	req := coreRequest(t)
	req.Artifact = protocol.ArtifactUpload{}
	req.Target, req.Platform = "windows-exe", "windows/amd64"
	delete(req.Values, "badge")
	req.Files = nil

	needGo(t)
	out := factorytest.Run(t, buildSchema, &bob{minionSrc: testMinionSrc}, st, req)
	res := out.Result
	if res.Status != protocol.BuildSucceeded || res.Artifact.Filename != "bob-5b0c4f6e.exe" ||
		res.Artifact.MediaType != "application/vnd.microsoft.portable-executable" {
		t.Fatalf("result %+v (error %+v, log %s)", res, res.Error, res.Log)
	}
	f, err := pe.NewFile(bytes.NewReader(out.Artifact))
	if err != nil || f.Machine != pe.IMAGE_FILE_MACHINE_AMD64 {
		t.Fatalf("artifact is no amd64 PE: %v", err)
	}
	if !strings.Contains(res.Log, "GOOS=windows GOARCH=amd64") {
		t.Fatalf("log: %s", res.Log)
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

	out := factorytest.Run(t, buildSchema, &bob{}, st, req)
	if res := out.Result; res.Status != protocol.BuildFailed || res.Error.Code != "badge_unreadable" || out.Artifact != nil {
		t.Fatalf("result %+v (error %+v)", res, res.Error)
	}
	if !logged(out.Log, "badge", "the badge does not read") {
		t.Fatalf("the log:\n%s", logLines(out.Log))
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
	out := factorytest.Run(t, buildSchema, b, factorytest.NewStore(t), noBadgeRequest(t))
	if res := out.Result; res.Status != protocol.BuildFailed || res.Error.Code != "knocked_out" || out.Artifact != nil {
		t.Fatalf("result %+v (error %+v)", res, res.Error)
	}
	if !logged(out.Log, "compile", "fail_builds is on") {
		t.Fatalf("the log:\n%s", logLines(out.Log))
	}
}
