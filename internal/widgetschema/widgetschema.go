// Package widgetschema checks stash values against copies of the server's widget catalog, so tests catch a value
// that Massdriver would reject. widget-schemas.json is copied from massdriver's lib/massdriver/platform/widget_catalog.ex;
// update it when a widget's schema changes there.
package widgetschema

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"
)

//go:embed widget-schemas.json
var catalogJSON []byte

// Widget is one key of the widget catalog.
type Widget struct {
	Scope  string         `json:"scope"`
	Format string         `json:"format"`
	Schema map[string]any `json:"schema"`
}

var catalog = func() map[string]Widget {
	var c map[string]Widget
	if err := json.Unmarshal(catalogJSON, &c); err != nil {
		panic(err)
	}
	return c
}()

// A named segment, such as {step}, after regexp.QuoteMeta.
var keySegment = regexp.MustCompile(`\\\{[a-z_]+\\\}`)

// For returns the catalog key whose pattern matches key. A segment such as {step} is a bundle step path.
func For(key string) (Widget, bool) {
	for pattern, w := range catalog {
		re := "^" + keySegment.ReplaceAllString(regexp.QuoteMeta(pattern), `[a-z0-9][a-z0-9_-]{0,62}`) + "$"
		if regexp.MustCompile(re).MatchString(key) {
			return w, true
		}
	}
	return Widget{}, false
}

// Check validates a stash entry against the widget that reads its key.
func Check(scope, key, format, value string) error {
	w, ok := For(key)
	if !ok {
		return fmt.Errorf("no widget reads %s", key)
	}
	if w.Scope != scope || !strings.EqualFold(w.Format, format) {
		return fmt.Errorf("%s: the widget wants a %s entry on a %s", key, w.Format, w.Scope)
	}
	var v any
	if strings.EqualFold(format, "json") {
		if err := json.Unmarshal([]byte(value), &v); err != nil {
			return fmt.Errorf("%s: %w", key, err)
		}
	} else {
		v = value
	}
	return Validate(w.Schema, v, key)
}

// Validate checks the JSON Schema keywords that the catalog uses.
func Validate(schema map[string]any, v any, path string) error {
	if enum, ok := schema["enum"].([]any); ok && !slices.Contains(enum, v) {
		return fmt.Errorf("%s: %v is not one of %v", path, v, enum)
	}
	switch schema["type"] {
	case "object":
		m, ok := v.(map[string]any)
		if !ok {
			return fmt.Errorf("%s: want an object", path)
		}
		if mn, ok := schema["minProperties"].(float64); ok && float64(len(m)) < mn {
			return fmt.Errorf("%s: fewer than %v properties", path, mn)
		}
		props, _ := schema["properties"].(map[string]any)
		required, _ := schema["required"].([]any)
		for _, r := range required {
			if _, ok := m[r.(string)]; !ok {
				return fmt.Errorf("%s: %s is required", path, r)
			}
		}
		for k, x := range m {
			p, ok := props[k].(map[string]any)
			if !ok {
				if schema["additionalProperties"] == false {
					return fmt.Errorf("%s: %s is not allowed", path, k)
				}
				continue
			}
			if err := Validate(p, x, path+"."+k); err != nil {
				return err
			}
		}
	case "array":
		a, ok := v.([]any)
		if !ok {
			return fmt.Errorf("%s: want an array", path)
		}
		if mn, ok := schema["minItems"].(float64); ok && float64(len(a)) < mn {
			return fmt.Errorf("%s: fewer than %v items", path, mn)
		}
		if mx, ok := schema["maxItems"].(float64); ok && float64(len(a)) > mx {
			return fmt.Errorf("%s: more than %v items", path, mx)
		}
		for i, x := range a {
			if err := Validate(schema["items"].(map[string]any), x, fmt.Sprintf("%s[%d]", path, i)); err != nil {
				return err
			}
		}
	case "string":
		s, ok := v.(string)
		if !ok {
			return fmt.Errorf("%s: want a string, got %T", path, v)
		}
		n := float64(utf8.RuneCountInString(s))
		if mn, ok := schema["minLength"].(float64); ok && n < mn {
			return fmt.Errorf("%s: shorter than %v", path, mn)
		}
		if mx, ok := schema["maxLength"].(float64); ok && n > mx {
			return fmt.Errorf("%s: longer than %v", path, mx)
		}
		if p, ok := schema["pattern"].(string); ok && !pattern(p).MatchString(s) {
			return fmt.Errorf("%s: %q does not match %s", path, s, p)
		}
	case "integer", "number":
		f, ok := v.(float64)
		if !ok || (schema["type"] == "integer" && f != math.Trunc(f)) {
			return fmt.Errorf("%s: want an %s, got %v", path, schema["type"], v)
		}
		if mn, ok := schema["minimum"].(float64); ok && f < mn {
			return fmt.Errorf("%s: less than %v", path, mn)
		}
	case "boolean":
		if _, ok := v.(bool); !ok {
			return fmt.Errorf("%s: want a boolean", path)
		}
	}
	return nil
}

// pattern compiles a catalog pattern. The catalog ends a pattern with (?![\s\S]), which RE2 lacks; \z means the same.
func pattern(p string) *regexp.Regexp {
	return regexp.MustCompile(strings.ReplaceAll(p, `(?![\s\S])`, `\z`))
}
