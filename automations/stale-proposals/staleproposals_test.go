package staleproposals

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/massdriver-cloud/massdriver-automations/pkg/automation"
	"github.com/massdriver-cloud/massdriver-automations/pkg/massdriver"
	"github.com/massdriver-cloud/massdriver-automations/pkg/slack/slacktest"
)

var now = time.Date(2026, 11, 27, 9, 0, 0, 0, time.UTC)

type fakeProposal struct {
	id, project, env, component, by, message string
	age                                      time.Duration
	attrs                                    map[string]string
}

type fakeAPI struct {
	mu        sync.Mutex
	proposals []fakeProposal
	pageSize  int
	reject    map[string]string
	rejected  []string
	pages     int
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
	var data any
	switch op {
	case "ProposedDeployments":
		f.pages++
		start := 0
		cur := req.Variables["cursor"].(map[string]any)
		if n, ok := cur["next"].(string); ok {
			start = int(n[0] - '0')
		}
		size := f.pageSize
		if size == 0 {
			size = len(f.proposals)
		}
		end := min(start+size, len(f.proposals))
		items := []any{}
		for _, p := range f.proposals[start:end] {
			attrs := map[string]any{"md-project": p.project, "md-environment": p.env, "md-component": p.component,
				"md-instance": p.project + "-" + p.env + "-" + p.component}
			for k, v := range p.attrs {
				attrs[k] = v
			}
			items = append(items, map[string]any{"id": p.id, "createdAt": now.Add(-p.age).Format(time.RFC3339),
				"message": p.message, "deployedBy": p.by, "effectiveAttributes": attrs})
		}
		var next any
		if end < len(f.proposals) {
			next = string(rune('0' + end))
		}
		data = map[string]any{"deployments": map[string]any{"cursor": map[string]any{"next": next}, "items": items}}
	case "RejectDeployment":
		id := req.Variables["id"].(string)
		if msg, ok := f.reject[id]; ok {
			data = map[string]any{"rejectDeployment": map[string]any{"successful": false, "result": nil,
				"messages": []any{map[string]any{"field": "id", "message": msg}}}}
			break
		}
		f.rejected = append(f.rejected, id)
		data = map[string]any{"rejectDeployment": map[string]any{"successful": true, "messages": []any{}, "result": map[string]any{"id": id}}}
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
}

type env struct {
	api   *fakeAPI
	slack *slacktest.Server
	s     *Stale
}

func setup(t *testing.T, cfgYAML string, proposals ...fakeProposal) *env {
	t.Helper()
	cfg, err := ParseConfig([]byte(cfgYAML))
	if err != nil {
		t.Fatal(err)
	}
	api := &fakeAPI{proposals: proposals, reject: map[string]string{}}
	srv := httptest.NewServer(api)
	t.Cleanup(srv.Close)
	sl := slacktest.NewServer(t)
	s := New(&massdriver.Client{Endpoint: srv.URL, OrganizationID: "acme", APIKey: "md_test"}, sl.Bot(), cfg, "run-token")
	s.now = func() time.Time { return now }
	s.run = func(f func()) { f() }
	return &env{api: api, slack: sl, s: s}
}

const base = `
slack:
  channel: "#proposals"
`

func TestDigestListsStaleProposalsByEnvironment(t *testing.T) {
	e := setup(t, base,
		fakeProposal{id: "d1", project: "api", env: "prod", component: "db", by: "sam@acme.com", message: "Bump storage\nmore", age: 50 * time.Hour},
		fakeProposal{id: "d2", project: "api", env: "prod", component: "cache", by: "lee@acme.com", age: 30 * time.Hour},
		fakeProposal{id: "d3", project: "api", env: "dev", component: "db", age: 25 * time.Hour},
		fakeProposal{id: "d4", project: "web", env: "prod", component: "cdn", age: 2 * time.Hour},
	)
	e.api.pageSize = 2
	if err := e.s.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if e.api.pages != 2 {
		t.Errorf("pages = %d, want 2", e.api.pages)
	}
	texts := e.slack.Texts()
	if len(texts) != 1 {
		t.Fatalf("slack = %q", texts)
	}
	link := func(env, comp, id string) string {
		return "<https://app.massdriver.cloud/orgs/acme/projects/api/environments/" + env + "/instances/" + comp +
			"?deployment=" + id + "&tab=history|api-" + env + "-" + comp + ">"
	}
	want := "3 proposed deployments have waited more than 1d for approval.\n\n" +
		"*api / dev*\n" +
		"- " + link("dev", "db", "d3") + " waiting 1d 1h\n\n" +
		"*api / prod*\n" +
		"- " + link("prod", "db", "d1") + " waiting 2d 2h, proposed by sam@acme.com: \"Bump storage\"\n" +
		"- " + link("prod", "cache", "d2") + " waiting 1d 6h, proposed by lee@acme.com"
	if texts[0] != want {
		t.Errorf("digest =\n%s\nwant\n%s", texts[0], want)
	}
}

func TestNothingStalePostsNothing(t *testing.T) {
	e := setup(t, base, fakeProposal{id: "d1", project: "api", env: "prod", component: "db", age: time.Hour})
	if err := e.s.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if texts := e.slack.Texts(); len(texts) != 0 {
		t.Errorf("slack = %q", texts)
	}
}

func TestMatchExcludeAndGroupByProject(t *testing.T) {
	e := setup(t, base+`
remind_after: 2h
group_by: project
match:
  team: [payments, billing]
exclude:
  md-environment: sandbox
`,
		fakeProposal{id: "d1", project: "api", env: "prod", component: "db", age: 3 * time.Hour, attrs: map[string]string{"team": "payments"}},
		fakeProposal{id: "d2", project: "api", env: "sandbox", component: "db", age: 3 * time.Hour, attrs: map[string]string{"team": "payments"}},
		fakeProposal{id: "d3", project: "api", env: "prod", component: "web", age: 3 * time.Hour, attrs: map[string]string{"team": "web"}},
		fakeProposal{id: "d4", project: "api", env: "prod", component: "cache", age: 3 * time.Hour},
	)
	if err := e.s.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	texts := e.slack.Texts()
	if len(texts) != 1 || !strings.HasPrefix(texts[0], "1 proposed deployment has waited more than 2h for approval.\n\n*api*\n") ||
		!strings.Contains(texts[0], "deployment=d1") || strings.Contains(texts[0], "deployment=d2") {
		t.Errorf("slack = %q", texts)
	}
}

func TestMentionsFollowMatchingRules(t *testing.T) {
	e := setup(t, base+`
mentions:
  - match: {md-environment: prod}
    mentions: ["<!subteam^S1>"]
  - match: {team: payments}
    mentions: ["<@U1>", "<!subteam^S1>"]
`,
		fakeProposal{id: "d1", project: "api", env: "prod", component: "db", age: 30 * time.Hour, attrs: map[string]string{"team": "payments"}},
		fakeProposal{id: "d2", project: "api", env: "dev", component: "db", age: 30 * time.Hour},
	)
	if err := e.s.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	text := e.slack.Texts()[0]
	lines := strings.Split(text, "\n")
	for _, l := range lines {
		switch {
		case strings.Contains(l, "deployment=d1"):
			if !strings.HasSuffix(l, " <!subteam^S1> <@U1>") {
				t.Errorf("d1 line = %q", l)
			}
		case strings.Contains(l, "deployment=d2"):
			if strings.Contains(l, "<@") || strings.Contains(l, "<!") {
				t.Errorf("d2 line = %q", l)
			}
		}
	}
}

func TestRejectAfter(t *testing.T) {
	e := setup(t, base+"reject_after: 7d\n",
		fakeProposal{id: "d1", project: "api", env: "prod", component: "db", age: 8 * 24 * time.Hour},
		fakeProposal{id: "d2", project: "api", env: "prod", component: "cache", age: 9 * 24 * time.Hour},
		fakeProposal{id: "d3", project: "api", env: "prod", component: "web", age: 2 * 24 * time.Hour},
	)
	e.api.reject["d2"] = "deployment is not PROPOSED"
	if err := e.s.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(e.api.rejected) != 1 || e.api.rejected[0] != "d1" {
		t.Errorf("rejected = %v", e.api.rejected)
	}
	text := e.slack.Texts()[0]
	if !strings.HasPrefix(text, "1 proposed deployment has waited more than 1d for approval.") {
		t.Errorf("digest = %q", text)
	}
	_, rejected, ok := strings.Cut(text, "\n*Rejected: waited more than 7d*\n")
	if !ok || !strings.Contains(rejected, "deployment=d1") ||
		!strings.Contains(rejected, "waited 9d Not rejected: id: deployment is not PROPOSED") {
		t.Errorf("digest = %q", text)
	}
}

func TestDryRunRejectsNothing(t *testing.T) {
	e := setup(t, base+"reject_after: 7d\ndry_run: true\n",
		fakeProposal{id: "d1", project: "api", env: "prod", component: "db", age: 8 * 24 * time.Hour})
	if err := e.s.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(e.api.rejected) != 0 {
		t.Errorf("rejected = %v", e.api.rejected)
	}
	if text := e.slack.Texts()[0]; !strings.HasPrefix(text, "*Would reject: waited more than 7d (dry run)*\n- ") {
		t.Errorf("digest = %q", text)
	}
}

func TestRunRoute(t *testing.T) {
	e := setup(t, base, fakeProposal{id: "d1", project: "api", env: "prod", component: "db", age: 30 * time.Hour})
	h := automation.Handler(e.s, []byte("0123456789abcdef0123456789abcdef"))
	post := func(token string) int {
		req := httptest.NewRequest(http.MethodPost, "/stale-proposals/run", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	if code := post("wrong"); code != http.StatusUnauthorized {
		t.Errorf("wrong token: %d", code)
	}
	if code := post("run-token"); code != http.StatusAccepted {
		t.Fatalf("run: %d", code)
	}
	if len(e.slack.Texts()) != 1 {
		t.Errorf("slack = %q", e.slack.Texts())
	}
}

func TestConfig(t *testing.T) {
	for name, doc := range map[string]string{
		"bad duration":         "remind_after: soon\n",
		"negative duration":    "remind_after: -1h\n",
		"reject before remind": "remind_after: 2d\nreject_after: 1d\n",
		"bad group":            "group_by: team\n",
		"bad schedule":         "schedule: daily\n",
		"bad timezone":         "timezone: Moon/Base\n",
		"empty match value":    "match: {a: []}\n",
		"mention without any":  "mentions: [{match: {a: b}}]\n",
	} {
		if _, err := ParseConfig([]byte(doc)); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	cfg, err := ParseConfig(nil)
	if err != nil {
		t.Fatal(err)
	}
	if time.Duration(cfg.RemindAfter) != 24*time.Hour || cfg.RejectAfter != 0 || cfg.GroupBy != GroupEnvironment {
		t.Errorf("defaults = %+v", cfg)
	}
	cfg, err = ParseConfig([]byte("remind_after: 36h\nreject_after: 1.5d\n"))
	if err != nil {
		t.Fatal(err)
	}
	if time.Duration(cfg.RejectAfter) != 36*time.Hour {
		t.Errorf("reject_after = %v", time.Duration(cfg.RejectAfter))
	}
}

func TestBundleConfigWithNullsLoads(t *testing.T) {
	cfg, err := ParseConfig([]byte(`"dry_run": null
"exclude": {}
"group_by": null
"massdriver": null
"match":
  "team":
  - "a"
"mentions":
- "match": {}
  "mentions":
  - "<@U1>"
"reject_after": null
"remind_after": ""
"schedule": null
"slack": null
"timezone": null
`))
	if err != nil {
		t.Fatal(err)
	}
	if time.Duration(cfg.RemindAfter) != 24*time.Hour || cfg.RejectAfter != 0 || len(cfg.Mentions) != 1 {
		t.Errorf("config = %+v", cfg)
	}
}

func TestExampleConfigLoads(t *testing.T) {
	var cfg Config
	if err := automation.LoadConfigFile("config.example.yaml", &cfg); err != nil {
		t.Fatal(err)
	}
}
