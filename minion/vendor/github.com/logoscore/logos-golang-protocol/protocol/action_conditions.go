package protocol

import (
	"cmp"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"
)

// ConditionOp compares the current value of an action condition's subject
// with the condition's value.
type ConditionOp string

const (
	OpEq  ConditionOp = "eq"
	OpNe  ConditionOp = "ne"
	OpGt  ConditionOp = "gt"
	OpGte ConditionOp = "gte"
	OpLt  ConditionOp = "lt"
	OpLte ConditionOp = "lte"
	OpIn  ConditionOp = "in" // Value is a list; holds when any item is equal
)

// ActionCondition is one condition an action is available under: the
// current value of a status value or of a property, compared with Value.
// Only the module knows which of its states make an action pointless, so it
// says so, and core and the UI evaluate it without knowing the module.
type ActionCondition struct {
	// Status or Property names the subject: a status key or a property key
	// of the same manifest. Exactly one is set.
	Status   string      `json:"status,omitempty"`
	Property string      `json:"property,omitempty"`
	Op       ConditionOp `json:"op"`
	// Value is a value of the subject's type; for OpIn, a list of them.
	// gt, gte, lt and lte need an ordered type: int, number, bytes,
	// duration or datetime.
	Value any `json:"value"`
	// Message says why the action is unavailable while the condition does
	// not hold, e.g. "Already draining". Without one, a reason is made
	// from the subject's label. Optional.
	Message string `json:"message,omitempty"`
}

// comparableTypes are the types a condition can compare; orderedTypes also
// take gt, gte, lt and lte.
var (
	comparableTypes = map[FieldType]bool{
		FieldString: true, FieldInt: true, FieldNumber: true, FieldBool: true, FieldEnum: true,
		FieldDuration: true, FieldBytes: true, FieldURL: true, FieldDatetime: true,
	}
	orderedTypes = map[FieldType]bool{
		FieldInt: true, FieldNumber: true, FieldBytes: true, FieldDuration: true, FieldDatetime: true,
	}
)

// ActionAvailable reports whether action a of manifest m is available, given
// the module's status values and effective property values; when it is not,
// reason is the message of the first condition that does not hold, or one made
// from its subject's label. A condition holds while its subject's value is
// absent or not of its type: whoever knows the value decides.
func ActionAvailable(m ManagementManifest, a ActionSpec, status, values map[string]any) (ok bool, reason string) {
	for _, c := range a.AvailableWhen {
		typ, label, current, known := subject(m, c, status, values)
		if !known {
			continue
		}
		if holds, decided := evaluate(typ, c.Op, current, c.Value); !decided || holds {
			continue
		}
		if c.Message != "" {
			return false, c.Message
		}
		return false, describeCondition(label, typ, c)
	}
	return true, ""
}

// subject is the type, label and current value of what c compares, and
// whether that value is known.
func subject(m ManagementManifest, c ActionCondition, status, values map[string]any) (FieldType, string, any, bool) {
	if c.Status != "" {
		for _, s := range m.Status {
			if s.Key == c.Status {
				v := status[s.Key]
				return s.Type, labelOf(s.Label, s.Key), v, v != nil
			}
		}
		return "", "", nil, false
	}
	for _, p := range m.Properties {
		if p.Key == c.Property {
			v := values[p.Key]
			return p.Type, labelOf(p.Label, p.Key), v, v != nil
		}
	}
	return "", "", nil, false
}

// evaluate compares current with want under op; decided is false when either
// is not a value of typ.
func evaluate(typ FieldType, op ConditionOp, current, want any) (holds, decided bool) {
	cur, ok := conditionValue(typ, current)
	if !ok {
		return false, false
	}
	if op == OpIn {
		for _, item := range listItems(want) {
			if w, ok := conditionValue(typ, item); ok && compareValues(cur, w) == 0 {
				return true, true
			}
		}
		return false, true
	}
	w, ok := conditionValue(typ, want)
	if !ok {
		return false, false
	}
	c := compareValues(cur, w)
	switch op {
	case OpEq:
		return c == 0, true
	case OpNe:
		return c != 0, true
	}
	if !orderedTypes[typ] {
		return false, false
	}
	switch op {
	case OpGt:
		return c > 0, true
	case OpGte:
		return c >= 0, true
	case OpLt:
		return c < 0, true
	case OpLte:
		return c <= 0, true
	}
	return false, false
}

// conditionValue converts v to what values of typ compare as: int64 for int
// and bytes, float64 for number, time.Duration, time.Time, bool or string.
func conditionValue(typ FieldType, v any) (any, bool) {
	switch typ {
	case FieldInt, FieldBytes:
		return AsInt64(v)
	case FieldNumber:
		return toFloat(v)
	case FieldDuration:
		d, err := toDuration(v)
		return d, err == nil
	case FieldDatetime:
		t, err := ParseDatetime(v)
		return t, err == nil
	case FieldBool:
		b, ok := v.(bool)
		return b, ok
	case FieldString, FieldEnum, FieldURL:
		s, ok := v.(string)
		return s, ok
	}
	return nil, false
}

