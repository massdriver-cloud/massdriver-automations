// Package proposalchatops posts each proposed Massdriver deployment to Slack with Approve and Reject buttons, and
// approves or rejects the deployment when somebody clicks one.
package proposalchatops

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/massdriver-cloud/massdriver-automations/pkg/automation"
	"github.com/massdriver-cloud/massdriver-automations/pkg/massdriver"
)

// InteractionsPath receives Slack's interactivity requests.
const InteractionsPath = "/slack/interactions"

const (
	slackTolerance = 5 * time.Minute
	maxBodyBytes   = 1 << 20
	postRetention  = 30 * 24 * time.Hour
	resolving      = "resolving"
)

var (
	//go:embed queries/approve_deployment.graphql
	approveDeploymentMutation string
	//go:embed queries/reject_deployment.graphql
	rejectDeploymentMutation string
)

// Definition registers the automation.
var Definition = automation.Definition{
	Name:        "proposal-chatops",
	Description: "Posts proposed deployments to Slack with Approve and Reject buttons.",
	Events:      []string{"deployment.proposed", "deployment.approved", "deployment.rejected"},
	New: func() (automation.Automation, error) {
		api, err := massdriver.FromEnv()
		if err != nil {
			return nil, err
		}
		v, err := automation.RequireEnv("SLACK_BOT_TOKEN", "SLACK_SIGNING_SECRET", "SLACK_CHANNEL")
		if err != nil {
			return nil, err
		}
		return New(api, Config{
			SlackURL:           "https://slack.com/api",
			SlackToken:         v["SLACK_BOT_TOKEN"],
			SlackSigningSecret: []byte(v["SLACK_SIGNING_SECRET"]),
			Channel:            v["SLACK_CHANNEL"],
			ChannelAttrKey:     automation.Env("SLACK_CHANNEL_ATTR_KEY", ""),
			AllowedUsers:       splitList(automation.Env("SLACK_ALLOWED_USERS", "")),
		}), nil
	},
}

// Config is the Slack side of the automation.
type Config struct {
	SlackURL           string
	SlackToken         string
	SlackSigningSecret []byte
	Channel            string
	ChannelAttrKey     string
	// AllowedUsers are Slack user IDs. Empty allows everybody in the channel.
	AllowedUsers []string
}

