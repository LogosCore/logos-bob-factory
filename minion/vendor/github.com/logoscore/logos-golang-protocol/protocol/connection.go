package protocol

import (
	"errors"
	"fmt"
	"maps"
	"slices"
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

// ConnectionSchemaV2 is the connection schema format: the connection kinds
// the channel carries, each with its own fields (docs:
// contracts/channel-core-connections/, The connection schema by kind).
const ConnectionSchemaV2 = "2"

// RegisterFieldConnection is the module.register key a channel declares its
// connection schema under.
const RegisterFieldConnection = "connection"

// MaxConnectionFields bounds a connection kind's fields, and its target's.
const MaxConnectionFields = 32

// ConnectionSchema is declared by a channel in module.register under
// "connection": for each connection kind it carries, the fields a minion is
// given to reach it.
type ConnectionSchema struct {
	// SchemaVersion is the format of this structure (ConnectionSchemaV2).
	SchemaVersion string `json:"schema_version"`
	// Version is the version of the channel's parameter set, starting at 1.
	// A channel raises it when minions built against the previous set would
	// read the new one wrongly (a field removed, renamed or given a new
	// meaning). Adding an optional field does not need a new version.
	Version int `json:"version"`
	// Kinds are the connection kinds the channel carries, by kind
	// (KindReverseCall, ...), each with its own fields: two kinds that share
	// a field each declare it.
	Kinds map[string]ConnectionKind `json:"kinds"`
}

// ConnectionKind is what a connection of one kind is given: Fields, built
// into a minion (and kept as an endpoint), and, for a bind kind, Target,
// what operators fill in for each target.
type ConnectionKind struct {
	Fields []FieldSpec `json:"fields"`
	Target []FieldSpec `json:"target,omitempty"`
}

// knownKinds are the connection kinds, in the order kinds are listed.
var knownKinds = []string{KindReverseCall, KindReversePersistent, KindBindCall, KindBindPersistent}

// KnownKind reports whether kind is a connection kind.
func KnownKind(kind string) bool { return slices.Contains(knownKinds, kind) }

// SupportedKinds are the kinds the channel carries, in the kinds' order.
func (s ConnectionSchema) SupportedKinds() []string {
	var out []string
	for _, k := range knownKinds {
		if _, ok := s.Kinds[k]; ok {
			out = append(out, k)
		}
	}
	return out
}

// KindFields are the fields a connection of kind is built with; none for a
// kind the channel does not carry.
func (s ConnectionSchema) KindFields(kind string) []FieldSpec {
	return s.Kinds[kind].Fields
}

// ValidateConnectionSchema checks a schema's structure, limits, and that every
// declared default satisfies its own field spec: at least one kind, each a
// known kind with its fields (and, for a bind kind only, its target), keys
// unique within the kind.
func ValidateConnectionSchema(s ConnectionSchema) error {
	if s.SchemaVersion != ConnectionSchemaV2 {
		return fmt.Errorf("unsupported connection schema_version %q", s.SchemaVersion)
	}
	if s.Version < 1 {
		return fmt.Errorf("version must be at least 1")
	}
	if len(s.Kinds) == 0 {
		return errors.New("kinds: at least one")
	}
	for _, kind := range slices.Sorted(maps.Keys(s.Kinds)) {
		if !KnownKind(kind) {
			return fmt.Errorf("kinds: %q is not a connection kind", kind)
		}
		k := s.Kinds[kind]
		bind := kind == KindBindCall || kind == KindBindPersistent
		switch {
		case len(k.Target) > 0 && !bind:
			return fmt.Errorf("kinds.%s: only a bind kind has target fields", kind)
		case len(k.Fields) == 0 && len(k.Target) == 0:
			return fmt.Errorf("kinds.%s: at least one field", kind)
		case len(k.Fields) > MaxConnectionFields || len(k.Target) > MaxConnectionFields:
			return fmt.Errorf("kinds.%s: too many fields (%d at most)", kind, MaxConnectionFields)
		}
		keys := map[string]bool{}
		for _, f := range append(slices.Clone(k.Fields), k.Target...) {
			if err := validateFieldSpec(f); err != nil {
				return fmt.Errorf("kinds.%s: field %q: %w", kind, f.Key, err)
			}
			if keys[f.Key] {
				return fmt.Errorf("kinds.%s: duplicate field key %q", kind, f.Key)
			}
			keys[f.Key] = true
		}
	}
	return nil
}

// ValidateConnectionValues checks values against kind's fields in s: every
// key must be declared, every value valid, every required field set. It
// returns the problems by key (nil when there are none); an undeclared key
// is reported under its own name. A kind the channel does not carry is a
// problem under "".
func ValidateConnectionValues(s ConnectionSchema, kind string, values map[string]any) map[string]string {
	if _, ok := s.Kinds[kind]; !ok {
		return map[string]string{"": fmt.Sprintf("the channel does not carry %q connections", kind)}
	}
	return validateValues(s.KindFields(kind), values, "not a field of this kind of connection")
}

// ResolveConnectionValues is what a minion is given for a connection of
// kind: values over the kind's defaults, for its fields only. Fields with
// neither are left out.
func ResolveConnectionValues(s ConnectionSchema, kind string, values map[string]any) map[string]any {
	return ResolveFieldValues(s.KindFields(kind), values)
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
