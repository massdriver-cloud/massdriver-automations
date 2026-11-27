package drift

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/massdriver-cloud/massdriver-automations/pkg/automation"
	"github.com/massdriver-cloud/massdriver-automations/pkg/massdriver"
	"github.com/massdriver-cloud/massdriver-automations/pkg/slack"
	"github.com/massdriver-cloud/massdriver-automations/pkg/slack/slacktest"
)

var secret = []byte("0123456789abcdef0123456789abcdef")

const driftLogs = `Initializing the backend...
OpenTofu will perform the following actions:

  # aws_db_instance.main will be updated in-place
  ~ resource "aws_db_instance" "main" {
      ~ allocated_storage = 50 -> 20
    }

  # aws_security_group_rule.extra will be destroyed
  - resource "aws_security_group_rule" "extra" {}

Plan: 0 to add, 1 to change, 1 to destroy.
`

const cleanLogs = "Initializing the backend...\nNo changes. Your infrastructure matches the configuration.\n"

type call struct {
	Op   string
	Vars map[string]any
}

type fakeInstance struct {
	status     string
	latest     string
	provision  string
	version    string
	resolved   string
	changedDep []string
	steps      []string
}

// fakeAPI serves instances in project api, environment prod. The attribute filter is not applied: every listed
// instance is what Massdriver would return for the check's match.
type fakeAPI struct {
	mu        sync.Mutex
	listed    []string
	instances map[string]fakeInstance
	logs      map[string]string
	calls     []call
	// rejectStash answers every stash write the way the server answers a key its widget catalog does not have.
	rejectStash bool
}

func newFakeAPI() *fakeAPI {
	return &fakeAPI{instances: map[string]fakeInstance{}, logs: map[string]string{}}
}

var opName = regexp.MustCompile(`(?:query|mutation) (\w+)`)

func (f *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Query     string         `json:"query"`
		Variables map[string]any `json:"variables"`
	}
	b, _ := io.ReadAll(r.Body)
	_ = json.Unmarshal(b, &req)
	op := opName.FindStringSubmatch(req.Query)[1]

	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call{Op: op, Vars: req.Variables})

	payload := func(field, id string) map[string]any {
		return map[string]any{field: map[string]any{"successful": true, "messages": []any{}, "result": map[string]any{"id": id}}}
	}
	var data any
	switch op {
	case "MatchingInstances":
		items := []any{}
		for _, id := range f.listed {
			items = append(items, map[string]any{"id": id})
		}
		data = map[string]any{"instances": map[string]any{"cursor": map[string]any{"next": nil}, "items": items}}
	case "Instance":
		id := req.Variables["id"].(string)
		fi := f.instances[id]
		comp := strings.TrimPrefix(id, "api-prod-")
		inst := map[string]any{
			"id":                  id,
			"status":              fi.status,
			"params":              map[string]any{"storage": 20},
			"resolvedVersion":     fi.resolved,
			"changedDependencies": append([]string{}, fi.changedDep...),
			"effectiveAttributes": map[string]any{"md-project": "api", "md-environment": "prod", "md-component": comp},
			"latestDeployment":    nil,
			"latestProvision":     nil,
		}
		steps := []any{}
		for _, s := range fi.steps {
			steps = append(steps, map[string]any{"stepName": s})
		}
		inst["statePaths"] = steps
		if fi.latest != "" {
			inst["latestDeployment"] = map[string]any{"id": "latest-" + id, "status": fi.latest}
		}
		if fi.provision != "" {
			inst["latestProvision"] = map[string]any{"id": fi.provision, "version": fi.version}
		}
		data = map[string]any{"instance": inst}
	case "DeploymentLogs":
		id := req.Variables["id"].(string)
		data = map[string]any{"deployment": map[string]any{"id": id, "logs": []any{
			map[string]any{"message": f.logs[id]},
		}}}
	case "PlanDeployment":
		data = payload("planDeployment", "plan-"+req.Variables["id"].(string))
	case "RollbackDeployment":
		data = payload("rollbackDeployment", "rollback-1")
	case "ApproveDeployment":
		data = payload("approveDeployment", req.Variables["id"].(string))
	case "CreateDeployment":
		data = payload("createDeployment", "deploy-1")
	case "ProposeDeployment":
		data = payload("proposeDeployment", "proposal-1")
	case "SetStashEntry":
		set := map[string]any{"successful": true, "messages": []any{}, "result": map[string]any{"key": req.Variables["key"]}}
		if f.rejectStash {
			set = map[string]any{"successful": false, "messages": []any{map[string]any{"field": "key", "message": "is reserved"}}}
		}
		data = map[string]any{"alphaSetStashEntry": set}
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
}

