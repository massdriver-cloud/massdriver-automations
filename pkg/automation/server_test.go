package automation

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

var testSecret = []byte("test-signing-secret")

type automationFunc func(ctx context.Context, ev Event) error

func (f automationFunc) Handle(ctx context.Context, ev Event) error { return f(ctx, ev) }

// logBuffer is safe for the server goroutine to write while the test reads.
type logBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *logBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *logBuffer) lines(t *testing.T) []map[string]any {
	t.Helper()
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(b.buf.String()), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("log line is not JSON: %q", line)
		}
		out = append(out, m)
	}
	return out
}

func (b *logBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func newTestLogger(t *testing.T, level string) (*slog.Logger, *logBuffer) {
	t.Helper()
	buf := &logBuffer{}
	l, err := NewLogger(buf, "json", level)
	if err != nil {
		t.Fatal(err)
	}
	return l.With("automation", "test-automation", "version", "1.2.3"), buf
}

func findMsg(t *testing.T, lines []map[string]any, msg string) map[string]any {
	t.Helper()
	for _, l := range lines {
		if l["msg"] == msg {
			return l
		}
	}
	t.Fatalf("no %q line in %v", msg, lines)
	return nil
}

func signedRequest(id, body string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, HookPath, strings.NewReader(body))
	SignRequest(req, testSecret, id, []byte(body), time.Now())
	return req
}

const eventBody = `{"id":"evt-1","type":"deployment.completed","data":{}}`

func TestNewLoggerRejectsUnknownSettings(t *testing.T) {
	if _, err := NewLogger(&bytes.Buffer{}, "logfmt", "info"); err == nil {
		t.Fatal("format logfmt: want error")
	}
	if _, err := NewLogger(&bytes.Buffer{}, "json", "trace"); err == nil {
		t.Fatal("level trace: want error")
	}
	if _, err := NewLogger(&bytes.Buffer{}, "TEXT", "WARN"); err != nil {
		t.Fatalf("TEXT/WARN: %v", err)
	}
}

func TestAccessLogIsJSONWithStandardKeys(t *testing.T) {
	log, buf := newTestLogger(t, "info")
	var handled map[string]any
	h := newHandler(automationFunc(func(ctx context.Context, ev Event) error {
		Logger(ctx).Info("deployment created", "deployment_id", "dep-9")
		return nil
	}), testSecret, log)

	req := signedRequest("evt-1", eventBody)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d", rec.Code)
	}

	lines := buf.lines(t)
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2: %s", len(lines), buf)
	}
	handled = findMsg(t, lines, "deployment created")
	for k, want := range map[string]any{
		"level": "INFO", "automation": "test-automation", "version": "1.2.3",
		"event_id": "evt-1", "event_type": "deployment.completed", "deployment_id": "dep-9",
	} {
		if handled[k] != want {
			t.Errorf("handler line %s = %v, want %v", k, handled[k], want)
		}
	}

	access := findMsg(t, lines, "request")
	for k, want := range map[string]any{
		"level": "INFO", "automation": "test-automation", "version": "1.2.3", "method": "POST",
		"path": HookPath, "status": float64(204), "bytes": float64(0),
		"event_id": "evt-1", "event_type": "deployment.completed",
	} {
		if access[k] != want {
			t.Errorf("access line %s = %v, want %v", k, access[k], want)
		}
	}
	if _, err := time.Parse(time.RFC3339Nano, access["time"].(string)); err != nil {
		t.Errorf("time = %v: %v", access["time"], err)
	}
	if _, ok := access["duration_ms"].(float64); !ok {
		t.Errorf("duration_ms = %v, want a number", access["duration_ms"])
	}

	for _, secret := range []string{req.Header.Get("webhook-signature"), `"data"`} {
		if strings.Contains(buf.String(), secret) {
			t.Errorf("log contains %q", secret)
		}
	}
}

func TestHealthzLogsOnlyAtDebug(t *testing.T) {
	log, buf := newTestLogger(t, "info")
	newHandler(automationFunc(nil), testSecret, log).
		ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if s := buf.String(); s != "" {
		t.Fatalf("info level logged healthz: %s", s)
	}

	log, buf = newTestLogger(t, "debug")
	newHandler(automationFunc(nil), testSecret, log).
		ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/healthz", nil))
	line := findMsg(t, buf.lines(t), "request")
	if line["level"] != "DEBUG" || line["path"] != "/healthz" || line["status"] != float64(204) {
		t.Fatalf("healthz line = %v", line)
	}
}

func TestVerifySignatureReasons(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	body := []byte(eventBody)
	ts := strconv.FormatInt(now.Unix(), 10)
	good := "v1," + Sign(testSecret, "evt-1", ts, body)
	headers := func(id, ts, sig string) http.Header {
		h := http.Header{}
		h.Set("webhook-id", id)
		h.Set("webhook-timestamp", ts)
		h.Set("webhook-signature", sig)
		return h
	}

	cases := map[string]struct {
		h    http.Header
		want error
	}{
		"valid":            {headers("evt-1", ts, good), nil},
		"no id":            {headers("", ts, good), ErrMissingHeaders},
		"no signature":     {headers("evt-1", ts, ""), ErrMissingHeaders},
		"no timestamp":     {headers("evt-1", "", good), ErrMissingHeaders},
		"bad timestamp":    {headers("evt-1", "yesterday", good), ErrBadTimestamp},
		"old timestamp":    {headers("evt-1", strconv.FormatInt(now.Add(-6*time.Minute).Unix(), 10), good), ErrTimestampOutOfRange},
		"future timestamp": {headers("evt-1", strconv.FormatInt(now.Add(6*time.Minute).Unix(), 10), good), ErrTimestampOutOfRange},
		"wrong signature":  {headers("evt-1", ts, "v1,"+Sign([]byte("other"), "evt-1", ts, body)), ErrSignatureMismatch},
	}
	for name, c := range cases {
		err := VerifySignature(testSecret, c.h, body, now)
		if !errors.Is(err, c.want) {
			t.Errorf("%s: err = %v, want %v", name, err, c.want)
		}
		if Verify(testSecret, c.h, body, now) != (c.want == nil) {
			t.Errorf("%s: Verify disagrees with VerifySignature", name)
		}
	}
}

