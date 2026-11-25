package automation_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/massdriver-cloud/massdriver-automations/automations"
	"github.com/massdriver-cloud/massdriver-automations/pkg/automation"
)

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// closedURL is the URL of a server that refuses connections, so every call to it fails.
func closedURL(t *testing.T) string {
	srv := httptest.NewServer(http.NotFoundHandler())
	srv.Close()
	return srv.URL
}

func TestLogsNeverHoldSecrets(t *testing.T) {
	signingKey := []byte("leak-signing-key-0123456789abcdef")
	dead := closedURL(t)
	secrets := map[string]string{
		"MASSDRIVER_API_KEY":        "md_leak_api_key",
		"MASSDRIVER_SIGNING_SECRET": "whsec_" + base64.StdEncoding.EncodeToString(signingKey),
		"SLACK_SIGNING_SECRET":      "leak-slack-signing-secret",
		"GITHUB_TOKEN":              "ghp_leak_github_token",
		"DD_API_KEY":                "leak-datadog-api-key",
		"SPLUNK_HEC_TOKEN":          "leak-splunk-hec-token",
		"CUSTOM_HEC_TOKEN":          "leak-custom-hec-token",
		"TEAMS_WEBHOOK_URL":         dead + "/workflows/leak-teams-path",
		"DRIFT_RUN_TOKEN":           "leak-drift-run-token",
		"FREEZE_ADMIN_TOKEN":        "leak-freeze-admin-token",
	}
	for k, v := range secrets {
		t.Setenv(k, v)
	}
	t.Setenv("MASSDRIVER_ORGANIZATION_ID", "acme")
	t.Setenv("MASSDRIVER_URL", dead)
	t.Setenv("STATE_DIR", t.TempDir())
	t.Setenv("AWS_REGION", "us-east-1")

	buf := &lockedBuffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	// Startup of every automation, with its example config, as a bot and as a webhook.
	for _, slackEnv := range []map[string]string{
		{"SLACK_BOT_TOKEN": "xoxb-leak-bot-token", "SLACK_WEBHOOK_URL": ""},
		{"SLACK_BOT_TOKEN": "", "SLACK_WEBHOOK_URL": dead + "/services/T000/B000/leak-webhook-path"},
	} {
		for k, v := range slackEnv {
			t.Setenv(k, v)
			if v != "" {
				secrets[k] = v
			}
		}
		for _, d := range automations.All {
			t.Setenv("CONFIG_FILE", filepath.Join("..", "..", "automations", d.Name, "config.example.yaml"))
			if _, err := d.New(); err != nil {
				slog.Error("exit", "automation", d.Name, "err", err)
			}
		}
	}

	// A config that names its own token variable, and a sink that fails.
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(cfgPath, []byte("splunk:\n  url: "+dead+"\n  token_env: CUSTOM_HEC_TOKEN\ndatadog:\n  url: "+dead+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CONFIG_FILE", cfgPath)
	archiver := definition(t, "audit-archiver")
	a, err := archiver.New()
	if err != nil {
		t.Fatal(err)
	}

	// Serve logs its startup and shutdown.
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- automation.Serve(ctx, "127.0.0.1:0", a) }()
	time.Sleep(100 * time.Millisecond)
	cancel()
	if err := <-served; err != nil {
		t.Fatal(err)
	}

	// One event whose sinks fail.
	body, _ := json.Marshal(map[string]any{"id": "evt-leak", "type": "project.created", "data": map[string]any{"entity": map[string]any{}}})
	req := httptest.NewRequest(http.MethodPost, automation.HookPath, bytes.NewReader(body))
	automation.SignRequest(req, signingKey, "evt-leak", body, time.Now())
	rec := httptest.NewRecorder()
	automation.Handler(a, signingKey).ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want the failed sinks to answer 500", rec.Code)
	}

	// One Slack post that fails, through the webhook.
	t.Setenv("CONFIG_FILE", filepath.Join(t.TempDir(), "missing.yaml"))
	rb, err := definition(t, "rollback-on-failure").New()
	if err != nil {
		t.Fatal(err)
	}
	failed, _ := json.Marshal(map[string]any{
		"id": "evt-rollback", "type": "deployment.failed", "subject": "mri:acme:api:prod:deployment:d1",
		"data": map[string]any{
			"entity":     map[string]any{"id": "d1", "rollback_of_deployment_id": "d0"},
			"attributes": map[string]any{"md-instance": "api-prod-db", "autorollback": "true"},
			"metadata":   map[string]any{"deployment_action": "PROVISION"},
		},
	})
	req = httptest.NewRequest(http.MethodPost, automation.HookPath, bytes.NewReader(failed))
	automation.SignRequest(req, signingKey, "evt-rollback", failed, time.Now())
	automation.Handler(rb, signingKey).ServeHTTP(httptest.NewRecorder(), req)

	logs := buf.String()
	for _, want := range []string{`"msg":"listening"`, `"msg":"event handler failed"`, `"msg":"slack post failed"`} {
		if !strings.Contains(logs, want) {
			t.Errorf("logs are missing %s", want)
		}
	}
	for name, v := range secrets {
		probe := v
		if i := strings.LastIndex(v, "/"); strings.HasPrefix(v, "http") && i > 0 {
			// A webhook URL's secret is its path; the host may be logged.
			probe = v[i+1:]
		}
		if strings.Contains(logs, probe) {
			t.Errorf("logs hold %s", name)
		}
	}
}

func definition(t *testing.T, name string) automation.Definition {
	t.Helper()
	for _, d := range automations.All {
		if d.Name == name {
			return d
		}
	}
	t.Fatalf("no automation %s", name)
	return automation.Definition{}
}
