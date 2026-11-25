package proposalchatops

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/massdriver-cloud/massdriver-automations/pkg/automation"
	"github.com/massdriver-cloud/massdriver-automations/pkg/massdriver"
	"github.com/massdriver-cloud/massdriver-automations/pkg/slack"
	"github.com/massdriver-cloud/massdriver-automations/pkg/slack/slacktest"
)

var (
	secret      = []byte("0123456789abcdef0123456789abcdef")
	slackSecret = []byte("slack-signing-secret")
)

const deploymentID = "b9dc3d18-19fc-429c-8d36-6753615fe17c"

func deploymentEvent(id, typ string, attrs map[string]any, actor string) []byte {
	if attrs == nil {
		attrs = map[string]any{}
	}
	attrs["md-instance"] = "api-prod-database"
	b, _ := json.Marshal(map[string]any{
		"id":   id,
		"type": typ,
		"time": "2026-11-24T05:54:03Z",
		"data": map[string]any{
			"entity": map[string]any{
				"id": deploymentID, "status": "PROPOSED", "action": "PROVISION", "version": "1.2.3",
				"deployed_by_name": "jane@example.com", "message": "Bump the instance size",
			},
			"actor":      map[string]any{"name": actor},
			"attributes": attrs,
			"metadata":   map[string]any{"deployment_action": "PROVISION"},
		},
	})
	return b
}

type call struct {
	Method string
	Body   map[string]any
}

type fakeAPI struct {
	mu       sync.Mutex
	calls    []call
	response map[string]any
}

func (f *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Query     string         `json:"query"`
		Variables map[string]any `json:"variables"`
	}
	b, _ := io.ReadAll(r.Body)
	_ = json.Unmarshal(b, &req)
	name := "rejectDeployment"
	if strings.Contains(req.Query, "mutation ApproveDeployment") {
		name = "approveDeployment"
	}
	f.mu.Lock()
	f.calls = append(f.calls, call{name, req.Variables})
	f.mu.Unlock()
	payload := f.response
	if payload == nil {
		payload = map[string]any{"successful": true, "messages": []any{}, "result": map[string]any{"id": deploymentID, "status": "APPROVED"}}
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{name: payload}})
}

type harness struct {
	c       *Chatops
	handler http.Handler
	slack   *slacktest.Server
	api     *fakeAPI
}

func setup(t *testing.T) *harness {
	t.Helper()
	sl, api := slacktest.NewServer(t), &fakeAPI{}
	apiSrv := httptest.NewServer(api)
	t.Cleanup(apiSrv.Close)
	c := New(&massdriver.Client{Endpoint: apiSrv.URL, OrganizationID: "acme", APIKey: "md_test"}, sl.Bot(), slackSecret, Config{
		Slack: SlackConfig{Channel: "#deploys", ChannelAttribute: "slack-channel"},
	})
	c.run = func(f func()) { f() }
	return &harness{c: c, handler: automation.Handler(c, secret), slack: sl, api: api}
}

func (h *harness) send(body []byte) int {
	var ev struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(body, &ev)
	req := httptest.NewRequest(http.MethodPost, automation.HookPath, strings.NewReader(string(body)))
	automation.SignRequest(req, secret, ev.ID, body, time.Now())
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	return rec.Code
}

func (h *harness) click(actionID, user string) int {
	payload, _ := json.Marshal(map[string]any{
		"type":      "block_actions",
		"user":      map[string]any{"id": user},
		"container": map[string]any{"channel_id": "C0DEPLOYS", "message_ts": "1700000000.000100"},
		"message": map[string]any{"blocks": []any{
			map[string]any{"type": "section", "text": map[string]any{"type": "mrkdwn", "text": "summary from slack"}},
		}},
		"actions": []any{map[string]any{"action_id": actionID, "value": deploymentID}},
	})
	body := []byte("payload=" + url.QueryEscape(string(payload)))
	req := httptest.NewRequest(http.MethodPost, InteractionsPath, strings.NewReader(string(body)))
	slack.SignRequest(req, slackSecret, body, time.Now())
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	return rec.Code
}

func blockText(blocks []any, i int) string {
	b := blocks[i].(map[string]any)
	if text, ok := b["text"].(map[string]any); ok {
		return text["text"].(string)
	}
	return b["elements"].([]any)[0].(map[string]any)["text"].(string)
}

func methods(s *slacktest.Server) []string {
	out := []string{}
	for _, c := range s.Calls() {
		out = append(out, c.Method)
	}
	return out
}

