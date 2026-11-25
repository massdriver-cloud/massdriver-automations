package router

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/massdriver-cloud/massdriver-automations/pkg/automation"
	"github.com/massdriver-cloud/massdriver-automations/pkg/massdriver"
)

var secret = []byte("0123456789abcdef0123456789abcdef")

var instanceAttrs = map[string]any{
	"md-project":     "api",
	"md-environment": "prod",
	"md-component":   "database",
	"md-instance":    "api-prod-database",
}

func event(id, typ string, entity, attrs, metadata map[string]any) []byte {
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

func deployment(id, typ, action string, attrs map[string]any) []byte {
	return event(id, typ,
		map[string]any{"id": "b9dc3d18-19fc-429c-8d36-6753615fe17c", "action": action, "status": "FAILED", "version": "1.2.3"},
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

func drain(r *Router) {
	for {
		select {
		case j := <-r.queue:
			r.process(j)
		default:
			return
		}
	}
}

type request struct {
	Method, Path, Auth, ContentType, Body string
}

// fakeService records requests and answers with the next status, or 204.
type fakeService struct {
	mu       sync.Mutex
	requests []request
	statuses []int
}

func (f *fakeService) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, request{r.Method, r.URL.Path, r.Header.Get("authorization"), r.Header.Get("content-type"), string(b)})
	status := http.StatusNoContent
	if len(f.statuses) > 0 {
		status, f.statuses = f.statuses[0], f.statuses[1:]
	}
	w.WriteHeader(status)
}

func (f *fakeService) paths() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var p []string
	for _, r := range f.requests {
		p = append(p, r.Path)
	}
	return p
}

type graphqlCall struct {
	Query     string         `json:"query"`
	Variables map[string]any `json:"variables"`
}

type fakeMassdriver struct {
	mu       sync.Mutex
	calls    []graphqlCall
	response string
}

func (f *fakeMassdriver) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var c graphqlCall
	_ = json.NewDecoder(r.Body).Decode(&c)
	f.mu.Lock()
	f.calls = append(f.calls, c)
	f.mu.Unlock()
	w.Header().Set("content-type", "application/json")
	_, _ = io.WriteString(w, f.response)
}