// mutations lists the mutations called after the plans started.
func (f *fakeAPI) mutations(except ...string) []call {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []call
	for _, c := range f.calls {
		if strings.HasSuffix(c.Op, "Deployment") && !contains(except, c.Op) {
			out = append(out, c)
		}
	}
	return out
}

// stash lists the stash writes as scope, key, label, and value.
func (f *fakeAPI) stash() [][4]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out [][4]string
	for _, c := range f.calls {
		if c.Op != "SetStashEntry" {
			continue
		}
		scope, _ := json.Marshal(c.Vars["scope"])
		input := c.Vars["input"].(map[string]any)
		out = append(out, [4]string{string(scope), c.Vars["key"].(string), input["label"].(string), input["value"].(string)})
	}
	return out
}

func (f *fakeAPI) count(op string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if c.Op == op {
			n++
		}
	}
	return n
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

type env struct {
	api   *fakeAPI
	slack *slacktest.Server
	d     *Drift
	h     http.Handler
	opts  Options
	cfg   *Config
}

func setup(t *testing.T, cfgYAML string, stateDir string) *env {
	t.Helper()
	api := newFakeAPI()
	apiSrv := httptest.NewServer(api)
	t.Cleanup(apiSrv.Close)
	cfg, err := ParseConfig([]byte(cfgYAML))
	if err != nil {
		t.Fatal(err)
	}
	e := &env{api: api, slack: slacktest.NewServer(t), cfg: cfg, opts: Options{
		RunToken: "run-token",
		StateDir: stateDir,
	}}
	e.start(t)
	return e
}

// start builds the automation as a fresh process would, from the state on disk.
func (e *env) start(t *testing.T) {
	t.Helper()
	srv := httptest.NewServer(e.api)
	t.Cleanup(srv.Close)
	d, err := New(&massdriver.Client{Endpoint: srv.URL, OrganizationID: "acme", APIKey: "md_test"}, e.slack.Bot(), e.cfg, e.opts)
	if err != nil {
		t.Fatal(err)
	}
	d.run = func(f func()) { f() }
	e.d = d
	e.h = automation.Handler(d, secret)
}

func (e *env) event(t *testing.T, id, typ, action, deploymentID string) int {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"id":   id,
		"type": typ,
		"data": map[string]any{
			"entity":     map[string]any{"id": deploymentID},
			"attributes": map[string]any{"md-instance": "api-prod-db"},
			"metadata":   map[string]any{"deployment_action": action},
		},
	})
	req := httptest.NewRequest(http.MethodPost, automation.HookPath, strings.NewReader(string(body)))
	automation.SignRequest(req, secret, id, body, time.Now())
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	return rec.Code
}

func provisioned(source string) fakeInstance {
	return fakeInstance{status: "PROVISIONED", latest: "COMPLETED", provision: source, version: "1.4.0", resolved: "1.4.0", steps: []string{"src"}}
}

const nightly = `
slack:
  channel: "#drift"
massdriver:
  app_url: https://app.massdriver.cloud/
checks:
  - name: nightly
    schedule: "0 6 * * *"
    timezone: America/Chicago
    match:
      drift: nightly
    mode: %s
    resolve_with: %s
`

func config(mode, resolveWith string) string {
	return strings.Replace(strings.Replace(nightly, "%s", mode, 1), "%s", resolveWith, 1)
}

