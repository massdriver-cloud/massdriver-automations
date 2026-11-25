package slack_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/massdriver-cloud/massdriver-automations/pkg/slack"
	"github.com/massdriver-cloud/massdriver-automations/pkg/slack/slacktest"
)

func TestBotPostsToChannelAndThread(t *testing.T) {
	srv := slacktest.NewServer(t)
	c := srv.Bot()
	ctx := context.Background()

	top, err := c.Post(ctx, slack.Message{Channel: "#infra", Text: "drift on api-prod-db"})
	if err != nil {
		t.Fatal(err)
	}
	if top.Channel != "CINFRA" || top.TS != "1700000000.000001" {
		t.Fatalf("posted = %+v", top)
	}
	if _, err := c.Post(ctx, slack.Message{Channel: top.Channel, ThreadTS: top.TS, Text: "resolved"}); err != nil {
		t.Fatal(err)
	}
	if err := c.Update(ctx, top.Channel, top.TS, "drift on api-prod-db (resolved)", nil); err != nil {
		t.Fatal(err)
	}
	if err := c.PostEphemeral(ctx, top.Channel, "U1", "not allowed"); err != nil {
		t.Fatal(err)
	}

	calls := srv.Calls()
	want := []slacktest.Call{
		{Method: "chat.postMessage", Channel: "#infra", Text: "drift on api-prod-db", TS: "1700000000.000001"},
		{Method: "chat.postMessage", Channel: "CINFRA", Text: "resolved", ThreadTS: "1700000000.000001", TS: "1700000000.000002"},
		{Method: "chat.update", Channel: "CINFRA", Text: "drift on api-prod-db (resolved)", TS: "1700000000.000001"},
		{Method: "chat.postEphemeral", Channel: "CINFRA", User: "U1", Text: "not allowed"},
	}
	if len(calls) != len(want) {
		t.Fatalf("calls = %+v", calls)
	}
	for i := range want {
		if calls[i].Method != want[i].Method || calls[i].Channel != want[i].Channel || calls[i].Text != want[i].Text ||
			calls[i].ThreadTS != want[i].ThreadTS || calls[i].TS != want[i].TS || calls[i].User != want[i].User {
			t.Errorf("call %d = %+v, want %+v", i, calls[i], want[i])
		}
	}
}

func TestBotNeedsAChannel(t *testing.T) {
	c := slacktest.NewServer(t).Bot()
	if c.Sends("") {
		t.Fatal("a bot without a channel sends")
	}
	if _, err := c.Post(context.Background(), slack.Message{Text: "x"}); err == nil {
		t.Fatal("posted without a channel")
	}
}

func TestWebhookIgnoresChannelAndThread(t *testing.T) {
	srv := slacktest.NewServer(t)
	c := srv.Webhook()
	if !c.Sends("") || c.HasBotToken() {
		t.Fatal("a webhook client must send without a channel and has no bot token")
	}
	posted, err := c.Post(context.Background(), slack.Message{Channel: "#other", ThreadTS: "1.2", Text: "hello"})
	if err != nil || posted != (slack.Posted{}) {
		t.Fatalf("posted = %+v, err = %v", posted, err)
	}
	calls := srv.Calls()
	if len(calls) != 1 || calls[0].Method != "webhook" || calls[0].Channel != "" || calls[0].ThreadTS != "" || calls[0].Text != "hello" {
		t.Fatalf("calls = %+v", calls)
	}
	if err := c.Update(context.Background(), "C1", "1.2", "x", nil); !errors.Is(err, slack.ErrNeedsBotToken) {
		t.Fatalf("Update err = %v", err)
	}
	if err := c.PostEphemeral(context.Background(), "C1", "U1", "x"); !errors.Is(err, slack.ErrNeedsBotToken) {
		t.Fatalf("PostEphemeral err = %v", err)
	}
}

func TestNilClientPostsNothing(t *testing.T) {
	var c *slack.Client
	if c.Sends("#infra") || c.HasBotToken() {
		t.Fatal("nil client sends")
	}
	if _, err := c.Post(context.Background(), slack.Message{Channel: "#infra", Text: "x"}); err != nil {
		t.Fatal(err)
	}
	if slack.New(slack.Options{}) != nil {
		t.Fatal("New without credentials returned a client")
	}
}

