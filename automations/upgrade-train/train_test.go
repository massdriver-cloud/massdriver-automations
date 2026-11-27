package upgradetrain

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/massdriver-cloud/massdriver-automations/pkg/automation"
	"github.com/massdriver-cloud/massdriver-automations/pkg/massdriver"
	"github.com/massdriver-cloud/massdriver-automations/pkg/slack/slacktest"
)

var secret = []byte("0123456789abcdef0123456789abcdef")

type fakeInstance struct {
	env      string
	resolved string
	deployed string
	busy     bool
	alarms   int
}

type call struct {
	Op   string
	Vars map[string]any
}

// fakeAPI serves instances of bundle api-db. Each instance is in the environment its env names, and the stage
// filter matches on md-environment only.
type fakeAPI struct {
	mu        sync.Mutex
	tags      []string
	instances map[string]*fakeInstance
	reject    string
	created   int
	calls     []call
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

	var data any
	switch op {
	case "BundleVersions":
		items := []any{}
		for i, tag := range f.tags {
			items = append(items, map[string]any{"tag": tag, "createdAt": time.Date(2026, 11, 20-i, 0, 0, 0, 0, time.UTC)})
		}
		data = map[string]any{"ociRepo": map[string]any{"tags": map[string]any{"items": items}}}
	case "StageInstances":
		envs := map[string]bool{}
		for _, a := range req.Variables["attributes"].([]any) {
			am := a.(map[string]any)
			if am["key"] != "md-environment" {
				continue
			}
			if v, ok := am["eq"].(string); ok {
				envs[v] = true
			}
			if vs, ok := am["in"].([]any); ok {
				for _, v := range vs {
					envs[v.(string)] = true
				}
			}
		}
		items := []any{}
		for _, id := range sortedKeys(f.instances) {
			if envs[f.instances[id].env] {
				items = append(items, map[string]any{"id": id})
			}
		}
		data = map[string]any{"instances": map[string]any{"cursor": map[string]any{"next": nil}, "items": items}}
	case "Instance":
		id := req.Variables["id"].(string)
		fi := f.instances[id]
		inst := map[string]any{
			"id": id, "status": "PROVISIONED", "version": "~1", "params": map[string]any{"size": "small"},
			"resolvedVersion": fi.resolved, "deployedVersion": fi.deployed, "firingAlarmCount": fi.alarms,
			"effectiveAttributes": map[string]any{"md-project": "api", "md-environment": fi.env, "md-component": "db"},
			"latestDeployment":    map[string]any{"id": "d", "status": "COMPLETED"},
		}
		if fi.busy {
			inst["latestDeployment"] = map[string]any{"id": "d", "status": "RUNNING"}
		}
		data = map[string]any{"instance": inst}
	case "CreateDeployment", "ProposeDeployment":
		field := strings.ToLower(op[:1]) + op[1:]
		if f.reject != "" {
			data = map[string]any{field: map[string]any{"successful": false, "result": nil,
				"messages": []any{map[string]any{"field": "params", "message": f.reject}}}}
			break
		}
		f.created++
		data = map[string]any{field: map[string]any{"successful": true, "messages": []any{},
			"result": map[string]any{"id": fmt.Sprintf("dep-%d", f.created)}}}
	case "SetStashEntry":
		data = map[string]any{"alphaSetStashEntry": map[string]any{"successful": true, "messages": []any{},
			"result": map[string]any{"key": req.Variables["key"]}}}
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
}

func sortedKeys(m map[string]*fakeInstance) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	for i := range keys {
		for j := i + 1; j < len(keys); j++ {
			if keys[j] < keys[i] {
				keys[i], keys[j] = keys[j], keys[i]
			}
		}
	}
	return keys
}