func TestScheduledRunPlansMatchedProvisionedInstances(t *testing.T) {
	e := setup(t, config("detect", "deployed"), t.TempDir())
	e.api.listed = []string{"api-prod-db", "api-prod-cache", "api-prod-app", "api-prod-new", "api-prod-old"}
	e.api.instances["api-prod-db"] = provisioned("dep-db-1")
	e.api.instances["api-prod-cache"] = provisioned("dep-cache-1")
	e.api.instances["api-prod-app"] = fakeInstance{status: "PROVISIONED", latest: "RUNNING", provision: "dep-app-1"}
	e.api.instances["api-prod-new"] = fakeInstance{status: "PROVISIONED", latest: "FAILED"}
	e.api.instances["api-prod-old"] = fakeInstance{status: "DECOMMISSIONED", latest: "COMPLETED", provision: "dep-old-1"}

	c := cron.New()
	if err := e.d.Schedule(c); err != nil {
		t.Fatal(err)
	}
	entries := c.Entries()
	if len(entries) != 1 {
		t.Fatalf("scheduled %d checks, want 1", len(entries))
	}
	entries[0].Job.Run()

	list := e.api.calls[0]
	if list.Op != "MatchingInstances" {
		t.Fatalf("first call = %s", list.Op)
	}
	if got, _ := json.Marshal(list.Vars["attributes"]); string(got) != `[{"eq":"nightly","key":"drift"}]` {
		t.Errorf("attributes = %s", got)
	}

	want := []call{
		{Op: "PlanDeployment", Vars: map[string]any{"org": "acme", "id": "dep-db-1"}},
		{Op: "PlanDeployment", Vars: map[string]any{"org": "acme", "id": "dep-cache-1"}},
	}
	if got := e.api.mutations(); !reflect.DeepEqual(got, want) {
		t.Fatalf("mutations = %v, want %v", got, want)
	}
	if got := len(e.d.st.Plans); got != 2 {
		t.Fatalf("tracking %d plans, want 2", got)
	}
	p := e.d.st.Plans["plan-dep-db-1"]
	if p.Check != "nightly" || p.Instance != "api-prod-db" || p.Source != "dep-db-1" {
		t.Errorf("plan = %+v", p)
	}
}

func TestDriftAlertsAndResolvesByMode(t *testing.T) {
	link := "Plan: <https://app.massdriver.cloud/orgs/acme/projects/api/environments/prod/instances/db?deployment=plan-dep-db-1&tab=history|plan-dep-db-1>"
	head := "Drift on api-prod-db (check nightly).\n" +
		"src: 0 to add, 1 to change, 1 to destroy\n" +
		"```\n# aws_db_instance.main will be updated in-place\n# aws_security_group_rule.extra will be destroyed\n```\n"
	saved := map[string]any{
		"action":  "PROVISION",
		"params":  `{"storage":20}`,
		"message": "drift: nightly resolving drift found by plan plan-dep-db-1",
	}

	tests := []struct {
		mode, resolveWith string
		note              string
		mutations         []call
	}{
		{"detect", "deployed", "", nil},
		{"resolve", "deployed", "Resolving: deployment rollback-1 re-applies deployment dep-db-1.", []call{
			{Op: "RollbackDeployment", Vars: map[string]any{"org": "acme", "id": "dep-db-1"}},
			{Op: "ApproveDeployment", Vars: map[string]any{"org": "acme", "id": "rollback-1"}},
		}},
		{"resolve", "saved", "Resolving: deployment deploy-1 applies the saved configuration.", []call{
			{Op: "CreateDeployment", Vars: map[string]any{"org": "acme", "id": "api-prod-db", "input": saved}},
		}},
		{"propose", "deployed", "Proposed deployment rollback-1 re-applies deployment dep-db-1. It waits for approval.", []call{
			{Op: "RollbackDeployment", Vars: map[string]any{"org": "acme", "id": "dep-db-1"}},
		}},
		{"propose", "saved", "Proposed deployment proposal-1 applies the saved configuration. It waits for approval.", []call{
			{Op: "ProposeDeployment", Vars: map[string]any{"org": "acme", "id": "api-prod-db", "input": saved}},
		}},
	}
	for _, tt := range tests {
		t.Run(tt.mode+"/"+tt.resolveWith, func(t *testing.T) {
			e := setup(t, config(tt.mode, tt.resolveWith), "")
			e.api.listed = []string{"api-prod-db"}
			e.api.instances["api-prod-db"] = provisioned("dep-db-1")
			e.api.logs["plan-dep-db-1"] = driftLogs
			if err := e.d.RunCheck(context.Background(), "nightly"); err != nil {
				t.Fatal(err)
			}

			if code := e.event(t, "evt-1", "deployment.completed", "PLAN", "plan-dep-db-1"); code != http.StatusNoContent {
				t.Fatalf("status = %d", code)
			}

			if got := e.api.mutations("PlanDeployment"); !reflect.DeepEqual(got, tt.mutations) {
				t.Errorf("mutations = %v, want %v", got, tt.mutations)
			}
			want := []slacktest.Call{{Method: "chat.postMessage", Channel: "#drift", Text: head + link, TS: "1700000000.000001"}}
			if tt.note != "" {
				want = append(want, slacktest.Call{Method: "chat.postMessage", Channel: "CDRIFT", Text: tt.note, ThreadTS: "1700000000.000001", TS: "1700000000.000002"})
			}
			if got := e.slack.Calls(); !reflect.DeepEqual(got, want) {
				t.Errorf("slack = %+v\nwant    %+v", got, want)
			}
		})
	}
}

