package protocol

import (
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"regexp"
	"sort"
	"sync"
	"time"
)

// This file defines module management: the manifest a module declares at
// module.register (manageable properties, triggerable actions, reported status
// values) and the core → module management RPC payloads that apply config and
// invoke actions. Core owns desired property values; the module is the
// authority on what it actually applied.

// ManagementSchemaV1 is the current manifest schema version.
const ManagementSchemaV1 = "1"

// Core → module management RPC operation types, sent to the module's rpc_queue.
const (
	TypeMgmtConfigApply  = "mgmt.config.apply"
	TypeMgmtActionInvoke = "mgmt.action.invoke"
	TypeMgmtStateGet     = "mgmt.state.get"
)

// Additional control-plane error codes used by management RPC.
const (
	CodeNotFound     = "not_found"
	CodeConflict     = "conflict"
	CodeActionFailed = "action_failed"
	// CodeActionUnavailable refuses an action whose available_when does not
	// hold; the message says why.
	CodeActionUnavailable = "action_unavailable"
)

// FieldType is the value type of a property, action parameter, or status value.
// Each maps to one UI control and one validation rule set.
type FieldType string

const (
	FieldString     FieldType = "string"
	FieldInt        FieldType = "int"
	FieldNumber     FieldType = "number"
	FieldBool       FieldType = "bool"
	FieldEnum       FieldType = "enum"
	FieldDuration   FieldType = "duration"    // Go duration string, e.g. "30s"
	FieldBytes      FieldType = "bytes"       // integer byte count
	FieldURL        FieldType = "url"         // absolute URL string
	FieldSecret     FieldType = "secret"      // write-only string; never echoed back
	FieldStringList FieldType = "string_list" // JSON array of strings
	FieldStringMap  FieldType = "string_map"  // JSON object of string keys to string values
	FieldDatetime   FieldType = "datetime"    // RFC 3339 timestamp string
	FieldEnumList   FieldType = "enum_list"   // JSON array of distinct option values
	FieldFile       FieldType = "file"        // a file's id: a build file, or a command's task file; build schemas only
)

// ApplyMode says whether a property change takes effect immediately or only
// after the module restarts.
type ApplyMode string

const (
	ApplyLive    ApplyMode = "live"
	ApplyRestart ApplyMode = "restart"
)

// Risk classifies an action. Core maps risk to the permission required to
// invoke it; modules never decide authorization.
type Risk string

const (
	RiskLow  Risk = "low"
	RiskHigh Risk = "high"
)

// EnumOption is one allowed value of an enum field.
type EnumOption struct {
	Value string `json:"value"`
	Label string `json:"label,omitempty"`
	// Description says what choosing this option does; shown under the label
	// in the option list.
	Description string `json:"description,omitempty"`
}

// Field formats: what a string field holds, beyond its type. A UI hint only
// (never validated): the UI may offer a helper for it, e.g. a generator.
const (
	FormatUserAgent = "user_agent" // an HTTP User-Agent string
	FormatPath      = "path"       // a URL path, e.g. /cdn/assets/
)

// FieldSpec describes a typed value plus its constraints and UI hints. It is
// shared by properties, action parameters, connection schemas and build
// schemas.
//
// Min and Max are numbers for int/number/bytes, duration strings for duration,
// RFC 3339 strings for datetime, and ignored otherwise. For string_list and
// string_map, Pattern and MaxLength apply to each item (each value of a map),
// KeyPattern to each map key, and MaxItems bounds the number of items or
// entries (of enum_list too).
type FieldSpec struct {
	Key         string    `json:"key"`
	Type        FieldType `json:"type"`
	Label       string    `json:"label,omitempty"`
	Description string    `json:"description,omitempty"`
	Group       string    `json:"group,omitempty"`
	Order       int       `json:"order,omitempty"`
	Unit        string    `json:"unit,omitempty"`
	Placeholder string    `json:"placeholder,omitempty"`
	// Format is what a string field holds (FormatUserAgent, ...): a UI hint,
	// never validated. Unknown formats are ignored.
	Format     string       `json:"format,omitempty"`
	Required   bool         `json:"required,omitempty"`
	Default    any          `json:"default,omitempty"`
	Min        any          `json:"min,omitempty"`
	Max        any          `json:"max,omitempty"`
	Pattern    string       `json:"pattern,omitempty"`
	MaxLength  int          `json:"max_length,omitempty"`
	KeyPattern string       `json:"key_pattern,omitempty"`
	MaxItems   int          `json:"max_items,omitempty"`
	Options    []EnumOption `json:"options,omitempty"`
	// Accept and MaxSize are for file: the extensions (".ico") or media
	// types ("image/x-icon", "image/*") a file may have, any when empty, and
	// its largest size in bytes, no limit when 0. The value is only the
	// file's id, so they are checked against the file itself (ValidateFile).
	Accept  []string `json:"accept,omitempty"`
	MaxSize int64    `json:"max_size,omitempty"`
}