// deployments returns the instance and message of each deployment created, in order.
func (f *fakeAPI) deployments() []call {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []call
	for _, c := range f.calls {
		if strings.HasSuffix(c.Op, "Deployment") {
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

type env struct {
	api   *fakeAPI
	slack *slacktest.Server
	t     *Trains
	h     http.Handler
	now   time.Time
	cfg   *Config
	opts  Options
}

const trainYAML = `
slack:
  channel: "#releases"
trains:
  - name: db
    bundles: [api-db]
    stages:
      - name: dev
        match: {md-environment: dev}
        mode: deploy
        soak: 1h
      - name: prod
        match: {md-environment: [prod, prod-eu]}
        soak: 30m
`

func setup(t *testing.T, cfgYAML, stateDir string) *env {
	t.Helper()
	cfg, err := ParseConfig([]byte(cfgYAML))
	if err != nil {
		t.Fatal(err)
	}
	e := &env{
		api: &fakeAPI{tags: []string{"1.3.0", "1.2.0"}, instances: map[string]*fakeInstance{
			"api-dev-db":    {env: "dev", resolved: "1.3.0", deployed: "1.2.0"},
			"api-prod-db":   {env: "prod", resolved: "1.3.0", deployed: "1.2.0"},
			"api-prodeu-db": {env: "prod-eu", resolved: "1.2.0", deployed: "1.2.0"},
		}},
		slack: slacktest.NewServer(t),
		now:   time.Date(2026, 11, 27, 9, 0, 0, 0, time.UTC),
		cfg:   cfg,
		opts:  Options{RunToken: "train-token", StateDir: stateDir},
	}
	e.start(t)
	return e
}

// start builds the automation as a fresh process would, from the state on disk.
func (e *env) start(t *testing.T) {
	t.Helper()
	srv := httptest.NewServer(e.api)
	t.Cleanup(srv.Close)
	tr, err := New(&massdriver.Client{Endpoint: srv.URL, OrganizationID: "acme", APIKey: "md_test"}, e.slack.Bot(), e.cfg, e.opts)
	if err != nil {
		t.Fatal(err)
	}
	tr.run = func(f func()) { f() }
	tr.now = func() time.Time { return e.now }
	e.t = tr
	e.h = automation.Handler(tr, secret)
}

func (e *env) send(t *testing.T, id, typ string, entity map[string]any) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"id": id, "type": typ, "time": "2026-11-27T09:00:00Z",
		"data": map[string]any{"entity": entity},
	})
	req := httptest.NewRequest(http.MethodPost, automation.HookPath, strings.NewReader(string(body)))
	automation.SignRequest(req, secret, id, body, time.Now())
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("%s: status %d", typ, rec.Code)
	}
}

func (e *env) published(t *testing.T) {
	t.Helper()
	e.send(t, "pub-1", "repo.published", map[string]any{"name": "api-db"})
}

func (e *env) finished(t *testing.T, id, typ, depID string) {
	t.Helper()
	e.send(t, id, typ, map[string]any{"id": depID, "message": "upgrade-train: db api-db@1.3.0 stage x"})
}

func (e *env) tick(advance time.Duration) {
	e.now = e.now.Add(advance)
	e.t.Tick(context.Background())
}

