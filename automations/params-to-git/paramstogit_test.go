package paramstogit

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/massdriver-cloud/massdriver-automations/pkg/automation"
	"github.com/massdriver-cloud/massdriver-automations/pkg/massdriver"
)

var secret = []byte("0123456789abcdef0123456789abcdef")

// ---------------------------------------------------------------- fake GitHub

type commit struct {
	branch, message string
	author          *person
}

type branch struct {
	sha   string
	files map[string][]byte
}

type fakePull struct {
	number           int
	head, base       string
	title, body      string
	open             bool
	users, teamSlugs []string
}

type fakeGitHub struct {
	mu      sync.Mutex
	t       *testing.T
	repos   map[string]map[string]*branch
	commits []commit
	pulls   []*fakePull
	seq     int
}

func newFakeGitHub(t *testing.T) *fakeGitHub {
	return &fakeGitHub{t: t, repos: map[string]map[string]*branch{}}
}

func (g *fakeGitHub) addRepo(repo string, files map[string]string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	b := &branch{files: map[string][]byte{}}
	for k, v := range files {
		b.files[k] = []byte(v)
	}
	g.seq++
	b.sha = fmt.Sprintf("c%03d", g.seq)
	g.repos[repo] = map[string]*branch{"main": b}
}

func (g *fakeGitHub) file(repo, br, path string) (string, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	b, ok := g.repos[repo][br]
	if !ok {
		return "", false
	}
	c, ok := b.files[path]
	return string(c), ok
}

func (g *fakeGitHub) commitsOn(br string) []commit {
	g.mu.Lock()
	defer g.mu.Unlock()
	var out []commit
	for _, c := range g.commits {
		if c.branch == br {
			out = append(out, c)
		}
	}
	return out
}

func (g *fakeGitHub) allCommits() []commit {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]commit(nil), g.commits...)
}

func (g *fakeGitHub) allPulls() []fakePull {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]fakePull, len(g.pulls))
	for i, p := range g.pulls {
		out[i] = *p
	}
	return out
}

func blobSHA(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:20])
}