func TestSlackFailureRetriesWithoutResolvingTwice(t *testing.T) {
	e := setup(t, config("resolve", "deployed"), t.TempDir())
	e.api.listed = []string{"api-prod-db"}
	e.api.instances["api-prod-db"] = provisioned("dep-db-1")
	e.api.logs["plan-dep-db-1"] = driftLogs
	e.slack.Fail("ratelimited")
	if err := e.d.RunCheck(context.Background(), "nightly"); err != nil {
		t.Fatal(err)
	}

	if code := e.event(t, "evt-1", "deployment.completed", "PLAN", "plan-dep-db-1"); code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 so Massdriver retries", code)
	}
	e.start(t)
	e.slack.Fail("")
	if code := e.event(t, "evt-1", "deployment.completed", "PLAN", "plan-dep-db-1"); code != http.StatusNoContent {
		t.Fatalf("retry status = %d", code)
	}

	if got := e.api.count("RollbackDeployment"); got != 1 {
		t.Errorf("rolled back %d times, want 1", got)
	}
	calls := e.slack.Calls()
	if len(calls) != 2 || calls[1].ThreadTS != calls[0].TS || calls[1].Text != "Resolving: deployment rollback-1 re-applies deployment dep-db-1." {
		t.Errorf("slack = %+v, want the alert and the outcome in its thread", calls)
	}
}

func TestFailedReplyRetriesOnlyTheReply(t *testing.T) {
	e := setup(t, config("resolve", "deployed"), t.TempDir())
	e.api.listed = []string{"api-prod-db"}
	e.api.instances["api-prod-db"] = provisioned("dep-db-1")
	e.api.logs["plan-dep-db-1"] = driftLogs
	if err := e.d.RunCheck(context.Background(), "nightly"); err != nil {
		t.Fatal(err)
	}
	// The alert posts, then Slack fails for the reply.
	e.d.slack = slack.New(slack.Options{BotToken: "xoxb-test", APIURL: e.slack.URL + "/api", HTTP: &http.Client{Transport: &failAfter{n: 1}}})
	if code := e.event(t, "evt-1", "deployment.completed", "PLAN", "plan-dep-db-1"); code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", code)
	}
	e.start(t)
	if code := e.event(t, "evt-1", "deployment.completed", "PLAN", "plan-dep-db-1"); code != http.StatusNoContent {
		t.Fatalf("retry status = %d", code)
	}
	calls := e.slack.Calls()
	if len(calls) != 2 || calls[0].ThreadTS != "" || calls[1].ThreadTS != calls[0].TS {
		t.Errorf("slack = %+v, want one alert and one reply in its thread", calls)
	}
	if got := e.api.count("RollbackDeployment"); got != 1 {
		t.Errorf("rolled back %d times, want 1", got)
	}
}

