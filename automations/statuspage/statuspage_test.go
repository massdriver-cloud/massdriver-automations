package statuspage

import (
	"context"
	"encoding/json"
	"html/template"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/massdriver-cloud/massdriver-automations/pkg/automation"
	"github.com/massdriver-cloud/massdriver-automations/pkg/massdriver"
)

var secret = []byte("0123456789abcdef0123456789abcdef")

var t0 = time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)

const testConfig = `
site:
  title: Acme Status
  base_url: https://status.acme.test
  timezone: UTC
  footer_links:
    - title: Support
      url: https://acme.test/support
history_days: 90
min_duration: 2m
maintenance:
  - title: Database upgrade
    description: Minor version upgrade.
    starts_at: 2026-12-01T02:00:00Z
    ends_at: 2026-12-01T04:00:00Z
    environments: [api-prod]
    components: [Database]
announcements:
  - message: New region launching soon.
    level: info
    pages: [foobar]
pages:
  - title: Foobar
    environments:
      - id: api-prod
        title: Production
        components:
          - title: Database
            match: {md-component: database}
            impact: partial_outage
          - title: API
            match: {md-component: api}
  - title: Internal
    slug: internal
    environments:
      - id: api-staging
        title: Staging
`

func attrs(project, env, component string, extra ...string) map[string]any {
	a := map[string]any{
		"md-project":     project,
		"md-environment": env,
		"md-component":   component,
		"md-instance":    project + "-" + env + "-" + component,
	}
	for i := 0; i+1 < len(extra); i += 2 {
		a[extra[i]] = extra[i+1]
	}
	return a
}

func fakeInstance(id, name, status string, a map[string]any, latest map[string]any) map[string]any {
	return map[string]any{"id": id, "name": name, "status": status, "effectiveAttributes": a, "latestDeployment": latest}
}

func fakeAlarm(id, name, status string, at time.Time) map[string]any {
	var state any
	if status != "" {
		state = map[string]any{"status": status, "message": "threshold crossed", "occurredAt": at.Format(time.RFC3339)}
	}
	return map[string]any{"id": id, "displayName": name, "currentState": state}
}

// fakeAPI serves two environments.
//
//	api-prod-database     PROVISIONED, alarm "High CPU" OK
//	api-prod-api          PROVISIONED, alarm "5xx rate" firing since 11:00
//	api-prod-secret       PROVISIONED, status_hidden=true      -> not shown
//	api-prod-old          DECOMMISSIONED                       -> not shown
//	api-staging-api       FAILED, status_title "Public API"
//	api-staging-worker    created after boot, served by instance()
//
// quiet reports the 5xx alarm as OK, as it would be after the alarm clears.
type fakeAPI struct{ quiet bool }

func (f fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Query     string         `json:"query"`
		Variables map[string]any `json:"variables"`
	}
	b, _ := io.ReadAll(r.Body)
	_ = json.Unmarshal(b, &req)
	id, _ := req.Variables["id"].(string)

	alarms := map[string][]any{
		"api-prod-database": {fakeAlarm("alarm-cpu", "High CPU", "OK", t0.Add(-24*time.Hour))},
		"api-prod-api":      {fakeAlarm("alarm-5xx", "5xx rate", "ALARM", t0.Add(-time.Hour))},
	}
	if f.quiet {
		alarms["api-prod-api"] = []any{fakeAlarm("alarm-5xx", "5xx rate", "OK", t0)}
	}

	var data any
	switch {
	case strings.Contains(req.Query, "StatusPageInstanceAlarms"):
		items := alarms[id]
		if items == nil {
			items = []any{}
		}
		data = map[string]any{"instance": map[string]any{"alarms": map[string]any{"cursor": map[string]any{"next": nil}, "items": items}}}
	case strings.Contains(req.Query, "StatusPageInstance("):
		inst := fakeInstance("api-staging-worker", "worker", "PROVISIONED", attrs("api", "staging", "worker"), nil)
		inst["environment"] = map[string]any{"id": "api-staging"}
		data = map[string]any{"instance": inst}
	case strings.Contains(req.Query, "StatusPageEnvironmentInstances"):
		envs := map[string][]any{
			"api-prod": {
				fakeInstance("api-prod-database", "database", "PROVISIONED", attrs("api", "prod", "database"), nil),
				fakeInstance("api-prod-api", "api", "PROVISIONED", attrs("api", "prod", "api"),
					map[string]any{"id": "dep-0", "status": "COMPLETED", "action": "PROVISION", "createdAt": t0.Add(-48 * time.Hour).Format(time.RFC3339)}),
				fakeInstance("api-prod-secret", "secret", "PROVISIONED", attrs("api", "prod", "secret", "status_hidden", "true"), nil),
				fakeInstance("api-prod-old", "old", "DECOMMISSIONED", attrs("api", "prod", "old"), nil),
			},
			"api-staging": {
				fakeInstance("api-staging-api", "api", "FAILED", attrs("api", "staging", "api", "status_title", "Public API"), nil),
			},
		}
		data = map[string]any{"environment": map[string]any{"instances": envs[id]}}
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
}