// PropertySpec is a manageable property. Default is the module's own effective
// default (typically its env/config-file value), used when core holds no value.
type PropertySpec struct {
	FieldSpec
	Apply ApplyMode `json:"apply"`
}

// ActionSpec is an operator-triggerable action.
type ActionSpec struct {
	Name           string      `json:"name"`
	Label          string      `json:"label,omitempty"`
	Description    string      `json:"description,omitempty"`
	Risk           Risk        `json:"risk"`
	Confirm        string      `json:"confirm,omitempty"` // confirmation prompt shown before invoking
	Params         []FieldSpec `json:"params,omitempty"`
	TimeoutSeconds int         `json:"timeout_seconds,omitempty"`
	// AvailableWhen are the conditions the action is available under, all
	// of which must hold (see ActionAvailable). Optional.
	AvailableWhen []ActionCondition `json:"available_when,omitempty"`
}

// StatusSpec describes a read-only status value the module reports in its
// heartbeat metrics under Key.
type StatusSpec struct {
	Key         string    `json:"key"`
	Type        FieldType `json:"type"`
	Label       string    `json:"label,omitempty"`
	Description string    `json:"description,omitempty"`
	Unit        string    `json:"unit,omitempty"`
	// Alert is the value that needs an operator's attention (e.g. true for a
	// "draining" flag). Core cannot know which value of a module's status is
	// bad, so the module says so; the UI highlights a matching value and shows
	// any other as normal. Only for bool, enum and string. Optional.
	Alert any `json:"alert,omitempty"`
}

// ManagementManifest is declared by a module in module.register under
// "management".
type ManagementManifest struct {
	SchemaVersion string         `json:"schema_version"`
	Properties    []PropertySpec `json:"properties,omitempty"`
	Actions       []ActionSpec   `json:"actions,omitempty"`
	Status        []StatusSpec   `json:"status,omitempty"`
	// Capabilities lists optional management features the module serves
	// (e.g. CapabilityLogs). Unknown values are ignored.
	Capabilities []string `json:"capabilities,omitempty"`
}

// Has reports whether the manifest declares capability c.
func (m ManagementManifest) Has(c string) bool {
	for _, x := range m.Capabilities {
		if x == c {
			return true
		}
	}
	return false
}

// ManagementConfig is the effective config core hands a module: in the
// module.register reply under config.management, and in mgmt.config.apply.
// Values holds every property core has a value for; a property absent from
// Values reverts to the module's own default. Applying it is idempotent.
type ManagementConfig struct {
	Revision int64          `json:"revision"`
	Values   map[string]any `json:"values"`
}

// Actor identifies who triggered a management operation, propagated by core
// for module-side audit logging.
type Actor struct {
	Type string `json:"type"` // user | agent | system
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
}

// ConfigApplyRequest is the mgmt.config.apply payload.
type ConfigApplyRequest struct {
	ManagementConfig
	Actor *Actor `json:"actor,omitempty"`
}

// ConfigApplyReply is the mgmt.config.apply reply payload. AppliedRevision is
// the revision the module processed, even if some keys were rejected, so a
// rejected value never causes an endless re-apply loop.
type ConfigApplyReply struct {
	AppliedRevision int64             `json:"applied_revision"`
	Rejected        map[string]string `json:"rejected,omitempty"`         // key -> reason
	RestartRequired []string          `json:"restart_required,omitempty"` // changed restart-mode keys
}