// failAfter lets n requests through and fails the rest.
type failAfter struct {
	mu sync.Mutex
	n  int
}

func (f *failAfter) RoundTrip(r *http.Request) (*http.Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.n == 0 {
		return nil, errors.New("connection reset")
	}
	f.n--
	return http.DefaultTransport.RoundTrip(r)
}

func TestWebhookPostsTheOutcomeAsItsOwnMessage(t *testing.T) {
	e := setup(t, config("resolve", "deployed"), "")
	e.d.slack = e.slack.Webhook()
	e.api.listed = []string{"api-prod-db"}
	e.api.instances["api-prod-db"] = provisioned("dep-db-1")
	e.api.logs["plan-dep-db-1"] = driftLogs
	if err := e.d.RunCheck(context.Background(), "nightly"); err != nil {
		t.Fatal(err)
	}
	e.event(t, "evt-1", "deployment.completed", "PLAN", "plan-dep-db-1")
	calls := e.slack.Calls()
	if len(calls) != 2 || calls[0].Method != "webhook" || calls[1].Method != "webhook" ||
		calls[1].Text != "Resolving: deployment rollback-1 re-applies deployment dep-db-1." {
		t.Errorf("slack = %+v", calls)
	}
}

func TestCheckChannelReplacesTheDefault(t *testing.T) {
	e := setup(t, strings.Replace(config("detect", "deployed"), "    mode:", "    slack: {channel: \"#db-team\"}\n    mode:", 1), "")
	e.api.listed = []string{"api-prod-db"}
	e.api.instances["api-prod-db"] = provisioned("dep-db-1")
	e.api.logs["plan-dep-db-1"] = driftLogs
	if err := e.d.RunCheck(context.Background(), "nightly"); err != nil {
		t.Fatal(err)
	}
	e.event(t, "evt-1", "deployment.completed", "PLAN", "plan-dep-db-1")
	calls := e.slack.Calls()
	if len(calls) != 1 || calls[0].Channel != "#db-team" {
		t.Errorf("slack = %+v", calls)
	}
}

func TestNoChannelLogsTheAlert(t *testing.T) {
	e := setup(t, strings.Replace(config("detect", "deployed"), "  channel: \"#drift\"", "  channel: \"\"", 1), "")
	e.api.listed = []string{"api-prod-db"}
	e.api.instances["api-prod-db"] = provisioned("dep-db-1")
	e.api.logs["plan-dep-db-1"] = driftLogs
	if err := e.d.RunCheck(context.Background(), "nightly"); err != nil {
		t.Fatal(err)
	}
	if code := e.event(t, "evt-1", "deployment.completed", "PLAN", "plan-dep-db-1"); code != http.StatusNoContent {
		t.Fatalf("status = %d", code)
	}
	if calls := e.slack.Calls(); len(calls) != 0 {
		t.Errorf("slack = %+v", calls)
	}
}

func TestCleanPlanSendsNothing(t *testing.T) {
	e := setup(t, config("resolve", "deployed"), "")
	e.api.listed = []string{"api-prod-db"}
	e.api.instances["api-prod-db"] = provisioned("dep-db-1")
	e.api.logs["plan-dep-db-1"] = cleanLogs
	if err := e.d.RunCheck(context.Background(), "nightly"); err != nil {
		t.Fatal(err)
	}
	e.event(t, "evt-1", "deployment.completed", "PLAN", "plan-dep-db-1")

	if got := e.slack.Texts(); len(got) != 0 {
		t.Errorf("slack = %q, want nothing", got)
	}
	if got := e.api.mutations("PlanDeployment"); len(got) != 0 {
		t.Errorf("mutations = %v, want none", got)
	}
	if len(e.d.st.Plans) != 0 {
		t.Errorf("still tracking %v", e.d.st.Plans)
	}
}