// compareValues orders two values converted by conditionValue for the same
// type. Values of an unordered type compare as 0 when equal and 1 otherwise.
func compareValues(a, b any) int {
	switch x := a.(type) {
	case int64:
		return cmp.Compare(x, b.(int64))
	case float64:
		return cmp.Compare(x, b.(float64))
	case time.Duration:
		return cmp.Compare(x, b.(time.Duration))
	case time.Time:
		return x.Compare(b.(time.Time))
	}
	if a == b {
		return 0
	}
	return 1
}

// listItems returns the items of a slice value, or nil for anything else.
func listItems(v any) []any {
	rv := reflect.ValueOf(v)
	if v == nil || rv.Kind() != reflect.Slice {
		return nil
	}
	out := make([]any, rv.Len())
	for i := range out {
		out[i] = rv.Index(i).Interface()
	}
	return out
}

// describeCondition is the reason an action is unavailable while c does not
// hold, when c has no message of its own.
func describeCondition(label string, typ FieldType, c ActionCondition) string {
	v := conditionText(c.Value)
	switch c.Op {
	case OpEq:
		return "Requires " + label + ": " + v
	case OpNe:
		return "Requires " + label + " other than " + v
	case OpGt:
		return "Requires " + label + " above " + v
	case OpGte:
		return "Requires " + label + " of at least " + v
	case OpLt:
		return "Requires " + label + " below " + v
	case OpLte:
		return "Requires " + label + " of at most " + v
	case OpIn:
		return "Requires " + label + ": one of " + v
	}
	return "Unavailable"
}

// conditionText renders a condition value for a reason: booleans as Yes and
// No, as the UI shows them, and lists comma-separated.
func conditionText(v any) string {
	if items := listItems(v); items != nil {
		parts := make([]string, len(items))
		for i, it := range items {
			parts[i] = conditionText(it)
		}
		return strings.Join(parts, ", ")
	}
	switch x := v.(type) {
	case bool:
		if x {
			return "Yes"
		}
		return "No"
	case string:
		return x
	}
	return fmt.Sprint(v)
}

func labelOf(label, key string) string {
	if label != "" {
		return label
	}
	return key
}

// validateConditions checks an action's conditions against manifest m.
func validateConditions(m ManagementManifest, conds []ActionCondition) error {
	if len(conds) > MaxActionConditions {
		return fmt.Errorf("too many conditions (%d > %d)", len(conds), MaxActionConditions)
	}
	for i, c := range conds {
		if err := validateCondition(m, c); err != nil {
			return fmt.Errorf("available_when[%d]: %w", i, err)
		}
	}
	return nil
}

func validateCondition(m ManagementManifest, c ActionCondition) error {
	var spec FieldSpec
	switch {
	case c.Status != "" && c.Property != "":
		return errors.New("set status or property, not both")
	case c.Status != "":
		i := indexOf(m.Status, func(s StatusSpec) bool { return s.Key == c.Status })
		if i < 0 {
			return fmt.Errorf("status %q is not declared", c.Status)
		}
		spec = FieldSpec{Key: c.Status, Type: m.Status[i].Type}
	case c.Property != "":
		i := indexOf(m.Properties, func(p PropertySpec) bool { return p.Key == c.Property })
		if i < 0 {
			return fmt.Errorf("property %q is not declared", c.Property)
		}
		spec = m.Properties[i].FieldSpec
	default:
		return errors.New("status or property is required")
	}
	if !comparableTypes[spec.Type] {
		return fmt.Errorf("%s values cannot be compared", spec.Type)
	}
	switch c.Op {
	case OpEq, OpNe, OpIn:
	case OpGt, OpGte, OpLt, OpLte:
		if !orderedTypes[spec.Type] {
			return fmt.Errorf("op %q needs an ordered type, not %s", c.Op, spec.Type)
		}
	default:
		return fmt.Errorf("unknown op %q", c.Op)
	}
	values := []any{c.Value}
	if c.Op == OpIn {
		values = listItems(c.Value)
		if len(values) == 0 || len(values) > MaxConditionValues {
			return fmt.Errorf(`op "in" needs a list of 1 to %d values`, MaxConditionValues)
		}
	}
	for _, v := range values {
		if _, ok := conditionValue(spec.Type, v); !ok {
			return fmt.Errorf("value %v is not a %s value", v, spec.Type)
		}
		if spec.Type == FieldEnum && len(spec.Options) > 0 &&
			indexOf(spec.Options, func(o EnumOption) bool { return o.Value == v }) < 0 {
			return fmt.Errorf("value %v is not one of the options", v)
		}
	}
	if len(c.Message) > MaxLabelLength {
		return fmt.Errorf("message longer than %d", MaxLabelLength)
	}
	return nil
}

func indexOf[T any](items []T, match func(T) bool) int {
	for i, it := range items {
		if match(it) {
			return i
		}
	}
	return -1
}