func TestTrainMovesThroughStagesAfterSoak(t *testing.T) {
	e := setup(t, trainYAML, t.TempDir())
	e.published(t)

	deps := e.api.deployments()
	if len(deps) != 1 || deps[0].Op != "CreateDeployment" || deps[0].Vars["id"] != "api-dev-db" {
		t.Fatalf("dev deployments = %+v", deps)
	}
	in := deps[0].Vars["input"].(map[string]any)
	if in["message"] != "upgrade-train: db api-db@1.3.0 stage dev" || in["params"] != `{"size":"small"}` {
		t.Errorf("input = %v", in)
	}

	e.api.instances["api-dev-db"].deployed = "1.3.0"
	e.finished(t, "done-1", "deployment.completed", "dep-1")
	e.tick(59 * time.Minute)
	if n := len(e.api.deployments()); n != 1 {
		t.Fatalf("prod started before the soak ended: %d deployments", n)
	}

	e.tick(2 * time.Minute)
	deps = e.api.deployments()
	if len(deps) != 2 || deps[1].Op != "ProposeDeployment" || deps[1].Vars["id"] != "api-prod-db" {
		t.Fatalf("prod deployments = %+v", deps)
	}

	e.api.instances["api-prod-db"].deployed = "1.3.0"
	e.finished(t, "done-2", "deployment.completed", "dep-2")
	e.tick(31 * time.Minute)

	texts := e.slack.Texts()
	want := []string{
		"Upgrade train db: api-db@1.3.0 stage dev (1 of 2): deploying 1, 0 already on it.",
		"Upgrade train db: api-db@1.3.0 stage dev is on the version (1 instances). Soaking for 1h0m0s.",
		"Upgrade train db: api-db@1.3.0 stage prod (2 of 2): proposing 1, 0 already on it. Left out, because their version does not resolve to 1.3.0: api-prodeu-db.",
		"Upgrade train db: api-db@1.3.0 stage prod is on the version (1 instances). Soaking for 30m0s.",
		"Upgrade train db: api-db@1.3.0 finished all 2 stages.",
	}
	if strings.Join(texts, "\n") != strings.Join(want, "\n") {
		t.Errorf("slack =\n%s\nwant\n%s", strings.Join(texts, "\n"), strings.Join(want, "\n"))
	}
	if len(e.t.st.Runs) != 0 {
		t.Errorf("runs left: %v", e.t.st.Runs)
	}
}

func TestFailedDeploymentHaltsAndRunResumes(t *testing.T) {
	e := setup(t, trainYAML, t.TempDir())
	e.published(t)
	e.finished(t, "fail-1", "deployment.failed", "dep-1")

	texts := e.slack.Texts()
	if !strings.Contains(texts[len(texts)-1], "Upgrade train db stopped api-db@1.3.0 at stage dev: deployment dep-1 of api-dev-db failed.") {
		t.Errorf("slack = %q", texts)
	}
	e.tick(2 * time.Hour)
	if n := len(e.api.deployments()); n != 1 {
		t.Fatalf("a halted train deployed: %d", n)
	}

	req := httptest.NewRequest(http.MethodPost, "/upgrade-train/run?train=db&version=1.3.0", nil)
	req.Header.Set("Authorization", "Bearer train-token")
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("run: %d", rec.Code)
	}
	if deps := e.api.deployments(); len(deps) != 2 || deps[1].Vars["id"] != "api-dev-db" {
		t.Errorf("resume did not deploy dev again: %+v", deps)
	}
}

func TestRejectedProposalHalts(t *testing.T) {
	e := setup(t, strings.Replace(trainYAML, "mode: deploy", "mode: propose", 1), t.TempDir())
	e.published(t)
	e.finished(t, "rej-1", "deployment.rejected", "dep-1")
	if r := e.t.st.Runs["db/api-db"]; r == nil || r.Phase != PhaseHalted {
		t.Errorf("run = %+v", r)
	}
}

func TestFiringAlarmDuringSoakHalts(t *testing.T) {
	e := setup(t, trainYAML, t.TempDir())
	e.published(t)
	e.api.instances["api-dev-db"].deployed = "1.3.0"
	e.finished(t, "done-1", "deployment.completed", "dep-1")
	e.api.instances["api-dev-db"].alarms = 2
	e.tick(10 * time.Minute)

	texts := e.slack.Texts()
	if got := texts[len(texts)-1]; !strings.Contains(got, "has 2 firing alarms during the soak of stage dev") ||
		!strings.Contains(got, "<https://app.massdriver.cloud/orgs/acme/projects/api/environments/dev/instances/db|api-dev-db>") {
		t.Errorf("slack = %q", got)
	}
	e.tick(2 * time.Hour)
	if n := len(e.api.deployments()); n != 1 {
		t.Errorf("prod started after an alarm: %d", n)
	}
}

