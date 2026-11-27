package prsync

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/massdriver-cloud/massdriver-automations/pkg/automation"
	"github.com/massdriver-cloud/massdriver-automations/pkg/massdriver"
)

var start = time.Date(2026, 11, 27, 9, 0, 0, 0, time.UTC)

// fakeGitHub serves the PullRequests query from canned pull requests by repository.
type fakeGitHub struct {
	mu      sync.Mutex
	open    map[string][]map[string]any
	merged  map[string][]map[string]any
	fail    map[string]int
	queries map[string]int
	token   string
}

func (f *fakeGitHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Query     string         `json:"query"`
		Variables map[string]any `json:"variables"`
	}
	b, _ := io.ReadAll(r.Body)
	_ = json.Unmarshal(b, &req)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.token = r.Header.Get("authorization")
	repo := fmt.Sprintf("%s/%s", req.Variables["owner"], req.Variables["name"])
	f.queries[repo]++
	if req.Variables["first"] != float64(maxPRsLimit) || !strings.Contains(req.Query, "query PullRequests") {
		http.Error(w, "bad query", http.StatusBadRequest)
		return
	}
	if code := f.fail[repo]; code != 0 {
		if code == http.StatusOK {
			_, _ = w.Write([]byte(`{"data":{"repository":null},"errors":[{"message":"Could not resolve to a Repository"}]}`))
			return
		}
		http.Error(w, "down", code)
		return
	}
	out := map[string]any{"data": map[string]any{"repository": map[string]any{
		"open":   map[string]any{"nodes": nonNil(f.open[repo])},
		"merged": map[string]any{"nodes": nonNil(f.merged[repo])},
	}}}
	_ = json.NewEncoder(w).Encode(out)
}

func nonNil(n []map[string]any) []map[string]any {
	if n == nil {
		return []map[string]any{}
	}
	return n
}

func (f *fakeGitHub) count(repo string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.queries[repo]
}

type setCall struct {
	Scope map[string]any
	Key   string
	Input map[string]any
}

// fakeAPI serves the stash mutation of the Massdriver API.
type fakeAPI struct {
	mu   sync.Mutex
	sets []setCall
}

func (f *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Variables map[string]any `json:"variables"`
	}
	b, _ := io.ReadAll(r.Body)
	_ = json.Unmarshal(b, &req)
	if req.Variables["org"] != "tendwell-health" {
		http.Error(w, "wrong org", http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	call := setCall{Scope: req.Variables["scope"].(map[string]any), Key: req.Variables["key"].(string),
		Input: req.Variables["input"].(map[string]any)}
	f.sets = append(f.sets, call)
	_, _ = w.Write([]byte(`{"data":{"alphaSetStashEntry":{"successful":true,"messages":[],"result":{"key":"x"}}}}`))
}

func (f *fakeAPI) calls() []setCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]setCall(nil), f.sets...)
}

type env struct {
	gh    *fakeGitHub
	api   *fakeAPI
	clock time.Time
	s     *Sync
}

const twoProjects = `
projects:
  - project: visits
    repos: [tendwell-health/visits, tendwell-health/infra]
  - project: billing
    repos: [tendwell-health/billing, tendwell-health/infra]
`

