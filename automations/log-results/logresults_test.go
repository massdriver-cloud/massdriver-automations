package logresults

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
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

func exampleConfig(t *testing.T) *Config {
	t.Helper()
	var cfg Config
	if err := automation.LoadConfigFile("config.example.yaml", &cfg); err != nil {
		t.Fatal(err)
	}
	return &cfg
}

func fixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func parse(t *testing.T, cfg *Config, logs string, d Deployment) map[string]any {
	t.Helper()
	entries, _, err := cfg.Parse(t.Context(), strings.NewReader(logs), d)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]any{}
	for _, e := range entries {
		if e.Format == FormatText {
			out[e.Key] = e.Value
			continue
		}
		var v any
		if err := json.Unmarshal([]byte(e.Value), &v); err != nil {
			t.Fatalf("%s: %v", e.Key, err)
		}
		out[e.Key] = v
	}
	return out
}

var prod = map[string]any{"md-project": "api", "md-environment": "prod", "md-instance": "api-prod-storage"}

func TestExampleRulesReadProvisionerLogs(t *testing.T) {
	cfg := exampleConfig(t)
	for _, tc := range []struct {
		fixture string
		d       Deployment
		want    map[string]any
	}{
		{"opentofu-apply.log", Deployment{Action: "PROVISION", Attributes: prod}, map[string]any{
			"results/v1/step/src": map[string]any{"tool": "opentofu", "action": "apply", "status": "succeeded",
				"message": "Apply complete! Resources: 2 added, 1 changed, 0 destroyed."},
			"results/v1/changes/src":    map[string]any{"tool": "opentofu", "add": 2.0, "change": 1.0, "destroy": 0.0},
			"results/v1/compliance/src": map[string]any{"tool": "checkov", "tool_version": "3.2.469", "passed": 36.0, "failed": 2.0, "skipped": 4.0},
		}},
		{"opentofu-plan-no-changes.log", Deployment{Action: "PLAN", Attributes: prod}, map[string]any{
			"results/v1/step/src": map[string]any{"tool": "opentofu", "tool_version": "1.10.6", "action": "plan",
				"status": "succeeded", "message": "No changes."},
			"results/v1/changes/src":    map[string]any{"tool": "opentofu", "add": 0.0, "change": 0.0, "destroy": 0.0},
			"results/v1/plan_summary":   "No changes.",
			"results/v1/compliance/src": map[string]any{"tool": "checkov", "tool_version": "3.2.469", "passed": 38.0, "failed": 0.0, "skipped": 4.0},
		}},
		{"terraform-failed.log", Deployment{Action: "PROVISION", Failed: true, Attributes: prod}, map[string]any{
			"results/v1/step/src": map[string]any{"tool": "terraform", "action": "apply", "status": "failed",
				"message": "modifying RDS DB Instance (api-prod-database): InvalidParameterCombination: RDS does not support " +
					"creating a DB instance with the following combination: DBInstanceClass=db.r6g.large, Engine=postgres, EngineVersion=12.19"},
			"results/v1/changes/src": map[string]any{"tool": "terraform", "add": 0.0, "change": 1.0, "destroy": 0.0},
		}},
		{"helm-upgrade.log", Deployment{Action: "PROVISION", Attributes: prod}, map[string]any{
			"results/v1/step/src": map[string]any{"tool": "helm", "action": "apply", "status": "succeeded",
				"release": "api-prod-cache", "namespace": "api-prod", "revision": 4.0, "chart_version": "20.6.2",
				"message": "api-prod-cache revision 4 deployed"},
		}},
		{"helm-local.log", Deployment{Action: "PROVISION", Attributes: prod}, map[string]any{
			"results/v1/step/src": map[string]any{"tool": "helm", "action": "apply", "status": "succeeded",
				"release": "api-prod-app", "namespace": "api-prod", "revision": 1.0, "message": "api-prod-app revision 1 deployed"},
			"results/v1/compliance/src": map[string]any{"tool": "checkov", "tool_version": "3.2.469", "passed": 61.0, "failed": 3.0, "skipped": 0.0},
		}},
		{"bicep-stack.log", Deployment{Action: "PROVISION", Attributes: prod}, map[string]any{
			"results/v1/step/src": map[string]any{"tool": "bicep", "action": "apply", "status": "succeeded",
				"stack": "api-prod-storage-src", "provisioning_state": "succeeded",
				"message": "Stack api-prod-storage-src deployed successfully."},
			"results/v1/compliance/src": map[string]any{"tool": "checkov", "tool_version": "3.2.469", "passed": 18.0, "failed": 1.0, "skipped": 0.0},
		}},
		{"bicep-what-if.log", Deployment{Action: "PLAN", Attributes: prod}, map[string]any{
			"results/v1/changes/src": map[string]any{"tool": "bicep", "add": 1.0, "change": 1.0, "destroy": 1.0, "no_op": 1.0},
		}},
	} {
		t.Run(tc.fixture, func(t *testing.T) {
			got := parse(t, cfg, fixture(t, tc.fixture), tc.d)
			if !reflect.DeepEqual(got, tc.want) {
				g, _ := json.MarshalIndent(got, "", "  ")
				w, _ := json.MarshalIndent(tc.want, "", "  ")
				t.Fatalf("got\n%s\nwant\n%s", g, w)
			}
		})
	}
}

