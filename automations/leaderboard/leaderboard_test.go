package leaderboard

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/massdriver-cloud/massdriver-automations/internal/widgetschema"
	"github.com/massdriver-cloud/massdriver-automations/pkg/automation"
	"github.com/massdriver-cloud/massdriver-automations/pkg/massdriver"
)

var start = time.Date(2026, 11, 27, 9, 0, 0, 0, time.UTC)

const day = 24 * time.Hour

// fakeGitHub serves the PullRequests query from canned pull requests by repository, newest update first, and
// applies the states, base, and labels arguments the way GitHub does.
type fakeGitHub struct {
	mu      sync.Mutex
	prs     map[string][]map[string]any
	fail    map[string]int
	queries []map[string]any
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
	v := req.Variables
	f.queries = append(f.queries, v)
	if !strings.Contains(req.Query, "query PullRequests") || v["first"] != float64(pageSize) {
		http.Error(w, "bad query", http.StatusBadRequest)
		return
	}
	repo := fmt.Sprintf("%s/%s", v["owner"], v["name"])
	if code := f.fail[repo]; code != 0 {
		http.Error(w, "down", code)
		return
	}
	var match []map[string]any
	for _, pr := range f.prs[repo] {
		if states, ok := v["states"].([]any); ok && !slices.Contains(states, pr["_state"]) {
			continue
		}
		if base, ok := v["base"].(string); ok && pr["_base"] != base {
			continue
		}
		node := map[string]any{}
		for k, val := range pr {
			if !strings.HasPrefix(k, "_") && (k != "files" || v["files"] == true) {
				node[k] = val
			}
		}
		labels := []any{}
		for _, l := range pr["_labels"].([]string) {
			labels = append(labels, map[string]any{"name": l})
		}
		node["labels"] = map[string]any{"nodes": labels}
		match = append(match, node)
	}
	from := 0
	if after, ok := v["after"].(string); ok {
		from, _ = strconv.Atoi(after)
	}
	to := min(from+pageSize, len(match))
	page := match[from:to]
	if page == nil {
		page = []map[string]any{}
	}
	out := map[string]any{"data": map[string]any{"repository": map[string]any{"pullRequests": map[string]any{
		"pageInfo": map[string]any{"hasNextPage": to < len(match), "endCursor": strconv.Itoa(to)},
		"nodes":    page,
	}}}}
	_ = json.NewEncoder(w).Encode(out)
}

func (f *fakeGitHub) add(repo string, prs ...map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.prs[repo] = append(f.prs[repo], prs...)
	slices.SortStableFunc(f.prs[repo], func(a, b map[string]any) int {
		return strings.Compare(b["updatedAt"].(string), a["updatedAt"].(string))
	})
}

func (f *fakeGitHub) calls() []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]map[string]any(nil), f.queries...)
}

type setCall struct {
	Scope map[string]any
	Key   string
	Input map[string]any
}

// fakeAPI serves the stash mutation of the Massdriver API.
type fakeAPI struct {
	mu      sync.Mutex
	sets    []setCall
	invalid []error
}

func (f *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Variables map[string]any `json:"variables"`
	}
	b, _ := io.ReadAll(r.Body)
	_ = json.Unmarshal(b, &req)
	f.mu.Lock()
	defer f.mu.Unlock()
	c := setCall{Scope: req.Variables["scope"].(map[string]any), Key: req.Variables["key"].(string),
		Input: req.Variables["input"].(map[string]any)}
	f.sets = append(f.sets, c)
	for scope := range c.Scope {
		if err := widgetschema.Check(scope, c.Key, c.Input["format"].(string), c.Input["value"].(string)); err != nil {
			f.invalid = append(f.invalid, err)
		}
	}
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
	l     *Leaderboard
}