func TestUnknownResultAlertsWithoutResolving(t *testing.T) {
	e := setup(t, config("resolve", "deployed"), "")
	e.api.listed = []string{"api-prod-db"}
	inst := provisioned("dep-db-1")
	inst.steps = []string{"src", "chart"}
	e.api.instances["api-prod-db"] = inst
	e.api.logs["plan-dep-db-1"] = cleanLogs + "Release \"db\" does not exist. Installing it now.\n"
	if err := e.d.RunCheck(context.Background(), "nightly"); err != nil {
		t.Fatal(err)
	}
	e.event(t, "evt-1", "deployment.completed", "PLAN", "plan-dep-db-1")

	want := []string{"Drift unknown on api-prod-db (check nightly): the plan logs show a result for 1 of 2 steps. Nothing was resolved.\n" +
		"Plan: <https://app.massdriver.cloud/orgs/acme/projects/api/environments/prod/instances/db?deployment=plan-dep-db-1&tab=history|plan-dep-db-1>"}
	if got := e.slack.Texts(); !reflect.DeepEqual(got, want) {
		t.Errorf("slack = %q, want %q", got, want)
	}
	if got := e.api.mutations("PlanDeployment"); len(got) != 0 {
		t.Errorf("mutations = %v, want none", got)
	}
}

func TestUndeployedChangesAreNotResolved(t *testing.T) {
	e := setup(t, config("resolve", "deployed"), "")
	e.api.listed = []string{"api-prod-db"}
	inst := provisioned("dep-db-1")
	inst.resolved = "1.5.0"
	inst.changedDep = []string{"network"}
	e.api.instances["api-prod-db"] = inst
	e.api.logs["plan-dep-db-1"] = driftLogs
	if err := e.d.RunCheck(context.Background(), "nightly"); err != nil {
		t.Fatal(err)
	}
	e.event(t, "evt-1", "deployment.completed", "PLAN", "plan-dep-db-1")

	got := e.slack.Texts()
	want := "The plan also includes changes that were never deployed: bundle version 1.4.0 to 1.5.0; dependencies network. Nothing was resolved.\n"
	if len(got) != 1 || !strings.Contains(got[0], want) {
		t.Errorf("slack = %q, want it to contain %q", got, want)
	}
	if got := e.api.mutations("PlanDeployment"); len(got) != 0 {
		t.Errorf("mutations = %v, want none", got)
	}
}

func TestFailedPlanAlerts(t *testing.T) {
	e := setup(t, config("resolve", "deployed"), "")
	e.api.listed = []string{"api-prod-db"}
	e.api.instances["api-prod-db"] = provisioned("dep-db-1")
	if err := e.d.RunCheck(context.Background(), "nightly"); err != nil {
		t.Fatal(err)
	}
	e.event(t, "evt-1", "deployment.failed", "PLAN", "plan-dep-db-1")

	want := []string{"Drift check failed for api-prod-db (check nightly): the plan was failed. Nothing was resolved.\n" +
		"Plan: <https://app.massdriver.cloud/orgs/acme/projects/api/environments/prod/instances/db?deployment=plan-dep-db-1&tab=history|plan-dep-db-1>"}
	if got := e.slack.Texts(); !reflect.DeepEqual(got, want) {
		t.Errorf("slack = %q, want %q", got, want)
	}
	if got := e.api.mutations("PlanDeployment"); len(got) != 0 {
		t.Errorf("mutations = %v, want none", got)
	}
}

func TestUntrackedPlansAreIgnored(t *testing.T) {
	e := setup(t, config("resolve", "deployed"), "")
	e.api.logs["plan-someone-else"] = driftLogs

	if code := e.event(t, "evt-1", "deployment.completed", "PLAN", "plan-someone-else"); code != http.StatusNoContent {
		t.Fatalf("status = %d", code)
	}
	if code := e.event(t, "evt-2", "deployment.completed", "PROVISION", "dep-db-1"); code != http.StatusNoContent {
		t.Fatalf("status = %d", code)
	}
	if n := len(e.api.calls); n != 0 {
		t.Errorf("made %d API calls, want 0: %v", n, e.api.calls)
	}
	if got := e.slack.Texts(); len(got) != 0 {
		t.Errorf("slack = %q, want nothing", got)
	}
}