func TestFromEnvPrefersTheBotToken(t *testing.T) {
	t.Setenv("SLACK_BOT_TOKEN", "")
	t.Setenv("SLACK_WEBHOOK_URL", "")
	if slack.FromEnv() != nil {
		t.Fatal("client without credentials")
	}
	t.Setenv("SLACK_WEBHOOK_URL", "https://hooks.slack.com/services/T/B/x")
	if c := slack.FromEnv(); c == nil || c.HasBotToken() {
		t.Fatal("want a webhook client")
	}
	t.Setenv("SLACK_BOT_TOKEN", "xoxb-1")
	if c := slack.FromEnv(); !c.HasBotToken() {
		t.Fatal("want a bot client")
	}
}

func TestErrorsHideTheWebhookURLAndToken(t *testing.T) {
	srv := slacktest.NewServer(t)
	srv.Fail("no_service")
	_, err := srv.Webhook().Post(context.Background(), slack.Message{Text: "x"})
	if err == nil || strings.Contains(err.Error(), "secret") || !strings.Contains(err.Error(), "HTTP 404") {
		t.Fatalf("err = %v", err)
	}

	closed := httptest.NewServer(http.NotFoundHandler())
	closed.Close()
	hook := closed.URL + "/services/T000/B000/secret"
	_, err = slack.New(slack.Options{WebhookURL: hook}).Post(context.Background(), slack.Message{Text: "x"})
	if err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatalf("err = %v", err)
	}
	_, err = slack.New(slack.Options{WebhookURL: "http://[::1]:namedport/secret"}).Post(context.Background(), slack.Message{Text: "x"})
	if err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatalf("err = %v", err)
	}

	_, err = slack.New(slack.Options{BotToken: "xoxb-secret", APIURL: closed.URL}).Post(context.Background(), slack.Message{Channel: "#a", Text: "x"})
	if err == nil || strings.Contains(err.Error(), "xoxb-secret") {
		t.Fatalf("err = %v", err)
	}
}

func TestBotErrorFromBody(t *testing.T) {
	srv := slacktest.NewServer(t)
	srv.Fail("channel_not_found")
	_, err := srv.Bot().Post(context.Background(), slack.Message{Channel: "#nope", Text: "x"})
	if err == nil || err.Error() != "slack chat.postMessage: channel_not_found" {
		t.Fatalf("err = %v", err)
	}
}

func TestWarnIgnoredChannels(t *testing.T) {
	srv := slacktest.NewServer(t)
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))

	srv.Bot().WarnIgnoredChannels(log, "#infra")
	srv.Webhook().WarnIgnoredChannels(log, "", "")
	if buf.Len() != 0 {
		t.Fatalf("logged %q", buf.String())
	}
	srv.Webhook().WarnIgnoredChannels(log, "", "#infra", "#other")
	if got := strings.Count(buf.String(), "msg=\"slack channels ignored\" reason=webhook_only"); got != 1 {
		t.Fatalf("logged %q", buf.String())
	}
}

func TestVerify(t *testing.T) {
	secret := []byte("8f742231b10e8888abcd99yyyzzz85a5")
	body := []byte("payload=%7B%7D")
	now := time.Unix(1700000000, 0)
	req := httptest.NewRequest(http.MethodPost, "/slack/interactions", bytes.NewReader(body))
	slack.SignRequest(req, secret, body, now)
	if !slack.Verify(secret, req.Header, body, now) {
		t.Fatal("valid signature rejected")
	}
	if slack.Verify([]byte("other"), req.Header, body, now) {
		t.Fatal("wrong secret accepted")
	}
	if slack.Verify(secret, req.Header, []byte("payload=changed"), now) {
		t.Fatal("changed body accepted")
	}
	if slack.Verify(secret, req.Header, body, now.Add(6*time.Minute)) {
		t.Fatal("old timestamp accepted")
	}
	if slack.Verify(secret, http.Header{}, body, now) {
		t.Fatal("no headers accepted")
	}
}

func TestConfigOr(t *testing.T) {
	def := slack.Config{Channel: "#infra"}
	if got := (slack.Config{}).Or(def); got.Channel != "#infra" {
		t.Fatalf("got %q", got.Channel)
	}
	if got := (slack.Config{Channel: "#db"}).Or(def); got.Channel != "#db" {
		t.Fatalf("got %q", got.Channel)
	}
}
