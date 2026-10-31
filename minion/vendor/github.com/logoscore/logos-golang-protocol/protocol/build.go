package protocol

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// This file defines the build schema: what a factory can build and which
// parameters each build takes. A factory declares it at module.register under
// "build", the way a channel declares its connection schema (connection.go).
// Operators fill it in, core validates their values and sends factory.build
// requests (build_ops.go) that follow it.
//
// A schema lists targets (what the factory can produce: a format, the
// platforms it runs on, its own fields) next to the fields every target
// shares, the channel kinds its minions speak, with the factory's own
// connection parameters for each, the payload crypto suites they implement,
// and the commands they can be built with, from which operators pick each
// build's. Values are validated with the same FieldSpec rules as management
// properties.

// BuildSchemaV1 is the current build schema format version.
const BuildSchemaV1 = "1"

// RegisterFieldBuild is the module.register key a factory declares its build
// schema under.
const RegisterFieldBuild = "build"

// Build schema limits.
const (
	MaxBuildTargets         = 32
	MaxBuildTargetFields    = 64 // shared fields plus the target's own
	MaxBuildPlatforms       = 16 // per target
	MaxBuildChannels        = 16
	MaxBuildChannelVersions = 16 // per channel kind
	MaxConnectionParams     = 32 // per channel kind
	MaxBuildCommands        = 256
	MaxCommandParams        = 32 // per command
)

var (
	// targetIDPattern is also what a target's format must match.
	targetIDPattern    = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,62}$`)
	platformPattern    = regexp.MustCompile(`^[a-z0-9]{1,31}/[a-z0-9]{1,31}$`)
	cryptoSuitePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)
	// commandIDPattern is a target id's: no dots, so a command's id can key a
	// document.
	commandIDPattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,62}$`)
	// moduleNamePattern: core only requires a module name, so a factory can
	// name any channel kind that registers.
	moduleNamePattern = regexp.MustCompile(`^\S{1,64}$`)
)

// BuildSchema is declared by a factory in module.register under "build".
type BuildSchema struct {
	// SchemaVersion is the format of this structure (BuildSchemaV1).
	SchemaVersion string `json:"schema_version"`
	// Version is the version of the factory's parameter set, starting at 1.
	// A factory raises it when values saved against the previous set would
	// build something else or fail (a field or target removed, renamed or
	// given a new meaning). Adding an optional field or a target does not
	// need a new version.
	Version  int            `json:"version"`
	Channels []BuildChannel `json:"channels"`
	// Crypto is the payload crypto suite the minions implement; core
	// generates the keys for it. Optional; core refuses a registration whose
	// suite it does not implement.
	Crypto string `json:"crypto,omitempty"`
	// Fields are the build parameters every target shares.
	Fields  []FieldSpec   `json:"fields,omitempty"`
	Targets []BuildTarget `json:"targets"`
	// Commands are what the minions can do. An operator picks a build's
	// from those of its target, and the minion has only those. Optional: a
	// factory without commands builds minions with none.
	Commands []BuildCommand `json:"commands,omitempty"`
	// PersistedSessions says the minions can claim a persisted session:
	// built with a BuildRequest.SessionID, they claim it in their hello
	// (HelloItem.SID). Optional.
	PersistedSessions bool `json:"persisted_sessions,omitempty"`
}

// ChannelVersions is a channel kind (its module_name) and the connection
// schema versions a factory's minions read.
type ChannelVersions struct {
	ModuleName string `json:"module_name"`
	Versions   []int  `json:"versions"`
}

// BuildChannel is a channel kind the minions speak. Fields are the factory's
// connection parameters for it: how its minions use the channel (e.g. HTTP
// method and path prefix), set for each connection of a build.
type BuildChannel struct {
	ChannelVersions
	Fields []FieldSpec `json:"fields,omitempty"`
}

// BuildTarget is something a factory can produce.
type BuildTarget struct {
	ID          string `json:"id"`
	Label       string `json:"label,omitempty"`
	Description string `json:"description,omitempty"`
	// Format is what the artifact is (exe, dll, elf, so, macho, dylib, bin,
	// script, zip, ...): shown and filtered on, never interpreted.
	Format string `json:"format"`
	// Platforms are where the artifact runs, as "os/arch" with Go's names;
	// "any" stands for either half.
	Platforms []string `json:"platforms"`
	// Fields are this target's own parameters, after the shared ones.
	Fields []FieldSpec `json:"fields,omitempty"`
	// Channels narrows the factory's channels for this target, to kinds and
	// versions the factory lists; empty means all of them. The connection
	// parameters stay the factory's.
	Channels []ChannelVersions `json:"channels,omitempty"`
}