func (g *fakeGitHub) handler() http.Handler {
	mux := http.NewServeMux()
	writeJSON := func(w http.ResponseWriter, status int, v any) {
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(v)
	}
	fail := func(w http.ResponseWriter, status int, msg string) {
		writeJSON(w, status, map[string]any{"message": msg})
	}
	repo := func(r *http.Request) (string, map[string]*branch, bool) {
		name := r.PathValue("owner") + "/" + r.PathValue("repo")
		br, ok := g.repos[name]
		return name, br, ok
	}

	mux.HandleFunc("GET /repos/{owner}/{repo}/contents/{path...}", func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		defer g.mu.Unlock()
		_, brs, ok := repo(r)
		b := brs[r.URL.Query().Get("ref")]
		if !ok || b == nil {
			fail(w, 404, "Not Found")
			return
		}
		c, ok := b.files[r.PathValue("path")]
		if !ok {
			fail(w, 404, "Not Found")
			return
		}
		enc := base64.StdEncoding.EncodeToString(c)
		// GitHub wraps the base64 at 60 characters.
		var wrapped strings.Builder
		for len(enc) > 60 {
			wrapped.WriteString(enc[:60] + "\n")
			enc = enc[60:]
		}
		wrapped.WriteString(enc)
		writeJSON(w, 200, map[string]any{"sha": blobSHA(c), "content": wrapped.String(), "encoding": "base64"})
	})

	write := func(w http.ResponseWriter, r *http.Request, del bool) {
		g.mu.Lock()
		defer g.mu.Unlock()
		_, brs, ok := repo(r)
		var in struct {
			Message string  `json:"message"`
			Content string  `json:"content"`
			SHA     string  `json:"sha"`
			Branch  string  `json:"branch"`
			Author  *person `json:"author"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		b := brs[in.Branch]
		if !ok || b == nil {
			fail(w, 404, "Branch not found")
			return
		}
		path := r.PathValue("path")
		old, exists := b.files[path]
		if exists && in.SHA != blobSHA(old) || !exists && in.SHA != "" {
			fail(w, 409, "sha does not match")
			return
		}
		if del {
			if !exists {
				fail(w, 404, "Not Found")
				return
			}
			delete(b.files, path)
		} else {
			c, err := base64.StdEncoding.DecodeString(in.Content)
			if err != nil {
				fail(w, 422, "bad content")
				return
			}
			b.files[path] = c
		}
		g.seq++
		b.sha = fmt.Sprintf("c%03d", g.seq)
		g.commits = append(g.commits, commit{branch: in.Branch, message: in.Message, author: in.Author})
		writeJSON(w, 200, map[string]any{"commit": map[string]any{"sha": b.sha}})
	}
	mux.HandleFunc("PUT /repos/{owner}/{repo}/contents/{path...}", func(w http.ResponseWriter, r *http.Request) { write(w, r, false) })
	mux.HandleFunc("DELETE /repos/{owner}/{repo}/contents/{path...}", func(w http.ResponseWriter, r *http.Request) { write(w, r, true) })

	mux.HandleFunc("GET /repos/{owner}/{repo}/git/ref/heads/{branch...}", func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		defer g.mu.Unlock()
		_, brs, _ := repo(r)
		b := brs[r.PathValue("branch")]
		if b == nil {
			fail(w, 404, "Not Found")
			return
		}
		writeJSON(w, 200, map[string]any{"object": map[string]any{"sha": b.sha}})
	})
	mux.HandleFunc("POST /repos/{owner}/{repo}/git/refs", func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		defer g.mu.Unlock()
		_, brs, _ := repo(r)
		var in struct{ Ref, SHA string }
		_ = json.NewDecoder(r.Body).Decode(&in)
		name := strings.TrimPrefix(in.Ref, "refs/heads/")
		if brs[name] != nil {
			fail(w, 422, "Reference already exists")
			return
		}
		src := g.findSHA(brs, in.SHA)
		if src == nil {
			fail(w, 422, "Object does not exist")
			return
		}
		brs[name] = src
		writeJSON(w, 201, map[string]any{"ref": in.Ref})
	})
	mux.HandleFunc("PATCH /repos/{owner}/{repo}/git/refs/heads/{branch...}", func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		defer g.mu.Unlock()
		_, brs, _ := repo(r)
		var in struct {
			SHA   string
			Force bool
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		name := r.PathValue("branch")
		src := g.findSHA(brs, in.SHA)
		if brs[name] == nil || src == nil || !in.Force {
			fail(w, 422, "Update is not a fast forward")
			return
		}
		brs[name] = src
		writeJSON(w, 200, map[string]any{})
	})

	mux.HandleFunc("GET /repos/{owner}/{repo}/pulls", func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		defer g.mu.Unlock()
		name, _, _ := repo(r)
		owner, _, _ := strings.Cut(name, "/")
		q := r.URL.Query()
		out := []map[string]any{}
		for _, p := range g.pulls {
			if p.open && q.Get("state") == "open" && owner+":"+p.head == q.Get("head") && p.base == q.Get("base") {
				out = append(out, map[string]any{"number": p.number})
			}
		}
		writeJSON(w, 200, out)
	})
	mux.HandleFunc("POST /repos/{owner}/{repo}/pulls", func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		defer g.mu.Unlock()
		var in struct{ Title, Head, Base, Body string }
		_ = json.NewDecoder(r.Body).Decode(&in)
		p := &fakePull{number: len(g.pulls) + 1, head: in.Head, base: in.Base, title: in.Title, body: in.Body, open: true}
		g.pulls = append(g.pulls, p)
		writeJSON(w, 201, map[string]any{"number": p.number})
	})
	mux.HandleFunc("POST /repos/{owner}/{repo}/pulls/{n}/requested_reviewers", func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		defer g.mu.Unlock()
		n, _ := strconv.Atoi(r.PathValue("n"))
		var in struct {
			Reviewers     []string `json:"reviewers"`
			TeamReviewers []string `json:"team_reviewers"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		p := g.pulls[n-1]
		p.users, p.teamSlugs = in.Reviewers, in.TeamReviewers
		writeJSON(w, 201, map[string]any{})
	})

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("authorization") != "Bearer ghp_test" {
			g.t.Errorf("%s %s: authorization header missing", r.Method, r.URL.Path)
		}
		mux.ServeHTTP(w, r)
	})
}

