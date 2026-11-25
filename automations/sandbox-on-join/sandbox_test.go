package sandboxonjoin

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/massdriver-cloud/massdriver-automations/pkg/automation"
	"github.com/massdriver-cloud/massdriver-automations/pkg/massdriver"
)

var secret = []byte("0123456789abcdef0123456789abcdef")

type fakeEnv struct {
	attrs     map[string]any
	parent    string
	statuses  []string
	deletable bool
}

type call struct {
	op   string
	vars map[string]any
}

// fakeAPI serves an organization. groups maps an account id to its group names; members maps an email to an
// account id.
type fakeAPI struct {
	mu      sync.Mutex
	groups  map[string][]string
	members map[string]string
	envs    map[string]fakeEnv
	calls   []call
}

var opName = regexp.MustCompile(`^(?:query|mutation) (\w+)`)

func (f *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Query     string         `json:"query"`
		Variables map[string]any `json:"variables"`
	}
	b, _ := io.ReadAll(r.Body)
	_ = json.Unmarshal(b, &req)
	op := opName.FindStringSubmatch(req.Query)[1]
	v := req.Variables
	done := map[string]any{"next": nil}

	f.mu.Lock()
	defer f.mu.Unlock()

	var data any
	switch op {
	case "AccountGroups":
		items := []any{}
		for _, g := range f.groups[v["accountId"].(string)] {
			items = append(items, map[string]any{"name": g})
		}
		data = map[string]any{"groups": map[string]any{"cursor": done, "items": items}}
	case "MembersByEmail":
		items := []any{}
		for email, id := range f.members {
			if strings.Contains(email, v["search"].(string)) {
				items = append(items, map[string]any{"id": id, "email": email})
			}
		}
		data = map[string]any{"organization": map[string]any{"members": map[string]any{"cursor": done, "items": items}}}
	case "Sandboxes":
		items := []any{}
		for id, e := range f.envs {
			if e.attrs["owner"] == v["owner"] && e.attrs["sandbox"] == "true" {
				items = append(items, envJSON(id, e))
			}
		}
		data = map[string]any{"environments": map[string]any{"cursor": done, "items": items}}
	case "EnvironmentIds":
		items := []any{}
		for _, id := range v["ids"].([]any) {
			if _, ok := f.envs[id.(string)]; ok {
				items = append(items, map[string]any{"id": id})
			}
		}
		data = map[string]any{"environments": map[string]any{"items": items}}
	case "Environment":
		id := v["id"].(string)
		data = map[string]any{"environment": envJSON(id, f.envs[id])}
	default:
		f.calls = append(f.calls, call{op: op, vars: v})
		field := strings.ToLower(op[:1]) + op[1:]
		result := map[string]any{"id": v["id"]}
		if op == "ForkEnvironment" {
			project, _, _ := strings.Cut(v["parentId"].(string), "-")
			result = map[string]any{"id": project + "-" + v["input"].(map[string]any)["id"].(string)}
		}
		data = map[string]any{field: map[string]any{"successful": true, "messages": []any{}, "result": result}}
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
}

func envJSON(id string, e fakeEnv) map[string]any {
	instances := []any{}
	for _, s := range e.statuses {
		instances = append(instances, map[string]any{"status": s})
	}
	var parent any
	if e.parent != "" {
		parent = map[string]any{"id": e.parent}
	}
	return map[string]any{
		"id": id, "attributes": e.attrs, "parent": parent, "instances": instances,
		"deletable": map[string]any{"result": e.deletable, "constraints": []any{}},
	}
}

var rules = []Rule{
	{Group: "Developers", Project: "api", Environment: "staging", Deploy: true, DeleteAfterDecommission: true, CopyEnvironmentDefaults: true},
	{Group: "Contractors", Project: "api", Environment: "staging"},
	{Group: "Data", Project: "etl", Environment: "dev"},
}

func setup(t *testing.T, api *fakeAPI) (*Sandboxes, http.Handler) {
	t.Helper()
	srv := httptest.NewServer(api)
	t.Cleanup(srv.Close)
	s := New(&massdriver.Client{Endpoint: srv.URL, OrganizationID: "acme", APIKey: "md_test"}, rules)
	s.run = func(f func()) { f() }
	return s, automation.Handler(s, secret)
}

