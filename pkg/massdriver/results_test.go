package massdriver

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

func TestResultKey(t *testing.T) {
	for prefix, want := range map[string]string{
		"":            "md/v1/step/rollback",
		"results/v1":  "results/v1/step/rollback",
		"results/v1/": "results/v1/step/rollback",
	} {
		if got := (Results{Prefix: prefix}).Key("step/rollback"); got != want {
			t.Errorf("prefix %q: got %q, want %q", prefix, got, want)
		}
	}
}

func TestResultWriteSendsALabeledJSONEntry(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Variables map[string]any `json:"variables"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		got = req.Variables
		_, _ = w.Write([]byte(`{"data":{"alphaSetStashEntry":{"successful":true,"messages":[],"result":{"key":"md/v1/drift"}}}}`))
	}))
	defer srv.Close()

	r := NewResults(&Client{Endpoint: srv.URL, OrganizationID: "acme", APIKey: "md_test"}, ResultsConfig{})
	r.Write(t.Context(), "instance", "api-prod-db", "drift", "Drift", Drift{Status: "none", CheckedAt: "2026-11-27T20:03:24Z"})

	want := map[string]any{
		"org":   "acme",
		"scope": map[string]any{"instance": "api-prod-db"},
		"key":   "md/v1/drift",
		"input": map[string]any{
			"format": "JSON",
			"label":  "Drift",
			"value":  `{"status":"none","resources":0,"checked_at":"2026-11-27T20:03:24Z"}`,
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("variables:\n got %v\nwant %v", got, want)
	}
}

func TestResultWriteSurvivesARejection(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		_, _ = w.Write([]byte(`{"data":{"alphaSetStashEntry":{"successful":false,"messages":[{"field":"key","message":"is reserved"}]}}}`))
	}))
	defer srv.Close()

	r := NewResults(&Client{Endpoint: srv.URL, OrganizationID: "acme", APIKey: "md_test"}, ResultsConfig{})
	r.Write(t.Context(), "deployment", "5c1de0a7-92b4-4f1e-8d3a-6e27c9f0b815", "source", "Source", Source{Repo: "acme/api"})
	if calls != 1 {
		t.Errorf("calls = %d, want 1 and no retry", calls)
	}

	Results{}.Write(t.Context(), "instance", "api-prod-db", "drift", "Drift", Drift{})
	r.Write(t.Context(), "deployment", "", "source", "Source", Source{})
	if calls != 1 {
		t.Errorf("calls = %d, want 1", calls)
	}
}
