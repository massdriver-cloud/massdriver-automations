package autodeploydownstream

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/massdriver-cloud/massdriver-automations/pkg/automation"
	"github.com/massdriver-cloud/massdriver-automations/pkg/massdriver"
)

var secret = []byte("0123456789abcdef0123456789abcdef")

func completedEvent(id, action, instance string) []byte {
	return completedEventIn(id, action, instance, "prod")
}

func completedEventIn(id, action, instance, environment string) []byte {
	b, _ := json.Marshal(map[string]any{
		"id":   id,
		"type": "deployment.completed",
		"data": map[string]any{
			"entity":     map[string]any{"id": "dep-1"},
			"attributes": map[string]any{"md-instance": instance, "md-environment": environment},
			"metadata":   map[string]any{"deployment_action": action},
		},
	})
	return b
}

func inst(id, status, autodeploy string, deps ...string) map[string]any {
	attrs := map[string]any{"md-environment": "prod"}
	if autodeploy != "" {
		attrs["autodeploy"] = autodeploy
	}
	if id == "api-prod-app" {
		attrs["tier"] = "frontend"
	}
	ds := []map[string]any{}
	for _, d := range deps {
		ds = append(ds, map[string]any{"resource": map[string]any{"id": d}})
	}
	return map[string]any{"id": id, "status": status, "params": map[string]any{"size": "large"}, "effectiveAttributes": attrs, "dependencies": ds}
}

// fakeAPI serves an organization where api-prod-db produces resource "db-writer".
//
//	api-prod-app      link to db-writer, autodeploy=true, tier=frontend   -> deployed
//	api-prod-worker   link to db-writer, no attribute                     -> skipped
//	api-prod-cache    no dependency on db-writer                          -> not downstream
//	api-prod-new      link to db-writer, autodeploy=true, INITIALIZED     -> skipped
//	etl-prod-job      remote reference to db-writer, autodeploy=true      -> deployed
//	ml-prod-train     environment default db-writer in ml-prod, opted in  -> deployed
type fakeAPI struct {
	mu       sync.Mutex
	deployed map[string]map[string]any
	proposed map[string]map[string]any
	stash    []map[string]any
}

func newFakeAPI() *fakeAPI {
	return &fakeAPI{deployed: map[string]map[string]any{}, proposed: map[string]map[string]any{}}
}

func (f *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Query     string         `json:"query"`
		Variables map[string]any `json:"variables"`
	}
	b, _ := io.ReadAll(r.Body)
	_ = json.Unmarshal(b, &req)

	var data any
	switch {
	case strings.Contains(req.Query, "mutation CreateDeployment"):
		f.mu.Lock()
		f.deployed[req.Variables["id"].(string)] = req.Variables["input"].(map[string]any)
		f.mu.Unlock()
		data = map[string]any{"createDeployment": map[string]any{"successful": true, "messages": []any{}, "result": map[string]any{"id": "new-dep"}}}
	case strings.Contains(req.Query, "mutation ProposeDeployment"):
		f.mu.Lock()
		f.proposed[req.Variables["id"].(string)] = req.Variables
		f.mu.Unlock()
		data = map[string]any{"proposeDeployment": map[string]any{"successful": true, "messages": []any{}, "result": map[string]any{"id": "proposed-dep"}}}
	case strings.Contains(req.Query, "mutation SetStashEntry"):
		f.mu.Lock()
		f.stash = append(f.stash, req.Variables)
		f.mu.Unlock()
		data = map[string]any{"alphaSetStashEntry": map[string]any{"successful": true, "messages": []any{}, "result": map[string]any{"key": req.Variables["key"]}}}
	case strings.Contains(req.Query, "query ResourceConsumers"):
		data = map[string]any{"resource": map[string]any{
			"remoteReferences":    map[string]any{"cursor": map[string]any{"next": nil}, "items": []any{map[string]any{"instance": inst("etl-prod-job", "PROVISIONED", "true", "db-writer")}}},
			"environmentDefaults": map[string]any{"cursor": map[string]any{"next": nil}, "items": []any{map[string]any{"environment": map[string]any{"id": "ml-prod"}}}},
		}}
	case strings.Contains(req.Query, "query EnvironmentInstances"):
		envs := map[string][]any{
			"api-prod": {
				inst("api-prod-db", "PROVISIONED", "true"),
				inst("api-prod-app", "PROVISIONED", "true", "db-writer"),
				inst("api-prod-worker", "PROVISIONED", "", "db-writer"),
				inst("api-prod-cache", "PROVISIONED", "true", "some-other-resource"),
				inst("api-prod-new", "INITIALIZED", "true", "db-writer"),
			},
			"ml-prod": {inst("ml-prod-train", "PROVISIONED", "true", "db-writer")},
		}
		data = map[string]any{"environment": map[string]any{"instances": envs[req.Variables["id"].(string)]}}
	case strings.Contains(req.Query, "query Upstream"):
		data = map[string]any{"instance": map[string]any{
			"environment": map[string]any{"id": "api-prod"},
			"resources":   []any{map[string]any{"resource": map[string]any{"id": "db-writer"}}},
		}}
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
}