func post(t *testing.T, h http.Handler, id string, ev map[string]any) int {
	t.Helper()
	ev["id"] = id
	body, _ := json.Marshal(ev)
	req := httptest.NewRequest(http.MethodPost, automation.HookPath, bytes.NewReader(body))
	automation.SignRequest(req, secret, id, body, time.Now())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code
}

func membershipEvent(typ, group string) map[string]any {
	return map[string]any{
		"type": typ,
		"data": map[string]any{
			"entity": map[string]any{
				"id": "m-1", "group_id": "g-1", "group_name": group,
				"account_id": "acct-sam", "account_email": "Sam.Lee@example.com", "account_name": "Sam Lee",
			},
			"attributes": map[string]any{},
			"metadata":   map[string]any{},
		},
	}
}

func decommissionedEvent(project, env string) map[string]any {
	return map[string]any{
		"type": "deployment.completed",
		"data": map[string]any{
			"entity":     map[string]any{"id": "dep-1", "action": "DECOMMISSION"},
			"attributes": map[string]any{"md-project": project, "md-environment": env, "sandbox": "true", "owner": "Sam.Lee@example.com"},
			"metadata":   map[string]any{"deployment_action": "DECOMMISSION"},
		},
	}
}

func ops(calls []call) []string {
	out := []string{}
	for _, c := range calls {
		if c.op == "ForkEnvironment" {
			out = append(out, c.op+" "+c.vars["parentId"].(string)+" "+c.vars["input"].(map[string]any)["id"].(string))
			continue
		}
		out = append(out, c.op+" "+c.vars["id"].(string))
	}
	return out
}

func TestJoinForksAndDeploysWithFreeIdentifier(t *testing.T) {
	api := &fakeAPI{
		groups: map[string][]string{"acct-sam": {"Developers"}},
		envs: map[string]fakeEnv{
			"api-staging":  {},
			"api-samlee":   {attrs: map[string]any{"owner": "samlee@other.example.com", "sandbox": "true"}, parent: "api-staging"},
			"api-samlee2":  {},
			"etl-samlee":   {},
			"api-unrelate": {},
		},
	}
	_, h := setup(t, api)

	if code := post(t, h, "evt-1", membershipEvent("group_membership.added", "Developers")); code != http.StatusNoContent {
		t.Fatalf("status = %d", code)
	}

	if len(api.calls) != 2 {
		t.Fatalf("calls = %v, want ForkEnvironment then DeployEnvironment", api.calls)
	}
	fork := api.calls[0]
	if fork.op != "ForkEnvironment" || fork.vars["parentId"] != "api-staging" || fork.vars["org"] != "acme" {
		t.Fatalf("fork = %v", fork)
	}
	want := map[string]any{
		"id":                      "samlee3",
		"name":                    "Sam Lee (samlee3)",
		"description":             "Sandbox of api-staging for Sam.Lee@example.com",
		"attributes":              `{"owner":"Sam.Lee@example.com","sandbox":"true"}`,
		"copySecrets":             false,
		"copyEnvironmentDefaults": true,
		"copyRemoteReferences":    false,
	}
	if !reflect.DeepEqual(fork.vars["input"], want) {
		t.Errorf("fork input = %v", fork.vars["input"])
	}
	if api.calls[1].op != "DeployEnvironment" || api.calls[1].vars["id"] != "api-samlee3" {
		t.Errorf("deploy = %v", api.calls[1])
	}
}

func TestJoinSkipsWhenSandboxExistsOrNoLongerMember(t *testing.T) {
	api := &fakeAPI{
		groups: map[string][]string{"acct-sam": {"Developers", "Contractors"}},
		envs: map[string]fakeEnv{
			"api-samlee": {attrs: map[string]any{"owner": "Sam.Lee@example.com", "sandbox": "true"}, parent: "api-staging"},
		},
	}
	_, h := setup(t, api)

	post(t, h, "evt-1", membershipEvent("group_membership.added", "Contractors"))
	post(t, h, "evt-2", membershipEvent("group_membership.added", "Data"))

	if len(api.calls) != 0 {
		t.Fatalf("calls = %v, want none", ops(api.calls))
	}
}

