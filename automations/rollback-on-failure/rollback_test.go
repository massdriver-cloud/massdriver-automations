package rollbackonfailure

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

type call struct {
	Op        string
	Variables map[string]any
}

// fakeAPI serves the deployment history of api-prod-database. Its fields are the pages the InstanceDeployments
// query returns.
type fakeAPI struct {
	mu            sync.Mutex
	calls         []call
	inProgress    []any
	lastCompleted []any
	proposed      []any
	approveFails  string
}

func (f *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Query     string         `json:"query"`
		Variables map[string]any `json:"variables"`
	}
	b, _ := io.ReadAll(r.Body)
	_ = json.Unmarshal(b, &req)

	op := strings.Fields(req.Query)[1]
	op = op[:strings.Index(op, "(")]
	f.mu.Lock()
	f.calls = append(f.calls, call{Op: op, Variables: req.Variables})
	f.mu.Unlock()

	page := func(items []any) map[string]any {
		if items == nil {
			items = []any{}
		}
		return map[string]any{"items": items}
	}
	ok := func(id string) map[string]any {
		return map[string]any{"successful": true, "messages": []any{}, "result": map[string]any{"id": id}}
	}

	var data any
	switch op {
	case "InstanceDeployments":
		data = map[string]any{"inProgress": page(f.inProgress), "lastCompleted": page(f.lastCompleted), "proposed": page(f.proposed)}
	case "RollbackDeployment":
		data = map[string]any{"rollbackDeployment": ok("rollback-dep")}
	case "ApproveDeployment":
		if f.approveFails != "" {
			data = map[string]any{"approveDeployment": map[string]any{
				"successful": false,
				"messages":   []any{map[string]any{"field": "separationOfDuty", "message": f.approveFails}},
				"result":     nil,
			}}
		} else {
			data = map[string]any{"approveDeployment": ok(req.Variables["id"].(string))}
		}
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
}

type fakeSlack struct {
	mu    sync.Mutex
	texts []string
}

