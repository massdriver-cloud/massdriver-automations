package attributeenforcer

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/massdriver-cloud/massdriver-automations/pkg/automation"
	"github.com/massdriver-cloud/massdriver-automations/pkg/massdriver"
)

var secret = []byte("0123456789abcdef0123456789abcdef")

const configYAML = `
actions: [set_default, notify]
project:
  - key: TEAM
    default: platform
  - key: COST_CENTER
    allowed: [cc-100, cc-200]
environment:
  - key: TIER
    default: standard
    allowed: [standard, critical]
component:
  - key: OWNER
`

func createdEvent(id, typ string, entity, attributes map[string]any) []byte {
	b, _ := json.Marshal(map[string]any{
		"id":   id,
		"type": typ,
		"time": "2026-11-24T05:54:03.482913Z",
		"data": map[string]any{
			"entity":     entity,
			"attributes": attributes,
			"metadata":   map[string]any{},
			"actor":      map[string]any{"account_id": "0e6f2c1a-8b4d-4f7e-a3c5-1d9b7e2f4a60", "name": "Jane Doe", "organization_id": "5a2c8e4f-1b7d-4e3a-9c6f-8d0b2a4e6c17"},
		},
	})
	return b
}

type call struct {
	query string
	vars  map[string]any
}

// fakeAPI answers each read with the attributes in entities, keyed by identifier.
type fakeAPI struct {
	mu       sync.Mutex
	calls    []call
	entities map[string]map[string]any
	refuse   bool
}

func (f *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Query     string         `json:"query"`
		Variables map[string]any `json:"variables"`
	}
	b, _ := io.ReadAll(r.Body)
	_ = json.Unmarshal(b, &req)
	f.mu.Lock()
	f.calls = append(f.calls, call{query: req.Query, vars: req.Variables})
	f.mu.Unlock()

	id, _ := req.Variables["id"].(string)
	var data any
	switch {
	case strings.HasPrefix(req.Query, "mutation UpdateProjectAttributes"):
		payload := map[string]any{"successful": true, "messages": []any{}, "result": map[string]any{"id": id}}
		if f.refuse {
			payload = map[string]any{"successful": false, "messages": []any{map[string]any{"field": "attributes", "message": "TEAM is not a custom attribute"}}, "result": nil}
		}
		data = map[string]any{"updateProject": payload}
	case strings.HasPrefix(req.Query, "mutation UpdateEnvironmentAttributes"):
		data = map[string]any{"updateEnvironment": map[string]any{"successful": true, "messages": []any{}, "result": map[string]any{"id": id}}}
	case strings.HasPrefix(req.Query, "query ProjectAttributes"):
		data = map[string]any{"project": f.entities[id]}
	case strings.HasPrefix(req.Query, "query EnvironmentAttributes"):
		data = map[string]any{"environment": f.entities[id]}
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
}

type fakeSlack struct {
	mu    sync.Mutex
	texts []string
}

func (s *fakeSlack) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var msg struct {
		Text string `json:"text"`
	}
	_ = json.NewDecoder(r.Body).Decode(&msg)
	s.mu.Lock()
	s.texts = append(s.texts, msg.Text)
	s.mu.Unlock()
}

func setup(t *testing.T, cfgYAML string, api *fakeAPI) (http.Handler, *fakeSlack) {
	t.Helper()
	cfg, err := ParseConfig([]byte(cfgYAML))
	if err != nil {
		t.Fatal(err)
	}
	apiSrv := httptest.NewServer(api)
	slack := &fakeSlack{}
	slackSrv := httptest.NewServer(slack)
	t.Cleanup(apiSrv.Close)
	t.Cleanup(slackSrv.Close)

	e := New(cfg, &massdriver.Client{Endpoint: apiSrv.URL, OrganizationID: "acme", APIKey: "md_test"}, slackSrv.URL)
	e.run = func(f func()) { f() }
	return automation.Handler("attribute-enforcer", e, secret), slack
}

func deliver(t *testing.T, h http.Handler, body []byte) int {
	t.Helper()
	var ev struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(body, &ev)
	req := httptest.NewRequest(http.MethodPost, automation.HookPath, strings.NewReader(string(body)))
	automation.SignRequest(req, secret, ev.ID, body, time.Now())
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w.Code
}

func TestSetsMissingDefaultsAndReportsTheRest(t *testing.T) {
	api := &fakeAPI{entities: map[string]map[string]any{
		"api": {
			"attributes":          map[string]any{"COST_CENTER": "cc-999"},
			"effectiveAttributes": map[string]any{"COST_CENTER": "cc-999", "md-project": "api", "md-id": "api"},
		},
	}}
	h, slack := setup(t, configYAML, api)

	ev := createdEvent("evt-1", "project.created",
		map[string]any{"name": "API", "identifier": "api", "attributes": map[string]any{"COST_CENTER": "cc-999"}},
		map[string]any{"COST_CENTER": "cc-999", "md-project": "api", "md-id": "api"})
	if code := deliver(t, h, ev); code != http.StatusNoContent {
		t.Fatalf("status = %d", code)
	}
	if code := deliver(t, h, ev); code != http.StatusNoContent {
		t.Fatalf("redelivery status = %d", code)
	}

	if len(api.calls) != 2 {
		t.Fatalf("api calls = %d, want 2 (one read, one update)", len(api.calls))
	}
	if got := api.calls[0].vars; got["org"] != "acme" || got["id"] != "api" {
		t.Errorf("read vars = %v", got)
	}
	update := api.calls[1]
	if !strings.HasPrefix(update.query, "mutation UpdateProjectAttributes") || update.vars["org"] != "acme" || update.vars["id"] != "api" {
		t.Errorf("update = %s %v", update.query, update.vars)
	}
	input, _ := update.vars["input"].(map[string]any)
	if input["attributes"] != `{"COST_CENTER":"cc-999","TEAM":"platform"}` || len(input) != 1 {
		t.Errorf("update input = %v", input)
	}

	want := "Project `api` was created by Jane Doe without the required attributes.\n" +
		"• `COST_CENTER` is `cc-999`. Allowed: `cc-100`, `cc-200`.\n" +
		"• `TEAM` was missing. Set it to `platform`."
	if len(slack.texts) != 1 || slack.texts[0] != want {
		t.Errorf("slack = %q\nwant %q", slack.texts, want)
	}
}

