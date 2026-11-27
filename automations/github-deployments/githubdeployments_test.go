package githubdeployments

import (
	"encoding/base64"
	"encoding/json"
	"errors"
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
)

var secret = []byte("0123456789abcdef0123456789abcdef")

func deploymentEvent(id, typ, action string, attrs map[string]any) []byte {
	attributes := map[string]any{
		"md-project":     "api",
		"md-environment": "prod",
		"md-component":   "database",
		"md-instance":    "api-prod-database",
		"md-bundle":      "aws-aurora@1.2.3",
		"github_repo":    "acme/api",
	}
	for k, v := range attrs {
		if v == nil {
			delete(attributes, k)
		} else {
			attributes[k] = v
		}
	}
	b, _ := json.Marshal(map[string]any{
		"id":      id,
		"type":    typ,
		"subject": "mri:acme:api:prod:deployment:5c1de0a7-92b4-4f1e-8d3a-6e27c9f0b815",
		"data": map[string]any{
			"entity": map[string]any{
				"id":               "5c1de0a7-92b4-4f1e-8d3a-6e27c9f0b815",
				"action":           action,
				"version":          "1.2.3",
				"message":          nil,
				"deployed_by_name": "Jane Doe",
			},
			"attributes": attributes,
			"metadata":   map[string]any{"deployment_action": action},
		},
	})
	return b
}

type call struct {
	Method string
	Path   string
	Body   map[string]any
}

type fakeGitHub struct {
	mu       sync.Mutex
	calls    []call
	existing []map[string]any
	statuses map[string][]string
	nextID   int64
	fail     bool
}

func newFakeGitHub() *fakeGitHub {
	return &fakeGitHub{statuses: map[string][]string{}, nextID: 9001}
}