func TestProposedPostsMessageWithButtons(t *testing.T) {
	h := setup(t)

	if code := h.send(deploymentEvent("evt-1", "deployment.proposed", nil, "Jane Doe")); code != http.StatusNoContent {
		t.Fatalf("status = %d", code)
	}
	h.send(deploymentEvent("evt-1", "deployment.proposed", nil, "Jane Doe"))

	if got := methods(h.slack); len(got) != 1 || got[0] != "chat.postMessage" {
		t.Fatalf("slack calls = %v", got)
	}
	post := h.slack.Calls()[0]
	if post.Channel != "#deploys" {
		t.Errorf("channel = %v", post.Channel)
	}
	want := "*Deployment proposed* for `api-prod-database`\n*Action:* PROVISION   *Version:* 1.2.3\n" +
		"*Proposed by:* jane@example.com\n*Message:* Bump the instance size"
	if got := blockText(post.Blocks, 0); got != want {
		t.Errorf("summary = %q", got)
	}
	buttons := post.Blocks[1].(map[string]any)["elements"].([]any)
	approve, reject := buttons[0].(map[string]any), buttons[1].(map[string]any)
	if approve["action_id"] != "approve" || approve["value"] != deploymentID {
		t.Errorf("approve button = %v", approve)
	}
	if reject["action_id"] != "reject" || reject["value"] != deploymentID {
		t.Errorf("reject button = %v", reject)
	}
}

func TestChannelFromAttribute(t *testing.T) {
	h := setup(t)
	h.send(deploymentEvent("evt-1", "deployment.proposed", map[string]any{"slack-channel": "#payments-deploys"}, "Jane Doe"))
	if got := h.slack.Calls()[0].Channel; got != "#payments-deploys" {
		t.Fatalf("channel = %v", got)
	}
}

func TestApproveFromSlack(t *testing.T) {
	h := setup(t)
	h.send(deploymentEvent("evt-1", "deployment.proposed", nil, "Jane Doe"))

	if code := h.click("approve", "U0SAM"); code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}

	if len(h.api.calls) != 1 || h.api.calls[0].Method != "approveDeployment" {
		t.Fatalf("api calls = %v", h.api.calls)
	}
	if v := h.api.calls[0].Body; v["org"] != "acme" || v["id"] != deploymentID {
		t.Errorf("variables = %v", v)
	}
	if got := methods(h.slack); len(got) != 2 {
		t.Fatalf("slack calls = %v", got)
	}
	update := h.slack.Calls()[1]
	if update.Method != "chat.update" || update.Channel != "C0DEPLOYS" || update.TS != "1700000000.000100" {
		t.Fatalf("update = %+v", update)
	}
	blocks := update.Blocks
	if len(blocks) != 2 || blockText(blocks, 1) != "Approved by <@U0SAM> in Slack" {
		t.Errorf("blocks = %v", blocks)
	}

	// The approval event that the service account causes must not replace the Slack user.
	h.send(deploymentEvent("evt-2", "deployment.approved", nil, "slack-chatops"))
	if got := methods(h.slack); len(got) != 2 {
		t.Errorf("slack calls = %v", got)
	}
}

func TestRejectFromSlackAfterRestart(t *testing.T) {
	h := setup(t)

	h.click("reject", "U0SAM")

	if len(h.api.calls) != 1 || h.api.calls[0].Method != "rejectDeployment" {
		t.Fatalf("api calls = %v", h.api.calls)
	}
	if got := methods(h.slack); len(got) != 1 || got[0] != "chat.update" {
		t.Fatalf("slack calls = %v", got)
	}
	blocks := h.slack.Calls()[0].Blocks
	if blockText(blocks, 0) != "summary from slack" || blockText(blocks, 1) != "Rejected by <@U0SAM> in Slack" {
		t.Errorf("blocks = %v", blocks)
	}
}

func TestFailedApprovalTellsUser(t *testing.T) {
	h := setup(t)
	h.api.response = map[string]any{"successful": false, "messages": []any{
		map[string]any{"field": "status", "message": "must be PROPOSED"},
	}}

	h.click("approve", "U0SAM")

	if got := methods(h.slack); len(got) != 1 {
		t.Fatalf("slack calls = %v", got)
	}
	c := h.slack.Calls()[0]
	if c.Method != "chat.postEphemeral" || c.User != "U0SAM" || c.Text != "Approval failed: status: must be PROPOSED" {
		t.Fatalf("call = %+v", c)
	}

	// The proposal is open again, so a second click calls the API again.
	h.api.response = nil
	h.click("approve", "U0SAM")
	if len(h.api.calls) != 2 {
		t.Fatalf("api calls = %v", h.api.calls)
	}
}

func TestUserNotAllowed(t *testing.T) {
	h := setup(t)
	h.c.allowed = map[string]bool{"U0LEAD": true}

	h.click("approve", "U0SAM")

	if len(h.api.calls) != 0 {
		t.Fatalf("api calls = %v", h.api.calls)
	}
	if c := h.slack.Calls()[0]; c.Method != "chat.postEphemeral" || c.Text != "You are not allowed to approve or reject deployments." {
		t.Fatalf("call = %v", c)
	}
}