func TestInheritedAttributeSatisfiesTheRule(t *testing.T) {
	api := &fakeAPI{}
	h, slack := setup(t, `
actions: [set_default, notify]
environment:
  - key: TEAM
    default: platform
`, api)

	ev := createdEvent("evt-2", "environment.created",
		map[string]any{"name": "prod", "identifier": "api-prod", "attributes": map[string]any{}},
		map[string]any{"TEAM": "payments", "md-project": "api", "md-environment": "prod", "md-id": "api-prod"})
	if code := deliver(t, h, ev); code != http.StatusNoContent {
		t.Fatalf("status = %d", code)
	}
	if len(api.calls) != 0 || len(slack.texts) != 0 {
		t.Errorf("api calls = %v, slack = %v, want none", api.calls, slack.texts)
	}
}

func TestKeySetAfterTheEventIsLeftAlone(t *testing.T) {
	api := &fakeAPI{entities: map[string]map[string]any{
		"api-prod": {
			"attributes":          map[string]any{"TIER": "critical"},
			"effectiveAttributes": map[string]any{"TIER": "critical", "md-project": "api", "md-environment": "prod"},
		},
	}}
	h, slack := setup(t, configYAML, api)

	ev := createdEvent("evt-3", "environment.created",
		map[string]any{"name": "prod", "identifier": "api-prod", "attributes": map[string]any{}},
		map[string]any{"md-project": "api", "md-environment": "prod", "md-id": "api-prod"})
	deliver(t, h, ev)

	if len(api.calls) != 1 || !strings.HasPrefix(api.calls[0].query, "query EnvironmentAttributes") {
		t.Errorf("api calls = %v, want one read", api.calls)
	}
	if len(slack.texts) != 0 {
		t.Errorf("slack = %q, want nothing", slack.texts)
	}
}

func TestNotifyOnlyNamesTheActorAndCallsNoAPI(t *testing.T) {
	api := &fakeAPI{}
	h, slack := setup(t, `
actions: [notify]
component:
  - key: OWNER
    default: platform
`, api)

	ev := createdEvent("evt-4", "component.created",
		map[string]any{"name": "database", "identifier": "api-database", "attributes": map[string]any{}},
		map[string]any{"md-project": "api", "md-component": "database", "md-repo": "aws-aurora", "md-id": "api-database"})
	deliver(t, h, ev)

	if len(api.calls) != 0 {
		t.Errorf("api calls = %v, want none", api.calls)
	}
	want := "Component `api-database` was created by Jane Doe without the required attributes.\n• `OWNER` is missing."
	if len(slack.texts) != 1 || slack.texts[0] != want {
		t.Errorf("slack = %q\nwant %q", slack.texts, want)
	}
}

func TestRefusedUpdateIsReported(t *testing.T) {
	api := &fakeAPI{refuse: true, entities: map[string]map[string]any{
		"web": {"attributes": map[string]any{}, "effectiveAttributes": map[string]any{"md-project": "web"}},
	}}
	h, slack := setup(t, `
actions: [set_default, notify]
project:
  - key: TEAM
    default: platform
`, api)

	deliver(t, h, createdEvent("evt-5", "project.cloned",
		map[string]any{"name": "Web", "identifier": "web", "attributes": map[string]any{}},
		map[string]any{"md-project": "web", "md-id": "web"}))

	want := "Project `web` was created by Jane Doe without the required attributes.\n" +
		"• `TEAM` is missing. Setting it to `platform` failed: attributes: TEAM is not a custom attribute"
	if len(slack.texts) != 1 || slack.texts[0] != want {
		t.Errorf("slack = %q\nwant %q", slack.texts, want)
	}
}

func TestIgnoresOtherEventTypes(t *testing.T) {
	api := &fakeAPI{}
	h, slack := setup(t, configYAML, api)

	ev := createdEvent("evt-6", "project.updated", map[string]any{"identifier": "api"}, map[string]any{"md-project": "api"})
	if code := deliver(t, h, ev); code != http.StatusNoContent {
		t.Fatalf("status = %d", code)
	}
	if len(api.calls) != 0 || len(slack.texts) != 0 {
		t.Errorf("api calls = %v, slack = %v, want none", api.calls, slack.texts)
	}
}

func TestParseConfigRefusesBadConfig(t *testing.T) {
	cases := map[string]string{
		"actions: [fix]":                   `actions: "fix" is not set_default or notify`,
		"project: []":                      "actions: list set_default, notify, or both",
		"actions: [notify]\nproject: [{}]": "project[0]: key is required",
		"actions: [notify]\nprojects: []":  "yaml: unmarshal errors:\n  line 2: field projects not found in type attributeenforcer.Config",
		"actions: [notify]\ncomponent: [{key: OWNER, default: x, allowed: [a]}]": `component[0]: default "x" is not in allowed`,
	}
	for in, want := range cases {
		if _, err := ParseConfig([]byte(in)); err == nil || err.Error() != want {
			t.Errorf("ParseConfig(%q) = %v, want %q", in, err, want)
		}
	}
}
