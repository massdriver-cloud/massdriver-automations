package freezeguard

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/massdriver-cloud/massdriver-automations/pkg/automation"
	"github.com/massdriver-cloud/massdriver-automations/pkg/massdriver"
	"github.com/massdriver-cloud/massdriver-automations/pkg/slack/slacktest"
)

var secret = []byte("0123456789abcdef0123456789abcdef")

const windowsYAML = `
slack:
  channel: "#changes"
timezone: America/New_York
exempt_groups: [SRE]
windows:
  - name: weekend
    match:
      md-environment: prod
    exempt_groups: [Release Managers]
    weekly:
      - from: Fri 17:00
        to: Mon 08:00
  - name: thanksgiving
    match:
      md-project: [api, web]
    actions: [instance:deploy]
    from: 2026-11-26
    to: 2026-11-27
    slack:
      channel: "#api-team"
`

var (
	// Friday 18:00 in New York.
	inWeekend = time.Date(2026, 11, 27, 23, 0, 0, 0, time.UTC)
	// Wednesday 10:00 in New York.
	weekday = time.Date(2026, 11, 25, 15, 0, 0, 0, time.UTC)
)

type fakePolicy struct {
	ID         string          `json:"id"`
	Actions    []string        `json:"actions"`
	Conditions json.RawMessage `json:"conditions"`
}

type fakeGroup struct {
	ID       string
	Name     string
	Role     string
	Policies []fakePolicy
}

type created struct {
	Group string
	Input map[string]any
}

type fakeAPI struct {
	mu      sync.Mutex
	groups  []*fakeGroup
	nextID  int
	created []created
	deleted []string
}

func (f *fakeAPI) addGroup(id, name, role string, policies ...fakePolicy) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.groups = append(f.groups, &fakeGroup{ID: id, Name: name, Role: role, Policies: policies})
}

func (f *fakeAPI) policyIDs(groupID string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	ids := []string{}
	for _, g := range f.groups {
		if g.ID == groupID {
			for _, p := range g.Policies {
				ids = append(ids, p.ID)
			}
		}
	}
	return ids
}