func TestNumbersStayNumbers(t *testing.T) {
	cfg := exampleConfig(t)
	entries, _, err := cfg.Parse(t.Context(), strings.NewReader(fixture(t, "opentofu-apply.log")),
		Deployment{Action: "PROVISION", Attributes: prod})
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Key == "results/v1/changes/src" && e.Value != `{"add":2,"change":1,"destroy":0,"tool":"opentofu"}` {
			t.Fatalf("value = %s", e.Value)
		}
	}
}

func mustParse(t *testing.T, src string) *Config {
	t.Helper()
	cfg, err := ParseConfig([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestModes(t *testing.T) {
	logs := "count 2 v1.10\ncount 3 v1.11\ncount 5 v1.12\n"
	for mode, want := range map[string]string{
		"first": `{"n":2,"version":"1.10"}`,
		"last":  `{"n":5,"version":"1.12"}`,
		"sum":   `{"n":10,"version":"1.12"}`,
	} {
		cfg := mustParse(t, "rules:\n  - name: r\n    patterns: ['count (?P<n>\\d+) v(?P<version>[0-9.]+)']\n    key: k\n    mode: "+mode+"\n")
		entries, _, err := cfg.Parse(t.Context(), strings.NewReader(logs), Deployment{Action: "PROVISION"})
		if err != nil || len(entries) != 1 || entries[0].Value != want {
			t.Fatalf("%s: entries=%v err=%v, want %s", mode, entries, err, want)
		}
	}
}

func TestValueVariables(t *testing.T) {
	cfg := mustParse(t, `
rules:
  - name: r
    patterns: ['^ok']
    key: k
    value: '{status: $status, action: $action, matches: $matches, id: $deployment_id, env: $attributes["md-environment"]}'
  - name: t
    patterns: ['^ok (?P<word>\w+)']
    key: t
    format: text
`)
	entries, _, err := cfg.Parse(t.Context(), strings.NewReader("ok go\nok fine\nnope\n"),
		Deployment{ID: "dep-1", Action: "DECOMMISSION", Failed: true, Attributes: prod})
	if err != nil {
		t.Fatal(err)
	}
	want := []Entry{
		{Rule: "r", Key: "results/v1/k", Label: "r", Format: "json",
			Value: `{"action":"destroy","env":"prod","id":"dep-1","matches":2,"status":"failed"}`},
		{Rule: "t", Key: "results/v1/t", Label: "t", Format: "text", Value: "fine"},
	}
	if !reflect.DeepEqual(entries, want) {
		t.Fatalf("got %+v", entries)
	}
}

func TestRulesThatDoNotApply(t *testing.T) {
	cfg := mustParse(t, `
actions: [PROVISION, PLAN]
rules:
  - name: prod-only
    match: {md-environment: [staging, prod]}
    patterns: ['x']
    key: a
  - name: plan-only
    actions: [PLAN]
    patterns: ['x']
    key: b
  - name: needs-helm
    detect: 'Happy Helming'
    patterns: ['x']
    key: c
  - name: never-matches
    patterns: ['y']
    key: d
  - name: bad-value
    patterns: ['x']
    key: e
    value: 'error("from the logs")'
`)
	entries, skips, err := cfg.Parse(t.Context(), strings.NewReader("x\n"),
		Deployment{Action: "PROVISION", Attributes: map[string]any{"md-environment": "dev"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("entries = %+v", entries)
	}
	got := map[string]string{}
	for _, s := range skips {
		got[s.Rule] = s.Reason
	}
	want := map[string]string{"prod-only": SkipNotApplicable, "plan-only": SkipNotApplicable,
		"needs-helm": SkipNotDetected, "never-matches": SkipNoMatch, "bad-value": SkipValueFailed}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("skips = %v", got)
	}
}

func TestLimits(t *testing.T) {
	cfg := mustParse(t, "max_matches: 3\nrules:\n  - name: r\n    patterns: ['n (?P<n>\\d+)']\n    key: k\n    mode: sum\n"+
		"  - name: long\n    patterns: ['^(?P<line>z+)$']\n    key: l\n    format: text\n")
	var b strings.Builder
	for range 10 {
		b.WriteString("n 1\n")
	}
	b.WriteString(strings.Repeat("z", maxLineBytes*3) + "\n")
	b.WriteString("after the long line\n")
	entries, _, err := cfg.Parse(t.Context(), strings.NewReader(b.String()), Deployment{Action: "PROVISION"})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || entries[0].Value != `{"n":3}` || len(entries[1].Value) != maxCaptureBytes {
		t.Fatalf("entries = %d, first = %s", len(entries), entries[0].Value)
	}
}

func TestInvalidConfigNamesTheFieldNotTheValue(t *testing.T) {
	for _, tc := range []struct{ yaml, want string }{
		{"rules: []", "rules: at least one rule is required"},
		{"rules:\n  - name: r\n    patterns: ['(?P<secretpattern>']\n    key: k", "rules[0].patterns[0] is not a valid regular expression"},
		{"rules:\n  - name: r\n    detect: '(secretdetect'\n    patterns: [x]\n    key: k", "rules[0].detect is not a valid regular expression"},
		{"rules:\n  - name: r\n    patterns: ['(?P<a>x)(?P<b>y)']\n    key: k\n    format: text", "rules[0].value is required for a text rule"},
		{"rules:\n  - name: r\n    patterns: [x]\n    key: 'a b'", "rules[0].key may contain only"},
		{"rules:\n  - name: r\n    patterns: [x]\n    key: k\n    value: '{secretvalue'", "rules[0].value does not compile as jq"},
		{"rules:\n  - name: r\n    patterns: [x]\n    key: k\n    actions: [DECOMMISSION]", "rules[0].actions: DECOMMISSION is not in the top-level actions"},
		{"actions: [DEPLOY]\nrules:\n  - name: r\n    patterns: [x]\n    key: k", "actions: use PROVISION, PLAN, or DECOMMISSION"},
		{"rules:\n  - name: r\n    patterns: [x]\n    key: k\n    mode: max", "rules[0].mode must be last, first, or sum"},
		{"rules:\n  - name: r\n    patterns: [x]\n    key: k\n  - name: r\n    patterns: [x]\n    key: k", "rules[1].name is used by an earlier rule"},
	} {
		_, err := ParseConfig([]byte(tc.yaml))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%q: err = %v, want %q", tc.yaml, err, tc.want)
			continue
		}
		if strings.Contains(err.Error(), "secret") {
			t.Errorf("%q: the error quotes the config: %v", tc.yaml, err)
		}
	}
}

func TestValueCannotReadTheEnvironment(t *testing.T) {
	t.Setenv("MASSDRIVER_API_KEY", "md_secret")
	cfg := mustParse(t, "rules:\n  - name: r\n    patterns: [x]\n    key: k\n    value: '[env.MASSDRIVER_API_KEY, $ENV.MASSDRIVER_API_KEY]'\n")
	entries, _, err := cfg.Parse(t.Context(), strings.NewReader("x\n"), Deployment{Action: "PROVISION"})
	if err != nil || len(entries) != 1 || strings.Contains(entries[0].Value, "md_secret") {
		t.Fatalf("entries = %+v err = %v", entries, err)
	}
}

var yamlBlock = regexp.MustCompile("(?s)```yaml\n(.*?)```")

func TestReadmeConfigsAreValid(t *testing.T) {
	b, err := os.ReadFile("README.md")
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, m := range yamlBlock.FindAllStringSubmatch(string(b), -1) {
		if !strings.Contains(m[1], "rules:") {
			continue
		}
		n++
		if _, err := ParseConfig([]byte(m[1])); err != nil {
			t.Errorf("README block %d: %v\n%s", n, err, m[1])
		}
	}
	if n < 3 {
		t.Fatalf("found %d configs in the README, want one for each provisioner", n)
	}
}

func TestTestCommand(t *testing.T) {
	var out bytes.Buffer
	err := Test([]string{"-config", "config.example.yaml", "-action", "plan", "-attr", "md-environment=prod",
		filepath.Join("testdata", "opentofu-plan-no-changes.log")}, nil, &out)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(strings.Fields(out.String()), " ")
	for _, want := range []string{
		`opentofu-changes results/v1/changes/src json OpenTofu changes {"add":0,"change":0,"destroy":0,"tool":"opentofu"}`,
		"opentofu-plan-summary results/v1/plan_summary text Plan No changes.",
		"helm-step - - - skipped: detect did not match",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}

	out.Reset()
	err = Test([]string{"-config", "config.example.yaml"}, strings.NewReader(fixture(t, "helm-upgrade.log")), &out)
	if err != nil || !strings.Contains(out.String(), `"revision":4`) {
		t.Fatalf("stdin: err=%v\n%s", err, out.String())
	}
}

// fakeAPI serves deployment logs and records stash writes.
type fakeAPI struct {
	mu       sync.Mutex
	logs     map[string]string
	logsErr  string
	rejected string
	writes   []map[string]any
}

var opName = regexp.MustCompile(`(?:query|mutation) (\w+)`)

func (f *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Query     string         `json:"query"`
		Variables map[string]any `json:"variables"`
	}
	b, _ := io.ReadAll(r.Body)
	_ = json.Unmarshal(b, &req)
	f.mu.Lock()
	defer f.mu.Unlock()
	switch opName.FindStringSubmatch(req.Query)[1] {
	case "DeploymentLogs":
		if f.logsErr != "" {
			_ = json.NewEncoder(w).Encode(map[string]any{"errors": []any{map[string]any{"message": f.logsErr}}})
			return
		}
		logs := []any{}
		if l, ok := f.logs[req.Variables["id"].(string)]; ok {
			// The API returns logs in batches, as the provisioner flushed them.
			for _, batch := range strings.SplitAfter(l, "\n\n") {
				logs = append(logs, map[string]any{"message": strings.TrimSuffix(batch, "\n")})
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"deployment": map[string]any{"logs": logs}}})
	case "SetStashEntry":
		f.writes = append(f.writes, req.Variables)
		ok := req.Variables["key"] != f.rejected
		msgs := []any{}
		if !ok {
			msgs = append(msgs, map[string]any{"field": "key", "message": "is reserved"})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"alphaSetStashEntry": map[string]any{
			"successful": ok, "messages": msgs, "result": map[string]any{"key": req.Variables["key"]},
		}}})
	}
}

