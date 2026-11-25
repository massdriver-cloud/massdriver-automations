package freezeguard

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

const windowsYAML = `
timezone: America/New_York
windows:
  - name: weekend
    match:
      md-environment: prod
    weekly:
      - from: Fri 17:00
        to: Mon 08:00
  - name: thanksgiving
    match:
      md-environment: prod
    from: 2026-11-26
    to: 2026-11-27
`

type fakeAPI struct {
	mu      sync.Mutex
	aborted []map[string]any
	refuse  bool
}

func (f *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Query     string         `json:"query"`
		Variables map[string]any `json:"variables"`
	}
	b, _ := io.ReadAll(r.Body)
	_ = json.Unmarshal(b, &req)
	if !strings.Contains(req.Query, "mutation AbortDeployment") {
		http.Error(w, "unexpected query", http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	f.aborted = append(f.aborted, req.Variables)
	f.mu.Unlock()
	payload := map[string]any{"successful": true, "messages": []any{}, "result": map[string]any{"status": "ABORTED"}}
	if f.refuse {
		payload = map[string]any{"successful": false, "messages": []any{map[string]any{"field": "status", "message": "is invalid"}}}
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"abortDeployment": payload}})
}

type fakeWebhook struct {
	mu    sync.Mutex
	texts []string
}

func (f *fakeWebhook) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var body map[string]string
	_ = json.NewDecoder(r.Body).Decode(&body)
	f.mu.Lock()
	f.texts = append(f.texts, body["text"])
	f.mu.Unlock()
	_, _ = w.Write([]byte("ok"))
}

type deploymentOpts struct {
	eventID, eventType, at, environment, status, action, message string
}

func event(o deploymentOpts) []byte {
	if o.eventType == "" {
		o.eventType = "deployment.enqueued"
	}
	if o.status == "" {
		o.status = "PENDING"
	}
	if o.action == "" {
		o.action = "PROVISION"
	}
	b, _ := json.Marshal(map[string]any{
		"id":   o.eventID,
		"type": o.eventType,
		"time": o.at,
		"data": map[string]any{
			"entity": map[string]any{
				"id": "b9dc3d18-19fc-429c-8d36-6753615fe17c", "status": o.status, "action": o.action,
				"version": "1.2.3", "message": o.message,
			},
			"attributes": map[string]any{"md-environment": o.environment, "md-instance": "api-" + o.environment + "-database"},
			"metadata":   map[string]any{"deployment_action": o.action},
		},
	})
	return b
}

type harness struct {
	handler http.Handler
	api     *fakeAPI
	webhook *fakeWebhook
}

func setup(t *testing.T) *harness {
	t.Helper()
	windows, err := ParseWindows([]byte(windowsYAML))
	if err != nil {
		t.Fatal(err)
	}
	api, hook := &fakeAPI{}, &fakeWebhook{}
	apiSrv, hookSrv := httptest.NewServer(api), httptest.NewServer(hook)
	t.Cleanup(apiSrv.Close)
	t.Cleanup(hookSrv.Close)
	g := New(&massdriver.Client{Endpoint: apiSrv.URL, OrganizationID: "acme", APIKey: "md_test"}, windows, "[freeze-override]", hookSrv.URL)
	return &harness{handler: automation.Handler(Definition.Name, g, secret), api: api, webhook: hook}
}

func (h *harness) send(t *testing.T, o deploymentOpts) int {
	t.Helper()
	body := event(o)
	req := httptest.NewRequest(http.MethodPost, automation.HookPath, strings.NewReader(string(body)))
	automation.SignRequest(req, secret, o.eventID, body, time.Now())
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	return rec.Code
}

func TestAbortsInsideWindow(t *testing.T) {
	h := setup(t)
	// Friday 18:00 in New York.
	code := h.send(t, deploymentOpts{eventID: "evt-1", at: "2026-11-27T23:00:00Z", environment: "prod", message: "Bump the instance size"})
	if code != http.StatusNoContent {
		t.Fatalf("status = %d", code)
	}

	if len(h.api.aborted) != 1 {
		t.Fatalf("aborted = %v", h.api.aborted)
	}
	if v := h.api.aborted[0]; v["org"] != "acme" || v["id"] != "b9dc3d18-19fc-429c-8d36-6753615fe17c" {
		t.Errorf("variables = %v", v)
	}
	want := `Aborted PROVISION deployment b9dc3d18-19fc-429c-8d36-6753615fe17c of api-prod-database (version 1.2.3) ` +
		`inside freeze window "weekend". Put [freeze-override] in the deployment message to deploy during a freeze.`
	if len(h.webhook.texts) != 1 || h.webhook.texts[0] != want {
		t.Errorf("slack = %q", h.webhook.texts)
	}
}