// ActionInvokeRequest is the mgmt.action.invoke payload. Modules must treat a
// repeated InvocationID as the same invocation.
type ActionInvokeRequest struct {
	InvocationID string         `json:"invocation_id"`
	Action       string         `json:"action"`
	Params       map[string]any `json:"params,omitempty"`
	Actor        *Actor         `json:"actor,omitempty"`
}

// ActionInvokeReply is the mgmt.action.invoke success payload. Failures are
// error replies with code action_failed (or validation_failed / not_found).
type ActionInvokeReply struct {
	InvocationID string `json:"invocation_id"`
	Result       any    `json:"result,omitempty"`
}

// StateGetReply is the mgmt.state.get reply payload: the module's effective
// values (secrets redacted) and current status values.
type StateGetReply struct {
	AppliedRevision int64          `json:"applied_revision"`
	Values          map[string]any `json:"values"`
	Status          map[string]any `json:"status,omitempty"`
	RestartRequired []string       `json:"restart_required,omitempty"`
}

// --- validation ----------------------------------------------------------

// Manifest limits keep a misbehaving module from flooding core or the UI.
const (
	MaxManifestProperties = 64
	MaxManifestActions    = 32
	MaxManifestStatus     = 64
	MaxCapabilities       = 16
	MaxActionParams       = 16
	MaxEnumOptions        = 64
	MaxLabelLength        = 120
	MaxDescriptionLength  = 1000
	MaxActionTimeout      = 60 // seconds
	MaxAcceptEntries      = 32 // file: accepted extensions and media types
	MaxFileIDLength       = 128
	MaxActionConditions   = 8
	MaxConditionValues    = 64 // values of an "in" condition
)

var keyPattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,62}$`)

var fieldTypes = map[FieldType]bool{
	FieldString: true, FieldInt: true, FieldNumber: true, FieldBool: true, FieldEnum: true,
	FieldDuration: true, FieldBytes: true, FieldURL: true, FieldSecret: true, FieldStringList: true,
	FieldStringMap: true, FieldDatetime: true, FieldEnumList: true, FieldFile: true,
}

// A file field's accept entries: an extension, or a media type whose subtype
// may be "*".
var (
	acceptExtPattern   = regexp.MustCompile(`^\.[A-Za-z0-9][A-Za-z0-9._+-]{0,31}$`)
	acceptMediaPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9!#$&^_.+-]{0,63}/(\*|[A-Za-z0-9][A-Za-z0-9!#$&^_.+-]{0,63})$`)
)

