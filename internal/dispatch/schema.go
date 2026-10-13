package dispatch

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
)

// ValidateArgs checks args against a JSON Schema. It implements the subset
// tool schemas use in practice, without a new dependency:
//
//	type (string or array), properties, required, additionalProperties
//	(bool or schema), items (single schema), enum, const,
//	minLength/maxLength, minimum/maximum, minItems/maxItems.
//
// Other keywords ($ref, oneOf, anyOf, allOf, pattern, format, ...) are
// ignored, i.e. treated permissively. Schema validation is a usability aid
// for the model, not a security boundary: handlers must still validate their
// input. An empty schema accepts anything.
func ValidateArgs(schema, args json.RawMessage) error {
	if len(bytes.TrimSpace(schema)) == 0 {
		return nil
	}
	var s map[string]any
	if err := json.Unmarshal(schema, &s); err != nil {
		// Registry already checked the schema is valid JSON; a non-object
		// schema (e.g. `true`) constrains nothing we understand.
		return nil
	}
	var v any
	dec := json.NewDecoder(bytes.NewReader(args))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		return fmt.Errorf("arguments are not valid JSON: %w", err)
	}
	return validate(s, v, "$")
}

func typeOf(v any) string {
	switch x := v.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case string:
		return "string"
	case json.Number:
		if f, err := x.Float64(); err == nil && f == math.Trunc(f) && !strings.ContainsAny(x.String(), ".eE") {
			return "integer"
		}
		return "number"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	}
	return "unknown"
}

func typeMatches(want, got string) bool {
	return want == got || (want == "number" && got == "integer")
}

func num(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case json.Number:
		f, err := x.Float64()
		return f, err == nil
	}
	return 0, false
}

func validate(s map[string]any, v any, path string) error {
	got := typeOf(v)
	switch t := s["type"].(type) {
	case string:
		if !typeMatches(t, got) {
			return fmt.Errorf("%s: expected %s, got %s", path, t, got)
		}
	case []any:
		ok := false
		var names []string
		for _, x := range t {
			if n, isStr := x.(string); isStr {
				names = append(names, n)
				ok = ok || typeMatches(n, got)
			}
		}
		if !ok && len(names) > 0 {
			return fmt.Errorf("%s: expected one of %s, got %s", path, strings.Join(names, ", "), got)
		}
	}
	if enum, ok := s["enum"].([]any); ok {
		match := false
		for _, e := range enum {
			if jsonEqual(e, v) {
				match = true
				break
			}
		}
		if !match {
			return fmt.Errorf("%s: value is not one of the allowed values", path)
		}
	}
	if c, ok := s["const"]; ok && !jsonEqual(c, v) {
		return fmt.Errorf("%s: value must equal the constant", path)
	}
	switch x := v.(type) {
	case string:
		n := float64(len([]rune(x)))
		if m, ok := num(s["minLength"]); ok && n < m {
			return fmt.Errorf("%s: shorter than %v characters", path, m)
		}
		if m, ok := num(s["maxLength"]); ok && n > m {
			return fmt.Errorf("%s: longer than %v characters", path, m)
		}
	case json.Number:
		f, _ := x.Float64()
		if m, ok := num(s["minimum"]); ok && f < m {
			return fmt.Errorf("%s: less than minimum %v", path, m)
		}
		if m, ok := num(s["maximum"]); ok && f > m {
			return fmt.Errorf("%s: greater than maximum %v", path, m)
		}
	case []any:
		n := float64(len(x))
		if m, ok := num(s["minItems"]); ok && n < m {
			return fmt.Errorf("%s: fewer than %v items", path, m)
		}
		if m, ok := num(s["maxItems"]); ok && n > m {
			return fmt.Errorf("%s: more than %v items", path, m)
		}
		if items, ok := s["items"].(map[string]any); ok {
			for i, el := range x {
				if err := validate(items, el, fmt.Sprintf("%s[%d]", path, i)); err != nil {
					return err
				}
			}
		}
	case map[string]any:
		if req, ok := s["required"].([]any); ok {
			for _, r := range req {
				if k, ok := r.(string); ok {
					if _, present := x[k]; !present {
						return fmt.Errorf("%s: missing required property %q", path, k)
					}
				}
			}
		}
		props, _ := s["properties"].(map[string]any)
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			p := path + "." + k
			if ps, ok := props[k].(map[string]any); ok {
				if err := validate(ps, x[k], p); err != nil {
					return err
				}
				continue
			}
			if _, declared := props[k]; declared {
				continue
			}
			switch ap := s["additionalProperties"].(type) {
			case bool:
				if !ap {
					return fmt.Errorf("%s: unexpected property %q", path, k)
				}
			case map[string]any:
				if err := validate(ap, x[k], p); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func jsonEqual(a, b any) bool {
	ab, err1 := json.Marshal(normalize(a))
	bb, err2 := json.Marshal(normalize(b))
	return err1 == nil && err2 == nil && bytes.Equal(ab, bb)
}

// normalize converts json.Number to float64 so 1 and 1.0 compare equal.
func normalize(v any) any {
	switch x := v.(type) {
	case json.Number:
		f, err := x.Float64()
		if err != nil {
			return x.String()
		}
		return f
	case []any:
		out := make([]any, len(x))
		for i := range x {
			out[i] = normalize(x[i])
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, val := range x {
			out[k] = normalize(val)
		}
		return out
	}
	return v
}