func (f *fakeAPI) written() []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]map[string]any(nil), f.writes...)
}

func newHandler(t *testing.T, f *fakeAPI) http.Handler {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	api := &massdriver.Client{Endpoint: srv.URL, OrganizationID: "acme", APIKey: "md_test"}
	return automation.Handler(New(api, exampleConfig(t)), secret)
}

func send(t *testing.T, h http.Handler, id, typ, action string) int {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"id":   id,
		"type": typ,
		"data": map[string]any{
			"entity":     map[string]any{"id": "dep-1"},
			"attributes": prod,
			"metadata":   map[string]any{"deployment_action": action},
		},
	})
	req := httptest.NewRequest(http.MethodPost, automation.HookPath, bytes.NewReader(body))
	automation.SignRequest(req, secret, id, body, time.Now())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code
}

func TestHandleWritesTheSameEntriesForTheSameDeployment(t *testing.T) {
	f := &fakeAPI{logs: map[string]string{"dep-1": fixture(t, "opentofu-apply.log")}}
	h := newHandler(t, f)
	if code := send(t, h, "evt-1", "deployment.completed", "PROVISION"); code != http.StatusNoContent {
		t.Fatalf("status = %d", code)
	}
	first := f.written()
	if len(first) != 3 {
		t.Fatalf("writes = %d, want 3", len(first))
	}
	w := first[1]
	if !reflect.DeepEqual(w["scope"], map[string]any{"deployment": "dep-1"}) || w["key"] != "results/v1/changes/src" ||
		!reflect.DeepEqual(w["input"], map[string]any{"format": "JSON", "label": "OpenTofu changes",
			"value": `{"add":2,"change":1,"destroy":0,"tool":"opentofu"}`}) {
		t.Fatalf("write = %v", w)
	}

	// Massdriver resends with the same id, and a second destination can send the same deployment with another.
	if code := send(t, h, "evt-2", "deployment.completed", "PROVISION"); code != http.StatusNoContent {
		t.Fatalf("status = %d", code)
	}
	if again := f.written()[3:]; !reflect.DeepEqual(again, first) {
		t.Fatalf("second delivery wrote\n%v\nwant\n%v", again, first)
	}
}