func post(t *testing.T, h http.Handler, body []byte, signed bool) int {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, automation.HookPath, strings.NewReader(string(body)))
	if signed {
		var ev struct {
			ID string `json:"id"`
		}
		_ = json.Unmarshal(body, &ev)
		automation.SignRequest(req, secret, ev.ID, body, time.Now())
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code
}

func TestDeploysOptedInProvisionedDependents(t *testing.T) {
	api := newFakeAPI()
	srv := httptest.NewServer(api)
	defer srv.Close()

	a := New(&massdriver.Client{Endpoint: srv.URL, OrganizationID: "acme", APIKey: "md_test"}, Config{Rules: configRules(t, "")})
	a.run = func(f func()) { f() }

	if code := post(t, automation.Handler(a, secret), completedEvent("evt-1", "PROVISION", "api-prod-db"), true); code != http.StatusNoContent {
		t.Fatalf("status = %d", code)
	}

	if len(api.deployed) != 3 {
		t.Fatalf("deployed %d instances, want 3: %v", len(api.deployed), api.deployed)
	}
	for _, id := range []string{"api-prod-app", "etl-prod-job", "ml-prod-train"} {
		input, ok := api.deployed[id]
		if !ok {
			t.Fatalf("%s was not deployed", id)
		}
		if input["action"] != "PROVISION" || input["params"] != `{"size":"large"}` {
			t.Errorf("%s input = %v", id, input)
		}
		if input["message"] != "Auto-deploy (rule autodeploy=true): upstream api-prod-db completed deployment dep-1" {
			t.Errorf("%s message = %q", id, input["message"])
		}
	}

	if len(api.stash) != 3 {
		t.Fatalf("stash writes = %d, want one per deployment", len(api.stash))
	}
	for _, v := range api.stash {
		scope, _ := json.Marshal(v["scope"])
		input := v["input"].(map[string]any)
		var step map[string]any
		_ = json.Unmarshal([]byte(input["value"].(string)), &step)
		if string(scope) != `{"deployment":"new-dep"}` || v["key"] != "md/v1/step/autodeploy" || input["label"] != "Autodeploy" ||
			step["tool"] != "autodeploy-downstream" || step["status"] != "succeeded" ||
			step["message"] != "Deployed by rule autodeploy=true after upstream api-prod-db completed deployment dep-1" {
			t.Errorf("stash write = %v", v)
		}
	}
}

func TestIgnoresOtherActionsAndUnsignedRequests(t *testing.T) {
	api := newFakeAPI()
	srv := httptest.NewServer(api)
	defer srv.Close()

	a := New(&massdriver.Client{Endpoint: srv.URL, OrganizationID: "acme", APIKey: "md_test"}, Config{Rules: configRules(t, "")})
	a.run = func(f func()) { f() }
	h := automation.Handler(a, secret)

	if code := post(t, h, completedEvent("evt-a", "PROVISION", "api-prod-db"), false); code != http.StatusUnauthorized {
		t.Errorf("unsigned status = %d, want 401", code)
	}
	if code := post(t, h, completedEvent("evt-b", "PLAN", "api-prod-db"), true); code != http.StatusNoContent {
		t.Errorf("plan status = %d, want 204", code)
	}
	if code := post(t, h, completedEvent("evt-c", "DECOMMISSION", "api-prod-db"), true); code != http.StatusNoContent {
		t.Errorf("decommission status = %d, want 204", code)
	}
	if len(api.deployed) != 0 {
		t.Fatalf("deployed %v, want nothing", api.deployed)
	}
}