func TestDecidedInMassdriverUpdatesMessage(t *testing.T) {
	h := setup(t)
	h.send(deploymentEvent("evt-1", "deployment.proposed", nil, "Jane Doe"))
	h.send(deploymentEvent("evt-2", "deployment.rejected", nil, "Sam Lee"))

	if got := methods(h.slack); len(got) != 2 || got[1] != "chat.update" {
		t.Fatalf("slack calls = %v", got)
	}
	blocks := h.slack.Calls()[1].Blocks
	if len(blocks) != 2 || blockText(blocks, 1) != "Rejected by Sam Lee in Massdriver" {
		t.Errorf("blocks = %v", blocks)
	}
}

func TestDecisionBeforeProposalPostsDecided(t *testing.T) {
	h := setup(t)
	h.send(deploymentEvent("evt-2", "deployment.approved", nil, "Sam Lee"))
	h.send(deploymentEvent("evt-1", "deployment.proposed", nil, "Jane Doe"))

	if got := methods(h.slack); len(got) != 1 || got[0] != "chat.postMessage" {
		t.Fatalf("slack calls = %v", got)
	}
	blocks := h.slack.Calls()[0].Blocks
	if len(blocks) != 2 || blockText(blocks, 1) != "Approved by Sam Lee in Massdriver" {
		t.Errorf("blocks = %v", blocks)
	}
}

func TestSlackFailureAsksForRetry(t *testing.T) {
	h := setup(t)
	h.slack.Fail("invalid_auth")
	if code := h.send(deploymentEvent("evt-1", "deployment.proposed", nil, "Jane Doe")); code != http.StatusInternalServerError {
		t.Fatalf("status = %d", code)
	}
	h.slack.Fail("")
	h.send(deploymentEvent("evt-1", "deployment.proposed", nil, "Jane Doe"))
	if got := methods(h.slack); len(got) != 1 || got[0] != "chat.postMessage" {
		t.Fatalf("slack calls = %v", got)
	}
}

func TestBadSlackSignature(t *testing.T) {
	h := setup(t)
	req := httptest.NewRequest(http.MethodPost, InteractionsPath, strings.NewReader("payload=%7B%7D"))
	req.Header.Set("X-Slack-Request-Timestamp", strconv.FormatInt(time.Now().Unix(), 10))
	req.Header.Set("X-Slack-Signature", "v0=deadbeef")
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d", rec.Code)
	}
}

func TestIgnoresOtherEvents(t *testing.T) {
	h := setup(t)
	h.send(deploymentEvent("evt-1", "deployment.completed", nil, "Jane Doe"))
	if got := methods(h.slack); len(got) != 0 {
		t.Fatalf("slack calls = %v", got)
	}
}

func TestStartNeedsABotTokenAndAChannel(t *testing.T) {
	t.Setenv("MASSDRIVER_ORGANIZATION_ID", "acme")
	t.Setenv("MASSDRIVER_API_KEY", "md_test")
	t.Setenv("SLACK_SIGNING_SECRET", "s")
	t.Setenv("SLACK_BOT_TOKEN", "")
	t.Setenv("SLACK_WEBHOOK_URL", "https://hooks.slack.com/services/T/B/x")
	path := filepath.Join(t.TempDir(), "config.yaml")
	t.Setenv("CONFIG_FILE", path)

	_, err := Definition.New()
	var ce *automation.ConfigError
	if !errors.As(err, &ce) || !strings.HasSuffix(err.Error(), "slack.channel is required") {
		t.Fatalf("no config: err = %v", err)
	}

	if err := os.WriteFile(path, []byte("slack:\n  channel: \"#deploys\"\n  allowed_users: [U0LEAD]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Definition.New(); err == nil || err.Error() != "missing required env vars: [SLACK_BOT_TOKEN]" {
		t.Fatalf("webhook only: err = %v", err)
	}
	t.Setenv("SLACK_BOT_TOKEN", "xoxb-1")
	a, err := Definition.New()
	if err != nil {
		t.Fatal(err)
	}
	if c := a.(*Chatops); !c.allowed["U0LEAD"] || !c.slack.HasBotToken() {
		t.Fatalf("chatops = %+v", c)
	}
}

func TestExampleConfigLoads(t *testing.T) {
	var cfg Config
	if err := automation.LoadConfigFile("config.example.yaml", &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Slack.Channel != "#deploys" || cfg.Slack.ChannelAttribute == "" || len(cfg.Slack.AllowedUsers) != 2 {
		t.Fatalf("config = %+v", cfg)
	}
}