func TestHandleSkipsActionsAndEventsItDoesNotRead(t *testing.T) {
	f := &fakeAPI{logs: map[string]string{"dep-1": fixture(t, "opentofu-apply.log")}}
	h := newHandler(t, f)
	send(t, h, "evt-1", "deployment.completed", "DECOMMISSION")
	send(t, h, "evt-2", "deployment.aborted", "PROVISION")
	if w := f.written(); len(w) != 0 {
		t.Fatalf("writes = %v", w)
	}
}

func TestHandleAsksForTheEventAgainWhenLogsAreMissing(t *testing.T) {
	f := &fakeAPI{logs: map[string]string{}}
	h := newHandler(t, f)
	if code := send(t, h, "evt-1", "deployment.failed", "PROVISION"); code != http.StatusInternalServerError {
		t.Fatalf("no logs: status = %d, want 500", code)
	}
	f.logsErr = "temporarily unavailable"
	if code := send(t, h, "evt-2", "deployment.failed", "PROVISION"); code != http.StatusInternalServerError {
		t.Fatalf("API error: status = %d, want 500", code)
	}
	if w := f.written(); len(w) != 0 {
		t.Fatalf("writes = %v", w)
	}
}

func TestHandleKeepsGoingAfterAFailedWrite(t *testing.T) {
	f := &fakeAPI{logs: map[string]string{"dep-1": fixture(t, "opentofu-apply.log")}, rejected: "results/v1/step/src"}
	h := newHandler(t, f)
	if code := send(t, h, "evt-1", "deployment.completed", "PROVISION"); code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", code)
	}
	if w := f.written(); len(w) != 3 {
		t.Fatalf("writes = %d, want 3", len(w))
	}
}