func TestPendingPlansSurviveRestart(t *testing.T) {
	e := setup(t, strings.Replace(config("resolve", "deployed"), "mode:", "max_concurrent: 1\n    mode:", 1), t.TempDir())
	e.api.listed = []string{"api-prod-db", "api-prod-cache"}
	e.api.instances["api-prod-db"] = provisioned("dep-db-1")
	e.api.instances["api-prod-cache"] = provisioned("dep-cache-1")
	e.api.logs["plan-dep-db-1"] = driftLogs
	e.api.logs["plan-dep-cache-1"] = cleanLogs
	if err := e.d.RunCheck(context.Background(), "nightly"); err != nil {
		t.Fatal(err)
	}
	if got := e.api.count("PlanDeployment"); got != 1 {
		t.Fatalf("started %d plans with max_concurrent 1", got)
	}

	e.start(t)
	e.event(t, "evt-1", "deployment.completed", "PLAN", "plan-dep-db-1")

	want := []call{
		{Op: "PlanDeployment", Vars: map[string]any{"org": "acme", "id": "dep-db-1"}},
		{Op: "RollbackDeployment", Vars: map[string]any{"org": "acme", "id": "dep-db-1"}},
		{Op: "ApproveDeployment", Vars: map[string]any{"org": "acme", "id": "rollback-1"}},
		{Op: "PlanDeployment", Vars: map[string]any{"org": "acme", "id": "dep-cache-1"}},
	}
	if got := e.api.mutations(); !reflect.DeepEqual(got, want) {
		t.Errorf("mutations = %v, want %v", got, want)
	}
	if got := len(e.slack.Texts()); got != 2 {
		t.Errorf("sent %d Slack messages, want the alert and its reply", got)
	}
}

func TestRunEndpoint(t *testing.T) {
	e := setup(t, config("detect", "deployed"), "")
	e.api.listed = []string{"api-prod-db"}
	e.api.instances["api-prod-db"] = provisioned("dep-db-1")

	post := func(check, auth string) int {
		req := httptest.NewRequest(http.MethodPost, "/drift/run?check="+check, nil)
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		rec := httptest.NewRecorder()
		e.h.ServeHTTP(rec, req)
		return rec.Code
	}

	if code := post("nightly", ""); code != http.StatusUnauthorized {
		t.Errorf("no token: status = %d, want 401", code)
	}
	if code := post("nightly", "Bearer wrong"); code != http.StatusUnauthorized {
		t.Errorf("wrong token: status = %d, want 401", code)
	}
	if code := post("weekly", "Bearer run-token"); code != http.StatusNotFound {
		t.Errorf("unknown check: status = %d, want 404", code)
	}
	if n := e.api.count("PlanDeployment"); n != 0 {
		t.Fatalf("planned %d times before an authorized request", n)
	}
	if code := post("nightly", "Bearer run-token"); code != http.StatusAccepted {
		t.Errorf("status = %d, want 202", code)
	}
	want := []call{{Op: "PlanDeployment", Vars: map[string]any{"org": "acme", "id": "dep-db-1"}}}
	if got := e.api.mutations(); !reflect.DeepEqual(got, want) {
		t.Errorf("mutations = %v, want %v", got, want)
	}
}

func TestAnalyzeImportAndForget(t *testing.T) {
	cfg, err := ParseConfig([]byte("checks: [{name: a}]"))
	if err != nil {
		t.Fatal(err)
	}
	logs := "\x1b[1mPlan:\x1b[0m 1 to import, 0 to add, 0 to change, 0 to destroy, 2 to forget.\nNo changes.\n"
	r := Analyze(logs, []string{"core", "iam"}, &cfg.Checks[0])
	if r.Status != Drifted {
		t.Fatalf("status = %s", r.Status)
	}
	got := []string{r.Steps[0].String(), r.Steps[1].String()}
	want := []string{"core: 1 to import, 0 to add, 0 to change, 0 to destroy, 2 to forget", "iam: no changes"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("steps = %q, want %q", got, want)
	}
}