func splitList(s string) []string {
	var out []string
	for _, v := range strings.Split(s, ",") {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// post is the Slack message of one proposed deployment. outcome is empty while the proposal is open.
type post struct {
	channel string
	ts      string
	summary string
	outcome string
	created time.Time
}

// Chatops holds the clients and the Slack message of each proposal.
type Chatops struct {
	api     *massdriver.Client
	slack   *slack
	cfg     Config
	allowed map[string]bool
	run     func(func())

	// Massdriver events are handled one at a time, so a proposal and its decision cannot race each other.
	events sync.Mutex

	mu    sync.Mutex
	posts map[string]*post
}

// New returns the automation.
func New(api *massdriver.Client, cfg Config) *Chatops {
	c := &Chatops{
		api:   api,
		slack: &slack{url: cfg.SlackURL, token: cfg.SlackToken},
		cfg:   cfg,
		run:   func(f func()) { go f() },
		posts: map[string]*post{},
	}
	if len(cfg.AllowedUsers) > 0 {
		c.allowed = map[string]bool{}
		for _, u := range cfg.AllowedUsers {
			c.allowed[u] = true
		}
	}
	return c
}

// Routes adds the Slack interactivity route.
func (c *Chatops) Routes(mux *http.ServeMux) {
	mux.HandleFunc("POST "+InteractionsPath, c.handleInteraction)
}

func (c *Chatops) postFor(deploymentID string) *post {
	now := time.Now()
	for k, p := range c.posts {
		if now.Sub(p.created) > postRetention {
			delete(c.posts, k)
		}
	}
	p, ok := c.posts[deploymentID]
	if !ok {
		p = &post{created: now}
		c.posts[deploymentID] = p
	}
	return p
}

// --- massdriver events -----------------------------------------------------

type deployment struct {
	ID             string `json:"id"`
	Action         string `json:"action"`
	Version        string `json:"version"`
	Message        string `json:"message"`
	DeployedByName string `json:"deployed_by_name"`
}

type actor struct {
	Name string `json:"name"`
}

// Handle posts or updates the Slack message. A Slack error returns an error, so Massdriver retries the event.
func (c *Chatops) Handle(ctx context.Context, ev automation.Event) error {
	switch ev.Type {
	case "deployment.proposed", "deployment.approved", "deployment.rejected":
	default:
		return nil
	}
	var dep deployment
	if err := ev.DecodeEntity(&dep); err != nil {
		return err
	}
	var who actor
	_ = ev.DecodeActor(&who)

	c.events.Lock()
	defer c.events.Unlock()
	if ev.Type == "deployment.proposed" {
		return c.proposed(ctx, ev, dep, who)
	}
	return c.decidedElsewhere(ctx, ev, dep, who)
}

func (c *Chatops) channelFor(ev automation.Event) string {
	if c.cfg.ChannelAttrKey != "" {
		if ch := ev.Attribute(c.cfg.ChannelAttrKey); ch != "" {
			return ch
		}
	}
	return c.cfg.Channel
}

func (c *Chatops) proposed(ctx context.Context, ev automation.Event, dep deployment, who actor) error {
	c.mu.Lock()
	p := c.postFor(dep.ID)
	alreadyPosted, outcome := p.ts != "", p.outcome
	c.mu.Unlock()
	if alreadyPosted {
		return nil
	}

	summary := summarize(ev.Attribute("md-instance"), dep, who)
	// Events arrive in any order. When the decision came first, post the proposal already decided.
	buttons := dep.ID
	if outcome != "" {
		buttons = ""
	}
	ch, ts, err := c.slack.postMessage(ctx, c.channelFor(ev), summary, blocks(summary, outcome, buttons))
	if err != nil {
		return err
	}

	c.mu.Lock()
	p.channel, p.ts, p.summary = ch, ts, summary
	c.mu.Unlock()
	automation.Logger(ctx).Info("proposal posted", "deployment_id", dep.ID, "channel", ch)
	return nil
}

func (c *Chatops) decidedElsewhere(ctx context.Context, ev automation.Event, dep deployment, who actor) error {
	verb := "Approved"
	if ev.Type == "deployment.rejected" {
		verb = "Rejected"
	}
	outcome := verb + " in Massdriver"
	if who.Name != "" {
		outcome = fmt.Sprintf("%s by %s in Massdriver", verb, escape(who.Name))
	}

	c.mu.Lock()
	p := c.postFor(dep.ID)
	// A decision made from Slack already updated the message with the Slack user.
	if p.outcome != "" {
		c.mu.Unlock()
		return nil
	}
	p.outcome = outcome
	channel, ts, summary := p.channel, p.ts, p.summary
	c.mu.Unlock()

	if ts == "" {
		return nil
	}
	return c.slack.update(ctx, channel, ts, summary, blocks(summary, outcome, ""))
}

func summarize(instance string, dep deployment, who actor) string {
	by := dep.DeployedByName
	if by == "" {
		by = who.Name
	}
	lines := []string{
		fmt.Sprintf("*Deployment proposed* for `%s`", escape(instance)),
		fmt.Sprintf("*Action:* %s   *Version:* %s", dep.Action, escape(dep.Version)),
		"*Proposed by:* " + escape(by),
	}
	if dep.Message != "" {
		lines = append(lines, "*Message:* "+escape(dep.Message))
	}
	return strings.Join(lines, "\n")
}

// blocks renders the message. A deployment id in buttons adds the Approve and Reject buttons.
func blocks(summary, outcome, buttons string) []any {
	out := []any{map[string]any{"type": "section", "text": map[string]any{"type": "mrkdwn", "text": summary}}}
	if buttons != "" {
		out = append(out, map[string]any{
			"type":     "actions",
			"block_id": "decision",
			"elements": []any{
				map[string]any{"type": "button", "action_id": "approve", "style": "primary", "value": buttons,
					"text": map[string]any{"type": "plain_text", "text": "Approve"}},
				map[string]any{"type": "button", "action_id": "reject", "style": "danger", "value": buttons,
					"text": map[string]any{"type": "plain_text", "text": "Reject"}},
			},
		})
	}
	if outcome != "" && outcome != resolving {
		out = append(out, map[string]any{
			"type":     "context",
			"elements": []any{map[string]any{"type": "mrkdwn", "text": outcome}},
		})
	}
	return out
}

func escape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}

// --- slack interactions ----------------------------------------------------

type interaction struct {
	Type string `json:"type"`
	User struct {
		ID string `json:"id"`
	} `json:"user"`
	Container struct {
		ChannelID string `json:"channel_id"`
		MessageTS string `json:"message_ts"`
	} `json:"container"`
	Message struct {
		Blocks []struct {
			Text struct {
				Text string `json:"text"`
			} `json:"text"`
		} `json:"blocks"`
	} `json:"message"`
	Actions []struct {
		ActionID string `json:"action_id"`
		Value    string `json:"value"`
	} `json:"actions"`
}

func (c *Chatops) handleInteraction(w http.ResponseWriter, req *http.Request) {
	log := automation.Logger(req.Context())
	body, err := io.ReadAll(http.MaxBytesReader(w, req.Body, maxBodyBytes))
	if err != nil {
		log.Warn("request body read failed", "err", err)
		http.Error(w, "body too large", http.StatusRequestEntityTooLarge)
		return
	}
	if !verifySlack(c.cfg.SlackSigningSecret, req.Header, body, time.Now()) {
		log.Warn("slack signature rejected")
		http.Error(w, "bad signature", http.StatusUnauthorized)
		return
	}

	form, err := parseForm(body)
	if err != nil {
		log.Warn("slack payload decode failed", "err", err)
		http.Error(w, "bad payload", http.StatusBadRequest)
		return
	}
	var in interaction
	if err := json.Unmarshal([]byte(form), &in); err != nil || in.Type != "block_actions" || len(in.Actions) == 0 {
		w.WriteHeader(http.StatusOK)
		return
	}

	// Slack waits 3 seconds for a response, and the Massdriver call can take longer.
	w.WriteHeader(http.StatusOK)
	bg := context.WithoutCancel(req.Context())
	c.run(func() { c.decide(bg, in) })
}

func parseForm(body []byte) (string, error) {
	req, _ := http.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
	req.Header.Set("content-type", "application/x-www-form-urlencoded")
	if err := req.ParseForm(); err != nil {
		return "", err
	}
	return req.PostForm.Get("payload"), nil
}

func (c *Chatops) decide(ctx context.Context, in interaction) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	action := in.Actions[0]
	deploymentID, user := action.Value, in.User.ID
	channel, ts := in.Container.ChannelID, in.Container.MessageTS
	log := automation.Logger(ctx).With("deployment_id", deploymentID, "slack_user", user, "action", action.ActionID)

	var mutation, field, verb, noun string
	switch action.ActionID {
	case "approve":
		mutation, field, verb, noun = approveDeploymentMutation, "approveDeployment", "Approved", "Approval"
	case "reject":
		mutation, field, verb, noun = rejectDeploymentMutation, "rejectDeployment", "Rejected", "Rejection"
	default:
		return
	}

	if c.allowed != nil && !c.allowed[user] {
		log.Warn("decision refused", "reason", "slack_user_not_allowed")
		c.tell(ctx, channel, user, "You are not allowed to approve or reject deployments.")
		return
	}

	c.mu.Lock()
	p := c.postFor(deploymentID)
	if p.outcome != "" {
		c.mu.Unlock()
		c.tell(ctx, channel, user, "This deployment is already decided.")
		return
	}
	p.outcome = resolving
	p.channel, p.ts = channel, ts
	// After a restart, the message itself is the only copy of the summary.
	if p.summary == "" && len(in.Message.Blocks) > 0 {
		p.summary = in.Message.Blocks[0].Text.Text
	}
	summary := p.summary
	c.mu.Unlock()

	if err := c.mutate(ctx, mutation, field, deploymentID); err != nil {
		c.mu.Lock()
		p.outcome = ""
		c.mu.Unlock()
		log.Error("decision failed", "err", err)
		c.tell(ctx, channel, user, fmt.Sprintf("%s failed: %s", noun, err))
		return
	}

	outcome := fmt.Sprintf("%s by <@%s> in Slack", verb, user)
	c.mu.Lock()
	p.outcome = outcome
	c.mu.Unlock()
	log.Info("deployment decided", "outcome", strings.ToLower(verb))

	if err := c.slack.update(ctx, channel, ts, summary, blocks(summary, outcome, "")); err != nil {
		log.Error("slack update failed", "err", err)
	}
}