// ValidateManifest checks a manifest's structure, limits, and that every
// declared default satisfies its own field spec.
func ValidateManifest(m ManagementManifest) error {
	if m.SchemaVersion != ManagementSchemaV1 {
		return fmt.Errorf("unsupported management schema_version %q", m.SchemaVersion)
	}
	if len(m.Properties) > MaxManifestProperties {
		return fmt.Errorf("too many properties (%d > %d)", len(m.Properties), MaxManifestProperties)
	}
	if len(m.Actions) > MaxManifestActions {
		return fmt.Errorf("too many actions (%d > %d)", len(m.Actions), MaxManifestActions)
	}
	if len(m.Status) > MaxManifestStatus {
		return fmt.Errorf("too many status values (%d > %d)", len(m.Status), MaxManifestStatus)
	}
	if len(m.Capabilities) > MaxCapabilities {
		return fmt.Errorf("too many capabilities (%d > %d)", len(m.Capabilities), MaxCapabilities)
	}

	seen := map[string]bool{}
	for _, p := range m.Properties {
		if err := validateFieldSpec(p.FieldSpec); err != nil {
			return fmt.Errorf("property %q: %w", p.Key, err)
		}
		if seen[p.Key] {
			return fmt.Errorf("duplicate property key %q", p.Key)
		}
		seen[p.Key] = true
		if p.Apply != ApplyLive && p.Apply != ApplyRestart {
			return fmt.Errorf("property %q: apply must be %q or %q", p.Key, ApplyLive, ApplyRestart)
		}
	}

	seen = map[string]bool{}
	for _, a := range m.Actions {
		if !keyPattern.MatchString(a.Name) {
			return fmt.Errorf("action %q: name must match %s", a.Name, keyPattern)
		}
		if seen[a.Name] {
			return fmt.Errorf("duplicate action %q", a.Name)
		}
		seen[a.Name] = true
		if a.Risk != RiskLow && a.Risk != RiskHigh {
			return fmt.Errorf("action %q: risk must be %q or %q", a.Name, RiskLow, RiskHigh)
		}
		if err := validateText(a.Label, a.Description); err != nil {
			return fmt.Errorf("action %q: %w", a.Name, err)
		}
		if a.TimeoutSeconds < 0 || a.TimeoutSeconds > MaxActionTimeout {
			return fmt.Errorf("action %q: timeout_seconds must be 0..%d", a.Name, MaxActionTimeout)
		}
		if len(a.Params) > MaxActionParams {
			return fmt.Errorf("action %q: too many params", a.Name)
		}
		params := map[string]bool{}
		for _, p := range a.Params {
			if err := validateFieldSpec(p); err != nil {
				return fmt.Errorf("action %q param %q: %w", a.Name, p.Key, err)
			}
			if params[p.Key] {
				return fmt.Errorf("action %q: duplicate param %q", a.Name, p.Key)
			}
			params[p.Key] = true
		}
	}

	seen = map[string]bool{}
	for _, s := range m.Status {
		if !keyPattern.MatchString(s.Key) {
			return fmt.Errorf("status %q: key must match %s", s.Key, keyPattern)
		}
		if seen[s.Key] {
			return fmt.Errorf("duplicate status key %q", s.Key)
		}
		seen[s.Key] = true
		if !fieldTypes[s.Type] || s.Type == FieldSecret || s.Type == FieldFile {
			return fmt.Errorf("status %q: invalid type %q", s.Key, s.Type)
		}
		if err := validateText(s.Label, s.Description); err != nil {
			return fmt.Errorf("status %q: %w", s.Key, err)
		}
		if s.Alert != nil {
			var ok bool
			switch s.Type {
			case FieldBool:
				_, ok = s.Alert.(bool)
			case FieldEnum, FieldString:
				_, ok = s.Alert.(string)
			default:
				return fmt.Errorf("status %q: alert is only for bool, enum and string", s.Key)
			}
			if !ok {
				return fmt.Errorf("status %q: alert must be a %s value", s.Key, s.Type)
			}
		}
	}

	for _, a := range m.Actions {
		if err := validateConditions(m, a.AvailableWhen); err != nil {
			return fmt.Errorf("action %q: %w", a.Name, err)
		}
	}
	return nil
}

func validateText(label, description string) error {
	if len(label) > MaxLabelLength {
		return fmt.Errorf("label longer than %d", MaxLabelLength)
	}
	if len(description) > MaxDescriptionLength {
		return fmt.Errorf("description longer than %d", MaxDescriptionLength)
	}
	return nil
}

// validateFieldSpec checks a field of a management manifest or a connection
// schema, where file fields are refused: only a build hands files to a module.
func validateFieldSpec(f FieldSpec) error { return checkFieldSpec(f, false) }

// validateBuildFieldSpec checks a field of a build schema.
func validateBuildFieldSpec(f FieldSpec) error { return checkFieldSpec(f, true) }

