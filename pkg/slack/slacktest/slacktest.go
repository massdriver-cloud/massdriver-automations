// Package slacktest is a fake Slack Web API and incoming webhook for tests.
package slacktest

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/massdriver-cloud/massdriver-automations/pkg/slack"
)

// Call is one request that the fake received.
type Call struct {
	// Method is the Web API method, such as chat.postMessage, or "webhook".
	Method   string
	Channel  string
	User     string
	Text     string
	Blocks   []any
	ThreadTS string
	// TS is the message that chat.update changed, or the ts that chat.postMessage returned.
	TS string
}

// Server records every call. Set Fail to make each call fail the way Slack does.
type Server struct {
	*httptest.Server

	mu    sync.Mutex
	calls []Call
	next  int
	fail  string
}

// NewServer starts a fake that closes when the test ends.
func NewServer(t testing.TB) *Server {
	s := &Server{}
	s.Server = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.Close)
	return s
}

// Bot returns a client that uses the Web API with a bot token.
func (s *Server) Bot() *slack.Client {
	return slack.New(slack.Options{BotToken: "xoxb-test", APIURL: s.URL + "/api"})
}

// Webhook returns a client that uses only an incoming webhook.
func (s *Server) Webhook() *slack.Client {
	return slack.New(slack.Options{WebhookURL: s.WebhookURL()})
}

// WebhookURL is the URL of the fake incoming webhook. It holds a secret-looking path, as a real one does.
func (s *Server) WebhookURL() string { return s.URL + "/services/T000/B000/secret" }

// Fail makes each later call fail with reason: an `ok: false` body for the Web API, a 404 for the webhook. An
// empty reason makes calls succeed again.
func (s *Server) Fail(reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fail = reason
}

// Calls returns every call so far.
func (s *Server) Calls() []Call {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Call(nil), s.calls...)
}

// Texts returns the text of every call so far.
func (s *Server) Texts() []string {
	var out []string
	for _, c := range s.Calls() {
		out = append(out, c.Text)
	}
	return out
}

// ChannelID is the ID that the fake answers for a channel: "#infra" is "CINFRA", and an ID is itself. Slack answers
// with the ID whichever form the request used.
func ChannelID(channel string) string {
	name, ok := strings.CutPrefix(channel, "#")
	if !ok {
		return channel
	}
	return "C" + strings.ToUpper(name)
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	var in struct {
		Channel  string `json:"channel"`
		User     string `json:"user"`
		Text     string `json:"text"`
		Blocks   []any  `json:"blocks"`
		ThreadTS string `json:"thread_ts"`
		TS       string `json:"ts"`
	}
	_ = json.Unmarshal(b, &in)
	method, isAPI := strings.CutPrefix(r.URL.Path, "/api/")
	if !isAPI {
		method = "webhook"
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail != "" {
		if isAPI {
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": s.fail})
		} else {
			http.Error(w, s.fail, http.StatusNotFound)
		}
		return
	}
	if isAPI && r.Header.Get("authorization") != "Bearer xoxb-test" {
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "not_authed"})
		return
	}

	call := Call{Method: method, Channel: in.Channel, User: in.User, Text: in.Text, Blocks: in.Blocks, ThreadTS: in.ThreadTS, TS: in.TS}
	if method == "chat.postMessage" {
		s.next++
		call.TS = fmt.Sprintf("1700000000.%06d", s.next)
	}
	s.calls = append(s.calls, call)

	if !isAPI {
		_, _ = w.Write([]byte("ok"))
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "channel": ChannelID(in.Channel), "ts": call.TS})
}