func TestExampleConfigLoads(t *testing.T) {
	var cfg Config
	if err := automation.LoadConfigFile("config.example.yaml", &cfg); err != nil {
		t.Fatal(err)
	}
	if got := len(cfg.Checks); got != 3 {
		t.Fatalf("loaded %d checks, want 3", got)
	}
	if got := cfg.Checks[2].Mode + "/" + cfg.Checks[2].ResolveWith; got != "detect/deployed" {
		t.Errorf("defaults = %s, want detect/deployed", got)
	}
	if got := cfg.Checks[0].Match["team"]; !reflect.DeepEqual(got, Values{"payments", "billing"}) {
		t.Errorf("match.team = %v", got)
	}
}

func TestPlanResultsGoToTheStash(t *testing.T) {
	checked := time.Date(2026, 11, 27, 20, 3, 24, 0, time.UTC)
	tests := []struct {
		name, logs, prefix string
		want               [][4]string
	}{
		{"drifted", driftLogs, "", [][4]string{
			{`{"deployment":"plan-dep-db-1"}`, "md/v1/changes/drift", "Changes", `{"tool":"drift","add":0,"change":1,"destroy":1}`},
			{`{"instance":"api-prod-db"}`, "md/v1/drift", "Drift", `{"status":"detected","resources":2,"checked_at":"2026-11-27T20:03:24Z"}`},
		}},
		{"clean", cleanLogs, "results/v1/", [][4]string{
			{`{"deployment":"plan-dep-db-1"}`, "results/v1/changes/drift", "Changes", `{"tool":"drift","add":0,"change":0,"destroy":0}`},
			{`{"instance":"api-prod-db"}`, "results/v1/drift", "Drift", `{"status":"none","resources":0,"checked_at":"2026-11-27T20:03:24Z"}`},
		}},
		{"unknown", "Initializing the backend...\n", "", [][4]string{
			{`{"instance":"api-prod-db"}`, "md/v1/drift", "Drift", `{"status":"unknown","resources":0,"checked_at":"2026-11-27T20:03:24Z"}`},
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := config("detect", "deployed")
			if tt.prefix != "" {
				cfg += "results:\n  prefix: " + tt.prefix + "\n"
			}
			e := setup(t, cfg, "")
			e.d.now = func() time.Time { return checked }
			e.api.listed = []string{"api-prod-db"}
			e.api.instances["api-prod-db"] = provisioned("dep-db-1")
			e.api.logs["plan-dep-db-1"] = tt.logs
			if err := e.d.RunCheck(context.Background(), "nightly"); err != nil {
				t.Fatal(err)
			}
			if code := e.event(t, "evt-1", "deployment.completed", "PLAN", "plan-dep-db-1"); code != http.StatusNoContent {
				t.Fatalf("status = %d", code)
			}
			if got := e.api.stash(); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("stash = %v\nwant    %v", got, tt.want)
			}
		})
	}
}

func TestRejectedStashWriteDoesNotFailThePlan(t *testing.T) {
	e := setup(t, config("detect", "deployed"), "")
	e.api.rejectStash = true
	e.api.listed = []string{"api-prod-db"}
	e.api.instances["api-prod-db"] = provisioned("dep-db-1")
	e.api.logs["plan-dep-db-1"] = driftLogs
	if err := e.d.RunCheck(context.Background(), "nightly"); err != nil {
		t.Fatal(err)
	}
	if code := e.event(t, "evt-1", "deployment.completed", "PLAN", "plan-dep-db-1"); code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204: a stash write must not make Massdriver retry", code)
	}
	if got := len(e.slack.Calls()); got != 1 {
		t.Errorf("slack posts = %d, want the alert", got)
	}
	if got := len(e.api.stash()); got != 2 {
		t.Errorf("stash writes = %d, want 2", got)
	}
}