func checkFieldSpec(f FieldSpec, allowFile bool) error {
	if !keyPattern.MatchString(f.Key) {
		return fmt.Errorf("key must match %s", keyPattern)
	}
	if !fieldTypes[f.Type] {
		return fmt.Errorf("invalid type %q", f.Type)
	}
	if f.Type == FieldFile && !allowFile {
		return fmt.Errorf("type file is only for build schemas")
	}
	if err := validateText(f.Label, f.Description); err != nil {
		return err
	}
	if f.Format != "" && (f.Type != FieldString || !keyPattern.MatchString(f.Format)) {
		return fmt.Errorf("format is a lowercase name, only for string")
	}
	if (f.Type == FieldEnum || f.Type == FieldEnumList) && (len(f.Options) == 0 || len(f.Options) > MaxEnumOptions) {
		return fmt.Errorf("%s needs 1..%d options", f.Type, MaxEnumOptions)
	}
	for _, o := range f.Options {
		if err := validateText(o.Label, o.Description); err != nil {
			return fmt.Errorf("option %q: %w", o.Value, err)
		}
	}
	if f.Pattern != "" {
		if _, err := regexp.Compile(f.Pattern); err != nil {
			return fmt.Errorf("invalid pattern: %w", err)
		}
	}
	if f.KeyPattern != "" {
		if f.Type != FieldStringMap {
			return fmt.Errorf("key_pattern is only for string_map")
		}
		if _, err := regexp.Compile(f.KeyPattern); err != nil {
			return fmt.Errorf("invalid key_pattern: %w", err)
		}
	}
	if f.MaxItems < 0 || (f.MaxItems > 0 && f.Type != FieldStringList && f.Type != FieldStringMap && f.Type != FieldEnumList) {
		return fmt.Errorf("max_items is a positive count, only for string_list, string_map and enum_list")
	}
	if f.Type != FieldFile && (len(f.Accept) > 0 || f.MaxSize != 0) {
		return fmt.Errorf("accept and max_size are only for file")
	}
	if f.MaxSize < 0 {
		return fmt.Errorf("max_size must not be negative")
	}
	if len(f.Accept) > MaxAcceptEntries {
		return fmt.Errorf("at most %d accept entries", MaxAcceptEntries)
	}
	for _, a := range f.Accept {
		if !acceptExtPattern.MatchString(a) && !acceptMediaPattern.MatchString(a) {
			return fmt.Errorf("accept %q is neither an extension (.ico) nor a media type (image/x-icon)", a)
		}
	}
	switch f.Type {
	case FieldInt, FieldNumber, FieldBytes:
		for _, b := range []any{f.Min, f.Max} {
			if b != nil {
				if _, ok := toFloat(b); !ok {
					return fmt.Errorf("min/max must be numbers")
				}
			}
		}
	case FieldDuration:
		for _, b := range []any{f.Min, f.Max} {
			if b != nil {
				if _, err := toDuration(b); err != nil {
					return fmt.Errorf("min/max must be durations: %w", err)
				}
			}
		}
	case FieldDatetime:
		for _, b := range []any{f.Min, f.Max} {
			if b != nil {
				if _, err := ParseDatetime(b); err != nil {
					return fmt.Errorf("min/max must be RFC 3339 timestamps")
				}
			}
		}
	case FieldFile:
		// A file belongs to an operation, which a schema cannot name.
		if f.Default != nil {
			return fmt.Errorf("a file field has no default")
		}
	}
	if f.Default != nil && f.Type != FieldSecret {
		if err := ValidateValue(f, f.Default); err != nil {
			return fmt.Errorf("default: %w", err)
		}
	}
	return nil
}