// BuildCommand is a command a factory's minions can be built with. An
// operator picks a build's commands, so a minion carries only what its job
// needs.
type BuildCommand struct {
	// ID names the command in builds (and in tasks, once minions take them),
	// unique in the schema: ^[a-z][a-z0-9_-]{0,62}$.
	ID          string `json:"id"`
	Label       string `json:"label,omitempty"`
	Description string `json:"description,omitempty"`
	// Targets narrows the command to these targets; empty means all of them.
	Targets []string `json:"targets,omitempty"`
	// Default commands are picked in a new build until the operator unpicks
	// them, and a build that names no commands gets them.
	Default bool `json:"default,omitempty"`
	// Required commands are in every build of a target that has them: the
	// minion cannot run without them (sleep, exit), so the operator cannot
	// leave them out.
	Required bool `json:"required,omitempty"`
	// Params are what a task of the command takes: field specs as a
	// management action's, file aside.
	Params []FieldSpec `json:"params,omitempty"`
}

// For reports whether a build of target id can have the command.
func (c BuildCommand) For(id string) bool {
	return len(c.Targets) == 0 || slices.Contains(c.Targets, id)
}

// Target returns the target with id, if declared.
func (s BuildSchema) Target(id string) (BuildTarget, bool) {
	for _, t := range s.Targets {
		if t.ID == id {
			return t, true
		}
	}
	return BuildTarget{}, false
}

// Channel returns the factory's entry for a channel kind, if declared.
func (s BuildSchema) Channel(moduleName string) (BuildChannel, bool) {
	for _, c := range s.Channels {
		if c.ModuleName == moduleName {
			return c, true
		}
	}
	return BuildChannel{}, false
}

// TargetFields returns the fields a build of t takes: the shared ones, then
// the target's own.
func (s BuildSchema) TargetFields(t BuildTarget) []FieldSpec {
	out := make([]FieldSpec, 0, len(s.Fields)+len(t.Fields))
	return append(append(out, s.Fields...), t.Fields...)
}

// TargetChannels returns the channel kinds and versions a build of t can
// connect through.
func (s BuildSchema) TargetChannels(t BuildTarget) []ChannelVersions {
	if len(t.Channels) > 0 {
		return t.Channels
	}
	out := make([]ChannelVersions, len(s.Channels))
	for i, c := range s.Channels {
		out[i] = c.ChannelVersions
	}
	return out
}

// Supports reports whether a build of t can connect through channel kind
// moduleName, whose connection schema is at version.
func (s BuildSchema) Supports(t BuildTarget, moduleName string, version int) bool {
	for _, c := range s.TargetChannels(t) {
		if c.ModuleName == moduleName {
			return slices.Contains(c.Versions, version)
		}
	}
	return false
}

// Command returns the command with id, if declared.
func (s BuildSchema) Command(id string) (BuildCommand, bool) {
	for _, c := range s.Commands {
		if c.ID == id {
			return c, true
		}
	}
	return BuildCommand{}, false
}

// TargetCommands returns the commands a build of t can have, in the schema's
// order.
func (s BuildSchema) TargetCommands(t BuildTarget) []BuildCommand {
	var out []BuildCommand
	for _, c := range s.Commands {
		if c.For(t.ID) {
			out = append(out, c)
		}
	}
	return out
}

// DefaultCommands returns the ids of the commands a build of t gets when it
// names none: its default and required ones, in the schema's order.
func (s BuildSchema) DefaultCommands(t BuildTarget) []string {
	out := []string{}
	for _, c := range s.TargetCommands(t) {
		if c.Default || c.Required {
			out = append(out, c.ID)
		}
	}
	return out
}

// HasPlatform reports whether t runs on platform.
func (t BuildTarget) HasPlatform(platform string) bool {
	return slices.Contains(t.Platforms, platform)
}