func TestJoinIgnoresServiceAccountsAndUnconfiguredGroups(t *testing.T) {
	api := &fakeAPI{groups: map[string][]string{"acct-sam": {"Developers", "Ops"}}, envs: map[string]fakeEnv{}}
	_, h := setup(t, api)

	sa := map[string]any{
		"type": "group_membership.added",
		"data": map[string]any{"entity": map[string]any{
			"service_account_id": "c3e9a7b2", "service_account_name": "ci-bot", "group_name": "Developers",
		}},
	}
	post(t, h, "evt-1", sa)
	post(t, h, "evt-2", membershipEvent("group_membership.added", "Ops"))
	post(t, h, "evt-3", map[string]any{"type": "project.created", "data": map[string]any{}})

	if len(api.calls) != 0 {
		t.Fatalf("calls = %v, want none", ops(api.calls))
	}
}

func TestLeaveDecommissionsDeployedAndDeletesEmptySandboxes(t *testing.T) {
	owner := map[string]any{"owner": "Sam.Lee@example.com", "sandbox": "true"}
	api := &fakeAPI{
		groups: map[string][]string{"acct-sam": {"Data"}},
		envs: map[string]fakeEnv{
			"api-samlee":  {attrs: owner, parent: "api-staging", statuses: []string{"PROVISIONED", "INITIALIZED"}},
			"api-samlee2": {attrs: owner, parent: "api-staging", statuses: []string{"INITIALIZED", "DECOMMISSIONED"}},
			"etl-samlee":  {attrs: owner, parent: "etl-dev", statuses: []string{"PROVISIONED"}},
		},
	}
	_, h := setup(t, api)

	post(t, h, "evt-1", membershipEvent("group_membership.removed", "Developers"))

	got := ops(api.calls)
	if len(got) != 2 {
		t.Fatalf("calls = %v", got)
	}
	want := map[string]bool{"DecommissionEnvironment api-samlee": true, "DeleteEnvironment api-samlee2": true}
	for _, c := range got {
		if !want[c] {
			t.Errorf("unexpected call %q in %v", c, got)
		}
	}
}

func TestLeaveKeepsSandboxWhenAnotherGroupGivesTheTemplate(t *testing.T) {
	api := &fakeAPI{
		groups: map[string][]string{"acct-sam": {"Contractors"}},
		envs: map[string]fakeEnv{
			"api-samlee": {attrs: map[string]any{"owner": "Sam.Lee@example.com", "sandbox": "true"}, parent: "api-staging", statuses: []string{"PROVISIONED"}},
		},
	}
	_, h := setup(t, api)

	post(t, h, "evt-1", membershipEvent("group_membership.removed", "Developers"))

	if len(api.calls) != 0 {
		t.Fatalf("calls = %v, want none", ops(api.calls))
	}
}