func TestBadSignatureLogsWarnWithReason(t *testing.T) {
	log, buf := newTestLogger(t, "info")
	h := newHandler(automationFunc(func(context.Context, Event) error {
		t.Fatal("handler ran for a bad signature")
		return nil
	}), testSecret, log)

	req := signedRequest("evt-1", eventBody)
	req.Header.Set("webhook-signature", "v1,"+Sign([]byte("other"), "evt-1", req.Header.Get("webhook-timestamp"), []byte(eventBody)))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d", rec.Code)
	}
	line := findMsg(t, buf.lines(t), "signature rejected")
	if line["level"] != "WARN" || line["reason"] != "signature_mismatch" {
		t.Fatalf("line = %v", line)
	}
	if strings.Contains(buf.String(), req.Header.Get("webhook-signature")) {
		t.Fatal("log contains the signature")
	}
}

func TestDuplicateEventLogsAtDebug(t *testing.T) {
	log, buf := newTestLogger(t, "debug")
	calls := 0
	h := newHandler(automationFunc(func(context.Context, Event) error { calls++; return nil }), testSecret, log)
	for range 2 {
		h.ServeHTTP(httptest.NewRecorder(), signedRequest("evt-1", eventBody))
	}
	if calls != 1 {
		t.Fatalf("handled %d times, want 1", calls)
	}
	line := findMsg(t, buf.lines(t), "duplicate event")
	if line["level"] != "DEBUG" || line["event_id"] != "evt-1" {
		t.Fatalf("line = %v", line)
	}
}

func TestHandlerErrorLogsError(t *testing.T) {
	log, buf := newTestLogger(t, "info")
	h := newHandler(automationFunc(func(context.Context, Event) error { return errors.New("api unavailable") }), testSecret, log)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, signedRequest("evt-1", eventBody))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d", rec.Code)
	}
	line := findMsg(t, buf.lines(t), "event handler failed")
	for k, want := range map[string]any{
		"level": "ERROR", "err": "api unavailable", "event_id": "evt-1", "event_type": "deployment.completed",
	} {
		if line[k] != want {
			t.Errorf("%s = %v, want %v", k, line[k], want)
		}
	}
}

func TestPanicRespondsWith500AndLogs(t *testing.T) {
	log, buf := newTestLogger(t, "info")
	h := newHandler(automationFunc(func(context.Context, Event) error { panic("nil map write") }), testSecret, log)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, signedRequest("evt-1", eventBody))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}

	lines := buf.lines(t)
	p := findMsg(t, lines, "panic recovered")
	if p["level"] != "ERROR" || p["panic"] != "nil map write" || p["event_id"] != "evt-1" {
		t.Fatalf("panic line = %v", p)
	}
	if stack, _ := p["stack"].(string); !strings.Contains(stack, "goroutine") {
		t.Fatalf("stack = %q", stack)
	}
	if access := findMsg(t, lines, "request"); access["status"] != float64(500) {
		t.Fatalf("access status = %v, want 500", access["status"])
	}
}

func TestShutdownDrainsInFlightRequest(t *testing.T) {
	log, buf := newTestLogger(t, "info")
	entered, release := make(chan struct{}), make(chan struct{})
	h := newHandler(automationFunc(func(context.Context, Event) error {
		close(entered)
		<-release
		return nil
	}), testSecret, log)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- serve(ctx, ln, newServer(h, log), log, 5*time.Second) }()

	status := make(chan int, 1)
	go func() {
		u := url.URL{Scheme: "http", Host: ln.Addr().String(), Path: HookPath}
		req, _ := http.NewRequest(http.MethodPost, u.String(), strings.NewReader(eventBody))
		SignRequest(req, testSecret, "evt-1", []byte(eventBody), time.Now())
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Error(err)
			status <- 0
			return
		}
		resp.Body.Close()
		status <- resp.StatusCode
	}()

	<-entered
	cancel()
	select {
	case err := <-served:
		t.Fatalf("serve returned %v before the in-flight request finished", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(release)

	if code := <-status; code != http.StatusNoContent {
		t.Fatalf("in-flight request status = %d, want 204", code)
	}
	if err := <-served; err != nil {
		t.Fatalf("serve: %v", err)
	}
	lines := buf.lines(t)
	if l := findMsg(t, lines, "listening"); l["addr"] != ln.Addr().String() {
		t.Errorf("listening addr = %v", l["addr"])
	}
	findMsg(t, lines, "shutting down")
	findMsg(t, lines, "stopped")
}

func TestRedactURLKeepsOnlySchemeAndHost(t *testing.T) {
	err := &url.Error{Op: "Post", URL: "https://hooks.slack.com/services/T0/B0/secret?x=1", Err: errors.New("dial tcp: timeout")}
	got := RedactURL(err).Error()
	if strings.Contains(got, "secret") || !strings.Contains(got, "https://hooks.slack.com") {
		t.Fatalf("redacted = %q", got)
	}
	plain := errors.New("HTTP 500")
	if RedactURL(plain) != plain {
		t.Fatal("non-URL error changed")
	}
}
