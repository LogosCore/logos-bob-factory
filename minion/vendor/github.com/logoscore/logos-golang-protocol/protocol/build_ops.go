package protocol

import (
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// This file defines the build operations. Builds are asynchronous: core sends
// factory.build to a factory instance's rpc_queue and the factory accepts it
// at once, with its plan for the build: the steps it will go through. While
// it works it reports the step it is on to core on logos.core.rpc; when it is
// done it uploads the artifact to a presigned URL and sends
// factory.build.result. Core can stop a build with factory.build.cancel. All
// four operations are request/reply.

// Build operation types.
const (
	TypeFactoryBuild         = "factory.build"          // core → factory
	TypeFactoryBuildCancel   = "factory.build.cancel"   // core → factory
	TypeFactoryBuildProgress = "factory.build.progress" // factory → core
	TypeFactoryBuildResult   = "factory.build.result"   // factory → core
)

// CodeBusy is the factory.build error of an instance that already runs as
// many builds as it can; core tries another instance.
const CodeBusy = "busy"

// ChannelEndpointExportV2 is the format of a channel endpoint export: a
// connection kind's settings. "logos.channel-endpoint.v1" is no longer
// taken.
const ChannelEndpointExportV2 = "logos.channel-endpoint.v2"

// Build operation limits.
const (
	MaxBuildLogBytes          = 64 << 10
	MaxBuildWarnings          = 32
	MaxBuildMessageLength     = 1000 // a progress message, warning or error message
	MaxArtifactFilenameLength = 255
	MaxBuildSteps             = 32
	MaxBuildStepLabelLength   = 64 // characters
)

var (
	buildIDPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	sha256Pattern  = regexp.MustCompile(`^[0-9a-f]{64}$`)
	// slugPattern is what error codes match.
	slugPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{0,62}$`)
	// stepIDPattern is what a build step's id matches: a slug without dots,
	// so it can key a build's steps in a document.
	stepIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)
)

// ChannelEndpointExport is a channel endpoint as a minion's connection
// settings (ChannelEndpointExportV2): the channel, the connection kind, the
// connection schema version the values follow, and the values over that
// kind's defaults.
type ChannelEndpointExport struct {
	Format  string        `json:"format"`
	Name    string        `json:"name"`
	Channel ExportChannel `json:"channel"`
	// Kind is the connection kind (KindReverseCall, ...) the values are
	// for.
	Kind   string         `json:"kind"`
	Params map[string]any `json:"params"`
}

// ExportChannel is the channel kind of an endpoint export.
type ExportChannel struct {
	ModuleName    string `json:"module_name"`
	SchemaVersion int    `json:"schema_version"`
}

// BuildConnection is one connection of a build: the endpoint's export, and
// the factory's connection parameter values for it.
type BuildConnection struct {
	Endpoint ChannelEndpointExport `json:"endpoint"`
	Values   map[string]any        `json:"values,omitempty"`
}

// ConnectionKind is the connection's kind: its endpoint's.
func (c BuildConnection) ConnectionKind() string { return c.Endpoint.Kind }

// BuildCrypto is the payload crypto a minion is built with. The payload
// crypto contract defines the material of each suite.
type BuildCrypto struct {
	Suite    string          `json:"suite"`
	Material json.RawMessage `json:"material,omitempty"`
}

// ArtifactUpload says where a factory uploads the artifact.
type ArtifactUpload struct {
	// UploadURL is a presigned upload URL, valid until the deadline.
	UploadURL string `json:"upload_url"`
	// MaxSize is the largest artifact core accepts, in bytes.
	MaxSize int64 `json:"max_size"`
}

// BuildRequest is the factory.build payload. Key material, secret values and
// files are only for the build: a factory must not log them, keep them after
// the build, or report them in progress or the build log.
type BuildRequest struct {
	// BuildID is a lowercase UUIDv4, the id of the build and of the minion
	// it makes: every build operation names the build by it, and the minion
	// sends it on the data plane. A repeated BuildID is the same build: the
	// factory accepts it again and never builds twice.
	BuildID       string `json:"build_id"`
	Target        string `json:"target"`
	Platform      string `json:"platform"`
	SchemaVersion int    `json:"schema_version"`
	// Values holds every field of the target that has a value or a default.
	// A file field holds the file's id; the file is in Files.
	Values map[string]any `json:"values"`
	// Commands are the ids of the commands the minion is built with: those
	// the operator picked, the target's required ones among them, in the
	// schema's order. The minion has these and no others.
	Commands []string `json:"commands,omitempty"`
	// Connections are the chosen endpoints, in the order the minion tries
	// them.
	Connections []BuildConnection `json:"connections"`
	// Files are the files that values name, by id.
	Files map[string]BuildFile `json:"files,omitempty"`
	// Crypto is set when the factory declares a crypto suite.
	Crypto *BuildCrypto `json:"crypto,omitempty"`
	// SessionID is the persisted session the minion claims in its hello, a
	// lowercase UUID; set only for a factory that declares
	// BuildSchema.PersistedSessions. The factory builds it in.
	SessionID string         `json:"session_id,omitempty"`
	Artifact  ArtifactUpload `json:"artifact"`
	// Deadline is when core stops waiting for the result.
	Deadline time.Time `json:"deadline"`
	Actor    *Actor    `json:"actor,omitempty"`
}

// BuildReply is the factory.build reply payload: the factory accepted the
// build and works on it. A factory that cannot start it replies with an error
// instead: CodeValidationFailed, CodeBusy or CodeUnsupportedVersion.
type BuildReply struct {
	BuildID  string `json:"build_id"`
	Accepted bool   `json:"accepted"`
	// Steps is the factory's plan for this build: the steps it goes through,
	// in order, for these parameters. Progress names the step it is on, and
	// the app shows the plan as the build's pipeline.
	Steps []BuildStep `json:"steps"`
}

// BuildStep is one step of a build's plan.
type BuildStep struct {
	// ID names the step in progress reports: a slug without dots,
	// ^[a-z0-9][a-z0-9_-]{0,62}$.
	ID string `json:"id"`
	// Label is what the operator sees, e.g. "Compile": 1 to
	// MaxBuildStepLabelLength characters on one line.
	Label string `json:"label"`
}

// Validate checks a reply: the build id, and a plan of 1 to MaxBuildSteps
// steps with unique, valid ids and labels.
func (r BuildReply) Validate() error {
	if err := validateBuildID(r.BuildID); err != nil {
		return err
	}
	if !r.Accepted {
		return fmt.Errorf("accepted must be true: a factory that does not take a build replies with an error")
	}
	return ValidateBuildSteps(r.Steps)
}

// ValidateBuildSteps checks a build's plan: 1 to MaxBuildSteps steps, ids
// matching the step pattern and unique, labels of 1 to
// MaxBuildStepLabelLength characters without control characters.
func ValidateBuildSteps(steps []BuildStep) error {
	if len(steps) == 0 {
		return fmt.Errorf("steps: a plan has at least one step")
	}
	if len(steps) > MaxBuildSteps {
		return fmt.Errorf("steps: too many (%d > %d)", len(steps), MaxBuildSteps)
	}
	seen := make(map[string]bool, len(steps))
	for i, st := range steps {
		switch n := utf8.RuneCountInString(st.Label); {
		case !stepIDPattern.MatchString(st.ID):
			return fmt.Errorf("steps[%d]: id must match %s", i, stepIDPattern)
		case seen[st.ID]:
			return fmt.Errorf("steps[%d]: id %q is used twice", i, st.ID)
		case strings.TrimSpace(st.Label) == "" || n > MaxBuildStepLabelLength:
			return fmt.Errorf("steps[%d]: label must be 1..%d characters", i, MaxBuildStepLabelLength)
		case !utf8.ValidString(st.Label) || strings.IndexFunc(st.Label, unicode.IsControl) >= 0:
			return fmt.Errorf("steps[%d]: label must be one line of text", i)
		}
		seen[st.ID] = true
	}
	return nil
}

// BuildCancelRequest is the factory.build.cancel payload. The factory stops
// the build, throws away what it produced and sends no result.
type BuildCancelRequest struct {
	BuildID string `json:"build_id"`
}

// BuildCancelReply is the factory.build.cancel reply payload. Cancelled is
// true also for a build the factory does not know.
type BuildCancelReply struct {
	BuildID   string `json:"build_id"`
	Cancelled bool   `json:"cancelled"`
}

// BuildProgress is the factory.build.progress payload: the factory is on a
// step of its plan. Progress is best effort: a factory does not send a lost
// one again, and core takes the steps before the one reported as done.
type BuildProgress struct {
	BuildID string `json:"build_id"`
	// Step is the id of the step of the plan the factory is on.
	Step string `json:"step"`
	// Message is shown to the operator with the step, e.g. "compiling 3 of
	// 10"; a report without one clears the step's last.
	Message string `json:"message,omitempty"`
}

// BuildProgressReply is the factory.build.progress reply payload. Core replies
// CodeNotFound instead when it no longer waits for the build, and the factory
// stops it.
type BuildProgressReply struct {
	BuildID string `json:"build_id"`
	Ack     bool   `json:"ack"`
}

// BuildStatus is how a build ended.
type BuildStatus string

const (
	BuildSucceeded BuildStatus = "succeeded"
	BuildFailed    BuildStatus = "failed"
)

// BuildArtifact describes the artifact a factory uploaded.
type BuildArtifact struct {
	Filename  string `json:"filename"`
	Size      int64  `json:"size"`
	SHA256    string `json:"sha256"` // lowercase hex
	MediaType string `json:"media_type,omitempty"`
}

// BuildError is why a build failed. Code is the factory's own, e.g.
// "compile_failed".
type BuildError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// BuildResult is the factory.build.result payload. The artifact is uploaded
// before the result is sent. A factory sends the result again until core
// replies or the deadline passes.
type BuildResult struct {
	BuildID  string         `json:"build_id"`
	Status   BuildStatus    `json:"status"`
	Artifact *BuildArtifact `json:"artifact,omitempty"`
	Warnings []string       `json:"warnings,omitempty"`
	// Log is the build output: its last MaxBuildLogBytes.
	Log   string      `json:"log,omitempty"`
	Error *BuildError `json:"error,omitempty"`
}

// BuildResultReply is the factory.build.result reply payload, the same for a
// repeated result. Core replies CodeNotFound instead when it no longer waits
// for the build, and the factory drops the result.
type BuildResultReply struct {
	BuildID  string `json:"build_id"`
	Recorded bool   `json:"recorded"`
}

// ValidateBuildRequest checks a factory.build request against the schema it
// follows. Core runs it before sending; the factory runs it again before
// building, since only it knows its build-time constraints beyond these.
//
// It returns the problems keyed by where they are (nil when there are none):
// "values.<key>", "commands[<i>]", "connections[<i>]",
// "connections[<i>].values.<key>", "files.<id>", or the request member
// ("build_id", "target", "commands", ...). The
// endpoint params themselves are not checked: core validated them against the
// channel's schema when it exported them.
func ValidateBuildRequest(s BuildSchema, r BuildRequest) map[string]string {
	var problems map[string]string
	add := func(key, format string, args ...any) {
		if problems == nil {
			problems = map[string]string{}
		}
		problems[key] = fmt.Sprintf(format, args...)
	}

	if !buildIDPattern.MatchString(r.BuildID) {
		add("build_id", "must be a lowercase UUIDv4")
	}
	if r.SchemaVersion != s.Version {
		add("schema_version", "the factory's build schema is version %d", s.Version)
	}
	if r.SessionID != "" {
		switch {
		case !s.PersistedSessions:
			add("session_id", "the factory's minions do not claim persisted sessions")
		case !buildIDPattern.MatchString(r.SessionID):
			add("session_id", "must be a lowercase UUIDv4")
		}
	}

	for id, f := range r.Files {
		if err := validateRequestFile(id, f); err != nil {
			add("files."+id, "%v", err)
		}
	}
	checkValues := func(prefix string, fields []FieldSpec, values map[string]any) {
		for k, reason := range ValidateFieldValues(fields, values) {
			add(prefix+k, "%s", reason)
		}
		for _, f := range fields {
			id, ok := values[f.Key].(string)
			if f.Type != FieldFile || !ok || id == "" {
				continue
			}
			file, ok := r.Files[id]
			if !ok {
				add(prefix+f.Key, "file %q is not in files", id)
			} else if err := ValidateFile(f, file); err != nil {
				add(prefix+f.Key, "%v", err)
			}
		}
	}

	t, ok := s.Target(r.Target)
	if !ok {
		// Values and connections follow the target: without it there is
		// nothing to check them against.
		add("target", "not a target of this factory")
	} else {
		if !t.HasPlatform(r.Platform) {
			add("platform", "not a platform of target %q", t.ID)
		}
		checkValues("values.", s.TargetFields(t), r.Values)
		for key, reason := range checkCommands(s, t, r.Commands) {
			add(key, "%s", reason)
		}

		if len(r.Connections) == 0 {
			add("connections", "at least one endpoint is required")
		}
		for i, c := range r.Connections {
			key := fmt.Sprintf("connections[%d]", i)
			e := c.Endpoint
			if e.Format != ChannelEndpointExportV2 {
				add(key, "endpoint format must be %q", ChannelEndpointExportV2)
				continue
			}
			if !s.Supports(t, e.Channel.ModuleName, e.Channel.SchemaVersion) {
				add(key, "target %q cannot connect through channel %q version %d", t.ID, e.Channel.ModuleName, e.Channel.SchemaVersion)
				continue
			}
			if kinds := s.TargetKinds(t, e.Channel.ModuleName); !slices.Contains(kinds, c.ConnectionKind()) {
				add(key+".kind", "target %q does not connect through channel %q as %s (%s)", t.ID, e.Channel.ModuleName,
					c.ConnectionKind(), strings.Join(kinds, ", "))
				continue
			}
			ch, _ := s.Channel(e.Channel.ModuleName)
			checkValues(key+".values.", ch.KindFields(c.ConnectionKind()), c.Values)
		}
	}

	switch {
	case s.Crypto == "" && r.Crypto != nil:
		add("crypto", "the factory declares no crypto suite")
	case s.Crypto != "" && r.Crypto == nil:
		add("crypto", "required: the factory declares a crypto suite")
	case r.Crypto != nil && r.Crypto.Suite != s.Crypto:
		add("crypto", "suite %q is not the factory's %q", r.Crypto.Suite, s.Crypto)
	}

	if !isAbsoluteURL(r.Artifact.UploadURL) {
		add("artifact.upload_url", "must be an absolute URL")
	}
	if r.Artifact.MaxSize <= 0 {
		add("artifact.max_size", "must be positive")
	}
	if r.Deadline.IsZero() {
		add("deadline", "is required")
	}
	return problems
}

// checkCommands checks a build's commands for target t: each one of the
// target's, listed once, with every required one among them. Problems are
// keyed "commands[<i>]", and "commands" for the required ones missing.
func checkCommands(s BuildSchema, t BuildTarget, commands []string) map[string]string {
	problems := map[string]string{}
	listed := make(map[string]bool, len(commands))
	for i, id := range commands {
		key := fmt.Sprintf("commands[%d]", i)
		c, ok := s.Command(id)
		switch {
		case !ok:
			problems[key] = fmt.Sprintf("%q is not a command of this factory", id)
		case !c.For(t.ID):
			problems[key] = fmt.Sprintf("%q is not a command of target %q", id, t.ID)
		case listed[id]:
			problems[key] = fmt.Sprintf("%q is listed twice", id)
		}
		listed[id] = true
	}
	var missing []string
	for _, c := range s.TargetCommands(t) {
		if c.Required && !listed[c.ID] {
			missing = append(missing, c.ID)
		}
	}
	if len(missing) > 0 {
		problems["commands"] = fmt.Sprintf("target %q requires %s", t.ID, strings.Join(missing, ", "))
	}
	return problems
}

func validateRequestFile(id string, f BuildFile) error {
	switch {
	case f.ID != id:
		return fmt.Errorf("id %q does not match its key", f.ID)
	case f.Name == "":
		return fmt.Errorf("name is required")
	case f.Size < 0:
		return fmt.Errorf("size must not be negative")
	case !sha256Pattern.MatchString(f.SHA256):
		return fmt.Errorf("sha256 must be 64 lowercase hex digits")
	case !isAbsoluteURL(f.URL):
		return fmt.Errorf("url must be an absolute URL")
	}
	return nil
}

func isAbsoluteURL(s string) bool {
	u, err := url.Parse(s)
	return err == nil && u.Scheme != "" && u.Host != ""
}

// Validate checks a progress report's fields.
func (p BuildProgress) Validate() error {
	if err := validateBuildID(p.BuildID); err != nil {
		return err
	}
	if !stepIDPattern.MatchString(p.Step) {
		return fmt.Errorf("step must be a step id of the plan (%s)", stepIDPattern)
	}
	if len(p.Message) > MaxBuildMessageLength {
		return fmt.Errorf("message longer than %d", MaxBuildMessageLength)
	}
	return nil
}

// Validate checks a result's fields: a succeeded build has its artifact and
// no error, a failed one an error and no artifact.
func (r BuildResult) Validate() error {
	if err := validateBuildID(r.BuildID); err != nil {
		return err
	}
	switch r.Status {
	case BuildSucceeded:
		if r.Artifact == nil {
			return fmt.Errorf("a succeeded build needs its artifact")
		}
		if r.Error != nil {
			return fmt.Errorf("a succeeded build has no error")
		}
		if err := r.Artifact.validate(); err != nil {
			return fmt.Errorf("artifact: %w", err)
		}
	case BuildFailed:
		if r.Error == nil || !slugPattern.MatchString(r.Error.Code) {
			return fmt.Errorf("a failed build needs an error code matching %s", slugPattern)
		}
		if len(r.Error.Message) > MaxBuildMessageLength {
			return fmt.Errorf("error message longer than %d", MaxBuildMessageLength)
		}
		if r.Artifact != nil {
			return fmt.Errorf("a failed build has no artifact")
		}
	default:
		return fmt.Errorf("status must be %q or %q", BuildSucceeded, BuildFailed)
	}
	if len(r.Warnings) > MaxBuildWarnings {
		return fmt.Errorf("too many warnings (%d > %d)", len(r.Warnings), MaxBuildWarnings)
	}
	for _, w := range r.Warnings {
		if len(w) > MaxBuildMessageLength {
			return fmt.Errorf("warning longer than %d", MaxBuildMessageLength)
		}
	}
	if len(r.Log) > MaxBuildLogBytes {
		return fmt.Errorf("log longer than %d bytes: send its end", MaxBuildLogBytes)
	}
	return nil
}

func validateBuildID(id string) error {
	if !buildIDPattern.MatchString(id) {
		return fmt.Errorf("build_id must be a lowercase UUIDv4")
	}
	return nil
}

func (a BuildArtifact) validate() error {
	if a.Filename == "" || a.Filename == "." || a.Filename == ".." ||
		len(a.Filename) > MaxArtifactFilenameLength ||
		strings.ContainsAny(a.Filename, `/\`) || strings.IndexFunc(a.Filename, unicode.IsControl) >= 0 {
		return fmt.Errorf("filename must be a plain file name of 1..%d bytes", MaxArtifactFilenameLength)
	}
	if a.Size < 0 {
		return fmt.Errorf("size must not be negative")
	}
	if !sha256Pattern.MatchString(a.SHA256) {
		return fmt.Errorf("sha256 must be 64 lowercase hex digits")
	}
	return nil
}
