package autodeploydownstream

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

func completedEvent(id, action, instance string) []byte {
	b, _ := json.Marshal(map[string]any{
		"id":   id,
		"type": "deployment.completed",
		"data": map[string]any{
			"entity":     map[string]any{"id": "dep-1"},
			"attributes": map[string]any{"md-instance": instance},
			"metadata":   map[string]any{"deployment_action": action},
		},
	})
	return b
}

func inst(id, status, autodeploy string, deps ...string) map[string]any {
	attrs := map[string]any{}
	if autodeploy != "" {
		attrs["autodeploy"] = autodeploy
	}
	ds := []map[string]any{}
	for _, d := range deps {
		ds = append(ds, map[string]any{"resource": map[string]any{"id": d}})
	}
	return map[string]any{"id": id, "status": status, "params": map[string]any{"size": "large"}, "effectiveAttributes": attrs, "dependencies": ds}
}

// fakeAPI serves an organization where api-prod-db produces resource "db-writer".
//
//	api-prod-app      link to db-writer, autodeploy=true                  -> deployed
//	api-prod-worker   link to db-writer, no attribute                     -> skipped
//	api-prod-cache    no dependency on db-writer                          -> not downstream
//	api-prod-new      link to db-writer, autodeploy=true, INITIALIZED     -> skipped
//	etl-prod-job      remote reference to db-writer, autodeploy=true      -> deployed
//	ml-prod-train     environment default db-writer in ml-prod, opted in  -> deployed
type fakeAPI struct {
	mu       sync.Mutex
	deployed map[string]map[string]any
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
	api := &fakeAPI{deployed: map[string]map[string]any{}}
	srv := httptest.NewServer(api)
	defer srv.Close()

	a := New(&massdriver.Client{Endpoint: srv.URL, OrganizationID: "acme", APIKey: "md_test"}, "autodeploy", "true")
	a.run = func(f func()) { f() }

	if code := post(t, automation.Handler("autodeploy-downstream", a, secret), completedEvent("evt-1", "PROVISION", "api-prod-db"), true); code != http.StatusNoContent {
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
		if input["message"] != "Auto-deploy: upstream api-prod-db completed deployment dep-1" {
			t.Errorf("%s message = %q", id, input["message"])
		}
	}
}

func TestIgnoresOtherActionsAndUnsignedRequests(t *testing.T) {
	api := &fakeAPI{deployed: map[string]map[string]any{}}
	srv := httptest.NewServer(api)
	defer srv.Close()

	a := New(&massdriver.Client{Endpoint: srv.URL, OrganizationID: "acme", APIKey: "md_test"}, "autodeploy", "true")
	a.run = func(f func()) { f() }
	h := automation.Handler("autodeploy-downstream", a, secret)

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
	api := &fakeAPI{deployed: map[string]map[string]any{}}
	srv := httptest.NewServer(api)
	defer srv.Close()

	a := New(&massdriver.Client{Endpoint: srv.URL, OrganizationID: "acme", APIKey: "md_test"}, "autodeploy", "true")
	calls := 0
	a.run = func(f func()) { calls++; f() }
	h := automation.Handler("autodeploy-downstream", a, secret)

	post(t, h, completedEvent("evt-1", "PROVISION", "api-prod-db"), true)
	post(t, h, completedEvent("evt-1", "PROVISION", "api-prod-db"), true)
	if calls != 1 {
		t.Fatalf("cascade ran %d times, want 1", calls)
	}
}