// ValidateValue checks v against f. v is a decoded JSON value (numbers may be
// float64, json.Number, or Go ints). A nil v is valid unless f is required.
// Core runs it before storing a value; the module runs it again before
// applying, since only the module knows its runtime constraints.
func ValidateValue(f FieldSpec, v any) error {
	if v == nil {
		if f.Required {
			return fmt.Errorf("value is required")
		}
		return nil
	}
	switch f.Type {
	case FieldString, FieldSecret, FieldURL, FieldEnum:
		s, ok := v.(string)
		if !ok {
			return fmt.Errorf("must be a string")
		}
		return validateString(f, s)
	case FieldBool:
		if _, ok := v.(bool); !ok {
			return fmt.Errorf("must be a boolean")
		}
	case FieldInt, FieldBytes, FieldNumber:
		n, ok := toFloat(v)
		if !ok {
			return fmt.Errorf("must be a number")
		}
		if f.Type != FieldNumber {
			if _, ok := AsInt64(v); !ok {
				return fmt.Errorf("must be an integer within int64 range")
			}
		}
		if f.Type == FieldBytes && n < 0 {
			return fmt.Errorf("must not be negative")
		}
		if lo, ok := toFloat(f.Min); ok && n < lo {
			return fmt.Errorf("must be >= %v", f.Min)
		}
		if hi, ok := toFloat(f.Max); ok && n > hi {
			return fmt.Errorf("must be <= %v", f.Max)
		}
	case FieldDuration:
		d, err := toDuration(v)
		if err != nil {
			return err
		}
		if f.Min != nil {
			if lo, err := toDuration(f.Min); err == nil && d < lo {
				return fmt.Errorf("must be >= %v", f.Min)
			}
		}
		if f.Max != nil {
			if hi, err := toDuration(f.Max); err == nil && d > hi {
				return fmt.Errorf("must be <= %v", f.Max)
			}
		}
	case FieldStringList:
		items, ok := v.([]any)
		if !ok {
			if ss, ok2 := v.([]string); ok2 {
				for _, s := range ss {
					items = append(items, s)
				}
			} else {
				return fmt.Errorf("must be a list of strings")
			}
		}
		if f.MaxItems > 0 && len(items) > f.MaxItems {
			return fmt.Errorf("at most %d items", f.MaxItems)
		}
		for i, it := range items {
			s, ok := it.(string)
			if !ok {
				return fmt.Errorf("item %d must be a string", i)
			}
			if err := validateString(f, s); err != nil {
				return fmt.Errorf("item %d: %w", i, err)
			}
		}
	case FieldStringMap:
		entries, ok := v.(map[string]any)
		if !ok {
			if sm, ok2 := v.(map[string]string); ok2 {
				entries = make(map[string]any, len(sm))
				for k, s := range sm {
					entries[k] = s
				}
			} else {
				return fmt.Errorf("must be an object of strings")
			}
		}
		if f.MaxItems > 0 && len(entries) > f.MaxItems {
			return fmt.Errorf("at most %d entries", f.MaxItems)
		}
		for _, k := range sortedKeys(entries) {
			if k == "" {
				return fmt.Errorf("keys must not be empty")
			}
			if f.KeyPattern != "" {
				re, err := compilePattern(f.KeyPattern)
				if err != nil {
					return fmt.Errorf("invalid key_pattern: %w", err)
				}
				if !re.MatchString(k) {
					return fmt.Errorf("key %q must match %s", k, f.KeyPattern)
				}
			}
			s, ok := entries[k].(string)
			if !ok {
				return fmt.Errorf("%q must be a string", k)
			}
			if err := validateString(f, s); err != nil {
				return fmt.Errorf("%q: %w", k, err)
			}
		}
	case FieldDatetime:
		t, err := ParseDatetime(v)
		if err != nil {
			return err
		}
		if f.Min != nil {
			if lo, err := ParseDatetime(f.Min); err == nil && t.Before(lo) {
				return fmt.Errorf("must not be before %v", f.Min)
			}
		}
		if f.Max != nil {
			if hi, err := ParseDatetime(f.Max); err == nil && t.After(hi) {
				return fmt.Errorf("must not be after %v", f.Max)
			}
		}
	case FieldEnumList:
		items, err := stringItems(v)
		if err != nil {
			return err
		}
		if f.Required && len(items) == 0 {
			return fmt.Errorf("at least one option is required")
		}
		if f.MaxItems > 0 && len(items) > f.MaxItems {
			return fmt.Errorf("at most %d items", f.MaxItems)
		}
		seen := make(map[string]bool, len(items))
		for _, s := range items {
			if !hasOption(f, s) {
				return fmt.Errorf("%q is not one of the declared options", s)
			}
			if seen[s] {
				return fmt.Errorf("%q is listed twice", s)
			}
			seen[s] = true
		}
	case FieldFile:
		s, ok := v.(string)
		if !ok || s == "" {
			return fmt.Errorf("must be a file id")
		}
		if len(s) > MaxFileIDLength {
			return fmt.Errorf("file id longer than %d characters", MaxFileIDLength)
		}
	default:
		return fmt.Errorf("unknown type %q", f.Type)
	}
	return nil
}

// stringItems reads an enum_list value: a JSON array of strings.
func stringItems(v any) ([]string, error) {
	switch items := v.(type) {
	case []string:
		return items, nil
	case []any:
		out := make([]string, len(items))
		for i, it := range items {
			s, ok := it.(string)
			if !ok {
				return nil, fmt.Errorf("item %d must be a string", i)
			}
			out[i] = s
		}
		return out, nil
	}
	return nil, fmt.Errorf("must be a list of option values")
}

func hasOption(f FieldSpec, value string) bool {
	for _, o := range f.Options {
		if o.Value == value {
			return true
		}
	}
	return false
}

// ValidateFieldValues checks a set of values against fields: every key must
// be declared, every value valid, every required field set. It returns the
// problems by key (nil when there are none); an undeclared key is reported
// under its own name.
func ValidateFieldValues(fields []FieldSpec, values map[string]any) map[string]string {
	return validateValues(fields, values, "not a declared field")
}