// ValidateBuildSchema checks a schema's structure, limits, and that every
// declared default satisfies its own field spec.
func ValidateBuildSchema(s BuildSchema) error {
	if s.SchemaVersion != BuildSchemaV1 {
		return fmt.Errorf("unsupported build schema_version %q", s.SchemaVersion)
	}
	if s.Version < 1 {
		return fmt.Errorf("version must be at least 1")
	}
	if len(s.Channels) == 0 {
		return fmt.Errorf("at least one channel is required")
	}
	if len(s.Channels) > MaxBuildChannels {
		return fmt.Errorf("too many channels (%d > %d)", len(s.Channels), MaxBuildChannels)
	}
	channels := make(map[string]ChannelVersions, len(s.Channels))
	for _, c := range s.Channels {
		if err := validateChannelVersions(c.ChannelVersions); err != nil {
			return fmt.Errorf("channel %q: %w", c.ModuleName, err)
		}
		if _, dup := channels[c.ModuleName]; dup {
			return fmt.Errorf("duplicate channel %q", c.ModuleName)
		}
		channels[c.ModuleName] = c.ChannelVersions
		if len(c.Fields) > MaxConnectionParams {
			return fmt.Errorf("channel %q: too many connection parameters (%d > %d)", c.ModuleName, len(c.Fields), MaxConnectionParams)
		}
		if _, err := validateBuildFields(c.Fields, nil); err != nil {
			return fmt.Errorf("channel %q: %w", c.ModuleName, err)
		}
	}

	if s.Crypto != "" && !cryptoSuitePattern.MatchString(s.Crypto) {
		return fmt.Errorf("crypto suite %q must match %s", s.Crypto, cryptoSuitePattern)
	}

	if len(s.Fields) > MaxBuildTargetFields {
		return fmt.Errorf("too many fields (%d > %d)", len(s.Fields), MaxBuildTargetFields)
	}
	shared, err := validateBuildFields(s.Fields, nil)
	if err != nil {
		return err
	}

	if len(s.Targets) == 0 {
		return fmt.Errorf("at least one target is required")
	}
	if len(s.Targets) > MaxBuildTargets {
		return fmt.Errorf("too many targets (%d > %d)", len(s.Targets), MaxBuildTargets)
	}
	ids := make(map[string]bool, len(s.Targets))
	for _, t := range s.Targets {
		if err := validateBuildTarget(t, shared, channels); err != nil {
			return fmt.Errorf("target %q: %w", t.ID, err)
		}
		if ids[t.ID] {
			return fmt.Errorf("duplicate target %q", t.ID)
		}
		ids[t.ID] = true
	}

	if len(s.Commands) > MaxBuildCommands {
		return fmt.Errorf("too many commands (%d > %d)", len(s.Commands), MaxBuildCommands)
	}
	commands := make(map[string]bool, len(s.Commands))
	for _, c := range s.Commands {
		if err := validateBuildCommand(c, ids); err != nil {
			return fmt.Errorf("command %q: %w", c.ID, err)
		}
		if commands[c.ID] {
			return fmt.Errorf("duplicate command %q", c.ID)
		}
		commands[c.ID] = true
	}
	return nil
}

// CommandHelp is core's own command, in every session's console: it lists
// the commands the session's minion takes, or one command's params. Core
// answers it; it never goes to a minion, and a schema may not declare it.
const CommandHelp = "help"

// validateBuildCommand checks a command; targets are the schema's target ids.
func validateBuildCommand(c BuildCommand, targets map[string]bool) error {
	if !commandIDPattern.MatchString(c.ID) {
		return fmt.Errorf("id must match %s", commandIDPattern)
	}
	if c.ID == CommandHelp {
		return fmt.Errorf("%q is core's own command: a factory does not declare it", CommandHelp)
	}
	if err := validateText(c.Label, c.Description); err != nil {
		return err
	}
	for i, t := range c.Targets {
		if !targets[t] {
			return fmt.Errorf("target %q is not one the factory lists", t)
		}
		if slices.Contains(c.Targets[:i], t) {
			return fmt.Errorf("duplicate target %q", t)
		}
	}
	if len(c.Params) > MaxCommandParams {
		return fmt.Errorf("too many params (%d > %d)", len(c.Params), MaxCommandParams)
	}
	params := make(map[string]bool, len(c.Params))
	for _, p := range c.Params {
		// A task hands no files to a minion yet: no file params.
		if err := validateFieldSpec(p); err != nil {
			return fmt.Errorf("param %q: %w", p.Key, err)
		}
		if params[p.Key] {
			return fmt.Errorf("duplicate param %q", p.Key)
		}
		params[p.Key] = true
	}
	return nil
}

func validateChannelVersions(c ChannelVersions) error {
	if !moduleNamePattern.MatchString(c.ModuleName) {
		return fmt.Errorf("module_name must be 1..64 characters without spaces")
	}
	if len(c.Versions) == 0 || len(c.Versions) > MaxBuildChannelVersions {
		return fmt.Errorf("needs 1..%d versions", MaxBuildChannelVersions)
	}
	for i, v := range c.Versions {
		if v < 1 {
			return fmt.Errorf("versions start at 1")
		}
		if slices.Contains(c.Versions[:i], v) {
			return fmt.Errorf("duplicate version %d", v)
		}
	}
	return nil
}