func TestWindows(t *testing.T) {
	cases := []struct {
		name  string
		opts  deploymentOpts
		abort bool
	}{
		{"weekend wraps into Sunday", deploymentOpts{at: "2026-11-29T17:00:00Z", environment: "prod"}, true},
		{"weekend ends Monday 08:00", deploymentOpts{at: "2026-11-30T13:00:00Z", environment: "prod"}, false},
		{"fixed range", deploymentOpts{at: "2026-11-26T15:00:00Z", environment: "prod"}, true},
		{"weekday outside windows", deploymentOpts{at: "2026-11-25T15:00:00Z", environment: "prod"}, false},
		{"other environment", deploymentOpts{at: "2026-11-27T23:00:00Z", environment: "staging"}, false},
		{"override token", deploymentOpts{at: "2026-11-27T23:00:00Z", environment: "prod", message: "Hotfix [freeze-override]"}, false},
		{"plan", deploymentOpts{at: "2026-11-27T23:00:00Z", environment: "prod", action: "PLAN"}, false},
		{"decommission", deploymentOpts{at: "2026-11-27T23:00:00Z", environment: "prod", action: "DECOMMISSION"}, true},
		{"approved", deploymentOpts{at: "2026-11-27T23:00:00Z", environment: "prod", status: "APPROVED"}, true},
		{"proposal", deploymentOpts{eventType: "deployment.created", at: "2026-11-27T23:00:00Z", environment: "prod", status: "PROPOSED"}, false},
		{"queued behind another deployment", deploymentOpts{eventType: "deployment.created", at: "2026-11-27T23:00:00Z", environment: "prod"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := setup(t)
			tc.opts.eventID = "evt-1"
			h.send(t, tc.opts)
			if got := len(h.api.aborted) == 1; got != tc.abort {
				t.Fatalf("aborted = %v, want abort %v", h.api.aborted, tc.abort)
			}
		})
	}
}

func TestAbortsOnce(t *testing.T) {
	h := setup(t)
	h.send(t, deploymentOpts{eventID: "evt-1", eventType: "deployment.created", at: "2026-11-27T23:00:00Z", environment: "prod"})
	h.send(t, deploymentOpts{eventID: "evt-2", eventType: "deployment.enqueued", at: "2026-11-27T23:00:01Z", environment: "prod"})
	if len(h.api.aborted) != 1 || len(h.webhook.texts) != 1 {
		t.Fatalf("aborted = %v, slack = %v", h.api.aborted, h.webhook.texts)
	}
}

func TestRefusedAbortIsNotRetried(t *testing.T) {
	h := setup(t)
	h.api.refuse = true
	if code := h.send(t, deploymentOpts{eventID: "evt-1", at: "2026-11-27T23:00:00Z", environment: "prod"}); code != http.StatusNoContent {
		t.Fatalf("status = %d", code)
	}
	if len(h.webhook.texts) != 0 {
		t.Fatalf("slack = %v", h.webhook.texts)
	}
}

func TestParseWindowsErrors(t *testing.T) {
	cases := map[string]string{
		"both kinds":   "windows: [{name: x, from: 2026-11-26, to: 2026-11-27, weekly: [{from: Fri 17:00, to: Mon 08:00}]}]",
		"neither kind": "windows: [{name: x}]",
		"bad timezone": "windows: [{name: x, timezone: Mars/Base, from: 2026-11-26, to: 2026-11-27}]",
		"bad weekday":  "windows: [{name: x, weekly: [{from: Funday 17:00, to: Mon 08:00}]}]",
		"backwards":    "windows: [{name: x, from: 2026-11-27, to: 2026-11-26}]",
		"unknown key":  "windows: [{name: x, form: 2026-11-26, to: 2026-11-27}]",
	}
	for name, src := range cases {
		if _, err := ParseWindows([]byte(src)); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

func TestParseWindowsJSON(t *testing.T) {
	ws, err := ParseWindows([]byte(`{"windows": [{"name": "x", "match": {"md-environment": "prod"}, "from": "2026-11-26T00:00:00Z", "to": "2026-11-27T00:00:00Z"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if !ws[0].Contains(time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)) {
		t.Fatal("JSON window does not contain its own range")
	}
}