func TestDecommissionCompletedDeletesTornDownSandbox(t *testing.T) {
	owner := map[string]any{"owner": "Sam.Lee@example.com", "sandbox": "true"}
	cases := []struct {
		name    string
		env     fakeEnv
		groups  []string
		members map[string]string
		want    []string
	}{
		{"owner left the organization", fakeEnv{attrs: owner, parent: "api-staging", statuses: []string{"DECOMMISSIONED", "INITIALIZED"}, deletable: true},
			nil, map[string]string{}, []string{"DeleteEnvironment api-samlee"}},
		{"owner left the group", fakeEnv{attrs: owner, parent: "api-staging", statuses: []string{"DECOMMISSIONED"}, deletable: true},
			[]string{"Data"}, map[string]string{"Sam.Lee@example.com": "acct-sam"}, []string{"DeleteEnvironment api-samlee"}},
		{"owner still in group", fakeEnv{attrs: owner, parent: "api-staging", statuses: []string{"DECOMMISSIONED"}, deletable: true},
			[]string{"Developers"}, map[string]string{"Sam.Lee@example.com": "acct-sam"}, []string{}},
		{"instance still provisioned", fakeEnv{attrs: owner, parent: "api-staging", statuses: []string{"DECOMMISSIONED", "PROVISIONED"}, deletable: true},
			nil, map[string]string{}, []string{}},
		{"not deletable", fakeEnv{attrs: owner, parent: "api-staging", statuses: []string{"DECOMMISSIONED"}, deletable: false},
			nil, map[string]string{}, []string{}},
		{"template without delete", fakeEnv{attrs: owner, parent: "etl-dev", statuses: []string{"DECOMMISSIONED"}, deletable: true},
			nil, map[string]string{}, []string{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			api := &fakeAPI{
				groups:  map[string][]string{"acct-sam": tc.groups},
				members: tc.members,
				envs:    map[string]fakeEnv{"api-samlee": tc.env},
			}
			_, h := setup(t, api)

			post(t, h, "evt-1", decommissionedEvent("api", "samlee"))

			if got := ops(api.calls); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("calls = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestHandleRejectsBadSignatureAndDedupes(t *testing.T) {
	api := &fakeAPI{groups: map[string][]string{"acct-sam": {"Developers"}}, envs: map[string]fakeEnv{}}
	_, h := setup(t, api)

	body, _ := json.Marshal(membershipEvent("group_membership.added", "Developers"))
	req := httptest.NewRequest(http.MethodPost, automation.HookPath, bytes.NewReader(body))
	automation.SignRequest(req, []byte("wrong-secret-wrong-secret-wrong!"), "evt-1", body, time.Now())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("bad signature status = %d", rec.Code)
	}

	post(t, h, "evt-2", membershipEvent("group_membership.added", "Developers"))
	post(t, h, "evt-2", membershipEvent("group_membership.added", "Developers"))
	if got := ops(api.calls); !reflect.DeepEqual(got, []string{"ForkEnvironment api-staging samlee", "DeployEnvironment api-samlee"}) {
		t.Fatalf("calls = %v, want one fork and one deploy", got)
	}
}

func TestBaseIdentifier(t *testing.T) {
	cases := map[string]string{
		"Sam.Lee@example.com":                   "samlee",
		"jean-luc_o'brien+test@example.com":     "jeanlucobrientest",
		"averyveryveryverylongname@example.com": "averyveryveryverylon",
		"__@example.com":                        "sandbox",
	}
	for email, want := range cases {
		if got := baseIdentifier(email); got != want {
			t.Errorf("baseIdentifier(%q) = %q, want %q", email, got, want)
		}
	}
	if got := candidate("averyveryveryverylon", 12); got != "averyveryveryveryl12" {
		t.Errorf("candidate = %q", got)
	}
}

func TestParseConfig(t *testing.T) {
	got, err := ParseConfig([]byte(`
sandboxes:
  - group: Developers
    project: api
    environment: staging
    deploy: true
    deleteAfterDecommission: true
    copyEnvironmentDefaults: true
`))
	if err != nil {
		t.Fatal(err)
	}
	want := []Rule{{Group: "Developers", Project: "api", Environment: "staging", Deploy: true, DeleteAfterDecommission: true, CopyEnvironmentDefaults: true}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("rules = %+v", got)
	}

	if _, err := ParseConfig([]byte("sandboxes:\n  - group: Developers\n    project: api\n")); err == nil {
		t.Error("missing environment: want error")
	}
	if _, err := ParseConfig([]byte("sandboxes:\n  - group: Developers\n    project: api\n    environment: staging\n    deploys: true\n")); err == nil {
		t.Error("unknown key: want error")
	}
}

func TestExampleConfigLoads(t *testing.T) {
	var cfg Config
	if err := automation.LoadConfigFile("config.example.yaml", &cfg); err != nil {
		t.Fatal(err)
	}
	if len(cfg.Sandboxes) != 1 || !cfg.Sandboxes[0].DeleteAfterDecommission {
		t.Fatalf("config = %+v", cfg)
	}
}

func TestNoConfigFileRefusesStart(t *testing.T) {
	t.Setenv("CONFIG_FILE", filepath.Join(t.TempDir(), "missing.yaml"))
	_, err := Definition.New()
	var ce *automation.ConfigError
	if !errors.As(err, &ce) || !strings.HasSuffix(err.Error(), "sandboxes: at least one rule is required") {
		t.Fatalf("err = %v", err)
	}
}
