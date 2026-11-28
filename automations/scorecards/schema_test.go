package scorecards

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/massdriver-cloud/massdriver-automations/pkg/massdriver"
)

// check validates v against the keywords that the widget catalog's scorecard schemas use.
func check(schema map[string]any, v any, path string) error {
	if enum, ok := schema["enum"].([]any); ok {
		for _, e := range enum {
			if reflect.DeepEqual(e, v) {
				return nil
			}
		}
		return fmt.Errorf("%s: %v is not in %v", path, v, enum)
	}
	switch schema["type"] {
	case "object":
		m, ok := v.(map[string]any)
		if !ok {
			return fmt.Errorf("%s: not an object", path)
		}
		for _, r := range asList(schema["required"]) {
			if _, ok := m[r.(string)]; !ok {
				return fmt.Errorf("%s.%s: required", path, r)
			}
		}
		if n, ok := schema["maxProperties"].(float64); ok && float64(len(m)) > n {
			return fmt.Errorf("%s: too many properties", path)
		}
		props, _ := schema["properties"].(map[string]any)
		for k, x := range m {
			if names, ok := schema["propertyNames"].(map[string]any); ok {
				if err := check(names, k, path+" name"); err != nil {
					return err
				}
			}
			switch p := props[k].(type) {
			case map[string]any:
				if err := check(p, x, path+"."+k); err != nil {
					return err
				}
				continue
			}
			switch extra := schema["additionalProperties"].(type) {
			case bool:
				return fmt.Errorf("%s.%s: not allowed", path, k)
			case map[string]any:
				if err := check(extra, x, path+"."+k); err != nil {
					return err
				}
			}
		}
	case "array":
		list, ok := v.([]any)
		if !ok {
			return fmt.Errorf("%s: not an array", path)
		}
		if n, ok := schema["maxItems"].(float64); ok && float64(len(list)) > n {
			return fmt.Errorf("%s: too many items", path)
		}
		for i, x := range list {
			if err := check(schema["items"].(map[string]any), x, fmt.Sprintf("%s[%d]", path, i)); err != nil {
				return err
			}
		}
	case "string":
		s, ok := v.(string)
		if !ok {
			return fmt.Errorf("%s: not a string", path)
		}
		n := float64(utf8.RuneCountInString(s))
		if m, ok := schema["minLength"].(float64); ok && n < m {
			return fmt.Errorf("%s: too short", path)
		}
		if m, ok := schema["maxLength"].(float64); ok && n > m {
			return fmt.Errorf("%s: too long", path)
		}
		if schema["format"] == "date-time" {
			if _, err := time.Parse(time.RFC3339, s); err != nil {
				return fmt.Errorf("%s: not a date-time", path)
			}
		}
	case "integer", "number":
		f, ok := v.(float64)
		if !ok || (schema["type"] == "integer" && f != math.Trunc(f)) {
			return fmt.Errorf("%s: not a %s", path, schema["type"])
		}
		if m, ok := schema["minimum"].(float64); ok && f < m {
			return fmt.Errorf("%s: below minimum", path)
		}
		if m, ok := schema["maximum"].(float64); ok && f > m {
			return fmt.Errorf("%s: above maximum", path)
		}
	case "boolean":
		if _, ok := v.(bool); !ok {
			return fmt.Errorf("%s: not a boolean", path)
		}
	}
	return nil
}

func asList(v any) []any {
	l, _ := v.([]any)
	return l
}

// Every value written in the end-to-end tests matches the schema of its key in massdriver-cloud/massdriver#3881.
func TestWrittenValuesMatchTheCatalog(t *testing.T) {
	b, err := os.ReadFile("testdata/schemas.json")
	if err != nil {
		t.Fatal(err)
	}
	var schemas map[string]map[string]any
	if err := json.Unmarshal(b, &schemas); err != nil {
		t.Fatal(err)
	}

	e := setup(t, readiness+`
parity:
  - {environment: billing-prod, reference: billing-staging}
`)
	prodApp(e)
	e.md.addInstance("billing", "prod", "ledger", nil)
	e.md.groups = []Group{{ID: "g1", Name: "Billing", Role: "CUSTOM"}}
	e.md.actions = map[string][]string{"billing-prod/g1": {"environment:deploy", "project:update"}}
	only := InstanceComparison{}
	only.Component.ID = "billing-cache"
	e.md.compare["billing-prod/billing-staging"] = []InstanceComparison{only}
	e.s.Pass()
	e.s.Drain(context.Background())

	seen := map[string]bool{}
	for _, w := range e.md.written() {
		name := w.Key
		if strings.HasPrefix(w.Key, "md/v1/scorecard/") {
			name = "instance.scorecard"
			if w.Scope.Kind != "instance" {
				name = "scorecard-rollup"
			}
		}
		schema, ok := schemas[name]
		if !ok {
			t.Fatalf("no schema for %s", name)
		}
		var v any
		if err := json.Unmarshal([]byte(w.Value), &v); err != nil {
			t.Fatal(err)
		}
		if err := check(schema, v, "value"); err != nil {
			t.Errorf("%s on %s %s: %v\n%s", w.Key, w.Scope.Kind, w.Scope.ID, err, w.Value)
		}
		seen[name] = true
	}
	for name := range schemas {
		if !seen[name] {
			t.Errorf("nothing wrote %s", name)
		}
	}
}