func validateValues(fields []FieldSpec, values map[string]any, undeclared string) map[string]string {
	var problems map[string]string
	add := func(key, reason string) {
		if problems == nil {
			problems = map[string]string{}
		}
		problems[key] = reason
	}
	declared := make(map[string]bool, len(fields))
	for _, f := range fields {
		declared[f.Key] = true
	}
	for key := range values {
		if !declared[key] {
			add(key, undeclared)
		}
	}
	for _, f := range fields {
		v, ok := values[f.Key]
		if !ok || v == nil {
			if f.Required && f.Default == nil {
				add(f.Key, "value is required")
			}
			continue
		}
		if err := ValidateValue(f, v); err != nil {
			add(f.Key, err.Error())
		}
	}
	return problems
}

// ResolveFieldValues returns values over the fields' defaults, for declared
// fields only: what a module is given, so it never interprets a schema.
// Fields with neither a value nor a default are left out.
func ResolveFieldValues(fields []FieldSpec, values map[string]any) map[string]any {
	out := make(map[string]any, len(fields))
	for _, f := range fields {
		if v, ok := values[f.Key]; ok && v != nil {
			out[f.Key] = v
		} else if f.Default != nil {
			out[f.Key] = f.Default
		}
	}
	return out
}

func validateString(f FieldSpec, s string) error {
	if f.MaxLength > 0 && len(s) > f.MaxLength {
		return fmt.Errorf("longer than %d characters", f.MaxLength)
	}
	if f.Required && s == "" {
		return fmt.Errorf("value is required")
	}
	if f.Pattern != "" {
		re, err := compilePattern(f.Pattern)
		if err != nil {
			return fmt.Errorf("invalid pattern: %w", err)
		}
		if !re.MatchString(s) {
			return fmt.Errorf("must match %s", f.Pattern)
		}
	}
	switch f.Type {
	case FieldURL:
		u, err := url.Parse(s)
		if err != nil || u.Scheme == "" || u.Host == "" {
			return fmt.Errorf("must be an absolute URL")
		}
	case FieldEnum:
		for _, o := range f.Options {
			if o.Value == s {
				return nil
			}
		}
		return fmt.Errorf("must be one of the declared options")
	}
	return nil
}

// sortedKeys orders map keys, so a map's first problem is reported the same
// way every time.
func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func toFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int32:
		return float64(n), true
	case int64:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	}
	return 0, false
}

func toDuration(v any) (time.Duration, error) {
	s, ok := v.(string)
	if !ok {
		return 0, fmt.Errorf("must be a duration string like \"30s\"")
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("must be a duration string like \"30s\"")
	}
	return d, nil
}

// ParseDuration parses a duration value as validated by ValidateValue.
func ParseDuration(v any) (time.Duration, error) { return toDuration(v) }

// ParseDatetime parses a datetime value as validated by ValidateValue: an
// RFC 3339 timestamp string.
func ParseDatetime(v any) (time.Time, error) {
	s, ok := v.(string)
	if ok {
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("must be an RFC 3339 timestamp like \"2027-01-31T00:00:00Z\"")
}

// AsInt64 converts a numeric value as validated by ValidateValue.
func AsInt64(v any) (int64, bool) {
	switch n := v.(type) {
	case int:
		return int64(n), true
	case int32:
		return int64(n), true
	case int64:
		return n, true
	case json.Number:
		// Exact for integers beyond float64's 2^53.
		if i, err := n.Int64(); err == nil {
			return i, true
		}
	}
	f, ok := toFloat(v)
	// 2^63 itself does not fit; float64 cannot say 2^63-1.
	if !ok || f != math.Trunc(f) || f < -(1<<63) || f >= 1<<63 {
		return 0, false
	}
	return int64(f), true
}

// patterns caches compiled FieldSpec patterns: a manifest's few patterns are
// checked on every config apply and action call.
var patterns sync.Map // string -> *regexp.Regexp

func compilePattern(p string) (*regexp.Regexp, error) {
	if re, ok := patterns.Load(p); ok {
		return re.(*regexp.Regexp), nil
	}
	re, err := regexp.Compile(p)
	if err != nil {
		return nil, err
	}
	patterns.Store(p, re)
	return re, nil
}
