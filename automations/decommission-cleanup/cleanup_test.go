package decommissioncleanup

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/massdriver-cloud/massdriver-automations/pkg/automation"
)

var secret = []byte("0123456789abcdef0123456789abcdef")

var instanceAttrs = map[string]any{
	"md-project":     "api",
	"md-environment": "prod",
	"md-component":   "database",
	"md-instance":    "api-prod-database",
}

func event(id, typ string, entity map[string]any, attrs map[string]any, metadata map[string]any) []byte {
	b, _ := json.Marshal(map[string]any{
		"specversion": "1.0",
		"id":          id,
		"type":        typ,
		"subject":     "mri:acme:api:prod:database",
		"time":        "2026-11-24T05:54:03Z",
		"data":        map[string]any{"entity": entity, "attributes": attrs, "metadata": metadata},
	})
	return b
}

func decommissioned(id, action string, attrs map[string]any) []byte {
	return event(id, "deployment.completed",
		map[string]any{"id": "b9dc3d18-19fc-429c-8d36-6753615fe17c", "action": action, "status": "COMPLETED"},
		attrs, map[string]any{"deployment_action": action})
}

func post(t *testing.T, h http.Handler, body []byte) int {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, automation.HookPath, strings.NewReader(string(body)))
	var ev struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(body, &ev)
	automation.SignRequest(req, secret, ev.ID, body, time.Now())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code
}

func drain(c *Cleanup) {
	for {
		select {
		case j := <-c.queue:
			c.process(j)
		default:
			return
		}
	}
}

type request struct {
	Method, Path, Auth, Body string
}

type fakeDNS struct {
	mu       sync.Mutex
	requests []request
}

func (f *fakeDNS) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.requests = append(f.requests, request{r.Method, r.URL.Path, r.Header.Get("authorization"), string(b)})
	f.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

func setup(t *testing.T, yamlSrc string) (*Cleanup, http.Handler) {
	t.Helper()
	hooks, err := ParseHooks([]byte(yamlSrc))
	if err != nil {
		t.Fatal(err)
	}
	c := New(hooks)
	return c, automation.Handler(automationName, c, secret)
}

func TestHTTPHookRunsForMatchingDecommission(t *testing.T) {
	dns := &fakeDNS{}
	srv := httptest.NewServer(dns)
	defer srv.Close()
	t.Setenv("DNS_TOKEN", "dns-secret")

	c, h := setup(t, `
hooks:
  - name: release-dns
    events: [deployment.completed, instance.deleted]
    match:
      md-environment: prod
    http:
      method: DELETE
      url: '`+srv.URL+`/records/{{ attr "md-instance" }}'
      headers:
        Authorization: 'Bearer {{ env "DNS_TOKEN" }}'
      body: '{"instance": {{ json (attr "md-instance") }}, "event": {{ json .id }}}'
`)

	staging := map[string]any{"md-environment": "staging", "md-instance": "api-staging-database"}
	for _, body := range [][]byte{
		decommissioned("evt-1", "DECOMMISSION", instanceAttrs),
		decommissioned("evt-2", "PROVISION", instanceAttrs),
		decommissioned("evt-3", "DECOMMISSION", staging),
	} {
		if code := post(t, h, body); code != http.StatusNoContent {
			t.Fatalf("status = %d", code)
		}
	}
	drain(c)

	want := []request{{
		Method: "DELETE",
		Path:   "/records/api-prod-database",
		Auth:   "Bearer dns-secret",
		Body:   `{"instance": "api-prod-database", "event": "evt-1"}`,
	}}
	if len(dns.requests) != 1 || dns.requests[0] != want[0] {
		t.Fatalf("requests = %+v\nwant %+v", dns.requests, want)
	}
}

func TestExecHookGetsTheEvent(t *testing.T) {
	out := filepath.Join(t.TempDir(), "out")
	t.Setenv("MASSDRIVER_SIGNING_SECRET", "whsec_should-not-leak")

	c, h := setup(t, `
hooks:
  - name: purge-cache
    events: [instance.*]
    exec:
      command:
        - /bin/sh
        - -c
        - 'echo "$MD_EVENT_ID $MD_EVENT_TYPE $MD_ATTR_MD_INSTANCE $1 secret=$MASSDRIVER_SIGNING_SECRET" > "$0"; cat >> "$0"'
        - '`+out+`'
        - '{{ .data.entity.identifier }}'
`)

	body := event("evt-1", "instance.deleted", map[string]any{"identifier": "api-prod-database"}, instanceAttrs, map[string]any{})
	if code := post(t, h, body); code != http.StatusNoContent {
		t.Fatalf("status = %d", code)
	}
	if code := post(t, h, body); code != http.StatusNoContent {
		t.Fatalf("repeat status = %d", code)
	}
	if len(c.queue) != 1 {
		t.Fatalf("queued %d jobs, want 1 for a repeated event", len(c.queue))
	}
	drain(c)

	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	firstLine, stdin, _ := strings.Cut(string(got), "\n")
	if firstLine != "evt-1 instance.deleted api-prod-database api-prod-database secret=" {
		t.Errorf("first line = %q", firstLine)
	}
	if stdin != string(body) {
		t.Errorf("stdin = %q, want the event", stdin)
	}
}

func TestExecHookStopsAtTimeout(t *testing.T) {
	c, _ := setup(t, `
hooks:
  - name: slow
    events: [environment.deleted]
    timeout: 100ms
    exec:
      command: [/bin/sleep, "5"]
`)
	body := event("evt-1", "environment.deleted", map[string]any{"identifier": "api-prod"}, map[string]any{}, map[string]any{})
	var ev automation.Event
	_ = json.Unmarshal(body, &ev)
	ev.Raw = body

	start := time.Now()
	_, err := c.run(c.hooks[0], ev, map[string]any{})
	if err == nil || err.Error() != "timed out after 100ms" {
		t.Fatalf("err = %v", err)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Errorf("took %s", d)
	}
}

func TestMissingAttributeSkipsTheRequest(t *testing.T) {
	dns := &fakeDNS{}
	srv := httptest.NewServer(dns)
	defer srv.Close()

	c, h := setup(t, `
hooks:
  - name: release-dns
    events: [environment.deleted]
    http:
      url: '`+srv.URL+`/records/{{ attr "md-instance" }}'
`)
	post(t, h, event("evt-1", "environment.deleted", map[string]any{"identifier": "api-prod"}, map[string]any{"md-environment": "prod"}, map[string]any{}))
	drain(c)

	if len(dns.requests) != 0 {
		t.Fatalf("requests = %+v, want none", dns.requests)
	}
}

func TestParseHooksRejectsMistakes(t *testing.T) {
	cases := map[string]string{
		"both types":    "hooks:\n  - name: a\n    events: [instance.deleted]\n    http: {url: 'https://x'}\n    exec: {command: [true]}\n",
		"no type":       "hooks:\n  - name: a\n    events: [instance.deleted]\n",
		"no events":     "hooks:\n  - name: a\n    exec: {command: [true]}\n",
		"unknown field": "hooks:\n  - name: a\n    event: [instance.deleted]\n    exec: {command: [true]}\n",
		"bad template":  "hooks:\n  - name: a\n    events: [instance.deleted]\n    exec: {command: ['{{ .id'] }\n",
		"same name":     "hooks:\n  - name: a\n    events: [x.y]\n    exec: {command: [true]}\n  - name: a\n    events: [x.y]\n    exec: {command: [true]}\n",
	}
	for name, src := range cases {
		if _, err := ParseHooks([]byte(src)); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}