func TestBusyInstanceWaitsThenIsLeftOut(t *testing.T) {
	e := setup(t, trainYAML, t.TempDir())
	e.api.instances["api-dev-db"].busy = true
	e.published(t)
	if n := len(e.api.deployments()); n != 0 {
		t.Fatalf("deployed a busy instance")
	}
	e.tick(time.Minute)
	e.api.instances["api-dev-db"].busy = false
	e.tick(time.Minute)
	if deps := e.api.deployments(); len(deps) != 1 {
		t.Fatalf("retry did not deploy: %d", len(deps))
	}

	e2 := setup(t, trainYAML, t.TempDir())
	e2.api.instances["api-dev-db"].busy = true
	e2.published(t)
	for range maxWaitTicks + 1 {
		e2.tick(time.Minute)
	}
	texts := e2.slack.Texts()
	joined := strings.Join(texts, "\n")
	if !strings.Contains(joined, "stage dev leaves out api-dev-db") || !strings.Contains(joined, "Soaking for") {
		t.Errorf("slack = %q", texts)
	}
}

func TestStateSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	e := setup(t, trainYAML, dir)
	e.published(t)
	e.start(t)
	e.api.instances["api-dev-db"].deployed = "1.3.0"
	e.finished(t, "done-1", "deployment.completed", "dep-1")
	if r := e.t.st.Runs["db/api-db"]; r == nil || r.Phase != PhaseSoaking {
		t.Errorf("run after restart = %+v", r)
	}
}

func TestNewerVersionReplacesTheRun(t *testing.T) {
	e := setup(t, trainYAML, t.TempDir())
	e.published(t)
	e.api.tags = []string{"1.3.1", "1.3.0"}
	for _, fi := range e.api.instances {
		if fi.resolved == "1.3.0" {
			fi.resolved = "1.3.1"
		}
	}
	e.send(t, "pub-2", "repo.published", map[string]any{"name": "api-db"})
	if r := e.t.st.Runs["db/api-db"]; r == nil || r.Version != "1.3.1" || r.Stage != 0 {
		t.Fatalf("run = %+v", r)
	}
	texts := e.slack.Texts()
	if !strings.Contains(strings.Join(texts, "\n"), "api-db@1.3.1 replaces api-db@1.3.0, which stops at stage dev.") {
		t.Errorf("slack = %q", texts)
	}
	// The old run's deployment ending is ignored.
	e.finished(t, "fail-old", "deployment.failed", "dep-1")
	if r := e.t.st.Runs["db/api-db"]; r.Phase == PhaseHalted {
		t.Error("the replaced run's failure halted the new run")
	}
}

func TestDevelopmentVersionsAndOtherBundlesAreIgnored(t *testing.T) {
	e := setup(t, trainYAML, t.TempDir())
	e.api.tags = []string{"1.4.0-dev.20261127T000000Z"}
	e.published(t)
	e.send(t, "pub-other", "repo.published", map[string]any{"name": "other"})
	if n := len(e.api.deployments()); n != 0 || len(e.t.st.Runs) != 0 {
		t.Errorf("deployments = %d, runs = %v", n, e.t.st.Runs)
	}
}

func TestPayloadErrorHalts(t *testing.T) {
	e := setup(t, trainYAML, t.TempDir())
	e.api.reject = "size is not allowed"
	e.published(t)
	texts := e.slack.Texts()
	if !strings.Contains(texts[len(texts)-1], "deploying api-dev-db failed: createDeployment: params: size is not allowed") {
		t.Errorf("slack = %q", texts)
	}
}

