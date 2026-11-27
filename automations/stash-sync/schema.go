package stashsync

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"
)

// widgetPrefix starts the keys that Massdriver reserves for widgets.
const widgetPrefix = "md/"

// catalog is the widget catalog by key.
type catalog map[string]Widget

func newCatalog(widgets []Widget) catalog {
	c := catalog{}
	for _, w := range widgets {
		c[w.Key] = w
	}
	return c
}

// prepare checks an md/ entry against its widget, and rounds the fields that the schema says are integers. It
// returns the data to encode.
func (c catalog) prepare(e Entry) (any, error) {
	w, ok := c[e.Key]
	if !ok {
		return nil, errors.New("no widget reads this key")
	}
	if w.Scope != e.Scope.Kind {
		return nil, fmt.Errorf("the key belongs in the stash of the %s, not of the %s", w.Scope, e.Scope.Kind)
	}
	if w.Format != e.Format {
		return nil, fmt.Errorf("the key needs format %s", w.Format)
	}
	data := e.Data
	if m, ok := data.(map[string]any); ok {
		data = roundIntegers(w.Schema, m)
	}
	if err := validateSchema(w.Schema, normalize(data), "value"); err != nil {
		return nil, err
	}
	return data, nil
}

// roundIntegers returns a copy of m with each number rounded whose property schema has type integer.
func roundIntegers(schema map[string]any, m map[string]any) map[string]any {
	props, _ := schema["properties"].(map[string]any)
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
		p, _ := props[k].(map[string]any)
		if f, ok := v.(float64); ok && p["type"] == "integer" {
			out[k] = math.Round(f)
		}
	}
	return out
}

// normalize turns a value into what json.Unmarshal would make of it, so the checks see JSON types.
func normalize(v any) any {
	b, err := json.Marshal(v)
	if err != nil {
		return v
	}
	var out any
	if err := json.Unmarshal(b, &out); err != nil {
		return v
	}
	return out
}

// validateSchema checks v against the JSON Schema keywords that the widget catalog uses: type, enum, const,
// minimum, maximum, exclusiveMinimum, exclusiveMaximum, minLength, maxLength, pattern, required, properties,
// additionalProperties, items, minItems, and maxItems. Other keywords, such as examples, do not constrain v.
func validateSchema(schema map[string]any, v any, path string) error {
	if t, ok := schema["type"]; ok && !hasType(t, v) {
		return fmt.Errorf("%s must be %s", path, typeNames(t))
	}
	if enum, ok := schema["enum"].([]any); ok {
		found := false
		for _, e := range enum {
			if reflect.DeepEqual(e, v) {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("%s must be one of %s", path, jsonList(enum))
		}
	}
	if c, ok := schema["const"]; ok && !reflect.DeepEqual(c, v) {
		return fmt.Errorf("%s must be %s", path, jsonList([]any{c}))
	}

	switch x := v.(type) {
	case float64:
		if n, ok := number(schema["minimum"]); ok && x < n {
			return fmt.Errorf("%s must be at least %g", path, n)
		}
		if n, ok := number(schema["maximum"]); ok && x > n {
			return fmt.Errorf("%s must be at most %g", path, n)
		}
		if n, ok := number(schema["exclusiveMinimum"]); ok && x <= n {
			return fmt.Errorf("%s must be more than %g", path, n)
		}
		if n, ok := number(schema["exclusiveMaximum"]); ok && x >= n {
			return fmt.Errorf("%s must be less than %g", path, n)
		}
	case string:
		length := float64(utf8.RuneCountInString(x))
		if n, ok := number(schema["minLength"]); ok && length < n {
			return fmt.Errorf("%s must have at least %g characters", path, n)
		}
		if n, ok := number(schema["maxLength"]); ok && length > n {
			return fmt.Errorf("%s must have at most %g characters", path, n)
		}
		if p, ok := schema["pattern"].(string); ok {
			if re := compilePattern(p); re != nil && !re.MatchString(x) {
				return fmt.Errorf("%s does not match the pattern", path)
			}
		}
	case map[string]any:
		if req, ok := schema["required"].([]any); ok {
			for _, k := range req {
				if name, _ := k.(string); name != "" {
					if _, ok := x[name]; !ok {
						return fmt.Errorf("%s.%s is required", path, name)
					}
				}
			}
		}
		props, _ := schema["properties"].(map[string]any)
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if p, ok := props[k].(map[string]any); ok {
				if err := validateSchema(p, x[k], path+"."+k); err != nil {
					return err
				}
				continue
			}
			switch extra := schema["additionalProperties"].(type) {
			case bool:
				if !extra {
					return fmt.Errorf("%s.%s is not allowed", path, k)
				}
			case map[string]any:
				if err := validateSchema(extra, x[k], path+"."+k); err != nil {
					return err
				}
			}
		}
	case []any:
		if n, ok := number(schema["minItems"]); ok && float64(len(x)) < n {
			return fmt.Errorf("%s must have at least %g items", path, n)
		}
		if n, ok := number(schema["maxItems"]); ok && float64(len(x)) > n {
			return fmt.Errorf("%s must have at most %g items", path, n)
		}
		if items, ok := schema["items"].(map[string]any); ok {
			for i, item := range x {
				if err := validateSchema(items, item, fmt.Sprintf("%s[%d]", path, i)); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func hasType(t any, v any) bool {
	switch t := t.(type) {
	case string:
		return isType(t, v)
	case []any:
		for _, x := range t {
			if s, ok := x.(string); ok && isType(s, v) {
				return true
			}
		}
	}
	return false
}

func isType(t string, v any) bool {
	switch t {
	case "string":
		_, ok := v.(string)
		return ok
	case "number":
		_, ok := v.(float64)
		return ok
	case "integer":
		f, ok := v.(float64)
		return ok && f == math.Trunc(f)
	case "boolean":
		_, ok := v.(bool)
		return ok
	case "object":
		_, ok := v.(map[string]any)
		return ok
	case "array":
		_, ok := v.([]any)
		return ok
	case "null":
		return v == nil
	}
	return false
}

func typeNames(t any) string {
	switch t := t.(type) {
	case string:
		return "a " + t
	case []any:
		parts := make([]string, 0, len(t))
		for _, x := range t {
			parts = append(parts, fmt.Sprint(x))
		}
		return "one of " + strings.Join(parts, ", ")
	}
	return fmt.Sprint(t)
}

func number(v any) (float64, bool) {
	f, ok := v.(float64)
	return f, ok
}

func jsonList(vs []any) string {
	parts := make([]string, 0, len(vs))
	for _, v := range vs {
		b, _ := json.Marshal(v)
		parts = append(parts, string(b))
	}
	return strings.Join(parts, ", ")
}

// compilePattern compiles an ECMA-262 pattern with Go's regexp. A negative lookahead for any character means
// the end of the text, which Go writes as \z. A pattern that Go cannot compile does not constrain the value:
// the server checks it.
func compilePattern(p string) *regexp.Regexp {
	p = strings.ReplaceAll(p, `(?![\s\S])`, `\z`)
	re, err := regexp.Compile(p)
	if err != nil {
		return nil
	}
	return re
}
