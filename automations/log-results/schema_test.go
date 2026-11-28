package logresults

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/massdriver-cloud/massdriver-automations/internal/widgetschema"
)

func TestExampleRulesWriteValuesTheWidgetCatalogAccepts(t *testing.T) {
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
				if err := widgetschema.Check("deployment", e.Key, e.Format, e.Value); err != nil {
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
	logs := "OpenTofu has been successfully initialized!\n│ Error: " + strings.Repeat("x", 600) + "\n"
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
