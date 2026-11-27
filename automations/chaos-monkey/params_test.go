package chaosmonkey

import (
	"encoding/json"
	"math/rand/v2"
	"reflect"
	"strings"
	"testing"
)

const testSchema = `{
  "required": ["size", "replicas"],
  "examples": [
    {"__name": "Small", "size": "small", "replicas": 1, "network": {"public": false}},
    {"__name": "Large", "size": "large", "replicas": 5, "network": {"cidr": "10.0.0.0/16"}},
    {"replicas": 3}
  ],
  "properties": {
    "size": {"type": "string", "enum": ["small", "medium", "large"]},
    "replicas": {"type": "integer", "minimum": 1, "maximum": 9},
    "region": {"type": "string", "readOnly": true},
    "ratio": {"type": "number", "minimum": 0.1, "maximum": 0.9},
    "tier": {"type": "string", "oneOf": [{"const": "gold"}, {"const": "silver"}]},
    "debug": {"type": "boolean"},
    "tags": {"type": "array", "items": {"type": "string", "minLength": 2, "maxLength": 4}},
    "network": {
      "type": "object",
      "properties": {
        "public": {"type": "boolean"},
        "cidr": {"type": "string"}
      }
    }
  }
}`

func testMonkey(t *testing.T, seed uint64, cfgYAML string) *monkey {
	t.Helper()
	cfg, err := ParseConfig([]byte("match: {chaos: \"true\"}\n" + cfgYAML))
	if err != nil {
		t.Fatal(err)
	}
	return &monkey{r: rand.New(rand.NewPCG(seed, seed)), cfg: cfg}
}

