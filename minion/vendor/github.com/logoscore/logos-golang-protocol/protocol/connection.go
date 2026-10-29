package protocol

import (
	"fmt"
	"sort"
)

// This file defines the minion connection schema: the parameters a minion
// needs to reach a channel (for an HTTP channel: its base URL, TLS and
// check-in settings). A channel declares the schema at module.register under
// "connection"; operators fill in values for each place the channel is
// reachable at (a channel endpoint), and minions are built with those values.
//
// The schema describes what a minion is given, not how the channel runs: the
// channel's own settings are its management properties. Values are validated
// with the same FieldSpec rules as properties.

// ConnectionSchemaV1 is the current connection schema format version.
const ConnectionSchemaV1 = "1"

// RegisterFieldConnection is the module.register key a channel declares its
// connection schema under.
const RegisterFieldConnection = "connection"

// MaxConnectionFields bounds a connection schema.
const MaxConnectionFields = 32

// ConnectionSchema is declared by a channel in module.register under
// "connection".
type ConnectionSchema struct {
	// SchemaVersion is the format of this structure (ConnectionSchemaV1).
	SchemaVersion string `json:"schema_version"`
	// Version is the version of the channel's parameter set, starting at 1.
	// A channel raises it when minions built against the previous set would
	// read the new one wrongly (a field removed, renamed or given a new
	// meaning). Adding an optional field does not need a new version.
	Version int         `json:"version"`
	Fields  []FieldSpec `json:"fields"`
}

// Field returns the field with key, if declared.
func (s ConnectionSchema) Field(key string) (FieldSpec, bool) {
	for _, f := range s.Fields {
		if f.Key == key {
			return f, true
		}
	}
	return FieldSpec{}, false
}

// ValidateConnectionSchema checks a schema's structure, limits, and that every
// declared default satisfies its own field spec.
func ValidateConnectionSchema(s ConnectionSchema) error {
	if s.SchemaVersion != ConnectionSchemaV1 {
		return fmt.Errorf("unsupported connection schema_version %q", s.SchemaVersion)
	}
	if s.Version < 1 {
		return fmt.Errorf("version must be at least 1")
	}
	if len(s.Fields) == 0 {
		return fmt.Errorf("at least one field is required")
	}
	if len(s.Fields) > MaxConnectionFields {
		return fmt.Errorf("too many fields (%d > %d)", len(s.Fields), MaxConnectionFields)
	}
	seen := map[string]bool{}
	for _, f := range s.Fields {
		if err := validateFieldSpec(f); err != nil {
			return fmt.Errorf("field %q: %w", f.Key, err)
		}
		if seen[f.Key] {
			return fmt.Errorf("duplicate field key %q", f.Key)
		}
		seen[f.Key] = true
	}
	return nil
}

// ValidateConnectionValues checks values against s: every key must be
// declared, every value valid, every required field set. It returns the
// problems by key (nil when there are none); an undeclared key is reported
// under its own name.
func ValidateConnectionValues(s ConnectionSchema, values map[string]any) map[string]string {
	return validateValues(s.Fields, values, "not a field of this channel's connection schema")
}

// ResolveConnectionValues is what a minion is given: values over the schema's
// defaults, for declared fields only. Fields with neither are left out.
func ResolveConnectionValues(s ConnectionSchema, values map[string]any) map[string]any {
	return ResolveFieldValues(s.Fields, values)
}

// SortedProblemKeys returns the keys of a problems map in a stable order, for
// messages.
func SortedProblemKeys(problems map[string]string) []string {
	keys := make([]string, 0, len(problems))
	for k := range problems {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