// validateBuildFields checks fields, and that their keys are unique, also
// against taken. It returns taken plus the new keys.
func validateBuildFields(fields []FieldSpec, taken map[string]bool) (map[string]bool, error) {
	keys := make(map[string]bool, len(taken)+len(fields))
	for k := range taken {
		keys[k] = true
	}
	for _, f := range fields {
		if err := validateBuildFieldSpec(f); err != nil {
			return nil, fmt.Errorf("field %q: %w", f.Key, err)
		}
		if keys[f.Key] {
			return nil, fmt.Errorf("duplicate field key %q", f.Key)
		}
		keys[f.Key] = true
	}
	return keys, nil
}

func validateBuildTarget(t BuildTarget, shared map[string]bool, channels map[string]ChannelVersions) error {
	if !targetIDPattern.MatchString(t.ID) {
		return fmt.Errorf("id must match %s", targetIDPattern)
	}
	if err := validateText(t.Label, t.Description); err != nil {
		return err
	}
	if !targetIDPattern.MatchString(t.Format) {
		return fmt.Errorf("format must match %s", targetIDPattern)
	}
	if len(t.Platforms) == 0 || len(t.Platforms) > MaxBuildPlatforms {
		return fmt.Errorf("needs 1..%d platforms", MaxBuildPlatforms)
	}
	for i, p := range t.Platforms {
		if !platformPattern.MatchString(p) {
			return fmt.Errorf("platform %q must be os/arch, e.g. windows/amd64", p)
		}
		if slices.Contains(t.Platforms[:i], p) {
			return fmt.Errorf("duplicate platform %q", p)
		}
	}
	if len(shared)+len(t.Fields) > MaxBuildTargetFields {
		return fmt.Errorf("too many fields (%d with the shared ones > %d)", len(shared)+len(t.Fields), MaxBuildTargetFields)
	}
	if _, err := validateBuildFields(t.Fields, shared); err != nil {
		return err
	}
	for i, c := range t.Channels {
		if err := validateChannelVersions(c); err != nil {
			return fmt.Errorf("channel %q: %w", c.ModuleName, err)
		}
		declared, ok := channels[c.ModuleName]
		if !ok {
			return fmt.Errorf("channel %q is not one the factory lists", c.ModuleName)
		}
		for _, v := range c.Versions {
			if !slices.Contains(declared.Versions, v) {
				return fmt.Errorf("channel %q: version %d is not one the factory lists", c.ModuleName, v)
			}
		}
		for _, prev := range t.Channels[:i] {
			if prev.ModuleName == c.ModuleName {
				return fmt.Errorf("duplicate channel %q", c.ModuleName)
			}
		}
	}
	return nil
}

// BuildFile is a file an operator uploaded for a file field. Build values hold
// its id; a factory.build request carries the file itself under files, with a
// presigned URL to download it from.
type BuildFile struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Size      int64  `json:"size"`
	SHA256    string `json:"sha256"` // lowercase hex
	MediaType string `json:"media_type,omitempty"`
	// URL is a presigned download URL, valid until the build's deadline. Set
	// in a factory.build request only.
	URL string `json:"url,omitempty"`
}

// ValidateFile checks a file against a file field's accept and max_size. Core
// runs it when a file is used in a value; a factory may run it again on the
// files of a request.
func ValidateFile(f FieldSpec, file BuildFile) error {
	if f.MaxSize > 0 && file.Size > f.MaxSize {
		return fmt.Errorf("larger than %d bytes", f.MaxSize)
	}
	if len(f.Accept) == 0 {
		return nil
	}
	name := strings.ToLower(file.Name)
	media, _, _ := strings.Cut(strings.ToLower(file.MediaType), ";")
	media = strings.TrimSpace(media)
	for _, a := range f.Accept {
		a = strings.ToLower(a)
		switch {
		case strings.HasPrefix(a, "."):
			if strings.HasSuffix(name, a) {
				return nil
			}
		case strings.HasSuffix(a, "/*"):
			if media != "" && strings.HasPrefix(media, strings.TrimSuffix(a, "*")) {
				return nil
			}
		case media == a:
			return nil
		}
	}
	return fmt.Errorf("must be one of: %s", strings.Join(f.Accept, ", "))
}