func TestRedeliveredEventDeploysOnce(t *testing.T) {
	api := newFakeAPI()
	srv := httptest.NewServer(api)
	defer srv.Close()

	a := New(&massdriver.Client{Endpoint: srv.URL, OrganizationID: "acme", APIKey: "md_test"}, Config{Rules: configRules(t, "")})
	calls := 0
	a.run = func(f func()) { calls++; f() }
	h := automation.Handler(a, secret)

	post(t, h, completedEvent("evt-1", "PROVISION", "api-prod-db"), true)
	post(t, h, completedEvent("evt-1", "PROVISION", "api-prod-db"), true)
	if calls != 1 {
		t.Fatalf("cascade ran %d times, want 1", calls)
	}
}

func configRules(t *testing.T, yaml string) []Rule {
	t.Helper()
	var cfg Config
	if err := automation.DecodeConfig([]byte(yaml), &cfg); err != nil {
		t.Fatal(err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	return cfg.Rules
}

func runCascade(t *testing.T, rules []Rule, event []byte) *fakeAPI {
	t.Helper()
	api := newFakeAPI()
	srv := httptest.NewServer(api)
	t.Cleanup(srv.Close)
	a := New(&massdriver.Client{Endpoint: srv.URL, OrganizationID: "acme", APIKey: "md_test"}, Config{Rules: rules})
	a.run = func(f func()) { f() }
	if code := post(t, automation.Handler(a, secret), event, true); code != http.StatusNoContent {
		t.Fatalf("status = %d", code)
	}
	return api
}

func deployedIDs(m map[string]map[string]any) []string {
	ids := make([]string, 0, len(m))
	for id := range m {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}

func TestRuleSelectsAnotherAttribute(t *testing.T) {
	rules := configRules(t, "rules:\n  - name: tier=frontend\n    downstream: {tier: frontend}\n")
	api := runCascade(t, rules, completedEvent("evt-1", "PROVISION", "api-prod-db"))

	if got := deployedIDs(api.deployed); !slices.Equal(got, []string{"api-prod-app"}) {
		t.Fatalf("deployed %v, want [api-prod-app]", got)
	}
	if msg := api.deployed["api-prod-app"]["message"]; msg != "Auto-deploy (rule tier=frontend): upstream api-prod-db completed deployment dep-1" {
		t.Errorf("message = %q", msg)
	}
}

func TestRuleNeedsEveryDownstreamAttribute(t *testing.T) {
	rules := configRules(t, `
rules:
  - name: prod-frontends
    downstream: {autodeploy: true, md-environment: prod, tier: frontend}
`)
	api := runCascade(t, rules, completedEvent("evt-1", "PROVISION", "api-prod-db"))

	if got := deployedIDs(api.deployed); !slices.Equal(got, []string{"api-prod-app"}) {
		t.Fatalf("deployed %v, want [api-prod-app]", got)
	}
}

func TestRuleListValueMatchesAnyOfThem(t *testing.T) {
	rules := configRules(t, `
rules:
  - name: any-opt-in
    downstream:
      autodeploy: ["true", "yes"]
      md-environment: [staging, prod]
`)
	api := runCascade(t, rules, completedEvent("evt-1", "PROVISION", "api-prod-db"))

	if got := deployedIDs(api.deployed); !slices.Equal(got, []string{"api-prod-app", "etl-prod-job", "ml-prod-train"}) {
		t.Fatalf("deployed %v", got)
	}
}

func TestRuleUpstreamFiltersTheEvent(t *testing.T) {
	rules := configRules(t, `
rules:
  - name: staging-cascade
    upstream: {md-environment: staging}
    downstream: {autodeploy: "true"}
`)
	api := runCascade(t, rules, completedEventIn("evt-1", "PROVISION", "api-prod-db", "prod"))
	if len(api.deployed) != 0 {
		t.Fatalf("prod event deployed %v, want nothing", deployedIDs(api.deployed))
	}

	api = runCascade(t, rules, completedEventIn("evt-2", "PROVISION", "api-prod-db", "staging"))
	if got := deployedIDs(api.deployed); !slices.Equal(got, []string{"api-prod-app", "etl-prod-job", "ml-prod-train"}) {
		t.Fatalf("staging event deployed %v", got)
	}
}

func TestFirstMatchingRuleWins(t *testing.T) {
	rules := configRules(t, `
rules:
  - name: review-frontends
    downstream: {tier: frontend}
    action: propose
  - name: deploy-the-rest
    downstream: {autodeploy: "true"}
    action: deploy
`)
	api := runCascade(t, rules, completedEvent("evt-1", "PROVISION", "api-prod-db"))

	if got := deployedIDs(api.proposed); !slices.Equal(got, []string{"api-prod-app"}) {
		t.Fatalf("proposed %v, want [api-prod-app]", got)
	}
	if got := deployedIDs(api.deployed); !slices.Equal(got, []string{"etl-prod-job", "ml-prod-train"}) {
		t.Fatalf("deployed %v, want [etl-prod-job ml-prod-train]", got)
	}
	if msg := api.deployed["etl-prod-job"]["message"]; msg != "Auto-deploy (rule deploy-the-rest): upstream api-prod-db completed deployment dep-1" {
		t.Errorf("message = %q", msg)
	}
}

func TestProposeSendsProposeDeployment(t *testing.T) {
	rules := configRules(t, `
rules:
  - name: review-frontends
    downstream: {tier: frontend}
    action: propose
`)
	api := runCascade(t, rules, completedEvent("evt-1", "PROVISION", "api-prod-db"))

	want := map[string]any{
		"org": "acme",
		"id":  "api-prod-app",
		"input": map[string]any{
			"action":  "PROVISION",
			"params":  `{"size":"large"}`,
			"message": "Auto-deploy (rule review-frontends): upstream api-prod-db completed deployment dep-1",
		},
	}
	if got := api.proposed["api-prod-app"]; !reflect.DeepEqual(got, want) {
		t.Fatalf("proposeDeployment variables = %v, want %v", got, want)
	}
	if len(api.deployed) != 0 {
		t.Fatalf("deployed %v, want nothing", deployedIDs(api.deployed))
	}
}

func TestStartRefusesBadConfig(t *testing.T) {
	t.Setenv("MASSDRIVER_ORGANIZATION_ID", "acme")
	t.Setenv("MASSDRIVER_API_KEY", "md_test")
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}

	cases := []struct {
		name string
		file string
		want string
	}{
		{"unknown key", write("unknown.yaml", "rules:\n  - name: all\n    downstream: {autodeploy: \"true\"}\n    when: always\n"), "line 4: field when not found"},
		{"no downstream", write("nodown.yaml", "rules:\n  - name: all\n    upstream: {md-environment: prod}\n"), "rules[0].downstream needs at least one attribute"},
		{"bad action", write("action.yaml", "rules:\n  - name: all\n    downstream: {autodeploy: \"true\"}\n    action: plan\n"), "rules[0].action: want deploy or propose"},
		{"two names", write("dup.yaml", "rules:\n  - name: a\n    downstream: {x: y}\n  - name: a\n    downstream: {x: z}\n"), "rules[1].name is used by an earlier rule"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("CONFIG_FILE", tc.file)
			_, err := Definition.New()
			var ce *automation.ConfigError
			if !errors.As(err, &ce) || !strings.Contains(err.Error(), tc.file+": ") || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want a config error for %s that contains %q", err, tc.file, tc.want)
			}
		})
	}
}

func TestNoConfigFileDeploysTheAutodeployAttribute(t *testing.T) {
	t.Setenv("MASSDRIVER_ORGANIZATION_ID", "acme")
	t.Setenv("MASSDRIVER_API_KEY", "md_test")
	t.Setenv("CONFIG_FILE", filepath.Join(t.TempDir(), "missing.yaml"))
	a, err := Definition.New()
	if err != nil {
		t.Fatal(err)
	}
	rules := a.(*Autodeploy).rules
	if len(rules) != 1 || rules[0].Name != "autodeploy=true" || !reflect.DeepEqual(rules[0].Downstream, map[string]Values{"autodeploy": {"true"}}) {
		t.Fatalf("rules = %+v", rules)
	}
}

func TestExampleConfigLoads(t *testing.T) {
	var cfg Config
	if err := automation.LoadConfigFile("config.example.yaml", &cfg); err != nil {
		t.Fatal(err)
	}
	if len(cfg.Rules) != 2 || cfg.Rules[1].Action != ActionPropose {
		t.Fatalf("rules = %+v", cfg.Rules)
	}
}