func (f *fakeGitHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	var body map[string]any
	b, _ := io.ReadAll(r.Body)
	_ = json.Unmarshal(b, &body)
	f.calls = append(f.calls, call{Method: r.Method, Path: r.URL.RequestURI(), Body: body})
	if f.fail {
		http.Error(w, `{"message":"Server Error"}`, http.StatusBadGateway)
		return
	}

	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/api":
		_ = json.NewEncoder(w).Encode(map[string]any{"default_branch": "trunk"})
	case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/api/deployments":
		_ = json.NewEncoder(w).Encode(f.existing)
	case r.Method == http.MethodPost && r.URL.Path == "/repos/acme/api/deployments":
		id := f.nextID
		f.nextID++
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": id, "ref": body["ref"], "sha": "a41c9e2"})
	case strings.HasSuffix(r.URL.Path, "/statuses"):
		id := strings.Split(r.URL.Path, "/")[5]
		if r.Method == http.MethodPost {
			f.statuses[id] = append([]string{body["state"].(string)}, f.statuses[id]...)
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{}`))
			return
		}
		out := []map[string]any{}
		for _, s := range f.statuses[id] {
			out = append(out, map[string]any{"state": s})
		}
		_ = json.NewEncoder(w).Encode(out)
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeGitHub) writes() []call {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []call
	for _, c := range f.calls {
		if c.Method == http.MethodPost {
			out = append(out, c)
		}
	}
	return out
}

func testConfig(githubURL string) Config {
	cfg := DefaultConfig()
	cfg.GitHub.APIURL = githubURL
	cfg.GitHub.token = "ghp_test"
	return cfg
}

func post(t *testing.T, h http.Handler, id string, body []byte) int {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, automation.HookPath, strings.NewReader(string(body)))
	automation.SignRequest(req, secret, id, body, time.Now())
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w.Code
}

const logURL = "https://app.massdriver.cloud/orgs/acme/projects/api/environments/prod/instances/database?deployment=5c1de0a7-92b4-4f1e-8d3a-6e27c9f0b815&tab=history"

func TestStartedThenCompletedCreatesOneDeploymentWithTwoStatuses(t *testing.T) {
	gh := newFakeGitHub()
	srv := httptest.NewServer(gh)
	defer srv.Close()
	h := automation.Handler(New(testConfig(srv.URL), nil), secret)

	if code := post(t, h, "ev-1", deploymentEvent("ev-1", "deployment.started", "PROVISION", nil)); code != http.StatusNoContent {
		t.Fatalf("started: got %d", code)
	}
	if code := post(t, h, "ev-2", deploymentEvent("ev-2", "deployment.completed", "PROVISION", nil)); code != http.StatusNoContent {
		t.Fatalf("completed: got %d", code)
	}

	want := []call{
		{Method: "POST", Path: "/repos/acme/api/deployments", Body: map[string]any{
			"ref":         "trunk",
			"task":        "massdriver",
			"environment": "prod",
			"description": "Deploy of api-prod-database (aws-aurora@1.2.3) by Jane Doe",
			"payload": map[string]any{
				"massdriver_deployment_id": "5c1de0a7-92b4-4f1e-8d3a-6e27c9f0b815",
				"instance":                 "api-prod-database",
				"bundle":                   "aws-aurora@1.2.3",
				"action":                   "PROVISION",
			},
			"auto_merge":        false,
			"required_contexts": []any{},
		}},
		{Method: "POST", Path: "/repos/acme/api/deployments/9001/statuses", Body: map[string]any{
			"state":       "in_progress",
			"log_url":     logURL,
			"description": "Deploy of api-prod-database started",
			"environment": "prod",
		}},
		{Method: "POST", Path: "/repos/acme/api/deployments/9001/statuses", Body: map[string]any{
			"state":       "success",
			"log_url":     logURL,
			"description": "Deploy of api-prod-database completed",
			"environment": "prod",
		}},
	}
	if got := gh.writes(); !reflect.DeepEqual(got, want) {
		t.Fatalf("writes:\n got %+v\nwant %+v", got, want)
	}
}

func TestRestartFindsExistingDeploymentByPayload(t *testing.T) {
	gh := newFakeGitHub()
	gh.existing = []map[string]any{
		{"id": 7001, "payload": map[string]any{"massdriver_deployment_id": "someone-else"}},
		{"id": 7002, "payload": map[string]any{"massdriver_deployment_id": "5c1de0a7-92b4-4f1e-8d3a-6e27c9f0b815"}},
	}
	srv := httptest.NewServer(gh)
	defer srv.Close()
	h := automation.Handler(New(testConfig(srv.URL), nil), secret)

	post(t, h, "ev-3", deploymentEvent("ev-3", "deployment.failed", "PROVISION", nil))

	want := []call{
		{Method: "POST", Path: "/repos/acme/api/deployments/7002/statuses", Body: map[string]any{
			"state":       "failure",
			"log_url":     logURL,
			"description": "Deploy of api-prod-database failed",
			"environment": "prod",
		}},
	}
	if got := gh.writes(); !reflect.DeepEqual(got, want) {
		t.Fatalf("writes:\n got %+v\nwant %+v", got, want)
	}
	gh.mu.Lock()
	defer gh.mu.Unlock()
	if got := gh.calls[0].Path; got != "/repos/acme/api/deployments?environment=prod&per_page=100&task=massdriver" {
		t.Fatalf("lookup path: %s", got)
	}
}

func TestStartedAfterCompletedDoesNotReopen(t *testing.T) {
	gh := newFakeGitHub()
	srv := httptest.NewServer(gh)
	defer srv.Close()
	h := automation.Handler(New(testConfig(srv.URL), nil), secret)

	post(t, h, "ev-4", deploymentEvent("ev-4", "deployment.completed", "PROVISION", nil))
	post(t, h, "ev-5", deploymentEvent("ev-5", "deployment.started", "PROVISION", nil))

	gh.mu.Lock()
	defer gh.mu.Unlock()
	if got := gh.statuses["9001"]; !reflect.DeepEqual(got, []string{"success"}) {
		t.Fatalf("statuses: %v", got)
	}
}

func TestRefAttributeProjectEnvironmentNameAndDecommission(t *testing.T) {
	gh := newFakeGitHub()
	srv := httptest.NewServer(gh)
	defer srv.Close()
	cfg := testConfig(srv.URL)
	cfg.GitHub.EnvironmentName = "project-environment"
	h := automation.Handler(New(cfg, nil), secret)

	post(t, h, "ev-6", deploymentEvent("ev-6", "deployment.completed", "DECOMMISSION", map[string]any{"github_ref": "release"}))

	writes := gh.writes()
	if len(writes) != 2 {
		t.Fatalf("writes: %+v", writes)
	}
	if writes[0].Body["ref"] != "release" || writes[0].Body["environment"] != "api-prod" {
		t.Fatalf("deployment: %+v", writes[0].Body)
	}
	if writes[1].Body["state"] != "inactive" || writes[1].Body["description"] != "Decommission of api-prod-database completed" {
		t.Fatalf("status: %+v", writes[1].Body)
	}
	gh.mu.Lock()
	defer gh.mu.Unlock()
	for _, c := range gh.calls {
		if c.Path == "/repos/acme/api" {
			t.Fatal("fetched the default branch although github_ref is set")
		}
	}
}

func TestIgnoredEvents(t *testing.T) {
	gh := newFakeGitHub()
	srv := httptest.NewServer(gh)
	defer srv.Close()
	h := automation.Handler(New(testConfig(srv.URL), nil), secret)

	cases := map[string][]byte{
		"ev-7":  deploymentEvent("ev-7", "deployment.completed", "PLAN", nil),
		"ev-8":  deploymentEvent("ev-8", "deployment.completed", "PROVISION", map[string]any{"github_repo": nil}),
		"ev-9":  deploymentEvent("ev-9", "deployment.completed", "PROVISION", map[string]any{"github_repo": "acme"}),
		"ev-10": deploymentEvent("ev-10", "deployment.created", "PROVISION", nil),
	}
	for id, body := range cases {
		if code := post(t, h, id, body); code != http.StatusNoContent {
			t.Errorf("%s: got %d", id, code)
		}
	}
	if n := len(gh.calls); n != 0 {
		t.Fatalf("GitHub was called %d times", n)
	}
}

func TestGitHubErrorAsksForRetry(t *testing.T) {
	gh := newFakeGitHub()
	gh.fail = true
	srv := httptest.NewServer(gh)
	defer srv.Close()
	h := automation.Handler(New(testConfig(srv.URL), nil), secret)

	body := deploymentEvent("ev-11", "deployment.aborted", "PROVISION", nil)
	if code := post(t, h, "ev-11", body); code != http.StatusInternalServerError {
		t.Fatalf("got %d", code)
	}

	gh.mu.Lock()
	gh.fail = false
	gh.mu.Unlock()
	if code := post(t, h, "ev-11", body); code != http.StatusNoContent {
		t.Fatalf("retry: got %d", code)
	}
	gh.mu.Lock()
	defer gh.mu.Unlock()
	if got := gh.statuses["9001"]; !reflect.DeepEqual(got, []string{"error"}) {
		t.Fatalf("statuses: %v", got)
	}
}

func TestBadSignature(t *testing.T) {
	h := automation.Handler(New(testConfig("http://unused"), nil), secret)
	body := deploymentEvent("ev-12", "deployment.completed", "PROVISION", nil)
	req := httptest.NewRequest(http.MethodPost, automation.HookPath, strings.NewReader(string(body)))
	automation.SignRequest(req, secret, "ev-12", body, time.Now())
	req.Header.Set("webhook-signature", "v1,"+base64.StdEncoding.EncodeToString([]byte("forged")))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("got %d", w.Code)
	}
}

func TestTruncate(t *testing.T) {
	if got := truncate(strings.Repeat("a", 150), 140); len([]rune(got)) != 140 || !strings.HasSuffix(got, "…") {
		t.Fatalf("got %q", got)
	}
	if got := truncate("short", 140); got != "short" {
		t.Fatalf("got %q", got)
	}
}

func TestStartReadsTheTokenFromTheNamedEnvVar(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("github:\n  token_env: GHE_TOKEN\n  api_url: https://ghe.example.com/api/v3/\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CONFIG_FILE", path)
	t.Setenv("GHE_TOKEN", "")
	if _, err := Definition.New(); err == nil || err.Error() != "missing required env vars: [GHE_TOKEN]" {
		t.Fatalf("err = %v", err)
	}
	t.Setenv("GHE_TOKEN", "ghp_enterprise")
	a, err := Definition.New()
	if err != nil {
		t.Fatal(err)
	}
	g := a.(*GitHubDeployments)
	if g.gh.token != "ghp_enterprise" || g.gh.url != "https://ghe.example.com/api/v3" || g.cfg.GitHub.RepoAttribute != "github_repo" {
		t.Fatalf("config = %+v", g.cfg)
	}
}

func TestStartRefusesBadConfig(t *testing.T) {
	for body, want := range map[string]string{
		"github: {environment_name: team}\n": "github.environment_name must be environment or project-environment",
		"github: {repo_attribute: \"\"}\n":   "github.repo_attribute must not be empty",
		"github: {token: ghp_x}\n":           "field token not found",
	} {
		path := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("CONFIG_FILE", path)
		_, err := Definition.New()
		var ce *automation.ConfigError
		if !errors.As(err, &ce) || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: err = %v, want %q", body, err, want)
		}
	}
}

func TestExampleConfigLoads(t *testing.T) {
	cfg := DefaultConfig()
	if err := automation.LoadConfigFile("config.example.yaml", &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.GitHub.RepoAttribute != "github_repo" || cfg.Massdriver.URL() != "https://app.massdriver.cloud" {
		t.Fatalf("config = %+v", cfg)
	}
}

// fakeStash records stash writes, and rejects them the way the server rejects a key its widget catalog lacks.
type fakeStash struct {
	mu     sync.Mutex
	writes []map[string]any
	reject bool
}

func (f *fakeStash) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Variables map[string]any `json:"variables"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.writes = append(f.writes, req.Variables)
	if f.reject {
		_, _ = w.Write([]byte(`{"data":{"alphaSetStashEntry":{"successful":false,"messages":[{"field":"key","message":"is reserved"}]}}}`))
		return
	}
	_, _ = w.Write([]byte(`{"data":{"alphaSetStashEntry":{"successful":true,"messages":[],"result":{"key":"x"}}}}`))
}

func TestSourceGoesToTheDeploymentStash(t *testing.T) {
	for _, reject := range []bool{false, true} {
		gh := newFakeGitHub()
		srv := httptest.NewServer(gh)
		defer srv.Close()
		stash := &fakeStash{reject: reject}
		md := httptest.NewServer(stash)
		defer md.Close()
		g := New(testConfig(srv.URL), &massdriver.Client{Endpoint: md.URL, OrganizationID: "acme", APIKey: "md_test"})
		// The fake serves /repos at its root. An Enterprise API URL gives the commit a link.
		g.cfg.GitHub.APIURL = srv.URL + "/api/v3"
		h := automation.Handler(g, secret)

		if code := post(t, h, "ev-1", deploymentEvent("ev-1", "deployment.started", "PROVISION", nil)); code != http.StatusNoContent {
			t.Fatalf("reject=%v: started: got %d", reject, code)
		}
		if code := post(t, h, "ev-2", deploymentEvent("ev-2", "deployment.completed", "PROVISION", nil)); code != http.StatusNoContent {
			t.Fatalf("reject=%v: completed: got %d", reject, code)
		}

		want := []map[string]any{{
			"org":   "acme",
			"scope": map[string]any{"deployment": "5c1de0a7-92b4-4f1e-8d3a-6e27c9f0b815"},
			"key":   "md/v1/source",
			"input": map[string]any{
				"format": "JSON",
				"label":  "Source",
				"value":  `{"repo":"acme/api","ref":"trunk","commit":"a41c9e2","url":"` + srv.URL + `/acme/api/commit/a41c9e2"}`,
			},
		}}
		stash.mu.Lock()
		if !reflect.DeepEqual(stash.writes, want) {
			t.Errorf("reject=%v: stash writes:\n got %+v\nwant %+v", reject, stash.writes, want)
		}
		stash.mu.Unlock()
		if got := len(gh.writes()); got != 3 {
			t.Errorf("reject=%v: github writes = %d, want 3", reject, got)
		}
	}
}

func TestCommitURL(t *testing.T) {
	for api, want := range map[string]string{
		"https://api.github.com":           "https://github.com/acme/api/commit/a41c9e2",
		"https://ghe.example.com/api/v3":   "https://ghe.example.com/acme/api/commit/a41c9e2",
		"https://proxy.example.com/github": "",
	} {
		g := New(Config{GitHub: GitHubConfig{APIURL: api}}, nil)
		if got := g.commitURL("acme/api", "a41c9e2"); got != want {
			t.Errorf("%s: got %q, want %q", api, got, want)
		}
	}
}
