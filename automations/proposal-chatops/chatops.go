// Package proposalchatops posts each proposed Massdriver deployment to Slack with Approve and Reject buttons, and
// approves or rejects the deployment when somebody clicks one.
package proposalchatops

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/massdriver-cloud/massdriver-automations/pkg/automation"
	"github.com/massdriver-cloud/massdriver-automations/pkg/massdriver"
	"github.com/massdriver-cloud/massdriver-automations/pkg/slack"
)

// InteractionsPath receives Slack's interactivity requests.
const InteractionsPath = "/slack/interactions"

const (
	maxBodyBytes  = 1 << 20
	postRetention = 30 * 24 * time.Hour
	resolving     = "resolving"
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
		var cfg Config
		if err := automation.LoadConfig(&cfg); err != nil {
			return nil, err
		}
		api, err := massdriver.FromEnv()
		if err != nil {
			return nil, err
		}
		// Buttons need chat.update and interactivity, which a webhook cannot do.
		if _, err := automation.RequireEnv("SLACK_BOT_TOKEN"); err != nil {
			return nil, err
		}
		signingSecret, err := slack.SigningSecretFromEnv()
		if err != nil {
			return nil, err
		}
		return New(api, slack.FromEnv(), signingSecret, cfg), nil
	},
}

// Config is the config file of proposal-chatops.
type Config struct {
	Slack SlackConfig `yaml:"slack"`
}

// SlackConfig is where proposals go and who can decide them.
type SlackConfig struct {
	Channel string `yaml:"channel"`
	// ChannelAttribute names an attribute whose value, when an instance has it, replaces Channel.
	ChannelAttribute string `yaml:"channel_attribute"`
	// AllowedUsers are Slack user IDs. Empty allows everybody in the channel.
	AllowedUsers []string `yaml:"allowed_users"`
}

// Validate requires the default channel.
func (c *Config) Validate() error {
	if c.Slack.Channel == "" {
		return errors.New("slack.channel is required")
	}
	return nil
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
	api           *massdriver.Client
	slack         *slack.Client
	signingSecret []byte
	cfg           Config
	allowed       map[string]bool
	run           func(func())

	// Massdriver events are handled one at a time, so a proposal and its decision cannot race each other.
	events sync.Mutex

	mu    sync.Mutex
	posts map[string]*post
}

// New returns the automation.
func New(api *massdriver.Client, sl *slack.Client, signingSecret []byte, cfg Config) *Chatops {
	c := &Chatops{
		api:           api,
		slack:         sl,
		signingSecret: signingSecret,
		cfg:           cfg,
		run:           func(f func()) { go f() },
		posts:         map[string]*post{},
	}
	if len(cfg.Slack.AllowedUsers) > 0 {
		c.allowed = map[string]bool{}
		for _, u := range cfg.Slack.AllowedUsers {
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
	if c.cfg.Slack.ChannelAttribute != "" {
		if ch := ev.Attribute(c.cfg.Slack.ChannelAttribute); ch != "" {
			return ch
		}
	}
	return c.cfg.Slack.Channel
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
	posted, err := c.slack.Post(ctx, slack.Message{Channel: c.channelFor(ev), Text: summary, Blocks: blocks(summary, outcome, buttons)})
	if err != nil {
		return err
	}

	c.mu.Lock()
	p.channel, p.ts, p.summary = posted.Channel, posted.TS, summary
	c.mu.Unlock()
	automation.Logger(ctx).Info("proposal posted", "deployment_id", dep.ID, "channel", posted.Channel)
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
	return c.slack.Update(ctx, channel, ts, summary, blocks(summary, outcome, ""))
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
	if !slack.Verify(c.signingSecret, req.Header, body, time.Now()) {
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

	if err := c.slack.Update(ctx, channel, ts, summary, blocks(summary, outcome, "")); err != nil {
		log.Error("slack update failed", "err", err)
	}
}

func (c *Chatops) tell(ctx context.Context, channel, user, text string) {
	if err := c.slack.PostEphemeral(ctx, channel, user, text); err != nil {
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
