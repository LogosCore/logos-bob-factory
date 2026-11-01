package protocol

import (
	"fmt"
	"math"
	"time"
)

// Task arguments (docs: contracts/minion-messages.md, Arguments): what core
// puts in a task's args and how a minion reads them back. Values arrive as
// JSON values (a duration "30s", a datetime in RFC 3339); a task carries each
// in its type's CBOR form.

// TaskArgs checks values against a command's params and returns the task's
// args: the values over the params' defaults, each in its type's CBOR form
// (a duration in milliseconds, a datetime in unix seconds). Problems come by
// param key, as ValidateFieldValues reports them; with any, args is nil.
func TaskArgs(params []FieldSpec, values map[string]any) (map[string]any, map[string]string) {
	if problems := ValidateFieldValues(params, values); problems != nil {
		return nil, problems
	}
	resolved := ResolveFieldValues(params, values)
	args := make(map[string]any, len(resolved))
	var problems map[string]string
	for _, f := range params {
		v, ok := resolved[f.Key]
		if !ok {
			continue
		}
		// A default is checked here: ValidateFieldValues sees only values.
		if err := ValidateValue(f, v); err != nil {
			problems = addProblem(problems, f.Key, err.Error())
			continue
		}
		cv, err := taskArg(f, v)
		if err != nil {
			problems = addProblem(problems, f.Key, err.Error())
			continue
		}
		args[f.Key] = cv
	}
	if problems != nil {
		return nil, problems
	}
	return args, nil
}

func addProblem(p map[string]string, key, reason string) map[string]string {
	if p == nil {
		p = map[string]string{}
	}
	p[key] = reason
	return p
}

// taskArg is a valid value in its type's CBOR form.
func taskArg(f FieldSpec, v any) (any, error) {
	switch f.Type {
	case FieldInt, FieldBytes:
		n, ok := AsInt64(v)
		if !ok {
			return nil, fmt.Errorf("must be an integer")
		}
		return n, nil
	case FieldNumber:
		n, ok := toFloat(v)
		if !ok {
			return nil, fmt.Errorf("must be a number")
		}
		return n, nil
	case FieldDuration:
		d, err := toDuration(v)
		if err != nil {
			return nil, err
		}
		if d < 0 {
			return nil, fmt.Errorf("must not be negative")
		}
		return uint64(d / time.Millisecond), nil
	case FieldDatetime:
		s, ok := v.(string)
		if !ok {
			return nil, fmt.Errorf("must be an RFC 3339 timestamp")
		}
		t, err := time.Parse(time.RFC3339, s)
		if err != nil {
			return nil, fmt.Errorf("must be an RFC 3339 timestamp")
		}
		return t.Unix(), nil
	case FieldStringList, FieldEnumList:
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
		return nil, fmt.Errorf("must be a list of strings")
	case FieldStringMap:
		switch m := v.(type) {
		case map[string]string:
			return m, nil
		case map[string]any:
			out := make(map[string]string, len(m))
			for k, it := range m {
				s, ok := it.(string)
				if !ok {
					return nil, fmt.Errorf("%q must be a string", k)
				}
				out[k] = s
			}
			return out, nil
		}
		return nil, fmt.Errorf("must be an object of strings")
	}
	// Text and bool travel as they are.
	return v, nil
}

// WireArgs are a task's args as it carries them: args (TaskArgs, by param
// key) by their params' numbers (FieldSpec.Num). An arg whose param has no
// number is left out.
func WireArgs(params []FieldSpec, args map[string]any) Args {
	if len(args) == 0 {
		return nil
	}
	out := make(Args, len(args))
	for _, p := range params {
		if v, ok := args[p.Key]; ok && p.Num > 0 {
			out[uint64(p.Num)] = v
		}
	}
	return out
}

// ArgString is a task's text arg (string, url, secret, enum): by number in
// a task's Args, or by key in args TaskArgs made.
func ArgString[K comparable](args map[K]any, key K) (string, bool) {
	s, ok := args[key].(string)
	return s, ok
}

// ArgInt is a task's integer arg (int, bytes; a datetime's unix seconds),
// whichever integer type the CBOR decoder gave. A float that is a whole
// number reads too: a core that kept the args in a document may send one.
func ArgInt[K comparable](args map[K]any, key K) (int64, bool) {
	switch n := args[key].(type) {
	case uint64:
		if n > 1<<63-1 {
			return 0, false
		}
		return int64(n), true
	case int64:
		return n, true
	case int:
		return int64(n), true
	case float64:
		if n != math.Trunc(n) || n > 1<<62 || n < -(1<<62) {
			return 0, false
		}
		return int64(n), true
	}
	return 0, false
}

// ArgDuration is a task's duration arg (milliseconds on the wire).
func ArgDuration[K comparable](args map[K]any, key K) (time.Duration, bool) {
	ms, ok := ArgInt(args, key)
	if !ok || ms < 0 {
		return 0, false
	}
	return time.Duration(ms) * time.Millisecond, true
}