func (c *Chatops) tell(ctx context.Context, channel, user, text string) {
	if err := c.slack.postEphemeral(ctx, channel, user, text); err != nil {
		automation.Logger(ctx).Error("slack post failed", "err", err)
	}
}

func (c *Chatops) mutate(ctx context.Context, mutation, field, deploymentID string) error {
	var out map[string]massdriver.Payload[struct {
		Status string `json:"status"`
	}]
	if err := c.api.Do(ctx, mutation, map[string]any{"org": c.api.OrganizationID, "id": deploymentID}, &out); err != nil {
		return err
	}
	return out[field].Err()
}

// Slack signs "v0:<timestamp>:<body>" with HMAC-SHA256 and sends it hex-encoded.
func verifySlack(secret []byte, h http.Header, body []byte, now time.Time) bool {
	ts, sig := h.Get("X-Slack-Request-Timestamp"), h.Get("X-Slack-Signature")
	sec, err := strconv.ParseInt(ts, 10, 64)
	if sig == "" || err != nil {
		return false
	}
	if d := now.Sub(time.Unix(sec, 0)); d > slackTolerance || d < -slackTolerance {
		return false
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte("v0:" + ts + ":"))
	mac.Write(body)
	return hmac.Equal([]byte(sig), []byte("v0="+hex.EncodeToString(mac.Sum(nil))))
}

