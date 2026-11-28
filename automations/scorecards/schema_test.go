package scorecards

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/massdriver-cloud/massdriver-automations/internal/widgetschema"
	"github.com/massdriver-cloud/massdriver-automations/pkg/massdriver"
)

// Every value written in the end-to-end tests matches the schema of its key in massdriver-cloud/massdriver#3881.
func TestWrittenValuesMatchTheCatalog(t *testing.T) {
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
		if err := widgetschema.Check(w.Scope.Kind, w.Key, "JSON", w.Value); err != nil {
			t.Errorf("%s on %s %s: %v\n%s", w.Key, w.Scope.Kind, w.Scope.ID, err, w.Value)
		}
		seen[w.Scope.Kind+" "+w.Key] = true
	}
	card := "md/v1/scorecard/production-readiness"
	for _, want := range []string{"instance " + KeyDelivery, "instance " + KeyReliability, "instance " + KeyCurrency,
		"instance " + KeyPending, "environment " + KeyParity, "environment " + KeyOwnership, "project " + KeyOwnership,
		"instance " + card, "environment " + card, "project " + card} {
		if !seen[want] {
			t.Errorf("nothing wrote %s", want)
		}
	}
}

// A roll-up is not an instance scorecard, and the other way around.
func TestScorecardShapesDependOnTheScope(t *testing.T) {
	card := "md/v1/scorecard/production-readiness"
	instance := `{"level":"silver","score":82,"passed":9,"failed":2,"unknown":1,"checks":[{"id":"no-drift","status":"passed"}],"evaluated_at":"2026-11-27T22:40:03Z"}`
	rollup := `{"level":"silver","instances":3,"levels":{"gold":1,"silver":2},"lowest_level":"silver","evaluated_at":"2026-11-27T22:40:03Z"}`
	if err := widgetschema.Check("instance", card, "JSON", instance); err != nil {
		t.Error(err)
	}
	for _, scope := range []string{"environment", "project"} {
		if err := widgetschema.Check(scope, card, "JSON", rollup); err != nil {
			t.Error(err)
		}
		if err := widgetschema.Check(scope, card, "JSON", instance); err == nil {
			t.Errorf("an instance scorecard on a %s: want an error", scope)
		}
	}
	if err := widgetschema.Check("instance", card, "JSON", rollup); err == nil {
		t.Error("a roll-up on an instance: want an error")
	}
	if err := widgetschema.Check("instance", KeyParity, "JSON", `{"reference_environment":"a","differences":0,"checked_at":"2026-11-27T22:40:03Z"}`); err == nil {
		t.Error("parity on an instance: want an error")
	}
	for name, v := range map[string]string{
		"score over 100":  strings.Replace(instance, `"score":82`, `"score":101`, 1),
		"empty message":   strings.Replace(instance, `"status":"passed"}`, `"status":"passed","message":""}`, 1),
		"bad status":      strings.Replace(instance, `"status":"passed"`, `"status":"skipped"`, 1),
		"not a date-time": strings.Replace(instance, `2026-11-27T22:40:03Z`, `yesterday`, 1),
		"extra field":     strings.Replace(instance, `"level"`, `"grade":"a","level"`, 1),
	} {
		if err := widgetschema.Check("instance", card, "JSON", v); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
	if err := widgetschema.Check("project", card, "JSON", strings.Replace(rollup, `"gold":1`, `"gold":-1`, 1)); err == nil {
		t.Error("negative level count: want an error")
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
		"ScorecardInstance": {`{"data":{"instance":null}}`},
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