func setup(t *testing.T, cfgYAML string) *env {
	t.Helper()
	var cfg Config
	if err := automation.DecodeConfig([]byte(cfgYAML), &cfg); err != nil {
		t.Fatal(err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	e := &env{
		gh: &fakeGitHub{open: map[string][]map[string]any{}, merged: map[string][]map[string]any{},
			fail: map[string]int{}, queries: map[string]int{}},
		api:   &fakeAPI{},
		clock: start,
	}
	ghSrv := httptest.NewServer(e.gh)
	t.Cleanup(ghSrv.Close)
	apiSrv := httptest.NewServer(e.api)
	t.Cleanup(apiSrv.Close)
	gh := &GraphQLGitHub{URL: ghSrv.URL, Token: "ghp_test", HTTP: ghSrv.Client()}
	client := &massdriver.Client{Endpoint: apiSrv.URL, OrganizationID: "tendwell-health", APIKey: "md_test"}
	e.s = New(&cfg, gh, &GraphQLStash{API: client}, func() time.Time { return e.clock })
	return e
}

func (e *env) cycle() { e.s.Cycle(context.Background()) }

// pr builds a GitHub pull request node.
func pr(number int, title string, updated time.Duration, opts ...func(map[string]any)) map[string]any {
	n := map[string]any{
		"number": number, "title": title, "url": fmt.Sprintf("https://github.com/x/y/pull/%d", number),
		"isDraft": false, "updatedAt": start.Add(-updated).Format(time.RFC3339), "mergedAt": nil,
		"reviewDecision": "REVIEW_REQUIRED", "isInMergeQueue": false, "author": map[string]any{"login": "lee"},
		"autoMergeRequest": nil, "commits": map[string]any{"nodes": []any{}},
	}
	for _, o := range opts {
		o(n)
	}
	return n
}

func checks(state string) func(map[string]any) {
	return func(n map[string]any) {
		n["commits"] = map[string]any{"nodes": []any{map[string]any{"commit": map[string]any{
			"statusCheckRollup": map[string]any{"state": state}}}}}
	}
}

func set(k string, v any) func(map[string]any) { return func(n map[string]any) { n[k] = v } }

func merged(ago time.Duration) func(map[string]any) {
	return func(n map[string]any) {
		n["mergedAt"] = start.Add(-ago).Format(time.RFC3339)
		n["updatedAt"] = n["mergedAt"]
	}
}

func decode(t *testing.T, c setCall) Value {
	t.Helper()
	var v Value
	if err := json.Unmarshal([]byte(c.Input["value"].(string)), &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func states(v Value) []string {
	out := make([]string, len(v.PRs))
	for i, p := range v.PRs {
		out[i] = fmt.Sprintf("%s#%d %s", p.Repo, p.Number, p.State)
	}
	return out
}

func TestEntryHasEveryStateInOrder(t *testing.T) {
	e := setup(t, `
projects:
  - project: visits
    repos: [tendwell-health/visits]
`)
	e.gh.open["tendwell-health/visits"] = []map[string]any{
		pr(1, "Plain", time.Minute),
		pr(2, "Draft", time.Minute, set("isDraft", true)),
		pr(3, "Approved", time.Minute, set("reviewDecision", "APPROVED"), checks("SUCCESS")),
		pr(4, "CI running", time.Minute, checks("PENDING")),
		pr(5, "CI red", time.Minute, set("reviewDecision", "APPROVED"), checks("FAILURE")),
		pr(6, "Changes", time.Minute, set("reviewDecision", "CHANGES_REQUESTED"), checks("SUCCESS")),
		pr(7, "Queued", time.Minute, set("isInMergeQueue", true), checks("PENDING")),
		pr(8, "Auto", time.Minute, set("autoMergeRequest", map[string]any{"enabledAt": start.Format(time.RFC3339)}),
			checks("PENDING")),
		pr(9, "Auto but red", 2*time.Minute, set("autoMergeRequest", map[string]any{"enabledAt": start.Format(time.RFC3339)}),
			checks("ERROR")),
		pr(10, "Newer review", 0, set("author", nil)),
	}
	e.gh.merged["tendwell-health/visits"] = []map[string]any{
		pr(11, "Merged an hour ago", 0, merged(time.Hour)),
		pr(12, "Merged just now", 0, merged(time.Minute)),
		pr(13, "Merged two days ago", 0, merged(48*time.Hour)),
	}
	e.cycle()

	calls := e.api.calls()
	if len(calls) != 1 {
		t.Fatalf("calls = %+v", calls)
	}
	c := calls[0]
	if !reflect.DeepEqual(c.Scope, map[string]any{"project": "visits"}) || c.Key != "github/prs" {
		t.Errorf("target = %v %s", c.Scope, c.Key)
	}
	if c.Input["format"] != "JSON" || c.Input["ttl"] != float64(360) || c.Input["label"] != "Pull requests" {
		t.Errorf("input = %v", c.Input)
	}
	v := decode(t, c)
	want := []string{
		"visits#7 merging", "visits#8 merging", "visits#4 checks", "visits#5 failed", "visits#6 failed",
		"visits#9 failed", "visits#3 approved", "visits#10 review", "visits#1 review", "visits#2 draft",
		"visits#12 merged", "visits#11 merged",
	}
	if got := states(v); !reflect.DeepEqual(got, want) {
		t.Errorf("order =\n%v\nwant\n%v", got, want)
	}
	if !v.UpdatedAt.Equal(start) {
		t.Errorf("updated_at = %v", v.UpdatedAt)
	}
	byNumber := map[int]PR{}
	for _, p := range v.PRs {
		byNumber[p.Number] = p
	}
	if p := byNumber[10]; p.Author != "ghost" || p.Review != "review_required" || p.Checks != "none" {
		t.Errorf("pr 10 = %+v", p)
	}
	if p := byNumber[2]; !p.Draft || p.MergedAt != nil {
		t.Errorf("pr 2 = %+v", p)
	}
	if p := byNumber[12]; p.MergedAt == nil || !p.MergedAt.Equal(start.Add(-time.Minute)) {
		t.Errorf("pr 12 = %+v", p)
	}
	if e.gh.token != "Bearer ghp_test" {
		t.Errorf("authorization = %q", e.gh.token)
	}
}

func TestEntryValueHasTheDocumentedKeys(t *testing.T) {
	e := setup(t, `
projects:
  - project: visits
    repos: [tendwell-health/visits]
`)
	e.gh.open["tendwell-health/visits"] = []map[string]any{pr(1, "Add visits", 0)}
	e.cycle()
	var raw map[string]any
	if err := json.Unmarshal([]byte(e.api.calls()[0].Input["value"].(string)), &raw); err != nil {
		t.Fatal(err)
	}
	got := raw["prs"].([]any)[0].(map[string]any)
	want := map[string]any{
		"repo": "visits", "number": float64(1), "title": "Add visits", "author": "lee",
		"url": "https://github.com/x/y/pull/1", "state": "review", "draft": false, "review": "review_required",
		"checks": "none", "updated_at": "2026-11-27T09:00:00Z", "merged_at": nil,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("pr =\n%v\nwant\n%v", got, want)
	}
	if raw["updated_at"] != "2026-11-27T09:00:00Z" {
		t.Errorf("updated_at = %v", raw["updated_at"])
	}
}

func TestSharedRepoIsQueriedOnceAndEachProjectGetsItsOwnEntry(t *testing.T) {
	e := setup(t, twoProjects)
	e.gh.open["tendwell-health/infra"] = []map[string]any{pr(1, "Shared", 0)}
	e.gh.open["tendwell-health/billing"] = []map[string]any{pr(2, "Billing", 0)}
	e.cycle()
	if n := e.gh.count("tendwell-health/infra"); n != 1 {
		t.Errorf("infra queried %d times", n)
	}
	got := map[string][]string{}
	for _, c := range e.api.calls() {
		got[c.Scope["project"].(string)] = states(decode(t, c))
	}
	want := map[string][]string{
		"visits":  {"infra#1 review"},
		"billing": {"billing#2 review", "infra#1 review"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("entries = %v, want %v", got, want)
	}
}

func TestWritesOnlyWhenChangedAndBeforeExpiry(t *testing.T) {
	e := setup(t, `
projects:
  - project: visits
    repos: [tendwell-health/visits]
`)
	e.gh.open["tendwell-health/visits"] = []map[string]any{pr(1, "A", 0)}
	e.cycle()
	e.clock = e.clock.Add(2 * time.Minute)
	e.cycle()
	if n := len(e.api.calls()); n != 1 {
		t.Fatalf("unchanged entry written again: %d writes", n)
	}

	e.gh.mu.Lock()
	e.gh.open["tendwell-health/visits"] = []map[string]any{pr(1, "A", 0, checks("PENDING"))}
	e.gh.mu.Unlock()
	e.clock = e.clock.Add(2 * time.Minute)
	e.cycle()
	calls := e.api.calls()
	if len(calls) != 2 || decode(t, calls[1]).PRs[0].State != StateChecks {
		t.Fatalf("calls = %+v", calls)
	}

	// The TTL is three intervals. An unchanged entry is written again on the second cycle after its write.
	e.clock = e.clock.Add(2 * time.Minute)
	e.cycle()
	if n := len(e.api.calls()); n != 2 {
		t.Fatalf("writes = %d after one interval, want 2", n)
	}
	e.clock = e.clock.Add(2 * time.Minute)
	e.cycle()
	if n := len(e.api.calls()); n != 3 {
		t.Fatalf("writes = %d after two intervals, want 3", n)
	}
}

func TestFailedRepoSkipsOnlyItsProjects(t *testing.T) {
	e := setup(t, twoProjects+`  - project: other
    repos: [tendwell-health/other]
`)
	e.gh.fail["tendwell-health/billing"] = http.StatusBadGateway
	e.gh.fail["tendwell-health/other"] = http.StatusOK
	e.cycle()
	var projects []string
	for _, c := range e.api.calls() {
		projects = append(projects, c.Scope["project"].(string))
	}
	if !reflect.DeepEqual(projects, []string{"visits"}) {
		t.Errorf("written = %v, want only visits", projects)
	}
}

func TestMergedWindowZeroShowsOnlyOpen(t *testing.T) {
	e := setup(t, `
merged_window: 0s
projects:
  - project: visits
    repos: [tendwell-health/visits]
`)
	e.gh.open["tendwell-health/visits"] = []map[string]any{pr(1, "Open", 0)}
	e.gh.merged["tendwell-health/visits"] = []map[string]any{pr(2, "Merged", 0, merged(time.Second))}
	e.cycle()
	if got := states(decode(t, e.api.calls()[0])); !reflect.DeepEqual(got, []string{"visits#1 review"}) {
		t.Errorf("prs = %v", got)
	}
}

func TestMaxPRsKeepsInFlightFirst(t *testing.T) {
	e := setup(t, `
max_prs: 2
projects:
  - project: visits
    repos: [tendwell-health/visits]
`)
	e.gh.open["tendwell-health/visits"] = []map[string]any{
		pr(1, "Draft", 0, set("isDraft", true)), pr(2, "Review", 0), pr(3, "Queued", time.Hour, set("isInMergeQueue", true)),
	}
	e.gh.merged["tendwell-health/visits"] = []map[string]any{pr(4, "Merged", 0, merged(time.Minute))}
	e.cycle()
	if got := states(decode(t, e.api.calls()[0])); !reflect.DeepEqual(got, []string{"visits#3 merging", "visits#2 review"}) {
		t.Errorf("prs = %v", got)
	}
}

func TestEmptyRepoWritesAnEmptyList(t *testing.T) {
	e := setup(t, `
projects:
  - project: visits
    repos: [tendwell-health/visits]
`)
	e.cycle()
	if got := e.api.calls()[0].Input["value"].(string); !strings.HasPrefix(got, `{"prs":[],`) {
		t.Errorf("value = %s", got)
	}
}

func TestHandleIgnoresEvents(t *testing.T) {
	e := setup(t, twoProjects)
	if err := e.s.Handle(context.Background(), automation.Event{Type: "deployment.completed"}); err != nil {
		t.Fatal(err)
	}
	if n := len(e.api.calls()); n != 0 {
		t.Errorf("writes = %d", n)
	}
}

func TestConfigErrors(t *testing.T) {
	for name, tc := range map[string]struct{ yaml, want string }{
		"no projects":    {"interval: 2m\n", "add at least one project"},
		"short interval": {"interval: 5s\n" + twoProjects, "interval must be at least 30s"},
		"bad repo":       {"projects:\n  - project: visits\n    repos: [visits]\n", "must look like owner/name"},
		"no repos":       {"projects:\n  - project: visits\n", "must name at least one repository"},
		"duplicate":      {twoProjects + "  - project: visits\n    repos: [a/b]\n", "appears more than once"},
		"max_prs":        {"max_prs: 51\n" + twoProjects, "max_prs must be from 1 to 50"},
		"key":            {"key: 'has space'\n" + twoProjects, "key must be"},
	} {
		t.Run(name, func(t *testing.T) {
			var cfg Config
			if err := automation.DecodeConfig([]byte(tc.yaml), &cfg); err != nil {
				t.Fatal(err)
			}
			err := cfg.Validate()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestNullSettingsTakeDefaults(t *testing.T) {
	var cfg Config
	doc := "interval: null\nkey: null\nlabel: null\nmerged_window: null\nmax_prs: null\n" +
		"github: {graphql_url: null, token_env: null}\n" + twoProjects
	if err := automation.DecodeConfig([]byte(doc), &cfg); err != nil {
		t.Fatal(err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if time.Duration(cfg.Interval) != 2*time.Minute || cfg.Key != "github/prs" || cfg.Label != "Pull requests" ||
		time.Duration(*cfg.MergedWindow) != 24*time.Hour || cfg.MaxPRs != 20 ||
		cfg.GitHub.GraphQLURL != "https://api.github.com/graphql" || cfg.GitHub.TokenEnv != "GITHUB_TOKEN" {
		t.Errorf("config = %+v", cfg)
	}
}

func TestExampleConfigLoads(t *testing.T) {
	var cfg Config
	if err := automation.LoadConfigFile("config.example.yaml", &cfg); err != nil {
		t.Fatal(err)
	}
	if len(cfg.Projects) != 2 || cfg.MaxPRs != 20 || time.Duration(*cfg.MergedWindow) != 24*time.Hour {
		t.Errorf("config = %+v", cfg)
	}
}