// findSHA returns a copy of the branch whose head is sha. A new branch shares no file map with its source.
func (g *fakeGitHub) findSHA(brs map[string]*branch, sha string) *branch {
	for _, b := range brs {
		if b.sha == sha {
			files := map[string][]byte{}
			for k, v := range b.files {
				files[k] = v
			}
			return &branch{sha: b.sha, files: files}
		}
	}
	return nil
}

// ---------------------------------------------------------------- fake Massdriver

type fakeAPI struct {
	mu        sync.Mutex
	instances map[string]map[string]any
	calls     int
	fail      bool
}

func (a *fakeAPI) set(id, status, deploymentID string, params map[string]any, schema map[string]any) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.instances == nil {
		a.instances = map[string]map[string]any{}
	}
	var lp any
	if deploymentID != "" {
		lp = map[string]any{"id": deploymentID, "params": params, "bundle": map[string]any{"paramsSchema": schema}}
	}
	a.instances[id] = map[string]any{"status": status, "latestProvision": lp}
}

func (a *fakeAPI) count() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.calls
}

func (a *fakeAPI) setFail(v bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.fail = v
}

func (a *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls++
	if a.fail {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	var req struct {
		Variables map[string]any `json:"variables"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	inst := a.instances[fmt.Sprint(req.Variables["id"])]
	_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"instance": inst}})
}

// ---------------------------------------------------------------- harness

const instanceID = "api-prod-database"

var schema = map[string]any{
	"properties": map[string]any{
		"size":     map[string]any{"type": "string"},
		"password": map[string]any{"type": "string", "format": "password"},
		"database": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"name": map[string]any{"type": "string"},
			},
			"dependencies": map[string]any{
				"name": map[string]any{"oneOf": []any{map[string]any{
					"properties": map[string]any{"token": map[string]any{"type": "string", "$md.sensitive": true}},
				}}},
			},
		},
	},
}

func params(size string) map[string]any {
	return map[string]any{
		"size":        size,
		"password":    "hunter2",
		"database":    map[string]any{"name": "app", "token": "tok-123"},
		"md_metadata": map[string]any{"name_prefix": instanceID, "deployment": map[string]any{"id": "x"}},
		"big":         json.Number("12345678901234567890"),
	}
}

func event(id, action, deploymentID string, attrs map[string]any) []byte {
	a := map[string]any{"md-project": "api", "md-environment": "prod", "md-component": "database", "md-instance": instanceID}
	for k, v := range attrs {
		a[k] = v
	}
	b, _ := json.Marshal(map[string]any{
		"id":   id,
		"type": "deployment.completed",
		"data": map[string]any{
			"entity":     map[string]any{"id": deploymentID, "action": action, "deployed_by_name": "Jane Doe"},
			"actor":      map[string]any{"name": "Jane Doe", "account_id": "acc-1"},
			"attributes": a,
			"metadata":   map[string]any{"deployment_action": action},
		},
	})
	return b
}

type harness struct {
	t   *testing.T
	gh  *fakeGitHub
	api *fakeAPI
	h   http.Handler
	p   *ParamsToGit
}

func newHarness(t *testing.T, yaml string, maskKey string) *harness {
	t.Helper()
	gh := newFakeGitHub(t)
	gh.addRepo("acme/platform", nil)
	gh.addRepo("acme/ecomm", nil)
	ghSrv := httptest.NewServer(gh.handler())
	t.Cleanup(ghSrv.Close)
	api := &fakeAPI{}
	apiSrv := httptest.NewServer(api)
	t.Cleanup(apiSrv.Close)

	cfg := DefaultConfig()
	if err := automation.DecodeConfig([]byte(yaml), &cfg); err != nil {
		t.Fatal(err)
	}
	cfg.GitHub.APIURL = ghSrv.URL
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	cfg.GitHub.token = "ghp_test"
	p := New(&massdriver.Client{Endpoint: apiSrv.URL, OrganizationID: "acme", APIKey: "md_test"}, cfg, []byte(maskKey))
	return &harness{t: t, gh: gh, api: api, p: p, h: automation.Handler(p, secret)}
}

func (h *harness) send(body []byte) int {
	h.t.Helper()
	var ev struct{ ID string }
	_ = json.Unmarshal(body, &ev)
	req := httptest.NewRequest(http.MethodPost, automation.HookPath, strings.NewReader(string(body)))
	automation.SignRequest(req, secret, ev.ID, body, time.Now())
	rec := httptest.NewRecorder()
	h.h.ServeHTTP(rec, req)
	return rec.Code
}

func (h *harness) deploy(eventID, deploymentID, size string, attrs map[string]any) int {
	h.api.set(instanceID, "PROVISIONED", deploymentID, params(size), schema)
	return h.send(event(eventID, "PROVISION", deploymentID, attrs))
}

const pushConfig = `
defaults:
  repo: acme/platform
  root: /
`

const filePath = "api/prod/database/params.json"

// ---------------------------------------------------------------- tests

func TestPushWritesStableMaskedParams(t *testing.T) {
	h := newHarness(t, pushConfig, "")
	if code := h.deploy("ev-1", "dep-1", "large", nil); code != http.StatusNoContent {
		t.Fatalf("status %d", code)
	}
	got, ok := h.gh.file("acme/platform", "main", filePath)
	if !ok {
		t.Fatal("params.json not written")
	}
	want := `{
  "big": 12345678901234567890,
  "database": {
    "name": "app",
    "token": "[SENSITIVE]"
  },
  "password": "[SENSITIVE]",
  "size": "large"
}
`
	if got != want {
		t.Fatalf("params.json =\n%s\nwant\n%s", got, want)
	}
	cs := h.gh.commitsOn("main")
	if len(cs) != 1 {
		t.Fatalf("commits = %d, want 1", len(cs))
	}
	if cs[0].message != "api-prod-database: provision deployment dep-1" {
		t.Fatalf("message = %q", cs[0].message)
	}
	if cs[0].author == nil || cs[0].author.Name != "Jane Doe" || cs[0].author.Email != "noreply@massdriver.cloud" {
		t.Fatalf("author = %+v", cs[0].author)
	}
}

func TestUnchangedParamsMakeNoCommit(t *testing.T) {
	h := newHarness(t, pushConfig, "")
	h.deploy("ev-1", "dep-1", "large", nil)
	h.deploy("ev-2", "dep-2", "large", nil)
	if n := len(h.gh.commitsOn("main")); n != 1 {
		t.Fatalf("commits = %d, want 1", n)
	}
	h.deploy("ev-3", "dep-3", "small", nil)
	if n := len(h.gh.commitsOn("main")); n != 2 {
		t.Fatalf("commits = %d, want 2", n)
	}
}

func TestEventHandledTwiceCommitsOnce(t *testing.T) {
	h := newHarness(t, pushConfig, "")
	h.deploy("ev-1", "dep-1", "large", nil)
	// A second automation process, or one restarted, has no memory of the event id.
	h.h = automation.Handler(h.p, secret)
	h.deploy("ev-1", "dep-1", "large", nil)
	if n := len(h.gh.commitsOn("main")); n != 1 {
		t.Fatalf("commits = %d, want 1", n)
	}
}

func TestOlderDeploymentDoesNotOverwrite(t *testing.T) {
	h := newHarness(t, pushConfig, "")
	h.deploy("ev-2", "dep-2", "large", nil)
	// dep-1 completed before dep-2, but its event arrives later.
	if code := h.send(event("ev-1", "PROVISION", "dep-1", nil)); code != http.StatusNoContent {
		t.Fatalf("status %d", code)
	}
	if n := len(h.gh.commitsOn("main")); n != 1 {
		t.Fatalf("commits = %d, want 1", n)
	}
}

func TestPlanAndFailedDeploymentsDoNotSync(t *testing.T) {
	h := newHarness(t, pushConfig, "")
	h.api.set(instanceID, "PROVISIONED", "dep-1", params("large"), schema)
	h.send(event("ev-1", "PLAN", "dep-1", nil))
	b := event("ev-2", "PROVISION", "dep-1", nil)
	h.send([]byte(strings.Replace(string(b), "deployment.completed", "deployment.failed", 1)))
	if h.api.count() != 0 || len(h.gh.allCommits()) != 0 {
		t.Fatalf("api calls %d, commits %d, want none", h.api.count(), len(h.gh.allCommits()))
	}
}

func TestMaskKeyFingerprintsSecrets(t *testing.T) {
	h := newHarness(t, pushConfig, "k3y")
	h.deploy("ev-1", "dep-1", "large", nil)
	first, _ := h.gh.file("acme/platform", "main", filePath)
	if strings.Contains(first, "hunter2") || strings.Contains(first, "tok-123") {
		t.Fatalf("plaintext secret in params.json:\n%s", first)
	}
	if !strings.Contains(first, `"password": "[SENSITIVE:`) {
		t.Fatalf("no fingerprint:\n%s", first)
	}

	p := params("large")
	p["password"] = "correct horse"
	h.api.set(instanceID, "PROVISIONED", "dep-2", p, schema)
	h.send(event("ev-2", "PROVISION", "dep-2", nil))
	second, _ := h.gh.file("acme/platform", "main", filePath)
	if first == second {
		t.Fatal("a changed secret did not change params.json")
	}
	if n := len(h.gh.commitsOn("main")); n != 2 {
		t.Fatalf("commits = %d, want 2", n)
	}
}

func TestOmitDropsSensitiveKeys(t *testing.T) {
	h := newHarness(t, pushConfig+"  sensitive: omit\n", "")
	h.deploy("ev-1", "dep-1", "large", nil)
	got, _ := h.gh.file("acme/platform", "main", filePath)
	var m map[string]any
	if err := json.Unmarshal([]byte(got), &m); err != nil {
		t.Fatal(err)
	}
	if _, ok := m["password"]; ok {
		t.Fatalf("password kept:\n%s", got)
	}
	if _, ok := m["database"].(map[string]any)["token"]; ok {
		t.Fatalf("token kept:\n%s", got)
	}
	if _, ok := m["md_metadata"]; ok {
		t.Fatalf("md_metadata kept:\n%s", got)
	}
}

func TestDecommissionDeletesOrKeeps(t *testing.T) {
	for _, tc := range []struct {
		mode      string
		wantFile  bool
		wantCount int
	}{{"delete", false, 2}, {"keep", true, 1}} {
		t.Run(tc.mode, func(t *testing.T) {
			h := newHarness(t, pushConfig+"  on_decommission: "+tc.mode+"\n", "")
			h.deploy("ev-1", "dep-1", "large", nil)
			h.api.set(instanceID, "DECOMMISSIONED", "dep-1", params("large"), schema)
			if code := h.send(event("ev-2", "DECOMMISSION", "dep-2", nil)); code != http.StatusNoContent {
				t.Fatalf("status %d", code)
			}
			if _, ok := h.gh.file("acme/platform", "main", filePath); ok != tc.wantFile {
				t.Fatalf("file exists = %v, want %v", ok, tc.wantFile)
			}
			if n := len(h.gh.commitsOn("main")); n != tc.wantCount {
				t.Fatalf("commits = %d, want %d", n, tc.wantCount)
			}
		})
	}
}

func TestDecommissionAfterRedeployKeepsFile(t *testing.T) {
	h := newHarness(t, pushConfig, "")
	h.deploy("ev-1", "dep-1", "large", nil)
	// The instance is provisioned again before the decommission's event arrives.
	h.send(event("ev-2", "DECOMMISSION", "dep-2", nil))
	if _, ok := h.gh.file("acme/platform", "main", filePath); !ok {
		t.Fatal("file deleted")
	}
}

const mappingConfig = `
defaults:
  repo: acme/platform
mappings:
  - name: previews
    match:
      md-environment: [preview, sandbox]
    skip: true
  - name: ecomm
    match:
      team: ecomm
    repo: acme/ecomm
    root: deploy/massdriver/
  - name: ecomm-second
    match:
      team: ecomm
    repo: acme/platform
    root: never
`

func TestFirstMatchingMappingWins(t *testing.T) {
	h := newHarness(t, mappingConfig, "")
	h.deploy("ev-1", "dep-1", "large", map[string]any{"team": "ecomm"})
	if _, ok := h.gh.file("acme/ecomm", "main", "deploy/massdriver/"+filePath); !ok {
		t.Fatal("not written to the first matching mapping")
	}
	if len(h.gh.commitsOn("main")) != 1 {
		t.Fatalf("commits = %v", h.gh.allCommits())
	}
}

func TestNoMatchUsesDefaults(t *testing.T) {
	h := newHarness(t, mappingConfig, "")
	h.deploy("ev-1", "dep-1", "large", map[string]any{"team": "data"})
	if _, ok := h.gh.file("acme/platform", "main", filePath); !ok {
		t.Fatal("not written to defaults")
	}
}

func TestSkipMappingSendsNothing(t *testing.T) {
	h := newHarness(t, mappingConfig, "")
	h.deploy("ev-1", "dep-1", "large", map[string]any{"md-environment": "sandbox"})
	if len(h.gh.allCommits()) != 0 || h.api.count() != 0 {
		t.Fatalf("commits %d, api calls %d, want none", len(h.gh.allCommits()), h.api.count())
	}
}

const prConfig = `
defaults:
  repo: acme/platform
  strategy: pull_request
  commit:
    author: bot
  pull_request:
    reviewers: [octocat, acme/platform-team]
`

func TestPullRequestOpensOnceAndAccumulates(t *testing.T) {
	h := newHarness(t, prConfig, "")
	const head = "massdriver/params/api-prod"
	h.deploy("ev-1", "dep-1", "large", nil)
	if _, ok := h.gh.file("acme/platform", "main", filePath); ok {
		t.Fatal("pull_request wrote to the base branch")
	}
	if _, ok := h.gh.file("acme/platform", head, filePath); !ok {
		t.Fatal("not written to the pull request branch")
	}
	if len(h.gh.allPulls()) != 1 {
		t.Fatalf("pulls = %d, want 1", len(h.gh.allPulls()))
	}
	pr := h.gh.allPulls()[0]
	if pr.head != head || pr.base != "main" || pr.title != "Massdriver params: api-prod" {
		t.Fatalf("pull = %+v", pr)
	}
	if strings.Join(pr.users, ",") != "octocat" || strings.Join(pr.teamSlugs, ",") != "platform-team" {
		t.Fatalf("reviewers = %v %v", pr.users, pr.teamSlugs)
	}
	if c := h.gh.commitsOn(head); len(c) != 1 || c[0].author != nil {
		t.Fatalf("commits = %+v, want one by the token's account", c)
	}

	h.deploy("ev-2", "dep-2", "small", nil)
	h.deploy("ev-3", "dep-3", "small", nil)
	if len(h.gh.allPulls()) != 1 {
		t.Fatalf("pulls = %d, want 1", len(h.gh.allPulls()))
	}
	if n := len(h.gh.commitsOn(head)); n != 2 {
		t.Fatalf("commits on the pull request branch = %d, want 2", n)
	}
	if got, _ := h.gh.file("acme/platform", head, filePath); !strings.Contains(got, `"size": "small"`) {
		t.Fatalf("params.json =\n%s", got)
	}
}

func TestPullRequestStartsAgainAfterMerge(t *testing.T) {
	h := newHarness(t, prConfig, "")
	const head = "massdriver/params/api-prod"
	h.deploy("ev-1", "dep-1", "large", nil)
	// Merge: the base gets the file, the pull request closes, and the branch stays.
	got, _ := h.gh.file("acme/platform", head, filePath)
	h.gh.mu.Lock()
	h.gh.repos["acme/platform"]["main"].files[filePath] = []byte(got)
	h.gh.repos["acme/platform"]["main"].sha = "merged"
	h.gh.pulls[0].open = false
	h.gh.mu.Unlock()

	h.deploy("ev-2", "dep-2", "large", nil)
	if len(h.gh.allPulls()) != 1 {
		t.Fatalf("pulls = %d, want 1: unchanged params opened a pull request", len(h.gh.allPulls()))
	}
	h.deploy("ev-3", "dep-3", "small", nil)
	if len(h.gh.allPulls()) != 2 || !h.gh.allPulls()[1].open {
		t.Fatalf("pulls = %d, want a second open one", len(h.gh.allPulls()))
	}
	if n := len(h.gh.commitsOn(head)); n != 2 {
		t.Fatalf("commits on the branch = %d, want 2", n)
	}
}

func TestPullRequestPerDeploymentWhenNotRolling(t *testing.T) {
	h := newHarness(t, prConfig+"    rolling: false\n", "")
	h.deploy("ev-1", "dep-1aaaaaaaa", "large", nil)
	h.deploy("ev-2", "dep-2bbbbbbbb", "small", nil)
	if len(h.gh.allPulls()) != 2 {
		t.Fatalf("pulls = %d, want 2", len(h.gh.allPulls()))
	}
	if h.gh.allPulls()[0].head != "massdriver/params/api-prod-dep-1aaa" || h.gh.allPulls()[1].head != "massdriver/params/api-prod-dep-2bbb" {
		t.Fatalf("heads = %+v", h.gh.allPulls())
	}
}

func TestPullRequestDeletesOnDecommission(t *testing.T) {
	h := newHarness(t, prConfig, "")
	h.gh.mu.Lock()
	h.gh.repos["acme/platform"]["main"].files[filePath] = []byte("{}\n")
	h.gh.mu.Unlock()
	h.api.set(instanceID, "DECOMMISSIONED", "dep-1", nil, nil)
	h.send(event("ev-1", "DECOMMISSION", "dep-2", nil))
	if _, ok := h.gh.file("acme/platform", "massdriver/params/api-prod", filePath); ok {
		t.Fatal("file not deleted on the pull request branch")
	}
	if len(h.gh.allPulls()) != 1 {
		t.Fatalf("pulls = %d, want 1", len(h.gh.allPulls()))
	}
}

func TestRetryOnlyWhenItCanSucceed(t *testing.T) {
	h := newHarness(t, pushConfig, "")
	h.api.setFail(true)
	if code := h.deploy("ev-1", "dep-1", "large", nil); code != http.StatusInternalServerError {
		t.Fatalf("api failure: status %d, want 500", code)
	}
	h.api.setFail(false)

	missing := newHarness(t, "defaults:\n  repo: acme/missing\n", "")
	if code := missing.deploy("ev-1", "dep-1", "large", nil); code != http.StatusNoContent {
		t.Fatalf("missing repo: status %d, want 204", code)
	}
}

func TestRetryableErrors(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want bool
	}{
		{&githubError{status: 404}, false},
		{&githubError{status: 403}, false},
		{&githubError{status: 403, limited: true}, true},
		{&githubError{status: 409}, true},
		{&githubError{status: 502}, true},
		{errors.New("connection reset"), true},
	} {
		if got := retryable(tc.err); got != tc.want {
			t.Errorf("retryable(%v) = %v, want %v", tc.err, got, tc.want)
		}
	}
}

