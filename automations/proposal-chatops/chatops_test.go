package proposalchatops

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/massdriver-cloud/massdriver-automations/pkg/automation"
	"github.com/massdriver-cloud/massdriver-automations/pkg/massdriver"
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

type fakeSlack struct {
	mu    sync.Mutex
	calls []call
}

func (f *fakeSlack) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	b, _ := io.ReadAll(r.Body)
	_ = json.Unmarshal(b, &body)
	f.mu.Lock()
	f.calls = append(f.calls, call{strings.TrimPrefix(r.URL.Path, "/"), body})
	f.mu.Unlock()
	if r.Header.Get("authorization") != "Bearer xoxb-test" {
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "invalid_auth"})
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "channel": "C0DEPLOYS", "ts": "1700000000.000100"})
}

func (f *fakeSlack) methods() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []string{}
	for _, c := range f.calls {
		out = append(out, c.Method)
	}
	return out
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
	slack   *fakeSlack
	api     *fakeAPI
}

func setup(t *testing.T) *harness {
	t.Helper()
	sl, api := &fakeSlack{}, &fakeAPI{}
	slSrv, apiSrv := httptest.NewServer(sl), httptest.NewServer(api)
	t.Cleanup(slSrv.Close)
	t.Cleanup(apiSrv.Close)
	c := New(&massdriver.Client{Endpoint: apiSrv.URL, OrganizationID: "acme", APIKey: "md_test"}, Config{
		SlackURL: slSrv.URL, SlackToken: "xoxb-test", SlackSigningSecret: slackSecret,
		Channel: "#deploys", ChannelAttrKey: "slack-channel",
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
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	mac := hmac.New(sha256.New, slackSecret)
	mac.Write([]byte("v0:" + ts + ":"))
	mac.Write(body)
	req.Header.Set("X-Slack-Request-Timestamp", ts)
	req.Header.Set("X-Slack-Signature", "v0="+hex.EncodeToString(mac.Sum(nil)))
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	return rec.Code
}

func blockText(blocks any, i int) string {
	b := blocks.([]any)[i].(map[string]any)
	if text, ok := b["text"].(map[string]any); ok {
		return text["text"].(string)
	}
	return b["elements"].([]any)[0].(map[string]any)["text"].(string)
}

func TestProposedPostsMessageWithButtons(t *testing.T) {
	h := setup(t)

	if code := h.send(deploymentEvent("evt-1", "deployment.proposed", nil, "Jane Doe")); code != http.StatusNoContent {
		t.Fatalf("status = %d", code)
	}
	h.send(deploymentEvent("evt-1", "deployment.proposed", nil, "Jane Doe"))

	if got := h.slack.methods(); len(got) != 1 || got[0] != "chat.postMessage" {
		t.Fatalf("slack calls = %v", got)
	}
	body := h.slack.calls[0].Body
	if body["channel"] != "#deploys" {
		t.Errorf("channel = %v", body["channel"])
	}
	want := "*Deployment proposed* for `api-prod-database`\n*Action:* PROVISION   *Version:* 1.2.3\n" +
		"*Proposed by:* jane@example.com\n*Message:* Bump the instance size"
	if got := blockText(body["blocks"], 0); got != want {
		t.Errorf("summary = %q", got)
	}
	buttons := body["blocks"].([]any)[1].(map[string]any)["elements"].([]any)
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
	if got := h.slack.calls[0].Body["channel"]; got != "#payments-deploys" {
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
	if got := h.slack.methods(); len(got) != 2 {
		t.Fatalf("slack calls = %v", got)
	}
	update := h.slack.calls[1]
	if update.Method != "chat.update" || update.Body["channel"] != "C0DEPLOYS" || update.Body["ts"] != "1700000000.000100" {
		t.Fatalf("update = %v", update)
	}
	blocks := update.Body["blocks"].([]any)
	if len(blocks) != 2 || blockText(blocks, 1) != "Approved by <@U0SAM> in Slack" {
		t.Errorf("blocks = %v", blocks)
	}

	// The approval event that the service account causes must not replace the Slack user.
	h.send(deploymentEvent("evt-2", "deployment.approved", nil, "slack-chatops"))
	if got := h.slack.methods(); len(got) != 2 {
		t.Errorf("slack calls = %v", got)
	}
}

func TestRejectFromSlackAfterRestart(t *testing.T) {
	h := setup(t)

	h.click("reject", "U0SAM")

	if len(h.api.calls) != 1 || h.api.calls[0].Method != "rejectDeployment" {
		t.Fatalf("api calls = %v", h.api.calls)
	}
	if got := h.slack.methods(); len(got) != 1 || got[0] != "chat.update" {
		t.Fatalf("slack calls = %v", got)
	}
	blocks := h.slack.calls[0].Body["blocks"].([]any)
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

	if got := h.slack.methods(); len(got) != 1 {
		t.Fatalf("slack calls = %v", got)
	}
	c := h.slack.calls[0]
	if c.Method != "chat.postEphemeral" || c.Body["user"] != "U0SAM" || c.Body["text"] != "Approval failed: status: must be PROPOSED" {
		t.Fatalf("call = %v", c)
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
	if c := h.slack.calls[0]; c.Method != "chat.postEphemeral" || c.Body["text"] != "You are not allowed to approve or reject deployments." {
		t.Fatalf("call = %v", c)
	}
}

func TestDecidedInMassdriverUpdatesMessage(t *testing.T) {
	h := setup(t)
	h.send(deploymentEvent("evt-1", "deployment.proposed", nil, "Jane Doe"))
	h.send(deploymentEvent("evt-2", "deployment.rejected", nil, "Sam Lee"))

	if got := h.slack.methods(); len(got) != 2 || got[1] != "chat.update" {
		t.Fatalf("slack calls = %v", got)
	}
	blocks := h.slack.calls[1].Body["blocks"].([]any)
	if len(blocks) != 2 || blockText(blocks, 1) != "Rejected by Sam Lee in Massdriver" {
		t.Errorf("blocks = %v", blocks)
	}
}

func TestDecisionBeforeProposalPostsDecided(t *testing.T) {
	h := setup(t)
	h.send(deploymentEvent("evt-2", "deployment.approved", nil, "Sam Lee"))
	h.send(deploymentEvent("evt-1", "deployment.proposed", nil, "Jane Doe"))

	if got := h.slack.methods(); len(got) != 1 || got[0] != "chat.postMessage" {
		t.Fatalf("slack calls = %v", got)
	}
	blocks := h.slack.calls[0].Body["blocks"].([]any)
	if len(blocks) != 2 || blockText(blocks, 1) != "Approved by Sam Lee in Massdriver" {
		t.Errorf("blocks = %v", blocks)
	}
}

func TestSlackFailureAsksForRetry(t *testing.T) {
	h := setup(t)
	h.c.slack.token = "xoxb-wrong"
	if code := h.send(deploymentEvent("evt-1", "deployment.proposed", nil, "Jane Doe")); code != http.StatusInternalServerError {
		t.Fatalf("status = %d", code)
	}
	h.c.slack.token = "xoxb-test"
	h.send(deploymentEvent("evt-1", "deployment.proposed", nil, "Jane Doe"))
	if got := h.slack.methods(); len(got) != 2 || got[1] != "chat.postMessage" {
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
	if got := h.slack.methods(); len(got) != 0 {
		t.Fatalf("slack calls = %v", got)
	}
}
