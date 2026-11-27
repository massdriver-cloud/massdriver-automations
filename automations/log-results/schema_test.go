package logresults

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"
)

// widget is one key of the server's widget catalog, copied from massdriver's lib/massdriver/platform/widget_catalog.ex.
type widget struct {
	Scope  string         `json:"scope"`
	Format string         `json:"format"`
	Schema map[string]any `json:"schema"`
}

// A named segment, such as {step}, after regexp.QuoteMeta.
var keySegment = regexp.MustCompile(`\\\{[a-z_]+\\\}`)

// widgetFor returns the catalog key whose pattern matches key. A segment such as {step} is a bundle step path.
func widgetFor(catalog map[string]widget, key string) (widget, bool) {
	for pattern, w := range catalog {
		re := "^" + keySegment.ReplaceAllString(regexp.QuoteMeta(pattern), `[a-z0-9][a-z0-9_-]{0,62}`) + "$"
		if regexp.MustCompile(re).MatchString(key) {
			return w, true
		}
	}
	return widget{}, false
}

func TestExampleRulesWriteValuesTheWidgetCatalogAccepts(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("testdata", "widget-schemas.json"))
	if err != nil {
		t.Fatal(err)
	}
	var catalog map[string]widget
	if err := json.Unmarshal(b, &catalog); err != nil {
		t.Fatal(err)
	}
	cfg := exampleConfig(t)
	if cfg.Results.Prefix != "md/v1" {
		t.Fatalf("prefix = %s, want md/v1", cfg.Results.Prefix)
	}
	fixtures, _ := filepath.Glob(filepath.Join("testdata", "*.log"))
	written := 0
	for _, f := range fixtures {
		for _, d := range []Deployment{
			{Action: "PROVISION", Attributes: prod},
			{Action: "PLAN", Attributes: prod},
			{Action: "PROVISION", Failed: true, Attributes: prod},
		} {
			entries, _, err := cfg.Parse(t.Context(), strings.NewReader(fixture(t, filepath.Base(f))), d)
			if err != nil {
				t.Fatal(err)
			}
			for _, e := range entries {
				written++
				w, ok := widgetFor(catalog, e.Key)
				if !ok {
					t.Errorf("%s %s: no widget reads %s", f, e.Rule, e.Key)
					continue
				}
				if w.Scope != "deployment" || w.Format != e.Format {
					t.Errorf("%s %s: widget wants a %s entry on a %s", f, e.Rule, w.Format, w.Scope)
				}
				var v any
				if err := json.Unmarshal([]byte(e.Value), &v); err != nil {
					t.Fatal(err)
				}
				if err := validate(w.Schema, v, e.Key); err != nil {
					t.Errorf("%s %s: %v\n%s", f, e.Rule, err, e.Value)
				}
			}
		}
	}
	if written == 0 {
		t.Fatal("no entries")
	}
}

func TestLongMessagesFitTheStepWidget(t *testing.T) {
	cfg := exampleConfig(t)
	logs := "OpenTofu has been successfully initialized!\nError: " + strings.Repeat("x", 600) + "\n"
	entries, _, err := cfg.Parse(t.Context(), strings.NewReader(logs), Deployment{Action: "PROVISION", Failed: true, Attributes: prod})
	if err != nil {
		t.Fatal(err)
	}
	var step map[string]any
	_ = json.Unmarshal([]byte(entries[0].Value), &step)
	if n := utf8.RuneCountInString(step["message"].(string)); n != 280 {
		t.Fatalf("message has %d characters, want 280", n)
	}
}

// validate checks the JSON Schema keywords that the catalog's result keys use.
func validate(schema map[string]any, v any, path string) error {
	if enum, ok := schema["enum"].([]any); ok && !slices.Contains(enum, v) {
		return fmt.Errorf("%s: %v is not one of %v", path, v, enum)
	}
	switch schema["type"] {
	case "object":
		m, ok := v.(map[string]any)
		if !ok {
			return fmt.Errorf("%s: want an object", path)
		}
		props, _ := schema["properties"].(map[string]any)
		for _, r := range schema["required"].([]any) {
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
			if err := validate(p, x, path+"."+k); err != nil {
				return err
			}
		}
	case "array":
		a, ok := v.([]any)
		if !ok {
			return fmt.Errorf("%s: want an array", path)
		}
		if mx, ok := schema["maxItems"].(float64); ok && float64(len(a)) > mx {
			return fmt.Errorf("%s: more than %v items", path, mx)
		}
		for i, x := range a {
			if err := validate(schema["items"].(map[string]any), x, fmt.Sprintf("%s[%d]", path, i)); err != nil {
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