func TestRenderFollowsRefsAndArrays(t *testing.T) {
	s := map[string]any{
		"$defs": map[string]any{"secret": map[string]any{"type": "string", "writeOnly": true}},
		"properties": map[string]any{
			"keys":  map[string]any{"type": "array", "items": map[string]any{"$ref": "#/$defs/secret"}},
			"users": map[string]any{"type": "array", "items": map[string]any{"properties": map[string]any{"pw": map[string]any{"$ref": "#/$defs/secret"}}}},
			"tags":  map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
		},
	}
	raw := json.RawMessage(`{"keys":["a","b"],"users":[{"name":"x","pw":"p"}],"tags":["<web>"]}`)
	got, err := render(raw, s, &redactor{})
	if err != nil {
		t.Fatal(err)
	}
	want := `{
  "keys": [
    "[SENSITIVE]",
    "[SENSITIVE]"
  ],
  "tags": [
    "<web>"
  ],
  "users": [
    {
      "name": "x",
      "pw": "[SENSITIVE]"
    }
  ]
}
`
	if string(got) != want {
		t.Fatalf("render =\n%s\nwant\n%s", got, want)
	}
}

func TestStartRefusesBadConfig(t *testing.T) {
	t.Setenv("MASSDRIVER_ORGANIZATION_ID", "acme")
	t.Setenv("MASSDRIVER_API_KEY", "md_test")
	t.Setenv("GITHUB_TOKEN", "ghp_test")
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	cases := []struct{ name, file, want string }{
		{"unknown key", write("unknown.yaml", "defaults:\n  repo: a/b\n  path: x\n"), "field path not found"},
		{"no repo", write("norepo.yaml", "defaults:\n  branch: main\n"), "defaults.repo must be owner/name"},
		{"bad strategy", write("strategy.yaml", "defaults:\n  repo: a/b\n  strategy: merge\n"), "defaults.strategy must be push or pull_request"},
		{"dot dot root", write("root.yaml", "defaults:\n  repo: a/b\n  root: ../x\n"), "defaults.root must not have a .. segment"},
		{"no match", write("nomatch.yaml", "defaults:\n  repo: a/b\nmappings:\n  - name: x\n"), "mappings[0].match needs at least one attribute"},
		{"bad template", write("tmpl.yaml", "defaults:\n  repo: a/b\n  commit:\n    message: '\\(.a | '\n"), "defaults.commit.message"},
		{"two names", write("dup.yaml", "defaults:\n  repo: a/b\nmappings:\n  - {name: a, match: {x: y}}\n  - {name: a, match: {x: z}}\n"), "mappings[1].name is used by an earlier mapping"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("CONFIG_FILE", tc.file)
			_, err := Definition.New()
			var ce *automation.ConfigError
			if !errors.As(err, &ce) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want a config error that contains %q", err, tc.want)
			}
		})
	}
}

func TestSkippedDefaultsNeedNoRepo(t *testing.T) {
	cfg := DefaultConfig()
	if err := automation.DecodeConfig([]byte("defaults:\n  skip: true\nmappings:\n  - name: a\n    match: {team: x}\n    repo: a/b\n    skip: false\n"), &cfg); err != nil {
		t.Fatal(err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if !cfg.routeFor(map[string]any{}).skip || cfg.routeFor(map[string]any{"team": "x"}).skip {
		t.Fatal("skip did not follow the mapping")
	}
}

func TestExampleConfigLoads(t *testing.T) {
	cfg := DefaultConfig()
	if err := automation.LoadConfigFile("config.example.yaml", &cfg); err != nil {
		t.Fatal(err)
	}
	if len(cfg.routes) != 4 || cfg.routes[0].strategy != StrategyPullRequest || cfg.routes[1].root != "deploy/massdriver" {
		t.Fatalf("routes = %+v", cfg.routes)
	}
	if got := cfg.routes[0].teams; len(got) != 1 || got[0] != "platform-team" {
		t.Fatalf("teams = %v", got)
	}
	if !cfg.routes[3].skip {
		t.Fatal("skip-previews does not skip")
	}
}
