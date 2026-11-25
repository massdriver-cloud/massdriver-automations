package releasenotes

import (
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
	slackpkg "github.com/massdriver-cloud/massdriver-automations/pkg/slack"
	"github.com/massdriver-cloud/massdriver-automations/pkg/slack/slacktest"
)

var secret = []byte("0123456789abcdef0123456789abcdef")

func publishedEvent(id, repo, at string) []byte {
	b, _ := json.Marshal(map[string]any{
		"id":      id,
		"type":    "repo.published",
		"subject": "mri:acme:repo:" + repo,
		"time":    at,
		"data": map[string]any{
			"entity":     map[string]any{"name": repo, "changelog": "stale repo-level changelog"},
			"attributes": map[string]any{"md-repo": repo, "md-repo-artifact-type": "bundle"},
			"metadata":   map[string]any{},
		},
	})
	return b
}

func inst(id, status, constraint, resolved string, deployed, upgrade any, env, project string) map[string]any {
	return map[string]any{
		"id": id, "status": status, "version": constraint, "resolvedVersion": resolved,
		"deployedVersion": deployed, "availableUpgrade": upgrade,
		"environment": map[string]any{"id": env, "project": map[string]any{"id": project}},
	}
}

type fakeAPI struct {
	mu        sync.Mutex
	tags      []map[string]any
	instances [][]any
	queries   []map[string]any
}

func (f *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Query     string         `json:"query"`
		Variables map[string]any `json:"variables"`
	}
	b, _ := io.ReadAll(r.Body)
	_ = json.Unmarshal(b, &req)
	f.mu.Lock()
	f.queries = append(f.queries, req.Variables)
	f.mu.Unlock()

	var data any
	if strings.Contains(req.Query, "ociRepo(") {
		data = map[string]any{"ociRepo": map[string]any{"tags": map[string]any{"items": f.tags}}}
	} else {
		page := 0
		if c, _ := req.Variables["cursor"].(map[string]any); c["next"] == "page-2" {
			page = 1
		}
		var next any
		if page == 0 && len(f.instances) > 1 {
			next = "page-2"
		}
		data = map[string]any{"instances": map[string]any{"cursor": map[string]any{"next": next}, "items": f.instances[page]}}
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
}

type hook struct {
	mu     sync.Mutex
	bodies []map[string]any
}

func (h *hook) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	h.mu.Lock()
	h.bodies = append(h.bodies, body)
	h.mu.Unlock()
	_, _ = w.Write([]byte("ok"))
}

// slackHook records the Slack posts as the text and blocks of each message.
type slackHook struct{ srv *slacktest.Server }

func (h slackHook) bodies() []map[string]any {
	out := []map[string]any{}
	for _, c := range h.srv.Calls() {
		out = append(out, map[string]any{"text": c.Text, "blocks": c.Blocks})
	}
	return out
}

func setup(t *testing.T, api *fakeAPI, includeDev bool) (http.Handler, slackHook, *hook) {
	t.Helper()
	apiSrv := httptest.NewServer(api)
	teams := &hook{}
	teamsSrv := httptest.NewServer(teams)
	t.Cleanup(func() { apiSrv.Close(); teamsSrv.Close() })
	sl := slacktest.NewServer(t)

	cfg := Config{IncludeDevelopment: includeDev, Slack: slackpkg.Config{Channel: "#releases"}, Teams: TeamsConfig{Enabled: true, webhookURL: teamsSrv.URL}}
	n := New(&massdriver.Client{Endpoint: apiSrv.URL, OrganizationID: "acme", APIKey: "md_test"}, sl.Bot(), cfg)
	n.run = func(f func()) { f() }
	return automation.Handler(n, secret), slackHook{sl}, teams
}

func post(t *testing.T, h http.Handler, id string, body []byte) int {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, automation.HookPath, strings.NewReader(string(body)))
	automation.SignRequest(req, secret, id, body, time.Now())
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w.Code
}