// fakeGraphQL answers the operations of GraphQLAPI with fixed pages, and records the variables of each call.
type fakeGraphQL struct {
	mu    sync.Mutex
	calls []map[string]any
	pages map[string][]string
}

var opName = regexp.MustCompile(`(?:query|mutation) (\w+)`)

func (f *fakeGraphQL) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Query     string         `json:"query"`
		Variables map[string]any `json:"variables"`
	}
	b, _ := io.ReadAll(r.Body)
	_ = json.Unmarshal(b, &req)
	f.mu.Lock()
	defer f.mu.Unlock()
	op := opName.FindStringSubmatch(req.Query)[1]
	f.calls = append(f.calls, map[string]any{"op": op, "vars": req.Variables})
	pages := f.pages[op]
	n := 0
	for _, c := range f.calls {
		if c["op"] == op {
			n++
		}
	}
	_, _ = w.Write([]byte(pages[min(n, len(pages))-1]))
}

func TestGraphQLAPIPagesAndFilters(t *testing.T) {
	f := &fakeGraphQL{pages: map[string][]string{
		"ScorecardDeployments": {
			`{"data":{"deployments":{"cursor":{"next":"p2"},"items":[{"id":"d3","status":"COMPLETED","createdAt":"2026-11-26T00:00:00Z"}]}}}`,
			`{"data":{"deployments":{"cursor":{"next":"p3"},"items":[{"id":"d2","status":"FAILED","createdAt":"2026-11-20T00:00:00Z"},{"id":"d1","status":"COMPLETED","createdAt":"2026-10-01T00:00:00Z"}]}}}`,
		},
		"ScorecardAuditLogs": {
			`{"data":{"auditLogs":{"cursor":{"next":null},"items":[{"occurredAt":"2026-11-26T00:00:00Z","type":"deployment.created","data":{"entity":{"id":"d3"}}}]}}}`,
		},
		"ScorecardInstance": {`{"data":{"instance":null},"errors":[{"message":"Instance not found","path":["instance"]}]}`},
	}}
	srv := httptest.NewServer(f)
	defer srv.Close()
	api := &GraphQLAPI{Client: &massdriver.Client{Endpoint: srv.URL, OrganizationID: "tendwell-health", APIKey: "md_test"}}
	ctx := context.Background()

	deps, err := api.Deployments(ctx, "billing-prod-app", now.Add(-30*day))
	if err != nil {
		t.Fatal(err)
	}
	// The second page reaches a deployment older than the window, so a third page is not read.
	if len(deps) != 2 || deps[1].ID != "d2" || len(f.calls) != 2 {
		t.Errorf("deployments = %+v, calls = %d", deps, len(f.calls))
	}
	vars := f.calls[1]["vars"].(map[string]any)
	if vars["instance"] != "billing-prod-app" || vars["org"] != "tendwell-health" ||
		vars["cursor"].(map[string]any)["next"] != "p2" {
		t.Errorf("vars = %v", vars)
	}

	events, err := api.AuditEvents(ctx, "billing-prod-app", []string{"deployment.created"}, "SERVICE_ACCOUNT", now.Add(-30*day))
	if err != nil || len(events) != 1 || entityString(events[0].Data, "id") != "d3" {
		t.Fatalf("events = %+v, err = %v", events, err)
	}
	filter := f.calls[2]["vars"].(map[string]any)["filter"].(map[string]any)
	want := map[string]any{"type": map[string]any{"in": []any{"deployment.created"}},
		"occurredAt": map[string]any{"gte": "2026-10-28T12:00:00Z"}, "attributes": `{"md-instance":"billing-prod-app"}`,
		"actorType": map[string]any{"eq": "SERVICE_ACCOUNT"}}
	if !reflect.DeepEqual(filter, want) {
		t.Errorf("filter = %v", filter)
	}

	if _, err := api.Instance(ctx, "gone"); err != ErrNotFound {
		t.Errorf("missing instance: err = %v", err)
	}
}

func TestAuditLogLongerThanTheLimitIsAnError(t *testing.T) {
	f := &fakeGraphQL{pages: map[string][]string{
		"ScorecardAuditLogs": {`{"data":{"auditLogs":{"cursor":{"next":"more"},"items":[]}}}`},
	}}
	srv := httptest.NewServer(f)
	defer srv.Close()
	api := &GraphQLAPI{Client: &massdriver.Client{Endpoint: srv.URL, OrganizationID: "tendwell-health", APIKey: "md_test"}}
	// A cut list would undercount alarms, so reliability is skipped instead.
	if _, err := api.AuditEvents(context.Background(), "billing-prod-app", []string{"alarm_state.created"}, "", now); err != errTruncated {
		t.Errorf("err = %v", err)
	}
	if len(f.calls) != maxPages {
		t.Errorf("calls = %d, want %d", len(f.calls), maxPages)
	}
}