func parseSchema(t *testing.T, s string) schema {
	t.Helper()
	var out schema
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestPresetsNameUnnamedOnesByPosition(t *testing.T) {
	ps := presets(parseSchema(t, testSchema))
	var names []string
	for _, p := range ps {
		names = append(names, p.Name)
		if _, ok := p.Params["__name"]; ok {
			t.Errorf("%s keeps __name", p.Name)
		}
	}
	if want := []string{"Small", "Large", "preset 3"}; !reflect.DeepEqual(names, want) {
		t.Errorf("names = %v, want %v", names, want)
	}
}

func TestPresetMergesOverCurrentParams(t *testing.T) {
	m := testMonkey(t, 1, "")
	s := parseSchema(t, testSchema)
	current := map[string]any{"region": "us-east-1", "network": map[string]any{"cidr": "10.1.0.0/16"}}
	for range 20 {
		ms := m.run(StrategyPreset, s, current)
		if ms.Params["region"] != "us-east-1" {
			t.Fatalf("preset dropped current params: %v", ms.Params)
		}
		if !strings.HasPrefix(ms.Detail, "preset ") {
			t.Errorf("detail = %q", ms.Detail)
		}
	}
	if current["network"].(map[string]any)["cidr"] != "10.1.0.0/16" {
		t.Error("the current params changed")
	}
}

func TestMergeAppliesAtLeastTwoPresets(t *testing.T) {
	m := testMonkey(t, 2, "")
	s := parseSchema(t, testSchema)
	for range 20 {
		ms := m.run(StrategyMerge, s, nil)
		if n := strings.Count(ms.Detail, " + "); n < 1 {
			t.Fatalf("detail = %q, want two or more presets", ms.Detail)
		}
	}
}

func TestMergeOfSmallAndLargeKeepsNestedKeysOfBoth(t *testing.T) {
	ps := presets(parseSchema(t, testSchema))
	got := deepMerge(deepMerge(map[string]any{}, ps[0].Params), ps[1].Params)
	want := map[string]any{"size": "large", "replicas": float64(5),
		"network": map[string]any{"public": false, "cidr": "10.0.0.0/16"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("merged = %v, want %v", got, want)
	}
}

func TestMutateStaysInSchemaAndSkipsReadOnly(t *testing.T) {
	m := testMonkey(t, 3, "mutate: {rate: 1}\n")
	s := parseSchema(t, testSchema)
	for range 200 {
		ms := m.run(StrategyMutate, s, map[string]any{"region": "us-east-1", "debug": true, "tags": []any{"ab", "cd"}})
		p := ms.Params
		if p["region"] != "us-east-1" {
			t.Fatalf("readOnly region changed to %v", p["region"])
		}
		if v := p["size"]; v != "small" && v != "medium" && v != "large" {
			t.Fatalf("size = %v", v)
		}
		if v := p["tier"]; v != "gold" && v != "silver" {
			t.Fatalf("tier = %v", v)
		}
		if v, ok := p["replicas"].(int64); !ok || v < 1 || v > 9 {
			t.Fatalf("replicas = %v (%T)", p["replicas"], p["replicas"])
		}
		if v, ok := p["ratio"].(float64); !ok || v < 0.1 || v > 0.9 {
			t.Fatalf("ratio = %v", p["ratio"])
		}
		if p["debug"] != false {
			t.Fatalf("debug = %v, want flipped", p["debug"])
		}
		if _, ok := p["network"].(map[string]any)["public"].(bool); !ok {
			t.Fatalf("network.public = %v", p["network"])
		}
		for _, tag := range p["tags"].([]any) {
			if s, ok := tag.(string); ok && (len(s) < 2 || len(s) > 4) {
				t.Fatalf("tag %q breaks its length", s)
			}
		}
		if strings.Contains(ms.Detail, "region") {
			t.Fatalf("detail names region: %q", ms.Detail)
		}
	}
}

func TestMutateAtATinyRateStillChangesOneField(t *testing.T) {
	m := testMonkey(t, 4, "mutate: {rate: 0.0001}\n")
	ms := m.run(StrategyMutate, parseSchema(t, testSchema), nil)
	if ms.Detail == "changed nothing" {
		t.Errorf("detail = %q", ms.Detail)
	}
}

func TestGibberishTouchesUpToMaxFields(t *testing.T) {
	m := testMonkey(t, 5, "gibberish: {max_fields: 2}\n")
	s := parseSchema(t, testSchema)
	for range 50 {
		ms := m.run(StrategyGibberish, s, nil)
		n := strings.Count(ms.Detail, ",") + 1
		if !strings.HasPrefix(ms.Detail, "garbled ") || n > 2 {
			t.Fatalf("detail = %q", ms.Detail)
		}
	}
}

func TestGibberishWithoutPropertiesAddsAKey(t *testing.T) {
	m := testMonkey(t, 6, "")
	ms := m.run(StrategyGibberish, schema{}, nil)
	if len(ms.Params) != 1 || !strings.HasPrefix(ms.Detail, "garbled chaos_") {
		t.Errorf("params = %v, detail = %q", ms.Params, ms.Detail)
	}
}

func TestMakeSkipsStrategiesThatDoNotFit(t *testing.T) {
	m := testMonkey(t, 7, "strategies: {preset: 5, merge: 5, mutate: 1}\n")
	noPresets := parseSchema(t, `{"properties": {"a": {"type": "boolean"}}}`)
	for range 20 {
		ms, ok := m.Make(noPresets, nil)
		if !ok || ms.Strategy != StrategyMutate {
			t.Fatalf("strategy = %q, ok = %v", ms.Strategy, ok)
		}
	}

	m = testMonkey(t, 8, "strategies: {merge: 1}\n")
	if _, ok := m.Make(noPresets, nil); ok {
		t.Error("merge ran without presets")
	}
}

func TestSeedRepeatsTheRun(t *testing.T) {
	s := parseSchema(t, testSchema)
	a, _ := testMonkey(t, 9, "").Make(s, nil)
	b, _ := testMonkey(t, 9, "").Make(s, nil)
	if !reflect.DeepEqual(a, b) {
		t.Errorf("same seed, different runs:\n%v\n%v", a, b)
	}
}