func TestAnnouncesVersionChangelogAndInstances(t *testing.T) {
	api := &fakeAPI{
		tags: []map[string]any{
			{"tag": "1.4.0", "createdAt": "2026-11-24T06:00:00Z", "changelog": "later version"},
			{"tag": "1.3.0", "createdAt": "2026-11-24T05:54:03Z", "changelog": "## Changes\n- **Faster** failover\n- See [docs](https://example.com)"},
		},
		instances: [][]any{
			{
				inst("api-prod-database", "PROVISIONED", "1.2.2", "1.2.2", "1.2.2", "1.3.0", "api-prod", "api"),
				inst("api-staging-database", "PROVISIONED", "~1", "1.3.0", "1.2.2", nil, "api-staging", "api"),
				inst("api-dev-database", "INITIALIZED", "1.2.0", "1.2.0", nil, "1.3.0", "api-dev", "api"),
			},
			{
				inst("web-prod-database", "FAILED", "1.2.0", "1.2.0", "1.2.0", "1.3.0", "web-prod", "web"),
				inst("api-prod-cache", "PROVISIONED", "1.1.0", "1.1.0", "1.1.0", "1.1.4", "api-prod", "api"),
			},
		},
	}
	h, slack, teams := setup(t, api, false)

	if code := post(t, h, "ev-1", publishedEvent("ev-1", "aws-aurora", "2026-11-24T05:54:03.482913Z")); code != http.StatusNoContent {
		t.Fatalf("got %d", code)
	}

	wantSlack := map[string]any{
		"text": "aws-aurora 1.3.0 published",
		"blocks": []any{
			map[string]any{"type": "header", "text": map[string]any{"type": "plain_text", "text": "aws-aurora 1.3.0 published"}},
			map[string]any{"type": "section", "text": map[string]any{"type": "mrkdwn",
				"text": "*Changes*\n• *Faster* failover\n• See <https://example.com|docs>"}},
			map[string]any{"type": "section", "text": map[string]any{"type": "mrkdwn",
				"text": "*Instances that can upgrade*\n*api* · `api-prod`: `api-prod-database` (1.2.2)\n*web* · `web-prod`: `web-prod-database` (1.2.0)"}},
			map[string]any{"type": "section", "text": map[string]any{"type": "mrkdwn",
				"text": "*Instances on a release channel that move to 1.3.0*\n*api* · `api-staging`: `api-staging-database` (deployed 1.2.2)"}},
		},
	}
	if len(slack.bodies()) != 1 || !reflect.DeepEqual(slack.bodies()[0], wantSlack) {
		t.Fatalf("slack:\n got %#v\nwant %#v", slack.bodies(), wantSlack)
	}

	if len(teams.bodies) != 1 {
		t.Fatalf("teams: %v", teams.bodies)
	}
	card := teams.bodies[0]["attachments"].([]any)[0].(map[string]any)
	if card["contentType"] != "application/vnd.microsoft.card.adaptive" {
		t.Fatalf("teams content type: %v", card["contentType"])
	}
	var texts []string
	for _, b := range card["content"].(map[string]any)["body"].([]any) {
		texts = append(texts, b.(map[string]any)["text"].(string))
	}
	wantTexts := []string{
		"aws-aurora 1.3.0 published",
		"**Changes**\n- **Faster** failover\n- See [docs](https://example.com)",
		"Instances that can upgrade",
		"**api** · `api-prod`: `api-prod-database` (1.2.2)\n**web** · `web-prod`: `web-prod-database` (1.2.0)",
		"Instances on a release channel that move to 1.3.0",
		"**api** · `api-staging`: `api-staging-database` (deployed 1.2.2)",
	}
	if !reflect.DeepEqual(texts, wantTexts) {
		t.Fatalf("teams:\n got %q\nwant %q", texts, wantTexts)
	}

	api.mu.Lock()
	defer api.mu.Unlock()
	if !reflect.DeepEqual(api.queries[0], map[string]any{"org": "acme", "repo": "aws-aurora"}) {
		t.Fatalf("version query vars: %v", api.queries[0])
	}
	if api.queries[1]["repo"] != "aws-aurora" {
		t.Fatalf("instances query vars: %v", api.queries[1])
	}
}

func TestSkipsDevelopmentVersions(t *testing.T) {
	api := &fakeAPI{
		tags:      []map[string]any{{"tag": "1.3.1-dev.20261124T055403Z", "createdAt": "2026-11-24T05:54:03Z", "changelog": nil}},
		instances: [][]any{{}},
	}
	h, slack, teams := setup(t, api, false)

	post(t, h, "ev-2", publishedEvent("ev-2", "aws-aurora", "2026-11-24T05:54:04Z"))

	if len(slack.bodies()) != 0 || len(teams.bodies) != 0 {
		t.Fatalf("posted a development version: %v %v", slack.bodies(), teams.bodies)
	}
}