type harness struct {
	t     *testing.T
	sp    *StatusPage
	srv   http.Handler
	clock time.Time
}

func newHarness(t *testing.T, stateDir, outputDir string, quiet bool) *harness {
	t.Helper()
	api := httptest.NewServer(fakeAPI{quiet: quiet})
	t.Cleanup(api.Close)

	path := filepath.Join(t.TempDir(), "statuspage.yaml")
	if err := os.WriteFile(path, []byte(testConfig), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	sp, err := New(&massdriver.Client{Endpoint: api.URL, OrganizationID: "acme", APIKey: "md_test"}, cfg, stateDir, outputDir)
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t, sp: sp, clock: t0}
	sp.now = func() time.Time { return h.clock }
	sp.run = func(f func()) { f() }
	if err := sp.Bootstrap(context.Background()); err != nil {
		t.Fatal(err)
	}
	h.srv = automation.Handler(sp, secret)
	return h
}

func (h *harness) post(id, typ string, at time.Time, entity, attributes, metadata map[string]any) int {
	h.t.Helper()
	body, _ := json.Marshal(map[string]any{
		"specversion": "1.0",
		"id":          id,
		"type":        typ,
		"time":        at.Format(time.RFC3339),
		"data":        map[string]any{"entity": entity, "attributes": attributes, "metadata": metadata},
	})
	req := httptest.NewRequest(http.MethodPost, automation.HookPath, strings.NewReader(string(body)))
	automation.SignRequest(req, secret, id, body, time.Now())
	rec := httptest.NewRecorder()
	h.srv.ServeHTTP(rec, req)
	return rec.Code
}

func (h *harness) advance(d time.Duration) {
	h.clock = h.clock.Add(d)
	h.sp.render()
}

func (h *harness) get(path string) (int, string) {
	h.t.Helper()
	rec := httptest.NewRecorder()
	h.srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec.Code, rec.Body.String()
}

func (h *harness) status() jsonStatus {
	h.t.Helper()
	_, body := h.get("/api/status.json")
	var s jsonStatus
	if err := json.Unmarshal([]byte(body), &s); err != nil {
		h.t.Fatalf("status.json: %v\n%s", err, body)
	}
	return s
}

// summary flattens status.json to "page/environment/component=status" lines.
func summary(s jsonStatus) []string {
	var out []string
	for _, p := range s.Pages {
		out = append(out, p.Slug+"="+p.Status)
		for _, e := range p.Environments {
			for _, c := range e.Components {
				out = append(out, p.Slug+"/"+e.Title+"/"+c.Title+"="+c.Status)
			}
		}
	}
	return out
}