func setup(t *testing.T, src string, md *massdriver.Client) (*Router, http.Handler) {
	t.Helper()
	cfg, err := ParseConfig([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	r := New(cfg, md)
	return r, automation.Handler(r, secret)
}

func TestRoutesMatchByTypeGlobAttributesAndWhen(t *testing.T) {
	svc := &fakeService{}
	srv := httptest.NewServer(svc)
	defer srv.Close()

	_, h := setup(t, `
routes:
  - name: exact
    events: [deployment.failed]
    do:
      - http: {url: '`+srv.URL+`/exact'}
  - name: glob
    events: [instance.*]
    do:
      - http: {url: '`+srv.URL+`/glob'}
  - name: prod-only
    events: [deployment.failed]
    match: {md-environment: prod}
    do:
      - http: {url: '`+srv.URL+`/prod-only'}
  - name: provision-only
    events: [deployment.failed]
    when: '.data.metadata.deployment_action == "PROVISION"'
    do:
      - http: {url: '`+srv.URL+`/provision-only'}
`, nil)

	staging := map[string]any{"md-environment": "staging"}
	for _, body := range [][]byte{
		deployment("evt-1", "deployment.failed", "PROVISION", instanceAttrs),
		deployment("evt-2", "deployment.failed", "PLAN", staging),
		event("evt-3", "instance.deleted", map[string]any{"identifier": "api-prod-database"}, instanceAttrs, map[string]any{}),
		event("evt-4", "environment.deleted", map[string]any{"identifier": "api-prod"}, instanceAttrs, map[string]any{}),
	} {
		if code := post(t, h, body); code != http.StatusNoContent {
			t.Fatalf("status = %d", code)
		}
	}

	got := svc.paths()
	want := map[string]int{"/exact": 2, "/prod-only": 1, "/provision-only": 1, "/glob": 1}
	counts := map[string]int{}
	for _, p := range got {
		counts[p]++
	}
	if !reflect.DeepEqual(counts, want) {
		t.Fatalf("requests = %v, want %v", counts, want)
	}
}

func TestHTTPStepBuildsRequestWithJQ(t *testing.T) {
	svc := &fakeService{}
	srv := httptest.NewServer(svc)
	defer srv.Close()
	t.Setenv("PAGER_TOKEN", "pager-secret")
	t.Setenv("MASSDRIVER_API_KEY", "md_should-not-leak")

	_, h := setup(t, `
routes:
  - name: page
    events: [deployment.failed]
    do:
      - http:
          url: '`+srv.URL+`/incidents/\(.data.attributes["md-instance"])'
          headers:
            Authorization: 'Token \(env.PAGER_TOKEN)'
          body: '{summary: "\(.data.attributes["md-instance"]) failed", event: .id, leaked: $ENV.MASSDRIVER_API_KEY}'
`, nil)

	if code := post(t, h, deployment("evt-1", "deployment.failed", "PROVISION", instanceAttrs)); code != http.StatusNoContent {
		t.Fatalf("status = %d", code)
	}

	want := request{
		Method:      "POST",
		Path:        "/incidents/api-prod-database",
		Auth:        "Token pager-secret",
		ContentType: "application/json",
		Body:        `{"event":"evt-1","leaked":null,"summary":"api-prod-database failed"}`,
	}
	if len(svc.requests) != 1 || svc.requests[0] != want {
		t.Fatalf("requests = %+v\nwant %+v", svc.requests, want)
	}
}

func TestHTTPStepBuildsRequestWithGoTemplates(t *testing.T) {
	svc := &fakeService{}
	srv := httptest.NewServer(svc)
	defer srv.Close()
	t.Setenv("DNS_TOKEN", "dns-secret")
	t.Setenv("MASSDRIVER_SIGNING_SECRET", "whsec_should-not-leak")

	_, h := setup(t, `
engine: go
routes:
  - name: release-dns
    events: [deployment.completed]
    when: '{{ eq .data.metadata.deployment_action "DECOMMISSION" }}'
    do:
      - http:
          method: DELETE
          url: '`+srv.URL+`/records/{{ attr "md-instance" }}'
          headers:
            Authorization: 'Bearer {{ env "DNS_TOKEN" }}{{ env "MASSDRIVER_SIGNING_SECRET" }}'
          body: '{"instance": {{ json (attr "md-instance") }}, "event": {{ json .id }}}'
`, nil)

	for _, body := range [][]byte{
		deployment("evt-1", "deployment.completed", "PROVISION", instanceAttrs),
		deployment("evt-2", "deployment.completed", "DECOMMISSION", instanceAttrs),
	} {
		if code := post(t, h, body); code != http.StatusNoContent {
			t.Fatalf("status = %d", code)
		}
	}

	want := request{
		Method:      "DELETE",
		Path:        "/records/api-prod-database",
		Auth:        "Bearer dns-secret",
		ContentType: "application/json",
		Body:        `{"instance": "api-prod-database", "event": "evt-2"}`,
	}
	if len(svc.requests) != 1 || svc.requests[0] != want {
		t.Fatalf("requests = %+v\nwant %+v", svc.requests, want)
	}
}

func TestExecStepGetsTheEventWithoutCredentials(t *testing.T) {
	out := filepath.Join(t.TempDir(), "out")
	t.Setenv("MASSDRIVER_SIGNING_SECRET", "whsec_should-not-leak")
	t.Setenv("MASSDRIVER_API_KEY", "md_should-not-leak")
	t.Setenv("BUCKET_REGION", "us-west-2")

	_, h := setup(t, `
routes:
  - name: purge-cache
    events: [instance.deleted]
    do:
      - exec:
          - /bin/sh
          - -c
          - 'echo "$MD_EVENT_ID $MD_EVENT_TYPE $MD_ATTR_MD_INSTANCE $1 $BUCKET_REGION secret=$MASSDRIVER_SIGNING_SECRET key=$MASSDRIVER_API_KEY" > "$0"; cat >> "$0"'
          - '`+out+`'
          - '\(.data.entity.identifier)'
`, nil)

	body := event("evt-1", "instance.deleted", map[string]any{"identifier": "api-prod-database"}, instanceAttrs, map[string]any{})
	if code := post(t, h, body); code != http.StatusNoContent {
		t.Fatalf("status = %d", code)
	}

	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	firstLine, stdin, _ := strings.Cut(string(got), "\n")
	if firstLine != "evt-1 instance.deleted api-prod-database api-prod-database us-west-2 secret= key=" {
		t.Errorf("first line = %q", firstLine)
	}
	if stdin != string(body) {
		t.Errorf("stdin = %q, want the event", stdin)
	}
}

const createDeployment = `mutation($org: ID!, $id: ID!, $input: CreateDeploymentInput!) {
  createDeployment(organizationId: $org, id: $id, input: $input) { successful messages { field message } }
}`

func massdriverClient(url string) *massdriver.Client {
	return &massdriver.Client{Endpoint: url + "/api/v2", OrganizationID: "acme", APIKey: "md_test"}
}

func TestMassdriverStepSendsVariablesWithOrg(t *testing.T) {
	for engine, variables := range map[string]string{
		"jq": `{id: .data.entity.identifier, input: {action: "PLAN", params: (.data.entity.params | tojson), message: "Version changed"}}`,
		"go": `{"id": {{ json .data.entity.identifier }}, "input": {"action": "PLAN", "params": {{ json (json .data.entity.params) }}, "message": "Version changed"}}`,
	} {
		t.Run(engine, func(t *testing.T) {
			md := &fakeMassdriver{response: `{"data": {"createDeployment": {"successful": true, "messages": []}}}`}
			srv := httptest.NewServer(md)
			defer srv.Close()

			_, h := setup(t, `
engine: `+engine+`
routes:
  - name: plan-on-update
    events: [instance.updated]
    do:
      - massdriver:
          query: '`+createDeployment+`'
          variables: '`+variables+`'
`, massdriverClient(srv.URL))

			body := event("evt-1", "instance.updated",
				map[string]any{"identifier": "api-prod-database", "params": map[string]any{"size": "large"}}, instanceAttrs, map[string]any{})
			if code := post(t, h, body); code != http.StatusNoContent {
				t.Fatalf("status = %d", code)
			}

			want := map[string]any{
				"org":   "acme",
				"id":    "api-prod-database",
				"input": map[string]any{"action": "PLAN", "params": `{"size":"large"}`, "message": "Version changed"},
			}
			if len(md.calls) != 1 || !reflect.DeepEqual(md.calls[0].Variables, want) {
				t.Fatalf("calls = %+v", md.calls)
			}
		})
	}
}

func TestMassdriverStepFailsOnUnsuccessfulPayload(t *testing.T) {
	md := &fakeMassdriver{response: `{"data": {"createDeployment": {"successful": false, "messages": [{"field": "params", "message": "is invalid"}]}}}`}
	srv := httptest.NewServer(md)
	defer srv.Close()

	r, _ := setup(t, `
routes:
  - name: plan
    events: [instance.updated]
    do:
      - massdriver:
          query: '`+createDeployment+`'
          variables: '{id: "api-prod-database", input: {action: "PLAN", params: "{}"}}'
`, massdriverClient(srv.URL))

	body := event("evt-1", "instance.updated", map[string]any{"identifier": "api-prod-database"}, instanceAttrs, map[string]any{})
	var ev automation.Event
	_ = json.Unmarshal(body, &ev)
	ev.Raw = body

	err := r.Handle(t.Context(), ev)
	if err == nil || err.Error() != `route "plan": step 1 (massdriver): createDeployment: params: is invalid` {
		t.Fatalf("err = %v", err)
	}
}

func TestStepsRunInOrderAndStopAtFailure(t *testing.T) {
	svc := &fakeService{statuses: []int{http.StatusOK, http.StatusBadGateway}}
	srv := httptest.NewServer(svc)
	defer srv.Close()

	_, h := setup(t, `
routes:
  - name: chain
    events: [deployment.failed]
    do:
      - http: {url: '`+srv.URL+`/first'}
      - http: {url: '`+srv.URL+`/second'}
      - http: {url: '`+srv.URL+`/third'}
`, nil)

	if code := post(t, h, deployment("evt-1", "deployment.failed", "PROVISION", instanceAttrs)); code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", code)
	}
	if got := svc.paths(); !reflect.DeepEqual(got, []string{"/first", "/second"}) {
		t.Fatalf("requests = %v", got)
	}
}

func TestSyncFailureRespondsErrorAndRetryRunsOnlyFailedRoutes(t *testing.T) {
	var mu sync.Mutex
	counts := map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		counts[r.URL.Path]++
		first := counts[r.URL.Path] == 1
		mu.Unlock()
		if r.URL.Path == "/flaky" && first {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	_, h := setup(t, `
routes:
  - name: notify
    events: [deployment.failed]
    do:
      - http: {url: '`+srv.URL+`/notify'}
  - name: flaky
    events: [deployment.failed]
    do:
      - http: {url: '`+srv.URL+`/flaky'}
`, nil)

	body := deployment("evt-1", "deployment.failed", "PROVISION", instanceAttrs)
	if code := post(t, h, body); code != http.StatusInternalServerError {
		t.Fatalf("first status = %d, want 500", code)
	}
	if code := post(t, h, body); code != http.StatusNoContent {
		t.Fatalf("retry status = %d, want 204", code)
	}
	if want := map[string]int{"/notify": 1, "/flaky": 2}; !reflect.DeepEqual(counts, want) {
		t.Fatalf("requests = %v, want %v", counts, want)
	}
}

func TestAsyncRouteRunsAfterTheResponse(t *testing.T) {
	svc := &fakeService{statuses: []int{http.StatusInternalServerError}}
	srv := httptest.NewServer(svc)
	defer srv.Close()

	r, h := setup(t, `
routes:
  - name: slow-cleanup
    events: [environment.deleted]
    async: true
    timeout: 5m
    do:
      - http: {url: '`+srv.URL+`/cleanup/\(.data.entity.identifier)'}
`, nil)

	body := event("evt-1", "environment.deleted", map[string]any{"identifier": "api-prod"}, map[string]any{}, map[string]any{})
	if code := post(t, h, body); code != http.StatusNoContent {
		t.Fatalf("status = %d", code)
	}
	if len(svc.paths()) != 0 {
		t.Fatalf("ran before the response: %v", svc.paths())
	}
	drain(r)
	if got := svc.paths(); !reflect.DeepEqual(got, []string{"/cleanup/api-prod"}) {
		t.Fatalf("requests = %v", got)
	}
	if code := post(t, h, body); code != http.StatusNoContent || len(r.queue) != 0 {
		t.Fatalf("repeat status = %d, queued %d; a failed async route does not run again", code, len(r.queue))
	}
}

func TestRouteStopsAtTimeout(t *testing.T) {
	r, _ := setup(t, `
routes:
  - name: slow
    events: [environment.deleted]
    timeout: 100ms
    do:
      - exec: [/bin/sleep, "5"]
`, nil)
	body := event("evt-1", "environment.deleted", map[string]any{"identifier": "api-prod"}, map[string]any{}, map[string]any{})
	var ev automation.Event
	_ = json.Unmarshal(body, &ev)
	ev.Raw = body

	start := time.Now()
	err := r.Handle(t.Context(), ev)
	if err == nil || !strings.Contains(err.Error(), "route timed out after 100ms") {
		t.Fatalf("err = %v", err)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Errorf("took %s", d)
	}
}

func TestParseConfigRejectsMistakes(t *testing.T) {
	step := "    do: [{exec: [true]}]\n"
	cases := map[string]string{
		"unknown engine":    "engine: lua\nroutes:\n  - name: a\n    events: [instance.deleted]\n" + step,
		"no routes":         "routes: []\n",
		"no events":         "routes:\n  - name: a\n" + step,
		"bad event":         "routes:\n  - name: a\n    events: [instance]\n" + step,
		"no steps":          "routes:\n  - name: a\n    events: [instance.deleted]\n",
		"two actions":       "routes:\n  - name: a\n    events: [instance.deleted]\n    do: [{exec: [true], http: {url: 'https://x'}}]\n",
		"unknown field":     "routes:\n  - name: a\n    event: [instance.deleted]\n" + step,
		"same name":         "routes:\n  - name: a\n    events: [x.y]\n" + step + "  - name: a\n    events: [x.y]\n" + step,
		"bad jq when":       "routes:\n  - name: a\n    events: [x.y]\n    when: '.data |'\n" + step,
		"bad jq body":       "routes:\n  - name: a\n    events: [x.y]\n    do: [{http: {url: 'https://x', body: '{a: }'}}]\n",
		"bad jq variable":   "routes:\n  - name: a\n    events: [x.y]\n    when: '$nope'\n" + step,
		"bad jq url":        "routes:\n  - name: a\n    events: [x.y]\n    do: [{http: {url: 'https://x/\\(.id'}}]\n",
		"bad go template":   "engine: go\nroutes:\n  - name: a\n    events: [x.y]\n    do: [{http: {url: 'https://x', body: '{{ .id'}}]\n",
		"unknown go func":   "engine: go\nroutes:\n  - name: a\n    events: [x.y]\n    when: '{{ nope .id }}'\n" + step,
		"long sync timeout": "routes:\n  - name: a\n    events: [x.y]\n    timeout: 9s\n" + step,
		"empty query":       "routes:\n  - name: a\n    events: [x.y]\n    do: [{massdriver: {query: ''}}]\n",
	}
	for name, src := range cases {
		if _, err := ParseConfig([]byte(src)); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

func TestExamplesParse(t *testing.T) {
	files, _ := filepath.Glob("examples/*.yaml")
	if len(files) == 0 {
		t.Fatal("no examples")
	}
	for _, f := range append(files, "config.example.yaml") {
		var cfg Config
		if err := automation.LoadConfigFile(f, &cfg); err != nil {
			t.Errorf("%s: %v", f, err)
		}
	}
}

func TestNoConfigFileRefusesStart(t *testing.T) {
	t.Setenv("CONFIG_FILE", filepath.Join(t.TempDir(), "missing.yaml"))
	_, err := Definition.New()
	var ce *automation.ConfigError
	if !errors.As(err, &ce) || !strings.HasSuffix(err.Error(), "routes: at least one route is required") {
		t.Fatalf("err = %v", err)
	}
}

func TestFailedStepLogsOnlyTheOutputTailAtError(t *testing.T) {
	_, h := setup(t, `
routes:
  - name: noisy
    events: [instance.deleted]
    do:
      - exec: [/bin/sh, -c, 'printf "printed-token-%01000d-last-line" 0; exit 3']
`, nil)

	run := func(level slog.Level) string {
		var buf bytes.Buffer
		prev := slog.Default()
		slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: level})))
		defer slog.SetDefault(prev)
		body := event("evt-"+level.String(), "instance.deleted", map[string]any{"identifier": "x"}, instanceAttrs, map[string]any{})
		if code := post(t, h, body); code != http.StatusInternalServerError {
			t.Fatalf("status = %d", code)
		}
		return buf.String()
	}

	info := run(slog.LevelInfo)
	if strings.Contains(info, "printed-token") || !strings.Contains(info, "-last-line") || !strings.Contains(info, "exit status 3") {
		t.Fatalf("info logs = %s", info)
	}
	if debug := run(slog.LevelDebug); !strings.Contains(debug, "printed-token") {
		t.Fatalf("debug logs = %s", debug)
	}
}
