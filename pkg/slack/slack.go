// Package slack posts messages to Slack for every automation, with a bot token or an incoming webhook.
//
// With SLACK_BOT_TOKEN, messages go to the channel that the automation's config names, and can be updated, sent
// ephemerally, and threaded. With only SLACK_WEBHOOK_URL, every message goes to the webhook's channel, and channels,
// threads, updates, and ephemeral messages are not available.
package slack

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/massdriver-cloud/massdriver-automations/pkg/automation"
)

// DefaultAPIURL is the Slack Web API.
const DefaultAPIURL = "https://slack.com/api"

// ErrNeedsBotToken is returned by the calls that a webhook cannot make.
var ErrNeedsBotToken = errors.New("slack: needs SLACK_BOT_TOKEN")

// Config is the `slack` section of an automation's config.
type Config struct {
	// Channel is a channel name, such as #infra, or a channel ID. It is ignored with only a webhook.
	Channel string `yaml:"channel"`
}

// Or returns c's channel, or fallback's when c has none.
func (c Config) Or(fallback Config) Config {
	if c.Channel == "" {
		return fallback
	}
	return c
}

// Options builds a Client. Tests set APIURL to a fake.
type Options struct {
	BotToken   string
	WebhookURL string
	APIURL     string
	HTTP       *http.Client
}

// Client posts to Slack. A nil Client posts nothing.
type Client struct {
	token   string
	webhook string
	api     string
	http    *http.Client
}

// FromEnv returns a Client for SLACK_BOT_TOKEN or SLACK_WEBHOOK_URL, or nil when neither is set. The bot token wins
// when both are set.
func FromEnv() *Client {
	return New(Options{BotToken: os.Getenv("SLACK_BOT_TOKEN"), WebhookURL: os.Getenv("SLACK_WEBHOOK_URL")})
}

// New returns a Client, or nil when opts has neither a bot token nor a webhook URL.
func New(opts Options) *Client {
	if opts.BotToken == "" && opts.WebhookURL == "" {
		return nil
	}
	c := &Client{token: opts.BotToken, webhook: opts.WebhookURL, api: opts.APIURL, http: opts.HTTP}
	if c.api == "" {
		c.api = DefaultAPIURL
	}
	c.api = strings.TrimRight(c.api, "/")
	if c.http == nil {
		c.http = automation.HTTPClient(10 * time.Second)
	}
	return c
}

// HasBotToken reports whether c can use channels, threads, updates, and ephemeral messages.
func (c *Client) HasBotToken() bool { return c != nil && c.token != "" }

// Sends reports whether a message for channel goes anywhere: with a bot token, when channel is set; with only a
// webhook, always.
func (c *Client) Sends(channel string) bool {
	if c == nil {
		return false
	}
	return c.token == "" || channel != ""
}

// WarnIgnoredChannels logs once that the config names channels that a webhook cannot use.
func (c *Client) WarnIgnoredChannels(log *slog.Logger, channels ...string) {
	if c == nil || c.token != "" {
		return
	}
	for _, ch := range channels {
		if ch != "" {
			log.Warn("slack channels ignored", "reason", "webhook_only")
			return
		}
	}
}

// Message is one Slack message. Text is the notification text, and the whole message when Blocks is empty.
type Message struct {
	Channel  string
	Text     string
	Blocks   []any
	ThreadTS string
}

// Posted is where a message landed. Both are empty for a webhook post.
type Posted struct {
	Channel string
	TS      string
}

// Post sends msg. With a bot token, it needs msg.Channel. With only a webhook, msg.Channel and msg.ThreadTS are
// ignored, so a reply arrives as a message of its own.
func (c *Client) Post(ctx context.Context, msg Message) (Posted, error) {
	if c == nil {
		return Posted{}, nil
	}
	if c.token == "" {
		payload := map[string]any{"text": msg.Text}
		if len(msg.Blocks) > 0 {
			payload["blocks"] = msg.Blocks
		}
		return Posted{}, c.postWebhook(ctx, payload)
	}
	if msg.Channel == "" {
		return Posted{}, errors.New("slack chat.postMessage: no channel")
	}
	payload := map[string]any{"channel": msg.Channel, "text": msg.Text}
	if len(msg.Blocks) > 0 {
		payload["blocks"] = msg.Blocks
	}
	if msg.ThreadTS != "" {
		payload["thread_ts"] = msg.ThreadTS
	}
	var out Posted
	var resp struct {
		Channel string `json:"channel"`
		TS      string `json:"ts"`
	}
	err := c.call(ctx, "chat.postMessage", payload, &resp)
	out.Channel, out.TS = resp.Channel, resp.TS
	return out, err
}

// Update replaces the text and blocks of the message at channel and ts.
func (c *Client) Update(ctx context.Context, channel, ts, text string, blocks []any) error {
	if !c.HasBotToken() {
		return ErrNeedsBotToken
	}
	return c.call(ctx, "chat.update", map[string]any{"channel": channel, "ts": ts, "text": text, "blocks": blocks}, nil)
}

// PostEphemeral shows text to one user in channel.
func (c *Client) PostEphemeral(ctx context.Context, channel, user, text string) error {
	if !c.HasBotToken() {
		return ErrNeedsBotToken
	}
	return c.call(ctx, "chat.postEphemeral", map[string]any{"channel": channel, "user": user, "text": text}, nil)
}

func (c *Client) call(ctx context.Context, method string, payload map[string]any, out any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(automation.WithClientSpan(ctx, "slack "+method), http.MethodPost,
		c.api+"/"+method, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("content-type", "application/json; charset=utf-8")
	req.Header.Set("authorization", "Bearer "+c.token)

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("slack %s: %w", method, automation.RedactURL(err))
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("slack %s: HTTP %d: %s", method, resp.StatusCode, bytes.TrimSpace(b))
	}

	// The Web API answers 200 for most failures and puts the reason in the body.
	var status struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(b, &status); err != nil {
		return fmt.Errorf("slack %s: %w", method, err)
	}
	if !status.OK {
		return fmt.Errorf("slack %s: %s", method, status.Error)
	}
	if out != nil {
		return json.Unmarshal(b, out)
	}
	return nil
}

func (c *Client) postWebhook(ctx context.Context, payload map[string]any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.webhook, bytes.NewReader(body))
	if err != nil {
		// The URL is a credential, and url.Parse errors quote it.
		return errors.New("slack webhook: SLACK_WEBHOOK_URL is not a valid URL")
	}
	req.Header.Set("content-type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("slack webhook: %w", automation.RedactURL(err))
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("slack webhook: HTTP %d: %s", resp.StatusCode, bytes.TrimSpace(b))
	}
	return nil
}