func (f *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("authorization") != "Bearer md_test" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var req struct {
		Query     string         `json:"query"`
		Variables map[string]any `json:"variables"`
	}
	b, _ := io.ReadAll(r.Body)
	_ = json.Unmarshal(b, &req)
	if req.Variables["org"] != "acme" {
		http.Error(w, "wrong org", http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()

	var data any
	switch {
	case strings.Contains(req.Query, "query FreezeGuardGroups"):
		items := []any{}
		for _, g := range f.groups {
			items = append(items, map[string]any{
				"id": g.ID, "name": g.Name, "role": g.Role,
				"policies": map[string]any{"cursor": map[string]any{"next": nil}, "items": g.Policies},
			})
		}
		data = map[string]any{"groups": map[string]any{"cursor": map[string]any{"next": nil}, "items": items}}
	case strings.Contains(req.Query, "mutation FreezeGuardCreatePolicy"):
		input := req.Variables["input"].(map[string]any)
		f.created = append(f.created, created{Group: req.Variables["group"].(string), Input: input})
		f.nextID++
		id := fmt.Sprintf("policy-%d", f.nextID)
		conditions := json.RawMessage(`"*"`)
		if c := input["conditions"].(string); c != "*" {
			conditions = json.RawMessage(c)
		}
		actions := []string{}
		for _, a := range input["actions"].([]any) {
			actions = append(actions, a.(string))
		}
		for _, g := range f.groups {
			if g.ID == req.Variables["group"] {
				g.Policies = append(g.Policies, fakePolicy{ID: id, Actions: actions, Conditions: conditions})
			}
		}
		data = map[string]any{"createGroupPolicy": map[string]any{"successful": true, "messages": []any{}, "result": map[string]any{"id": id}}}
	case strings.Contains(req.Query, "mutation FreezeGuardDeletePolicy"):
		id := req.Variables["id"].(string)
		f.deleted = append(f.deleted, id)
		found := false
		for _, g := range f.groups {
			for i, p := range g.Policies {
				if p.ID == id {
					g.Policies = append(g.Policies[:i], g.Policies[i+1:]...)
					found = true
					break
				}
			}
		}
		payload := map[string]any{"successful": true, "messages": []any{}, "result": map[string]any{"id": id}}
		if !found {
			payload = map[string]any{"successful": false, "messages": []any{map[string]any{"field": "id", "message": "Policy not found"}}}
		}
		data = map[string]any{"deletePolicy": payload}
	default:
		http.Error(w, "unexpected query", http.StatusBadRequest)
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
}

type harness struct {
	api      *fakeAPI
	slack    *slacktest.Server
	apiURL   string
	stateDir string
	now      time.Time
}

func setup(t *testing.T) *harness {
	t.Helper()
	api := &fakeAPI{}
	api.addGroup("group-admins", "Administrators", "ORGANIZATION_ADMIN")
	api.addGroup("group-contractors", "Contractors", "CUSTOM",
		fakePolicy{ID: "foreign-1", Actions: []string{"instance:deploy"}, Conditions: json.RawMessage(`{"md-environment":["prod"]}`)})
	api.addGroup("group-developers", "Developers", "CUSTOM")
	api.addGroup("group-release", "Release Managers", "CUSTOM")
	api.addGroup("group-sre", "SRE", "CUSTOM")
	apiSrv := httptest.NewServer(api)
	t.Cleanup(apiSrv.Close)
	return &harness{api: api, slack: slacktest.NewServer(t), apiURL: apiSrv.URL, stateDir: t.TempDir()}
}

func (h *harness) guard(t *testing.T) *Guard {
	t.Helper()
	windows, err := ParseWindows([]byte(windowsYAML))
	if err != nil {
		t.Fatal(err)
	}
	g, err := New(&massdriver.Client{Endpoint: h.apiURL, OrganizationID: "acme", APIKey: "md_test"}, h.slack.Bot(), windows,
		Options{StateDir: h.stateDir, AdminToken: "admin-token"})
	if err != nil {
		t.Fatal(err)
	}
	g.now = func() time.Time { return h.now }
	return g
}

func reconcile(t *testing.T, g *Guard) {
	t.Helper()
	if err := g.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func weekendInput() map[string]any {
	return map[string]any{
		"effect":     "DENY",
		"actions":    []any{"environment:decommission", "environment:deploy", "instance:decommission", "instance:deploy"},
		"conditions": `{"md-environment":["prod"]}`,
	}
}

func TestWindowStartCreatesDenyPolicyPerNonExemptGroup(t *testing.T) {
	h := setup(t)
	h.now = inWeekend
	reconcile(t, h.guard(t))

	want := []created{
		{Group: "group-contractors", Input: weekendInput()},
		{Group: "group-developers", Input: weekendInput()},
	}
	if !reflect.DeepEqual(h.api.created, want) {
		t.Fatalf("created = %#v", h.api.created)
	}
	if len(h.api.deleted) != 0 {
		t.Fatalf("deleted = %v", h.api.deleted)
	}
	for group, ids := range map[string][]string{
		"group-admins":      {},
		"group-contractors": {"foreign-1", "policy-1"},
		"group-developers":  {"policy-2"},
		"group-release":     {},
		"group-sre":         {},
	} {
		if got := h.api.policyIDs(group); !reflect.DeepEqual(got, ids) {
			t.Errorf("%s policies = %v, want %v", group, got, ids)
		}
	}
	wantText := "Freeze window \"weekend\" started. Denied environment:decommission, environment:deploy, " +
		"instance:decommission, instance:deploy where md-environment is prod.\n" +
		"Frozen groups: Contractors, Developers.\nExempt groups: Release Managers, SRE."
	calls := h.slack.Calls()
	if len(calls) != 1 || calls[0].Text != wantText || calls[0].Channel != "#changes" {
		t.Fatalf("slack = %+v", calls)
	}
}

func TestReconcileInsideWindowIsIdempotent(t *testing.T) {
	h := setup(t)
	h.now = inWeekend
	g := h.guard(t)
	reconcile(t, g)
	reconcile(t, g)
	if len(h.api.created) != 2 || len(h.slack.Texts()) != 1 {
		t.Fatalf("created = %v, slack = %q", h.api.created, h.slack.Texts())
	}
}

func TestWindowEndDeletesOnlyItsPolicies(t *testing.T) {
	h := setup(t)
	h.now = inWeekend
	g := h.guard(t)
	reconcile(t, g)

	h.now = time.Date(2026, 11, 30, 13, 0, 0, 0, time.UTC) // Monday 08:00 in New York.
	reconcile(t, g)

	if !reflect.DeepEqual(h.api.deleted, []string{"policy-1", "policy-2"}) {
		t.Fatalf("deleted = %v", h.api.deleted)
	}
	if got := h.api.policyIDs("group-contractors"); !reflect.DeepEqual(got, []string{"foreign-1"}) {
		t.Fatalf("contractors policies = %v", got)
	}
	if got := h.slack.Texts()[1]; got != `Freeze window "weekend" ended. Removed its deny policies.` {
		t.Fatalf("slack = %q", got)
	}
}

func TestNewGroupDuringWindowIsFrozenOnNextReconcile(t *testing.T) {
	h := setup(t)
	h.now = inWeekend
	g := h.guard(t)
	reconcile(t, g)

	h.api.addGroup("group-data", "Data", "CUSTOM")
	handler := automation.Handler(g, secret)
	body := []byte(`{"id":"evt-1","type":"group.created","data":{"entity":{"id":"group-data","name":"Data"}}}`)
	req := httptest.NewRequest(http.MethodPost, automation.HookPath, strings.NewReader(string(body)))
	automation.SignRequest(req, secret, "evt-1", body, time.Now())
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d", rec.Code)
	}
	select {
	case <-g.wake:
	default:
		t.Fatal("group.created did not wake the reconcile loop")
	}

	reconcile(t, g)
	if len(h.api.created) != 3 || !reflect.DeepEqual(h.api.created[2], created{Group: "group-data", Input: weekendInput()}) {
		t.Fatalf("created = %#v", h.api.created)
	}
}

func TestDeletedPolicyIsCreatedAgain(t *testing.T) {
	h := setup(t)
	h.now = inWeekend
	g := h.guard(t)
	reconcile(t, g)

	h.api.mu.Lock()
	h.api.groups[2].Policies = nil // someone deleted Developers' deny policy by hand
	h.api.mu.Unlock()
	reconcile(t, g)
	if got := h.api.policyIDs("group-developers"); !reflect.DeepEqual(got, []string{"policy-3"}) {
		t.Fatalf("developers policies = %v", got)
	}
}

func TestRestartWithState(t *testing.T) {
	h := setup(t)
	h.now = inWeekend
	reconcile(t, h.guard(t))

	b, err := os.ReadFile(filepath.Join(h.stateDir, "freeze-guard.json"))
	if err != nil {
		t.Fatal(err)
	}
	want := `{"policies":{"weekend":{"group-contractors":"policy-1","group-developers":"policy-2"}},"active":{"weekend":true},"lifts":{}}`
	if string(b) != want {
		t.Fatalf("state = %s", b)
	}

	restarted := h.guard(t)
	reconcile(t, restarted)
	if len(h.api.created) != 2 || len(h.slack.Texts()) != 1 {
		t.Fatalf("created = %v, slack = %q", h.api.created, h.slack.Texts())
	}

	h.now = weekday.AddDate(0, 0, 7)
	reconcile(t, h.guard(t))
	if len(h.api.deleted) != 2 {
		t.Fatalf("deleted = %v", h.api.deleted)
	}
}

func TestRestartWithoutStateAdoptsExactMatches(t *testing.T) {
	h := setup(t)
	h.api.addGroup("group-data", "Data", "CUSTOM",
		fakePolicy{ID: "lost-1", Actions: []string{"instance:deploy", "instance:decommission", "environment:deploy", "environment:decommission"},
			Conditions: json.RawMessage(`{"md-environment":["prod"]}`)},
		fakePolicy{ID: "near-1", Actions: []string{"instance:deploy", "instance:decommission", "environment:deploy", "environment:decommission"},
			Conditions: json.RawMessage(`{"md-environment":["prod","staging"]}`)},
		fakePolicy{ID: "lost-2", Actions: []string{"instance:deploy"}, Conditions: json.RawMessage(`{"md-project":["web","api"]}`)},
	)
	h.now = weekday
	reconcile(t, h.guard(t))

	if !reflect.DeepEqual(h.api.deleted, []string{"lost-1", "lost-2"}) {
		t.Fatalf("deleted = %v", h.api.deleted)
	}
	if got := h.api.policyIDs("group-data"); !reflect.DeepEqual(got, []string{"near-1"}) {
		t.Fatalf("data policies = %v", got)
	}
	if got := h.api.policyIDs("group-contractors"); !reflect.DeepEqual(got, []string{"foreign-1"}) {
		t.Fatalf("contractors policies = %v", got)
	}
}

func TestRestartWithoutStateInsideWindowKeepsExactMatch(t *testing.T) {
	h := setup(t)
	h.now = inWeekend
	reconcile(t, h.guard(t))
	if err := os.Remove(filepath.Join(h.stateDir, "freeze-guard.json")); err != nil {
		t.Fatal(err)
	}

	reconcile(t, h.guard(t))
	if len(h.api.created) != 2 || len(h.api.deleted) != 0 {
		t.Fatalf("created = %v, deleted = %v", h.api.created, h.api.deleted)
	}
}

func lift(t *testing.T, g *Guard, token, window string) *httptest.ResponseRecorder {
	t.Helper()
	mux := http.NewServeMux()
	g.Routes(mux)
	req := httptest.NewRequest(http.MethodPost, "/freeze/lift?window="+window, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func TestEmergencyLift(t *testing.T) {
	h := setup(t)
	h.now = inWeekend
	g := h.guard(t)
	reconcile(t, g)

	if rec := lift(t, g, "wrong", "weekend"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong token status = %d", rec.Code)
	}
	if rec := lift(t, g, "admin-token", "holiday"); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown window status = %d", rec.Code)
	}
	if rec := lift(t, g, "admin-token", "thanksgiving"); rec.Code != http.StatusConflict {
		t.Fatalf("inactive window status = %d", rec.Code)
	}

	rec := lift(t, g, "admin-token", "weekend")
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != `{"until":"2026-12-04T22:00:00Z","window":"weekend"}` {
		t.Fatalf("lift = %d %s", rec.Code, rec.Body)
	}
	if len(h.api.deleted) != 2 || len(h.api.policyIDs("group-developers")) != 0 {
		t.Fatalf("deleted = %v", h.api.deleted)
	}
	if got := h.slack.Texts()[1]; got != `Freeze window "weekend" lifted until 2026-12-04 17:00 EST. Removed its deny policies.` {
		t.Fatalf("slack = %q", got)
	}

	// The lift survives a restart and holds for the rest of this weekend.
	h.now = time.Date(2026, 11, 29, 17, 0, 0, 0, time.UTC)
	reconcile(t, h.guard(t))
	if len(h.api.created) != 2 {
		t.Fatalf("created during lift = %v", h.api.created)
	}

	// The next weekend freezes again.
	h.now = time.Date(2026, 12, 4, 22, 0, 0, 0, time.UTC)
	reconcile(t, h.guard(t))
	if len(h.api.created) != 4 {
		t.Fatalf("created after lift = %v", h.api.created)
	}
}

func TestLiftRouteNeedsToken(t *testing.T) {
	h := setup(t)
	g := h.guard(t)
	g.opts.AdminToken = ""
	if rec := lift(t, g, "", "weekend"); rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestFixedWindowWithOwnActions(t *testing.T) {
	h := setup(t)
	h.now = time.Date(2026, 11, 26, 15, 0, 0, 0, time.UTC)
	reconcile(t, h.guard(t))
	input := map[string]any{"effect": "DENY", "actions": []any{"instance:deploy"}, "conditions": `{"md-project":["api","web"]}`}
	want := []created{{Group: "group-contractors", Input: input}, {Group: "group-developers", Input: input}, {Group: "group-release", Input: input}}
	if !reflect.DeepEqual(h.api.created, want) {
		t.Fatalf("created = %#v", h.api.created)
	}
	wantText := "Freeze window \"thanksgiving\" started. Denied instance:deploy where md-project is one of api, web.\n" +
		"Frozen groups: Contractors, Developers, Release Managers.\nExempt groups: SRE."
	calls := h.slack.Calls()
	if len(calls) != 1 || calls[0].Text != wantText || calls[0].Channel != "#api-team" {
		t.Fatalf("slack = %+v", calls)
	}
}

func TestWildcardConditions(t *testing.T) {
	ws, err := ParseWindows([]byte("windows: [{name: all, from: 2026-11-26, to: 2026-11-27}, {name: any-project, match: {md-project: '*'}, from: 2026-11-26, to: 2026-11-27}]"))
	if err != nil {
		t.Fatal(err)
	}
	if got := ws[0].conditionsInput(); got != "*" {
		t.Errorf("no match = %q", got)
	}
	if got := ws[1].conditionsInput(); got != `{"md-project":"*"}` {
		t.Errorf("per-key wildcard = %q", got)
	}
	if !matches(policy{Actions: DefaultActions, Conditions: json.RawMessage(`"*"`)}, ws[0]) {
		t.Error("wildcard policy does not match a window with no match")
	}
}

func TestContainsAndNextStart(t *testing.T) {
	ws, err := ParseWindows([]byte(windowsYAML))
	if err != nil {
		t.Fatal(err)
	}
	weekend := ws[0]
	cases := []struct {
		at   time.Time
		want bool
	}{
		{inWeekend, true},
		{time.Date(2026, 11, 29, 17, 0, 0, 0, time.UTC), true},
		{time.Date(2026, 11, 30, 13, 0, 0, 0, time.UTC), false},
		{weekday, false},
	}
	for _, tc := range cases {
		if got := weekend.Contains(tc.at); got != tc.want {
			t.Errorf("Contains(%s) = %v", tc.at, got)
		}
	}
	if got := weekend.NextStart(weekday); !got.Equal(time.Date(2026, 11, 27, 22, 0, 0, 0, time.UTC)) {
		t.Errorf("NextStart(weekday) = %s", got)
	}
	if got := ws[1].NextStart(weekday); !got.Equal(time.Date(2026, 11, 27, 5, 0, 0, 0, time.UTC)) {
		t.Errorf("fixed NextStart = %s", got)
	}
}

func TestParseWindowsErrors(t *testing.T) {
	cases := map[string]string{
		"no name":        "windows: [{from: 2026-11-26, to: 2026-11-27}]",
		"duplicate name": "windows: [{name: x, from: 2026-11-26, to: 2026-11-27}, {name: x, from: 2026-11-28, to: 2026-11-29}]",
		"bad action":     "windows: [{name: x, actions: [deploy], from: 2026-11-26, to: 2026-11-27}]",
		"both kinds":     "windows: [{name: x, from: 2026-11-26, to: 2026-11-27, weekly: [{from: Fri 17:00, to: Mon 08:00}]}]",
		"neither kind":   "windows: [{name: x}]",
		"bad timezone":   "windows: [{name: x, timezone: Mars/Base, from: 2026-11-26, to: 2026-11-27}]",
		"bad weekday":    "windows: [{name: x, weekly: [{from: Funday 17:00, to: Mon 08:00}]}]",
		"backwards":      "windows: [{name: x, from: 2026-11-27, to: 2026-11-26}]",
		"unknown key":    "windows: [{name: x, form: 2026-11-26, to: 2026-11-27}]",
		"slack url":      "windows: [{name: x, from: 2026-11-26, to: 2026-11-27, slack: {webhook_url: https://hooks.slack.com/x}}]",
	}
	for name, src := range cases {
		if _, err := ParseWindows([]byte(src)); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

func TestRemovedWindowPoliciesAreDeleted(t *testing.T) {
	h := setup(t)
	h.now = inWeekend
	reconcile(t, h.guard(t))

	windows, err := ParseWindows([]byte("windows: [{name: holiday, from: 2026-12-24, to: 2026-12-26}]"))
	if err != nil {
		t.Fatal(err)
	}
	g, err := New(&massdriver.Client{Endpoint: h.apiURL, OrganizationID: "acme", APIKey: "md_test"}, nil, windows, Options{StateDir: h.stateDir})
	if err != nil {
		t.Fatal(err)
	}
	g.now = func() time.Time { return h.now }
	reconcile(t, g)
	if !reflect.DeepEqual(h.api.deleted, []string{"policy-1", "policy-2"}) {
		t.Fatalf("deleted = %v", h.api.deleted)
	}
	b, err := os.ReadFile(filepath.Join(h.stateDir, "freeze-guard.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `{"policies":{},"active":{},"lifts":{}}` {
		t.Fatalf("state = %s", b)
	}
}

func TestExampleConfigLoads(t *testing.T) {
	var cfg Config
	if err := automation.LoadConfigFile("config.example.yaml", &cfg); err != nil {
		t.Fatal(err)
	}
	if len(cfg.windows) != 2 || cfg.windows[1].Slack.Channel != "#release" || cfg.windows[0].Slack.Channel != "#changes" {
		t.Fatalf("windows = %+v", cfg.windows)
	}
}

func TestNoConfigFileIsValid(t *testing.T) {
	t.Setenv("CONFIG_FILE", filepath.Join(t.TempDir(), "missing.yaml"))
	var cfg Config
	if err := automation.LoadConfig(&cfg); err != nil || len(cfg.windows) != 0 {
		t.Fatalf("windows = %v, err = %v", cfg.windows, err)
	}
}
