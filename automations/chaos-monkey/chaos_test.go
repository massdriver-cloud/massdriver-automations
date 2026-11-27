package chaosmonkey

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/massdriver-cloud/massdriver-automations/pkg/automation"
	"github.com/massdriver-cloud/massdriver-automations/pkg/massdriver"
	"github.com/massdriver-cloud/massdriver-automations/pkg/slack/slacktest"
)

var secret = []byte("0123456789abcdef0123456789abcdef")

type call struct {
	Op   string
	Vars map[string]any
}

type fakeInstance struct {
	latest   string
	proposed bool
}

// fakeAPI serves instances in project api, environment sandbox. The filter is not applied: every listed instance
// is what Massdriver would return for the match.
type fakeAPI struct {
	mu        sync.Mutex
	listed    []string
	instances map[string]fakeInstance
	reject    string
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

	payload := func(field, id string) map[string]any {
		if f.reject != "" {
			return map[string]any{field: map[string]any{"successful": false, "result": nil,
				"messages": []any{map[string]any{"field": "params", "message": f.reject}}}}
		}
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
		var schema map[string]any
		_ = json.Unmarshal([]byte(testSchema), &schema)
		inst := map[string]any{
			"id":           id,
			"status":       "PROVISIONED",
			"params":       map[string]any{"size": "small", "replicas": 1, "region": "us-east-1"},
			"paramsSchema": schema,
			"effectiveAttributes": map[string]any{"md-project": "api", "md-environment": "sandbox",
				"md-component": strings.TrimPrefix(id, "api-sandbox-")},
			"latestDeployment":             nil,
			"mostRecentProposedDeployment": nil,
		}
		if fi.latest != "" {
			inst["latestDeployment"] = map[string]any{"id": "latest-" + id, "status": fi.latest}
		}
		if fi.proposed {
			inst["mostRecentProposedDeployment"] = map[string]any{"id": "proposal-" + id}
		}
		data = map[string]any{"instance": inst}
	case "CreateDeployment":
		data = payload("createDeployment", "deploy-1")
	case "ProposeDeployment":
		data = payload("proposeDeployment", "proposal-1")
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
}

func (f *fakeAPI) mutations() []call {
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

type env struct {
	api   *fakeAPI
	slack *slacktest.Server
	c     *Chaos
	h     http.Handler
}

func setup(t *testing.T, cfgYAML string) *env {
	t.Helper()
	cfg, err := ParseConfig([]byte(cfgYAML))
	if err != nil {
		t.Fatal(err)
	}
	api := &fakeAPI{instances: map[string]fakeInstance{}}
	srv := httptest.NewServer(api)
	t.Cleanup(srv.Close)
	sl := slacktest.NewServer(t)
	c := New(&massdriver.Client{Endpoint: srv.URL, OrganizationID: "acme", APIKey: "md_test"}, sl.Bot(), cfg, "run-token")
	c.run = func(f func()) { f() }
	return &env{api: api, slack: sl, c: c, h: automation.Handler(c, secret)}
}

const base = `
slack:
  channel: "#chaos"
seed: 42
signature: null
match:
  md-environment: sandbox
  chaos: ["true", "yes"]
`

func input(t *testing.T, c call) (map[string]any, map[string]any) {
	t.Helper()
	in := c.Vars["input"].(map[string]any)
	var params map[string]any
	if err := json.Unmarshal([]byte(in["params"].(string)), &params); err != nil {
		t.Fatalf("params is not a JSON string: %v", err)
	}
	return in, params
}

func TestRunProposesByDefault(t *testing.T) {
	e := setup(t, base)
	e.api.listed = []string{"api-sandbox-db"}
	if err := e.c.Run(context.Background(), ""); err != nil {
		t.Fatal(err)
	}

	match := e.api.calls[0]
	if match.Op != "MatchingInstances" {
		t.Fatalf("first call = %s", match.Op)
	}
	attrs, _ := json.Marshal(match.Vars["attributes"])
	if want := `[{"in":["true","yes"],"key":"chaos"},{"eq":"sandbox","key":"md-environment"}]`; string(attrs) != want {
		t.Errorf("attributes = %s, want %s", attrs, want)
	}
	if status, _ := json.Marshal(match.Vars["status"]); string(status) != `["PROVISIONED"]` {
		t.Errorf("status = %s", status)
	}

	muts := e.api.mutations()
	if len(muts) != 1 || muts[0].Op != "ProposeDeployment" {
		t.Fatalf("mutations = %+v", muts)
	}
	in, params := input(t, muts[0])
	if in["action"] != "PROVISION" || !strings.HasPrefix(in["message"].(string), MessagePrefix) {
		t.Errorf("input = %v", in)
	}
	if params["region"] != "us-east-1" {
		t.Errorf("readOnly region lost: %v", params)
	}

	texts := e.slack.Texts()
	if len(texts) != 1 || !strings.HasPrefix(texts[0], "Chaos monkey proposed <https://app.massdriver.cloud/orgs/acme/projects/api/environments/sandbox/instances/db|api-sandbox-db> with ") {
		t.Errorf("slack = %q", texts)
	}
	if !strings.Contains(texts[0], "deployment=proposal-1") {
		t.Errorf("slack does not link the proposal: %q", texts[0])
	}
}

func TestRunPlansAndDeploys(t *testing.T) {
	for mode, action := range map[string]string{ModePlan: "PLAN", ModeDeploy: "PROVISION"} {
		t.Run(mode, func(t *testing.T) {
			e := setup(t, base+"mode: "+mode+"\n")
			e.api.listed = []string{"api-sandbox-db"}
			if err := e.c.Run(context.Background(), ""); err != nil {
				t.Fatal(err)
			}
			muts := e.api.mutations()
			if len(muts) != 1 || muts[0].Op != "CreateDeployment" {
				t.Fatalf("mutations = %+v", muts)
			}
			if in, _ := input(t, muts[0]); in["action"] != action {
				t.Errorf("action = %v, want %s", in["action"], action)
			}
		})
	}
}

func TestRunHitsUpToInstancesAndSkipsBusyOnes(t *testing.T) {
	e := setup(t, base+"instances: 2\n")
	e.api.listed = []string{"api-sandbox-a", "api-sandbox-b", "api-sandbox-c", "api-sandbox-d"}
	e.api.instances["api-sandbox-a"] = fakeInstance{latest: "RUNNING"}
	e.api.instances["api-sandbox-b"] = fakeInstance{proposed: true}
	if err := e.c.Run(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	muts := e.api.mutations()
	if len(muts) != 2 {
		t.Fatalf("mutations = %d, want 2", len(muts))
	}
	for _, m := range muts {
		if id := m.Vars["id"]; id != "api-sandbox-c" && id != "api-sandbox-d" {
			t.Errorf("deployed busy instance %v", id)
		}
	}
}

func TestDeployModeIgnoresWaitingProposals(t *testing.T) {
	e := setup(t, base+"mode: deploy\n")
	e.api.listed = []string{"api-sandbox-b"}
	e.api.instances["api-sandbox-b"] = fakeInstance{proposed: true}
	if err := e.c.Run(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	if n := len(e.api.mutations()); n != 1 {
		t.Errorf("mutations = %d, want 1", n)
	}
}

func TestRejectedParamsArePosted(t *testing.T) {
	e := setup(t, base)
	e.api.listed = []string{"api-sandbox-db"}
	e.api.reject = "replicas must be at most 9"
	if err := e.c.Run(context.Background(), StrategyGibberish); err != nil {
		t.Fatal(err)
	}
	texts := e.slack.Texts()
	if len(texts) != 1 || !strings.Contains(texts[0], "with gibberish: garbled ") ||
		!strings.HasSuffix(texts[0], "Massdriver rejected the params: params: replicas must be at most 9") {
		t.Errorf("slack = %q", texts)
	}
}

func TestAcceptedGibberishIsCalledOut(t *testing.T) {
	e := setup(t, base+"mode: deploy\n")
	e.api.listed = []string{"api-sandbox-db"}
	if err := e.c.Run(context.Background(), StrategyGibberish); err != nil {
		t.Fatal(err)
	}
	if texts := e.slack.Texts(); len(texts) != 1 || !strings.HasSuffix(texts[0], "Massdriver accepted gibberish.") {
		t.Errorf("slack = %q", texts)
	}
}

func event(t *testing.T, h http.Handler, id, typ, message string) int {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"id":      id,
		"type":    typ,
		"subject": "mri:acme:api:sandbox:db",
		"data": map[string]any{
			"entity": map[string]any{"id": "deploy-1", "message": message},
			"attributes": map[string]any{"md-instance": "api-sandbox-db", "md-project": "api",
				"md-environment": "sandbox", "md-component": "db"},
			"metadata": map[string]any{"deployment_action": "PROVISION"},
		},
	})
	req := httptest.NewRequest(http.MethodPost, automation.HookPath, strings.NewReader(string(body)))
	automation.SignRequest(req, secret, id, body, time.Now())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code
}

func TestOutcomesOfChaosDeploymentsArePosted(t *testing.T) {
	e := setup(t, base)
	if code := event(t, e.h, "e1", "deployment.failed", MessagePrefix+"merge: presets \"Small\" + \"Large\""); code != http.StatusNoContent {
		t.Fatalf("status = %d", code)
	}
	if code := event(t, e.h, "e2", "deployment.completed", "a person deployed this"); code != http.StatusNoContent {
		t.Fatalf("status = %d", code)
	}
	texts := e.slack.Texts()
	want := `Chaos deployment <https://app.massdriver.cloud/orgs/acme/projects/api/environments/sandbox/instances/db?deployment=deploy-1&tab=history|deploy-1> of <https://app.massdriver.cloud/orgs/acme/projects/api/environments/sandbox/instances/db|api-sandbox-db> failed: merge: presets "Small" + "Large"`
	if len(texts) != 1 || texts[0] != want {
		t.Errorf("slack = %q", texts)
	}
}

func TestRunRoute(t *testing.T) {
	e := setup(t, base)
	e.api.listed = []string{"api-sandbox-db"}

	post := func(token, query string) int {
		req := httptest.NewRequest(http.MethodPost, "/chaos-monkey/run"+query, nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		rec := httptest.NewRecorder()
		e.h.ServeHTTP(rec, req)
		return rec.Code
	}
	if code := post("wrong", ""); code != http.StatusUnauthorized {
		t.Errorf("wrong token: %d", code)
	}
	if code := post("run-token", "?strategy=banana"); code != http.StatusBadRequest {
		t.Errorf("unknown strategy: %d", code)
	}
	if code := post("run-token", "?strategy=merge"); code != http.StatusAccepted {
		t.Fatalf("run: %d", code)
	}
	muts := e.api.mutations()
	if len(muts) != 1 {
		t.Fatalf("mutations = %d", len(muts))
	}
	if in, _ := input(t, muts[0]); !strings.HasPrefix(in["message"].(string), MessagePrefix+"merge: presets ") {
		t.Errorf("message = %v", in["message"])
	}
}

func TestConfigRequiresMatch(t *testing.T) {
	for name, doc := range map[string]string{
		"no match":       "mode: deploy\n",
		"bad mode":       "match: {a: b}\nmode: yolo\n",
		"bad strategy":   "match: {a: b}\nstrategies: {banana: 1}\n",
		"all weights 0":  "match: {a: b}\nstrategies: {preset: 0, mutate: 0}\n",
		"bad rate":       "match: {a: b}\nmutate: {rate: 2}\n",
		"bad status":     "match: {a: b}\nstatus: [ON_FIRE]\n",
		"bad schedule":   "match: {a: b}\nschedule: whenever\n",
		"bad timezone":   "match: {a: b}\ntimezone: Mars/Olympus\n",
		"empty value":    "match: {a: []}\n",
		"zero instances": "match: {a: b}\ninstances: -1\n",
	} {
		if _, err := ParseConfig([]byte(doc)); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	cfg, err := ParseConfig([]byte("match: {a: b}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Mode != ModePropose || cfg.Instances != 1 || len(cfg.Strategies) != 4 || cfg.Mutate.Rate != 0.3 {
		t.Errorf("defaults = %+v", cfg)
	}
}

func TestExampleConfigLoads(t *testing.T) {
	var cfg Config
	if err := automation.LoadConfigFile("config.example.yaml", &cfg); err != nil {
		t.Fatal(err)
	}
}

func TestSignatureEndsMessagesAndPosts(t *testing.T) {
	e := setup(t, strings.Replace(base, "signature: null", `signature: "Ook."`, 1))
	e.api.listed = []string{"api-sandbox-db"}
	if err := e.c.Run(context.Background(), StrategyMerge); err != nil {
		t.Fatal(err)
	}
	in, _ := input(t, e.api.mutations()[0])
	msg := in["message"].(string)
	if !strings.HasSuffix(msg, "\" Ook.") {
		t.Errorf("message = %q", msg)
	}
	if texts := e.slack.Texts(); len(texts) != 1 || !strings.HasSuffix(texts[0], "\nOok.") {
		t.Errorf("slack = %q", texts)
	}

	event(t, e.h, "e1", "deployment.completed", msg)
	texts := e.slack.Texts()
	if got := texts[len(texts)-1]; strings.Count(got, "Ook.") != 1 {
		t.Errorf("outcome repeats the signature: %q", got)
	}
}

func TestSignatureConfig(t *testing.T) {
	for doc, want := range map[string][]string{
		"":                          DefaultSignatures,
		"signature: null\n":         nil,
		"signature: ~\n":            nil,
		"signature: \"\"\n":         nil,
		"signature: hi\n":           {"hi"},
		"signature: [a, \"\", b]\n": {"a", "b"},
	} {
		cfg, err := ParseConfig([]byte("match: {a: b}\n" + doc))
		if err != nil {
			t.Fatalf("%q: %v", doc, err)
		}
		if !slices.Equal(cfg.signatures, want) {
			t.Errorf("%q: signatures = %q, want %q", doc, cfg.signatures, want)
		}
	}
	if _, err := ParseConfig([]byte("match: {a: b}\nsignature: {a: b}\n")); err == nil {
		t.Error("a map signature: no error")
	}
	if len(DefaultSignatures) != 10 {
		t.Errorf("%d default signatures", len(DefaultSignatures))
	}
}

func TestEachDeploymentGetsARandomDefaultSignature(t *testing.T) {
	e := setup(t, strings.Replace(base, "signature: null\n", "", 1)+"mode: deploy\n")
	e.api.listed = []string{"api-sandbox-db"}
	seen := map[string]bool{}
	for range 40 {
		if err := e.c.Run(context.Background(), StrategyMutate); err != nil {
			t.Fatal(err)
		}
	}
	for _, m := range e.api.mutations() {
		in, _ := input(t, m)
		for _, sig := range DefaultSignatures {
			if strings.HasSuffix(in["message"].(string), " "+sig) {
				seen[sig] = true
			}
		}
	}
	if len(seen) < 5 {
		t.Errorf("40 deployments used %d signatures", len(seen))
	}
}

func TestLongMessagesKeepTheSignature(t *testing.T) {
	got := deploymentMessage(strings.Repeat("x", 500), "bye")
	if len(got) != maxMessage || !strings.HasSuffix(got, "... bye") {
		t.Errorf("message = %d bytes, ends %q", len(got), got[len(got)-10:])
	}
}

func TestBundleConfigWithNullsLoads(t *testing.T) {
	doc := `"gibberish": null
"instances": null
"massdriver": null
"match":
  "chaos":
  - "true"
"mode": "propose"
"mutate": null
"schedule": null
"slack": null
"status": null
"strategies": null
"timezone": null
`
	cfg, err := ParseConfig([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(cfg.signatures, DefaultSignatures) || cfg.Instances != 1 || len(cfg.Strategies) != 4 {
		t.Errorf("defaults = %+v", cfg)
	}
	cfg, err = ParseConfig([]byte(doc + "\"signature\": null\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.signatures != nil {
		t.Errorf("signatures = %q, want none", cfg.signatures)
	}
}

func TestRejectedProposalsArePosted(t *testing.T) {
	e := setup(t, base)
	event(t, e.h, "e1", "deployment.rejected", MessagePrefix+`preset: preset "Small"`)
	if texts := e.slack.Texts(); len(texts) != 1 || !strings.Contains(texts[0], ` was rejected: preset: preset "Small"`) {
		t.Errorf("slack = %q", texts)
	}
}

func TestOutcomePutsTheSignatureOnItsOwnLine(t *testing.T) {
	e := setup(t, strings.Replace(base, "signature: null", "signature: [Ook., Eek.]", 1))
	event(t, e.h, "e1", "deployment.completed", MessagePrefix+"mutate: changed size Eek.")
	if texts := e.slack.Texts(); len(texts) != 1 || !strings.HasSuffix(texts[0], ": mutate: changed size\nEek.") {
		t.Errorf("slack = %q", texts)
	}
}