// --- slack web api ---------------------------------------------------------

type slack struct {
	url   string
	token string
}

func (s *slack) postMessage(ctx context.Context, channel, text string, blocks []any) (string, string, error) {
	var out struct {
		Channel string `json:"channel"`
		TS      string `json:"ts"`
	}
	err := s.call(ctx, "chat.postMessage", map[string]any{"channel": channel, "text": text, "blocks": blocks}, &out)
	return out.Channel, out.TS, err
}

func (s *slack) update(ctx context.Context, channel, ts, text string, blocks []any) error {
	return s.call(ctx, "chat.update", map[string]any{"channel": channel, "ts": ts, "text": text, "blocks": blocks}, nil)
}

func (s *slack) postEphemeral(ctx context.Context, channel, user, text string) error {
	return s.call(ctx, "chat.postEphemeral", map[string]any{"channel": channel, "user": user, "text": text}, nil)
}

func (s *slack) call(ctx context.Context, method string, payload map[string]any, out any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.url+"/"+method, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("content-type", "application/json; charset=utf-8")
	req.Header.Set("authorization", "Bearer "+s.token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("slack %s: HTTP %d: %s", method, resp.StatusCode, b)
	}

	// Slack answers 200 for most failures and puts the error in the body.
	var status struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(b, &status); err != nil {
		return err
	}
	if !status.OK {
		return fmt.Errorf("slack %s: %s", method, status.Error)
	}
	if out != nil {
		return json.Unmarshal(b, out)
	}
	return nil
}