func TestIncludeDevelopmentAndNoUpgrades(t *testing.T) {
	api := &fakeAPI{
		tags:      []map[string]any{{"tag": "1.3.1-dev.20261124T055403Z", "createdAt": "2026-11-24T05:54:03Z", "changelog": nil}},
		instances: [][]any{{}},
	}
	h, slack, _ := setup(t, api, true)

	post(t, h, "ev-3", publishedEvent("ev-3", "aws-aurora", "2026-11-24T05:54:04Z"))

	want := map[string]any{
		"text": "aws-aurora 1.3.1-dev.20261124T055403Z published",
		"blocks": []any{
			map[string]any{"type": "header", "text": map[string]any{"type": "plain_text", "text": "aws-aurora 1.3.1-dev.20261124T055403Z published"}},
			map[string]any{"type": "section", "text": map[string]any{"type": "mrkdwn", "text": "No instances can upgrade to 1.3.1-dev.20261124T055403Z."}},
		},
	}
	if len(slack.bodies()) != 1 || !reflect.DeepEqual(slack.bodies()[0], want) {
		t.Fatalf("slack: %#v", slack.bodies())
	}
}

func TestSameVersionIsAnnouncedOnce(t *testing.T) {
	api := &fakeAPI{
		tags:      []map[string]any{{"tag": "1.3.0", "createdAt": "2026-11-24T05:54:03Z", "changelog": nil}},
		instances: [][]any{{}},
	}
	h, slack, _ := setup(t, api, false)

	post(t, h, "ev-4", publishedEvent("ev-4", "aws-aurora", "2026-11-24T05:54:04Z"))
	post(t, h, "ev-5", publishedEvent("ev-5", "aws-aurora", "2026-11-24T05:54:05Z"))

	if len(slack.bodies()) != 1 {
		t.Fatalf("slack posts: %d", len(slack.bodies()))
	}
}

func TestIgnoresOtherEvents(t *testing.T) {
	api := &fakeAPI{}
	h, slack, _ := setup(t, api, false)

	body, _ := json.Marshal(map[string]any{"id": "ev-6", "type": "repo.created", "data": map[string]any{"entity": map[string]any{"name": "aws-aurora"}}})
	if code := post(t, h, "ev-6", body); code != http.StatusNoContent {
		t.Fatalf("got %d", code)
	}
	if len(api.queries) != 0 || len(slack.bodies()) != 0 {
		t.Fatalf("acted on repo.created")
	}
}

func TestStartNeedsSomewhereToPost(t *testing.T) {
	t.Setenv("MASSDRIVER_ORGANIZATION_ID", "acme")
	t.Setenv("MASSDRIVER_API_KEY", "md_test")
	t.Setenv("SLACK_BOT_TOKEN", "")
	t.Setenv("SLACK_WEBHOOK_URL", "")
	t.Setenv("TEAMS_WEBHOOK_URL", "")
	path := filepath.Join(t.TempDir(), "config.yaml")
	t.Setenv("CONFIG_FILE", path)

	if _, err := Definition.New(); err == nil || !strings.HasPrefix(err.Error(), "nothing to post to") {
		t.Fatalf("no destination: err = %v", err)
	}
	t.Setenv("SLACK_BOT_TOKEN", "xoxb-1")
	if _, err := Definition.New(); err == nil || !strings.HasPrefix(err.Error(), "nothing to post to") {
		t.Fatalf("bot token without a channel: err = %v", err)
	}
	t.Setenv("SLACK_BOT_TOKEN", "")
	t.Setenv("SLACK_WEBHOOK_URL", "https://hooks.slack.com/services/T/B/x")
	if _, err := Definition.New(); err != nil {
		t.Fatalf("webhook: err = %v", err)
	}

	t.Setenv("SLACK_WEBHOOK_URL", "")
	if err := os.WriteFile(path, []byte("teams:\n  enabled: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Definition.New(); err == nil || err.Error() != "missing required env vars: [TEAMS_WEBHOOK_URL]" {
		t.Fatalf("teams without its URL: err = %v", err)
	}
	t.Setenv("TEAMS_WEBHOOK_URL", "https://example.webhook.office.com/x")
	a, err := Definition.New()
	if err != nil {
		t.Fatal(err)
	}
	if n := a.(*ReleaseNotes); n.cfg.Teams.webhookURL != "https://example.webhook.office.com/x" {
		t.Fatalf("teams = %+v", n.cfg.Teams)
	}

	if err := os.WriteFile(path, []byte("slack_webhook_url: https://hooks.slack.com/x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var ce *automation.ConfigError
	if _, err := Definition.New(); !errors.As(err, &ce) {
		t.Fatalf("unknown key: err = %v", err)
	}
}

func TestExampleConfigLoads(t *testing.T) {
	var cfg Config
	if err := automation.LoadConfigFile("config.example.yaml", &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Slack.Channel != "#releases" || !cfg.Teams.Enabled || cfg.Teams.WebhookURLEnv != "TEAMS_WEBHOOK_URL" {
		t.Fatalf("config = %+v", cfg)
	}
}