func assertLines(t *testing.T, got, want []string) {
	t.Helper()
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("got\n  %s\nwant\n  %s", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
}

func TestBootstrapLoadsCurrentState(t *testing.T) {
	h := newHarness(t, "", "", false)

	assertLines(t, summary(h.status()), []string{
		"foobar=degraded",
		"foobar/Production/Database=operational",
		"foobar/Production/API=degraded",
		"internal=partial_outage",
		"internal/Staging/Public API=partial_outage",
	})

	code, html := h.get("/")
	if code != http.StatusOK {
		t.Fatalf("GET / = %d", code)
	}
	for _, want := range []string{"Acme Status", "Degraded performance", "Production", "Database", "API", "API: degraded performance", "New region launching soon."} {
		if !strings.Contains(html, want) {
			t.Errorf("page is missing %q", want)
		}
	}
	for _, internal := range []string{"api-prod", "alarm-5xx", "5xx rate", "secret", ">old<"} {
		if strings.Contains(html, internal) {
			t.Errorf("page exposes %q", internal)
		}
	}

	if code, _ := h.get("/internal"); code != http.StatusMovedPermanently {
		t.Errorf("GET /internal = %d, want 301", code)
	}
	if _, html := h.get("/internal/"); !strings.Contains(html, "Public API") || strings.Contains(html, "New region launching soon.") {
		t.Errorf("internal page content is wrong")
	}
}

func TestEventsChangeTheSite(t *testing.T) {
	h := newHarness(t, "", "", false)
	db := attrs("api", "prod", "database")
	api := attrs("api", "prod", "api")

	if code := h.post("e1", "alarm_state.created", t0,
		map[string]any{"package_alarm_id": "alarm-5xx", "status": "OK", "alarm_name": "5xx rate", "occurred_at": t0.Format(time.RFC3339)}, api, nil); code != http.StatusNoContent {
		t.Fatalf("alarm_state.created = %d", code)
	}
	h.post("e2", "alarm_state.created", t0,
		map[string]any{"package_alarm_id": "alarm-cpu", "status": "ALARM", "alarm_name": "High CPU", "occurred_at": t0.Format(time.RFC3339)}, db, nil)

	// The alarm has fired for less than min_duration.
	assertLines(t, summary(h.status())[:3], []string{
		"foobar=operational",
		"foobar/Production/Database=operational",
		"foobar/Production/API=operational",
	})

	h.advance(3 * time.Minute)
	assertLines(t, summary(h.status())[:3], []string{
		"foobar=partial_outage",
		"foobar/Production/Database=partial_outage",
		"foobar/Production/API=operational",
	})
	s := h.status()
	inc := s.Pages[0].Incidents[0]
	if inc.Title != "Database: partial outage" || inc.Status != "ongoing" || inc.Environment != "Production" ||
		inc.StartedAt != t0 || inc.Updates[0].Message != "Partial outage." {
		t.Errorf("incident = %+v", inc)
	}

	h.post("e3", "deployment.started", t0.Add(4*time.Minute),
		map[string]any{"id": "dep-1", "status": "RUNNING", "action": "PROVISION"}, api, map[string]any{"deployment_action": "PROVISION"})
	if got := summary(h.status())[2]; got != "foobar/Production/API=maintenance" {
		t.Errorf("during deploy: %s", got)
	}
	_, html := h.get("/")
	if !strings.Contains(html, "Deployment in progress") || !strings.Contains(html, "Production · API") {
		t.Errorf("page does not list the deploy as maintenance")
	}

	h.post("e4", "deployment.failed", t0.Add(5*time.Minute),
		map[string]any{"id": "dep-1", "status": "FAILED", "action": "PROVISION"}, api, map[string]any{"deployment_action": "PROVISION"})
	// A late start must not reopen a finished deployment.
	h.post("e3-late", "deployment.started", t0.Add(4*time.Minute),
		map[string]any{"id": "dep-1", "status": "RUNNING", "action": "PROVISION"}, api, map[string]any{"deployment_action": "PROVISION"})
	if got := summary(h.status())[2]; got != "foobar/Production/API=partial_outage" {
		t.Errorf("after failed deploy: %s", got)
	}

	h.clock = t0.Add(10 * time.Minute)
	h.post("e5", "alarm_state.created", t0.Add(10*time.Minute),
		map[string]any{"package_alarm_id": "alarm-cpu", "status": "OK", "alarm_name": "High CPU", "occurred_at": t0.Add(10 * time.Minute).Format(time.RFC3339)}, db, nil)
	h.post("e6", "deployment.completed", t0.Add(10*time.Minute),
		map[string]any{"id": "dep-2", "status": "COMPLETED", "action": "PROVISION"}, api, map[string]any{"deployment_action": "PROVISION"})
	// An older ALARM state that arrives late does not override the newer OK.
	h.post("e2-late", "alarm_state.created", t0.Add(time.Minute),
		map[string]any{"package_alarm_id": "alarm-cpu", "status": "ALARM", "alarm_name": "High CPU", "occurred_at": t0.Add(time.Minute).Format(time.RFC3339)}, db, nil)

	s = h.status()
	assertLines(t, summary(s)[:3], []string{
		"foobar=operational",
		"foobar/Production/Database=operational",
		"foobar/Production/API=operational",
	})
	var resolved []string
	for _, inc := range s.Pages[0].Incidents {
		resolved = append(resolved, inc.Title+" "+inc.Status+" "+inc.ResolvedAt.Format("15:04"))
	}
	assertLines(t, resolved, []string{
		"API: partial outage resolved 12:10",
		"Database: partial outage resolved 12:10",
		"API: degraded performance resolved 12:00",
	})

	// History starts at boot (12:00). Ten minutes of partial outage in 1000 observed minutes.
	h.advance(990 * time.Minute)
	if got := *h.status().Pages[0].Environments[0].Components[0].UptimePercent; got != 99 {
		t.Errorf("database uptime = %v, want 99", got)
	}

	_, feed := h.get("/foobar/feed.atom")
	if !strings.Contains(feed, "<title>Production · Database: partial outage (resolved)</title>") {
		t.Errorf("feed:\n%s", feed)
	}
	if _, badge := h.get("/badge.svg"); !strings.Contains(badge, "status: operational") {
		t.Errorf("badge:\n%s", badge)
	}
}

func TestInstanceEvents(t *testing.T) {
	h := newHarness(t, "", "", false)

	h.post("c1", "instance.created", t0, map[string]any{"identifier": "api-staging-worker", "status": "INITIALIZED"},
		attrs("api", "staging", "worker"), nil)
	assertLines(t, summary(h.status())[3:], []string{
		"internal=partial_outage",
		"internal/Staging/Public API=partial_outage",
		"internal/Staging/worker=operational",
	})

	h.post("c2", "instance.deleted", t0, map[string]any{"identifier": "api-staging-worker"}, attrs("api", "staging", "worker"), nil)
	h.post("c3", "alarm.deleted", t0, map[string]any{"id": "alarm-5xx"}, attrs("api", "prod", "api"), nil)
	assertLines(t, summary(h.status()), []string{
		"foobar=operational",
		"foobar/Production/Database=operational",
		"foobar/Production/API=operational",
		"internal=partial_outage",
		"internal/Staging/Public API=partial_outage",
	})

	// Events from environments that no page shows change nothing.
	h.post("c4", "alarm_state.created", t0, map[string]any{"package_alarm_id": "x", "status": "ALARM", "occurred_at": t0.Format(time.RFC3339)},
		attrs("web", "prod", "db"), nil)
	if len(h.sp.instances) != 5 {
		t.Errorf("instances = %d, want 5", len(h.sp.instances))
	}
}

func TestWebhookResponses(t *testing.T) {
	h := newHarness(t, "", "", false)
	body := `{"id":"u1","type":"alarm_state.created","data":{}}`
	req := httptest.NewRequest(http.MethodPost, automation.HookPath, strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("unsigned = %d, want 401", rec.Code)
	}
	if code := h.post("p1", "project.created", t0, map[string]any{"name": "api"}, nil, nil); code != http.StatusNoContent {
		t.Errorf("project.created = %d, want 204", code)
	}
	if code, _ := h.get("/nope/"); code != http.StatusNotFound {
		t.Errorf("GET /nope/ = %d, want 404", code)
	}
}

func TestHistoryAndOutputSurviveRestart(t *testing.T) {
	stateDir, outputDir := t.TempDir(), t.TempDir()
	h := newHarness(t, stateDir, outputDir, false)
	h.post("e1", "alarm_state.created", t0,
		map[string]any{"package_alarm_id": "alarm-5xx", "status": "OK", "occurred_at": t0.Format(time.RFC3339)}, attrs("api", "prod", "api"), nil)

	restarted := newHarness(t, stateDir, outputDir, true)
	s := restarted.status()
	if len(s.Pages[0].Incidents) != 1 || s.Pages[0].Incidents[0].Title != "API: degraded performance" || s.Pages[0].Incidents[0].Status != "resolved" {
		t.Errorf("incidents after restart = %+v", s.Pages[0].Incidents)
	}

	for _, f := range []string{"index.html", "foobar/index.html", "internal/index.html", "api/status.json", "feed.atom", "badge.svg", "internal/feed.atom", "internal/badge.svg"} {
		if _, err := os.Stat(filepath.Join(outputDir, f)); err != nil {
			t.Errorf("output_dir is missing %s", f)
		}
	}
}

func TestTemplateRendersEverySection(t *testing.T) {
	tmpl := template.Must(template.New("page").Parse(defaultTemplate))
	view := PageView{
		Site:        siteView{Title: "Acme Status", Accent: "#2563eb", FooterLinks: []Link{{Title: "Support", URL: "https://acme.test"}}},
		Nav:         []navView{{Title: "Foobar", Href: "./", Current: true}, {Title: "Internal", Href: "internal/"}},
		Title:       "Foobar",
		Description: "Public services",
		Status:      majorOutage,
		Headline:    "Major outage",
		Home:        "./",
		HistoryDays: 3,
		Announcements: []announcementView{
			{Message: "Scheduled upgrade on Friday.", Level: "warning"},
		},
		Environments: []envView{{
			Title: "Production", Status: majorOutage, StatusLabel: "Major outage",
			Components: []componentView{{
				Title: "Database", Status: majorOutage, StatusLabel: "Major outage", Uptime: "99.50%",
				Days: []dayView{{Status: noData, Label: "Nov 22, 2026: no data"}, {Status: operational, Label: "Nov 23"}, {Status: majorOutage, Label: "Nov 24", Recent: true}},
			}},
		}},
		ActiveIncidents: []incidentView{{ID: "abc", Title: "Database: major outage", Impact: majorOutage, ImpactLabel: "Major outage",
			Environment: "Production", Started: "Nov 24, 2026 12:00 UTC", Duration: "5m", Updates: []updateView{{At: "12:00", Status: "ongoing", Message: "Major outage."}}}},
		PastIncidents: []incidentView{{ID: "def", Title: "API: degraded performance", Resolved: "Nov 20, 2026 13:00 UTC"}},
		Maintenance:   []maintenanceView{{Title: "Database upgrade", Window: "Dec 1, 2026 02:00 UTC to Dec 1, 2026 04:00 UTC", Scope: "Production"}},
		FeedHref:      "foobar/feed.atom",
		JSONHref:      "api/status.json",
	}
	var b strings.Builder
	if err := tmpl.Execute(&b, view); err != nil {
		t.Fatal(err)
	}
	html := b.String()
	for _, want := range []string{
		`<section class="banner major_outage" id="status" role="status">`,
		`<section class="card" id="active-incidents">`,
		`<article class="incident" id="incident-abc">`,
		`<section class="card" id="maintenance">`,
		`<span class="label maintenance">Scheduled</span> · Database upgrade`,
		`<section class="card" id="history">`,
		`<article class="incident" id="incident-def">`,
		`<span class="none" title="Nov 22, 2026: no data"></span>`,
		`<span class="major_outage recent" title="Nov 24"></span>`,
		`<span>99.50% uptime</span>`,
		`<a href="internal/">Internal</a>`,
		`<div class="notice warning" role="note">Scheduled upgrade on Friday.</div>`,
		`--accent: #2563eb;`,
		`<a href="https://acme.test">Support</a>`,
		`prefers-color-scheme: dark`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("template output is missing %s", want)
		}
	}
}

func TestLoadConfigRejectsBadConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "statuspage.yaml")
	_ = os.WriteFile(path, []byte(`
pages:
  - title: API
    slug: Bad Slug
    environments:
      - id: api-prod
        components:
          - title: Database
            impact: down
`), 0o644)
	_, err := LoadConfig(path)
	if err == nil {
		t.Fatal("want an error")
	}
	for _, want := range []string{
		`pages[0].slug "Bad Slug" must be`,
		"pages[0].environments[0].title is required",
		"pages[0].environments[0].components[0].match needs at least one attribute",
		"pages[0].environments[0].components[0].impact must be degraded, partial_outage, or major_outage",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error is missing %q:\n%v", want, err)
		}
	}
}

func TestExampleConfigLoads(t *testing.T) {
	cfg, err := LoadConfig("config.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(cfg.environmentIDs(), ","); got != "api-prod,web-prod,api-staging" {
		t.Errorf("environments = %s", got)
	}
}

func TestOutputDirFromConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("output_dir: /srv/status\npages: [{title: Acme, environments: [{id: api-prod, title: Production}]}]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.OutputDir != "/srv/status" {
		t.Fatalf("output_dir = %q", cfg.OutputDir)
	}
}