func (s *fakeSlack) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Text string `json:"text"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	s.mu.Lock()
	s.texts = append(s.texts, body.Text)
	s.mu.Unlock()
}

func failedEvent(id, action string, entity map[string]any, attrs map[string]any) []byte {
	base := map[string]any{"id": "failed-dep", "message": "Bump the instance size", "created_at": "2026-11-25T12:00:00.000000Z"}
	for k, v := range entity {
		base[k] = v
	}
	attributes := map[string]any{
		"md-project":     "api",
		"md-environment": "prod",
		"md-component":   "database",
		"md-instance":    "api-prod-database",
		"autorollback":   "true",
	}
	for k, v := range attrs {
		attributes[k] = v
	}
	b, _ := json.Marshal(map[string]any{
		"id":      id,
		"type":    "deployment.failed",
		"subject": "mri:acme:api:prod:deployment:failed-dep",
		"data": map[string]any{
			"entity":     base,
			"attributes": attributes,
			"metadata":   map[string]any{"deployment_action": action},
		},
	})
	return b
}

func completed(id, version, createdAt string) map[string]any {
	return map[string]any{"id": id, "version": version, "createdAt": createdAt}
}

func setup(t *testing.T, api *fakeAPI) (http.Handler, *fakeSlack) {
	t.Helper()
	apiSrv := httptest.NewServer(api)
	t.Cleanup(apiSrv.Close)
	slack := &fakeSlack{}
	slackSrv := httptest.NewServer(slack)
	t.Cleanup(slackSrv.Close)

	r := New(&massdriver.Client{Endpoint: apiSrv.URL, OrganizationID: "acme", APIKey: "md_test"}, Config{
		AttrKey:         "autorollback",
		AttrValue:       "true",
		SlackWebhookURL: slackSrv.URL,
		AppURL:          "https://app.example.com/",
	})
	return automation.Handler("rollback-on-failure", r, secret), slack
}

func post(t *testing.T, h http.Handler, body []byte) int {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, automation.HookPath, strings.NewReader(string(body)))
	var ev struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(body, &ev)
	automation.SignRequest(req, secret, ev.ID, body, time.Now())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code
}

func ops(api *fakeAPI) []string {
	var out []string
	for _, c := range api.calls {
		out = append(out, c.Op)
	}
	return out
}

func TestRollsBackToLastCompletedProvision(t *testing.T) {
	api := &fakeAPI{lastCompleted: []any{completed("good-dep", "1.2.3", "2026-11-24T09:00:00Z")}}
	h, slack := setup(t, api)

	if code := post(t, h, failedEvent("evt-1", "PROVISION", nil, nil)); code != http.StatusNoContent {
		t.Fatalf("status = %d", code)
	}

	if got := strings.Join(ops(api), ","); got != "InstanceDeployments,RollbackDeployment,ApproveDeployment" {
		t.Fatalf("calls = %s", got)
	}
	if v := api.calls[0].Variables; v["org"] != "acme" || v["instance"] != "api-prod-database" {
		t.Errorf("InstanceDeployments variables = %v", v)
	}
	if v := api.calls[1].Variables; v["org"] != "acme" || v["id"] != "good-dep" {
		t.Errorf("RollbackDeployment variables = %v", v)
	}
	if v := api.calls[2].Variables; v["org"] != "acme" || v["id"] != "rollback-dep" {
		t.Errorf("ApproveDeployment variables = %v", v)
	}

	want := "Deployment <https://app.example.com/orgs/acme/projects/api/environments/prod/instances/database?deployment=failed-dep&tab=history|failed-dep> " +
		"of <https://app.example.com/orgs/acme/projects/api/environments/prod/instances/database|api-prod-database> failed. " +
		"Rollback <https://app.example.com/orgs/acme/projects/api/environments/prod/instances/database?deployment=rollback-dep&tab=history|rollback-dep> " +
		"restores deployment <https://app.example.com/orgs/acme/projects/api/environments/prod/instances/database?deployment=good-dep&tab=history|good-dep> (v1.2.3)."
	if len(slack.texts) != 1 || slack.texts[0] != want {
		t.Errorf("slack = %q", slack.texts)
	}
}

func TestApprovesTheExistingProposalOnRetry(t *testing.T) {
	api := &fakeAPI{
		lastCompleted: []any{completed("good-dep", "1.2.3", "2026-11-24T09:00:00Z")},
		proposed: []any{
			map[string]any{"id": "someone-elses", "message": "Scale up"},
			map[string]any{"id": "earlier-rollback", "message": "Rollback to deployment good-dep (v1.2.3)\nBump the instance size"},
		},
	}
	h, _ := setup(t, api)

	if code := post(t, h, failedEvent("evt-1", "PROVISION", nil, nil)); code != http.StatusNoContent {
		t.Fatalf("status = %d", code)
	}

	if got := strings.Join(ops(api), ","); got != "InstanceDeployments,ApproveDeployment" {
		t.Fatalf("calls = %s", got)
	}
	if v := api.calls[1].Variables; v["id"] != "earlier-rollback" {
		t.Errorf("ApproveDeployment variables = %v", v)
	}
}

func TestLeavesTheProposalWhenApprovalIsRefused(t *testing.T) {
	api := &fakeAPI{
		lastCompleted: []any{completed("good-dep", "1.2.3", "2026-11-24T09:00:00Z")},
		approveFails:  "is enabled on environment api-prod",
	}
	h, slack := setup(t, api)

	if code := post(t, h, failedEvent("evt-1", "PROVISION", nil, nil)); code != http.StatusNoContent {
		t.Fatalf("status = %d", code)
	}
	if got := strings.Join(ops(api), ","); got != "InstanceDeployments,RollbackDeployment,ApproveDeployment" {
		t.Fatalf("calls = %s", got)
	}
	if len(slack.texts) != 1 || !strings.Contains(slack.texts[0], "waits for approval: approveDeployment: separationOfDuty: is enabled on environment api-prod") {
		t.Errorf("slack = %q", slack.texts)
	}
}

func TestSkips(t *testing.T) {
	good := []any{completed("good-dep", "1.2.3", "2026-11-24T09:00:00Z")}
	cases := []struct {
		name      string
		api       *fakeAPI
		event     []byte
		wantCalls string
		wantSlack string
	}{
		{
			name:      "plan deployment",
			api:       &fakeAPI{lastCompleted: good},
			event:     failedEvent("evt-1", "PLAN", nil, nil),
			wantCalls: "",
		},
		{
			name:      "instance not opted in",
			api:       &fakeAPI{lastCompleted: good},
			event:     failedEvent("evt-1", "PROVISION", nil, map[string]any{"autorollback": "false"}),
			wantCalls: "",
		},
		{
			name:      "failed deployment is a rollback",
			api:       &fakeAPI{lastCompleted: good},
			event:     failedEvent("evt-1", "PROVISION", map[string]any{"rollback_of_deployment_id": "good-dep"}, nil),
			wantCalls: "",
			wantSlack: "Rollback <https://app.example.com/orgs/acme/projects/api/environments/prod/instances/database?deployment=failed-dep&tab=history|failed-dep> " +
				"of <https://app.example.com/orgs/acme/projects/api/environments/prod/instances/database|api-prod-database> failed. " +
				"No further rollback. The instance needs attention.",
		},
		{
			name:      "failed deployment has a rollback message",
			api:       &fakeAPI{lastCompleted: good},
			event:     failedEvent("evt-1", "PROVISION", map[string]any{"message": "Rollback to deployment good-dep (v1.2.3)"}, nil),
			wantCalls: "",
			wantSlack: "Rollback <https://app.example.com/orgs/acme/projects/api/environments/prod/instances/database?deployment=failed-dep&tab=history|failed-dep> " +
				"of <https://app.example.com/orgs/acme/projects/api/environments/prod/instances/database|api-prod-database> failed. " +
				"No further rollback. The instance needs attention.",
		},
		{
			name:      "deployment in progress",
			api:       &fakeAPI{lastCompleted: good, inProgress: []any{map[string]any{"id": "next-dep", "status": "RUNNING"}}},
			event:     failedEvent("evt-1", "PROVISION", nil, nil),
			wantCalls: "InstanceDeployments",
		},
		{
			name:      "no completed provision",
			api:       &fakeAPI{},
			event:     failedEvent("evt-1", "PROVISION", nil, nil),
			wantCalls: "InstanceDeployments",
		},
		{
			name:      "a later provision completed",
			api:       &fakeAPI{lastCompleted: []any{completed("newer-dep", "1.2.4", "2026-11-25T13:00:00Z")}},
			event:     failedEvent("evt-1", "PROVISION", nil, nil),
			wantCalls: "InstanceDeployments",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, slack := setup(t, tc.api)
			if code := post(t, h, tc.event); code != http.StatusNoContent {
				t.Fatalf("status = %d", code)
			}
			if got := strings.Join(ops(tc.api), ","); got != tc.wantCalls {
				t.Errorf("calls = %q, want %q", got, tc.wantCalls)
			}
			wantSlack := []string(nil)
			if tc.wantSlack != "" {
				wantSlack = []string{tc.wantSlack}
			}
			if strings.Join(slack.texts, "|") != strings.Join(wantSlack, "|") || len(slack.texts) != len(wantSlack) {
				t.Errorf("slack = %q, want %q", slack.texts, wantSlack)
			}
		})
	}
}

func TestReturnsErrorWhenTheAPIIsDown(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()
	r := New(&massdriver.Client{Endpoint: srv.URL, OrganizationID: "acme", APIKey: "md_test"}, Config{AttrKey: "autorollback", AttrValue: "true"})

	if code := post(t, automation.Handler("rollback-on-failure", r, secret), failedEvent("evt-1", "PROVISION", nil, nil)); code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", code)
	}
}