func TestRoutes(t *testing.T) {
	e := setup(t, trainYAML, t.TempDir())
	post := func(path, token string) int {
		req := httptest.NewRequest(http.MethodPost, path, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		e.h.ServeHTTP(rec, req)
		return rec.Code
	}
	if code := post("/upgrade-train/run?train=db", "wrong"); code != http.StatusUnauthorized {
		t.Errorf("wrong token: %d", code)
	}
	if code := post("/upgrade-train/run?train=nope", "train-token"); code != http.StatusNotFound {
		t.Errorf("unknown train: %d", code)
	}
	if code := post("/upgrade-train/run?train=db&bundle=other", "train-token"); code != http.StatusBadRequest {
		t.Errorf("uncovered bundle: %d", code)
	}
	if code := post("/upgrade-train/run?train=db", "train-token"); code != http.StatusAccepted {
		t.Fatalf("run latest: %d", code)
	}
	if r := e.t.st.Runs["db/api-db"]; r == nil || r.Version != "1.3.0" {
		t.Fatalf("run = %+v", r)
	}
	if code := post("/upgrade-train/halt?train=db", "train-token"); code != http.StatusOK {
		t.Errorf("halt: %d", code)
	}
	if r := e.t.st.Runs["db/api-db"]; r.Phase != PhaseHalted {
		t.Errorf("phase = %s", r.Phase)
	}
}

func TestConfig(t *testing.T) {
	for name, doc := range map[string]string{
		"no trains":      "slack: {channel: x}\n",
		"no bundles":     "trains: [{name: a, stages: [{name: s, match: {a: b}}]}]\n",
		"no stages":      "trains: [{name: a, bundles: [x]}]\n",
		"no match":       "trains: [{name: a, bundles: [x], stages: [{name: s}]}]\n",
		"bad mode":       "trains: [{name: a, bundles: [x], stages: [{name: s, match: {a: b}, mode: yolo}]}]\n",
		"bad soak":       "trains: [{name: a, bundles: [x], stages: [{name: s, match: {a: b}, soak: 1d}]}]\n",
		"dup train":      "trains: [{name: a, bundles: [x], stages: [{name: s, match: {a: b}}]}, {name: a, bundles: [x], stages: [{name: s, match: {a: b}}]}]\n",
		"dup stage name": "trains: [{name: a, bundles: [x], stages: [{name: s, match: {a: b}}, {name: s, match: {a: c}}]}]\n",
	} {
		if _, err := ParseConfig([]byte(doc)); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	cfg, err := ParseConfig([]byte("trains: [{name: a, bundles: [x], stages: [{name: s, match: {a: b}}, {name: t, match: {a: c}, soak: 0s}]}]\n"))
	if err != nil {
		t.Fatal(err)
	}
	st := cfg.Trains[0].Stages
	if st[0].Mode != ModePropose || st[0].Soak.Duration != DefaultSoak || st[1].Soak.Duration != 0 {
		t.Errorf("defaults = %+v", st)
	}
}

func TestBundleConfigWithNullsLoads(t *testing.T) {
	cfg, err := ParseConfig([]byte(`"massdriver": null
"slack": null
"trains":
- "bundles":
  - "api-db"
  "include_development": null
  "name": "db"
  "slack": null
  "stages":
  - "match":
      "md-environment":
      - "dev"
    "mode": "deploy"
    "name": "dev"
    "soak": null
`))
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Trains[0].Stages[0].Soak.Duration; got != DefaultSoak {
		t.Errorf("soak = %s", got)
	}
}

func TestExampleConfigLoads(t *testing.T) {
	var cfg Config
	if err := automation.LoadConfigFile("config.example.yaml", &cfg); err != nil {
		t.Fatal(err)
	}
}

func TestStageResultsGoToTheStash(t *testing.T) {
	e := setup(t, trainYAML, t.TempDir())
	e.published(t)
	e.api.instances["api-dev-db"].deployed = "1.3.0"
	e.finished(t, "done-1", "deployment.completed", "dep-1")

	want := [][4]string{
		{`{"deployment":"dep-1"}`, "md/v1/step/upgrade-train", "Upgrade train",
			`{"tool":"upgrade-train","action":"apply","status":"succeeded","finished_at":"2026-11-27T09:00:00Z","message":"Stage dev of train db: api-db@1.3.0"}`},
		{`{"instance":"api-dev-db"}`, "md/v1/release", "Release", `{"version":"1.3.0","deployed_at":"2026-11-27T09:00:00Z"}`},
	}
	if got := e.api.stash(); !reflect.DeepEqual(got, want) {
		t.Errorf("stash = %v\nwant    %v", got, want)
	}
}