func setup(t *testing.T, cfgYAML string) *env {
	t.Helper()
	var cfg Config
	if err := automation.DecodeConfig([]byte(cfgYAML), &cfg); err != nil {
		t.Fatal(err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	e := &env{gh: &fakeGitHub{prs: map[string][]map[string]any{}, fail: map[string]int{}}, api: &fakeAPI{}, clock: start}
	ghSrv := httptest.NewServer(e.gh)
	t.Cleanup(ghSrv.Close)
	apiSrv := httptest.NewServer(e.api)
	t.Cleanup(apiSrv.Close)
	t.Cleanup(func() {
		for _, err := range e.api.invalid {
			t.Errorf("the server would reject this write: %v", err)
		}
	})
	gh := &GraphQLGitHub{URL: ghSrv.URL, Token: "ghp_test", HTTP: ghSrv.Client()}
	client := &massdriver.Client{Endpoint: apiSrv.URL, OrganizationID: "tendwell-health", APIKey: "md_test"}
	e.l = New(&cfg, gh, client, func() time.Time { return e.clock })
	return e
}

func (e *env) cycle() { e.l.Cycle(context.Background()) }

// pr builds a pull request by login. It is merged ago before start unless an option changes it.
func pr(login string, ago time.Duration, opts ...func(map[string]any)) map[string]any {
	at := start.Add(-ago).Format(time.RFC3339)
	n := map[string]any{
		"createdAt": at, "updatedAt": at, "mergedAt": at, "additions": 10, "deletions": 5,
		"author": map[string]any{"__typename": "User", "login": login,
			"avatarUrl": "https://avatars.githubusercontent.com/u/1?v=4", "name": ""},
		"files":  map[string]any{"nodes": []any{map[string]any{"path": "README.md"}}},
		"_state": "MERGED", "_base": "main", "_labels": []string{},
	}
	for _, o := range opts {
		o(n)
	}
	return n
}

func open(n map[string]any) { n["mergedAt"] = nil; n["_state"] = "OPEN" }

func set(k string, v any) func(map[string]any) { return func(n map[string]any) { n[k] = v } }

func author(typ, name string) func(map[string]any) {
	return func(n map[string]any) {
		a := n["author"].(map[string]any)
		a["__typename"], a["name"] = typ, name
	}
}

func files(paths ...string) func(map[string]any) {
	return func(n map[string]any) {
		nodes := make([]any, len(paths))
		for i, p := range paths {
			nodes[i] = map[string]any{"path": p}
		}
		n["files"] = map[string]any{"nodes": nodes}
	}
}

type value struct {
	Title   string           `json:"title"`
	Entries []map[string]any `json:"entries"`
}

func decode(t *testing.T, c setCall) value {
	t.Helper()
	var v value
	if err := json.Unmarshal([]byte(c.Input["value"].(string)), &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func handles(v value) []string {
	out := make([]string, len(v.Entries))
	for i, p := range v.Entries {
		out[i] = p["handles"].([]any)[0].(string)
	}
	return out
}

func TestRanksMergedPullRequestsAndWritesEachEnvironment(t *testing.T) {
	e := setup(t, `
routes:
  - environments: [billing-prod, billing-staging]
    title: Most merged this month
    pull_requests:
      repos: [tendwell-health/billing, tendwell-health/infra]
`)
	e.gh.add("tendwell-health/billing",
		pr("jo", day, author("User", "Jo Park")), pr("jo", 2*day), pr("lee", 3*day),
		pr("sam", time.Hour, func(n map[string]any) { open(n) }),
		pr("old", 40*day),
	)
	e.gh.add("tendwell-health/infra", pr("lee", day), pr("lee", 2*day), pr("ana", day))
	e.cycle()

	calls := e.api.calls()
	if len(calls) != 2 {
		t.Fatalf("calls = %+v", calls)
	}
	envs := []string{calls[0].Scope["environment"].(string), calls[1].Scope["environment"].(string)}
	if !reflect.DeepEqual(envs, []string{"billing-prod", "billing-staging"}) {
		t.Errorf("environments = %v", envs)
	}
	c := calls[0]
	if c.Key != "md/v1/leaderboard" || c.Input["format"] != "JSON" || c.Input["label"] != "Leaderboard" ||
		c.Input["ttl"] != float64(2700) {
		t.Errorf("write = %s %v", c.Key, c.Input)
	}
	v := decode(t, c)
	if v.Title != "Most merged this month" {
		t.Errorf("title = %q", v.Title)
	}
	if got := handles(v); !reflect.DeepEqual(got, []string{"github:lee", "github:jo", "github:ana"}) {
		t.Errorf("players = %v", got)
	}
	jo := v.Entries[1]
	want := map[string]any{"name": "Jo Park", "image_url": "https://avatars.githubusercontent.com/u/1?v=4",
		"handles": []any{"github:jo"}}
	if !reflect.DeepEqual(jo, want) {
		t.Errorf("jo = %v", jo)
	}
	if v.Entries[0]["name"] != "lee" {
		t.Errorf("a user without a name shows the login: %v", v.Entries[0])
	}
	for _, q := range e.gh.calls() {
		if !reflect.DeepEqual(q["states"], []any{"MERGED"}) || q["files"] != false || q["base"] != nil {
			t.Errorf("query = %v", q)
		}
	}
	if e.gh.token != "Bearer ghp_test" {
		t.Errorf("authorization = %q", e.gh.token)
	}
}

func TestFiltersByBaseLabelPathAndAuthor(t *testing.T) {
	e := setup(t, `
routes:
  - environments: [billing-prod]
    pull_requests:
      repos: [tendwell-health/monorepo]
      base: main
      labels: [feature, fix]
      paths: [services/billing/]
      exclude_authors: [Renovate]
`)
	in := func(n map[string]any) { n["_labels"] = []string{"docs", "fix"} }
	e.gh.add("tendwell-health/monorepo",
		pr("jo", day, in, files("services/billing/api.go")),
		pr("jo", day, in, files("docs/a.md", "services/billing/db.go")),
		pr("lee", day, in, files("services/visits/api.go")),
		pr("ana", day, files("services/billing/api.go")),
		pr("sam", day, in, files("services/billing/api.go"), set("_base", "release")),
		pr("renovate", day, in, files("services/billing/go.mod")),
		pr("dependabot", day, in, files("services/billing/go.mod"), author("Bot", "")),
	)
	e.cycle()
	if got := handles(decode(t, e.api.calls()[0])); !reflect.DeepEqual(got, []string{"github:jo"}) {
		t.Errorf("players = %v", got)
	}
	q := e.gh.calls()[0]
	if q["base"] != "main" || q["labels"] != nil || q["files"] != true {
		t.Errorf("query = %v", q)
	}
}

func TestOpenedAndLinesScores(t *testing.T) {
	e := setup(t, `
routes:
  - environments: [visits-prod]
    pull_requests:
      repos: [tendwell-health/visits]
      score: opened
      window: 168h
  - environments: [visits-staging]
    pull_requests:
      repos: [tendwell-health/visits]
      score: lines
      include_bots: true
`)
	e.gh.add("tendwell-health/visits",
		pr("jo", time.Hour, func(n map[string]any) { open(n) }),
		pr("jo", 2*time.Hour, func(n map[string]any) { open(n) }),
		pr("lee", day, set("additions", 900)),
		pr("bot", day, author("Bot", ""), set("additions", 400)),
		pr("ana", 8*day, set("updatedAt", start.Add(-time.Hour).Format(time.RFC3339))),
	)
	e.cycle()
	got := map[string][]string{}
	for _, c := range e.api.calls() {
		got[c.Scope["environment"].(string)] = handles(decode(t, c))
	}
	want := map[string][]string{
		"visits-prod":    {"github:jo", "github:lee"},
		"visits-staging": {"github:lee", "github:bot", "github:ana"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("boards = %v, want %v", got, want)
	}
	if n := len(e.gh.calls()); n != 2 {
		t.Errorf("queries = %d, want one opened and one merged", n)
	}
}

func TestSharedQueryRunsOnce(t *testing.T) {
	e := setup(t, `
routes:
  - environments: [billing-prod]
    pull_requests: {repos: [tendwell-health/infra, tendwell-health/billing]}
  - environments: [visits-prod]
    pull_requests: {repos: [tendwell-health/infra]}
`)
	e.gh.add("tendwell-health/infra", pr("jo", day))
	e.cycle()
	if n := len(e.gh.calls()); n != 2 {
		t.Errorf("queries = %d, want 2", n)
	}
	if n := len(e.api.calls()); n != 2 {
		t.Errorf("writes = %d, want 2", n)
	}
}

func TestPagesStopAtTheWindow(t *testing.T) {
	e := setup(t, `
routes:
  - environments: [billing-prod]
    max_players: 25
    pull_requests: {repos: [tendwell-health/billing], window: 24h}
`)
	for i := range 120 {
		e.gh.add("tendwell-health/billing", pr(fmt.Sprintf("dev%03d", i), time.Duration(i)*time.Hour))
	}
	e.cycle()
	if n := len(e.gh.calls()); n != 1 {
		t.Errorf("pages = %d, want 1: the first page already reaches past the window", n)
	}
	if n := len(decode(t, e.api.calls()[0]).Entries); n != 25 {
		t.Errorf("players = %d, want 25", n)
	}
}

func TestPagesUntilTheWindow(t *testing.T) {
	e := setup(t, `
routes:
  - environments: [billing-prod]
    pull_requests: {repos: [tendwell-health/billing], window: 720h}
`)
	for i := range 120 {
		e.gh.add("tendwell-health/billing", pr("jo", time.Duration(i)*time.Hour))
	}
	e.cycle()
	if n := len(e.gh.calls()); n != 3 {
		t.Errorf("pages = %d, want 3", n)
	}
}

func TestMaxPlayersAndTies(t *testing.T) {
	e := setup(t, `
routes:
  - environments: [billing-prod]
    max_players: 2
    pull_requests: {repos: [tendwell-health/billing]}
`)
	e.gh.add("tendwell-health/billing", pr("zed", day), pr("Bea", day), pr("amy", day), pr("zed", day))
	e.cycle()
	if got := handles(decode(t, e.api.calls()[0])); !reflect.DeepEqual(got, []string{"github:zed", "github:amy"}) {
		t.Errorf("players = %v", got)
	}
}

func TestFailedRepoSkipsOnlyItsRoutes(t *testing.T) {
	e := setup(t, `
routes:
  - environments: [billing-prod]
    pull_requests: {repos: [tendwell-health/billing, tendwell-health/infra]}
  - environments: [visits-prod]
    pull_requests: {repos: [tendwell-health/visits]}
`)
	e.gh.fail["tendwell-health/infra"] = http.StatusBadGateway
	e.gh.add("tendwell-health/billing", pr("jo", day))
	e.gh.add("tendwell-health/visits", pr("lee", day))
	e.cycle()
	calls := e.api.calls()
	if len(calls) != 1 || calls[0].Scope["environment"] != "visits-prod" {
		t.Errorf("calls = %+v", calls)
	}
}

func TestNoPlayersWritesNothing(t *testing.T) {
	e := setup(t, `
routes:
  - environments: [billing-prod]
    pull_requests: {repos: [tendwell-health/billing]}
`)
	e.cycle()
	if n := len(e.api.calls()); n != 0 {
		t.Errorf("writes = %d", n)
	}
}

func TestStaticRouteWritesTheListWithoutGitHub(t *testing.T) {
	var cfg Config
	doc := `
routes:
  - environments: [sandbox-dev]
    title: Demo
    static:
      - name: Jo Park
        email: jo@example.com
        handles: [slack:@jo, github:jo]
      - handles: [github:lee]
`
	if err := automation.DecodeConfig([]byte(doc), &cfg); err != nil {
		t.Fatal(err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if cfg.usesGitHub() {
		t.Error("a static route needs no GitHub token")
	}
	api := &fakeAPI{}
	srv := httptest.NewServer(api)
	t.Cleanup(srv.Close)
	client := &massdriver.Client{Endpoint: srv.URL, OrganizationID: "tendwell-health", APIKey: "md_test"}
	clock := start
	l := New(&cfg, nil, client, func() time.Time { return clock })
	l.Cycle(context.Background())
	calls := api.calls()
	if len(calls) != 1 {
		t.Fatalf("calls = %+v", calls)
	}
	if got := calls[0].Input["value"]; got != `{"title":"Demo","entries":[{"name":"Jo Park","email":"jo@example.com",`+
		`"handles":["slack:@jo","github:jo"]},{"handles":["github:lee"]}]}` {
		t.Errorf("value = %s", got)
	}

	clock = clock.Add(15 * time.Minute)
	l.Cycle(context.Background())
	if n := len(api.calls()); n != 1 {
		t.Errorf("unchanged list written again after one interval: %d writes", n)
	}
	clock = clock.Add(15 * time.Minute)
	l.Cycle(context.Background())
	if n := len(api.calls()); n != 2 {
		t.Errorf("writes = %d after two intervals, want 2 so it never expires", n)
	}
}

func TestHandleIgnoresEvents(t *testing.T) {
	e := setup(t, "routes:\n  - environments: [a]\n    static: [{name: Jo}]\n")
	if err := e.l.Handle(context.Background(), automation.Event{Type: "deployment.completed"}); err != nil {
		t.Fatal(err)
	}
	if n := len(e.api.calls()); n != 0 {
		t.Errorf("writes = %d", n)
	}
}

func TestConfigErrors(t *testing.T) {
	pullRequests := "    pull_requests: {repos: [a/b]}\n"
	for name, tc := range map[string]struct{ yaml, want string }{
		"no routes":       {"interval: 15m\n", "add at least one route"},
		"short interval":  {"interval: 30s\nroutes:\n  - environments: [a]\n" + pullRequests, "interval must be at least 1m0s"},
		"no environments": {"routes:\n  -\n" + pullRequests, "must name at least one environment"},
		"bad environment": {"routes:\n  - environments: [Prod]\n" + pullRequests, "must be an environment identifier"},
		"shared env": {"routes:\n  - environments: [a]\n" + pullRequests + "  - environments: [a]\n" + pullRequests,
			"is in more than one route"},
		"both":        {"routes:\n  - environments: [a]\n" + pullRequests + "    static: [{name: Jo}]\n", "exactly one of"},
		"neither":     {"routes:\n  - environments: [a]\n", "exactly one of"},
		"max_players": {"routes:\n  - environments: [a]\n    max_players: 26\n" + pullRequests, "max_players must be"},
		"bad repo":    {"routes:\n  - environments: [a]\n    pull_requests: {repos: [b]}\n", "must look like owner/name"},
		"no repos":    {"routes:\n  - environments: [a]\n    pull_requests: {base: main}\n", "at least one repository"},
		"score":       {"routes:\n  - environments: [a]\n    pull_requests: {repos: [a/b], score: stars}\n", "score must be"},
		"window":      {"routes:\n  - environments: [a]\n    pull_requests: {repos: [a/b], window: 10m}\n", "window must be"},
		"title": {"routes:\n  - environments: [a]\n    title: " + strings.Repeat("x", 65) + "\n" + pullRequests,
			"title must be at most 64"},
		"empty static": {"routes:\n  - environments: [a]\n    static: []\n", "must have 1 to 25 people"},
		"empty person": {"routes:\n  - environments: [a]\n    static: [{}]\n", "set at least one of"},
		"bad email": {"routes:\n  - environments: [a]\n    static: [{email: 'a@b.com?cc=x'}]\n",
			"email must be one plain address"},
		"bad handle": {"routes:\n  - environments: [a]\n    static: [{handles: [jo]}]\n", "must look like service:handle"},
		"bad image":  {"routes:\n  - environments: [a]\n    static: [{image_url: 'ftp://x'}]\n", "image_url must be"},
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
	doc := "interval: null\nlabel: null\ngithub: {graphql_url: null, token_env: null}\n" +
		"routes:\n  - environments: [a]\n    max_players: null\n" +
		"    pull_requests: {repos: [a/b], window: null, score: null}\n"
	if err := automation.DecodeConfig([]byte(doc), &cfg); err != nil {
		t.Fatal(err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	r := cfg.Routes[0]
	if time.Duration(cfg.Interval) != 15*time.Minute || cfg.Label != "Leaderboard" ||
		cfg.GitHub.GraphQLURL != "https://api.github.com/graphql" || cfg.GitHub.TokenEnv != "GITHUB_TOKEN" ||
		r.MaxPlayers != 10 || time.Duration(r.PullRequests.Window) != 30*day || r.PullRequests.Score != ScoreMerged {
		t.Errorf("config = %+v %+v", cfg, r.PullRequests)
	}
}

func TestExampleConfigLoads(t *testing.T) {
	var cfg Config
	if err := automation.LoadConfigFile("config.example.yaml", &cfg); err != nil {
		t.Fatal(err)
	}
	if len(cfg.Routes) != 3 || cfg.Routes[0].PullRequests == nil || cfg.Routes[2].Static == nil {
		t.Errorf("config = %+v", cfg)
	}
}
