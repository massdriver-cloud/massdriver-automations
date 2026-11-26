package costexplorer

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/massdriver-cloud/massdriver-automations/pkg/automation"
	"github.com/massdriver-cloud/massdriver-automations/pkg/massdriver"
)

func cost(amount any) map[string]any {
	if amount == nil {
		return map[string]any{"amount": nil, "currency": nil}
	}
	return map[string]any{"amount": amount, "currency": "USD"}
}

func instance(id, name string, attrs map[string]any, day, month any) map[string]any {
	return map[string]any{
		"id": id, "name": name, "status": "PROVISIONED", "effectiveAttributes": attrs,
		"bundle": map[string]any{"name": "aws-rds-postgres"},
		"cost": map[string]any{
			"lastDay": cost(day), "dailyAverage": cost(day), "lastMonth": cost(month), "monthlyAverage": cost(month),
		},
	}
}

// fakeAPI serves two pages of instances.
func fakeAPI(t *testing.T) *massdriver.Client {
	t.Helper()
	pages := [][]any{
		{instance("api-prod-db", "api-prod-db", map[string]any{
			"md-project": "api", "md-environment": "prod", "md-component": "db", "team": "payments", "secret-ish": "x",
		}, 12.5, 380.0)},
		{instance("api-dev-db", "api-dev-db", map[string]any{
			"md-project": "api", "md-environment": "dev", "md-component": "db",
		}, nil, nil)},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Variables map[string]any `json:"variables"`
		}
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &req)
		page, next := 0, any("p2")
		if c, _ := req.Variables["cursor"].(map[string]any); c["next"] == "p2" {
			page, next = 1, nil
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"instances": map[string]any{
			"cursor": map[string]any{"next": next}, "items": pages[page],
		}}})
	}))
	t.Cleanup(srv.Close)
	return &massdriver.Client{Endpoint: srv.URL, OrganizationID: "acme", APIKey: "md_test"}
}

func newExplorer(t *testing.T, cfg Config, password, dir string) *Explorer {
	t.Helper()
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	e, err := New(fakeAPI(t), cfg, password, dir)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func get(t *testing.T, h http.Handler, path, user, pass string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if user != "" {
		req.SetBasicAuth(user, pass)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestRefreshServesConfiguredDimensionsAndLinks(t *testing.T) {
	e := newExplorer(t, Config{Dimensions: []string{"md-project", "team", "bundle"}}, "hunter2", t.TempDir())
	if err := e.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	rec := get(t, automation.Handler(e, []byte("whsec")), "/api/costs.json", "massdriver", "hunter2")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var s struct {
		Instances []Row `json:"instances"`
		History   []day `json:"history"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &s); err != nil {
		t.Fatal(err)
	}
	if len(s.Instances) != 2 {
		t.Fatalf("instances = %d, want 2 across both pages", len(s.Instances))
	}
	prod := s.Instances[1]
	if prod.ID != "api-prod-db" || *prod.LastMonth != 380 || prod.Currency != "USD" {
		t.Errorf("prod = %+v", prod)
	}
	want := map[string]string{"md-project": "api", "team": "payments", "bundle": "aws-rds-postgres"}
	if len(prod.Dimensions) != len(want) || prod.Dimensions["team"] != "payments" || prod.Dimensions["bundle"] != "aws-rds-postgres" {
		t.Errorf("dimensions = %v, want %v and no other attributes", prod.Dimensions, want)
	}
	if prod.URL != "https://app.massdriver.cloud/orgs/acme/projects/api/environments/prod/instances/db" {
		t.Errorf("url = %s", prod.URL)
	}
	if dev := s.Instances[0]; dev.LastMonth != nil || dev.Currency != "" {
		t.Errorf("dev without billing data = %+v", dev)
	}
	if len(s.History) != 1 || s.History[0].Costs["api-prod-db"] != 12.5 || len(s.History[0].Costs) != 1 {
		t.Errorf("history = %+v", s.History)
	}
}

func TestHistoryKeepsOneSamplePerDayAndDropsOldDays(t *testing.T) {
	dir := t.TempDir()
	e := newExplorer(t, Config{HistoryDays: 2}, "pw", dir)
	for _, ts := range []string{"2026-11-20T10:00:00Z", "2026-11-21T09:00:00Z", "2026-11-21T23:00:00Z", "2026-11-22T01:00:00Z"} {
		now, _ := time.Parse(time.RFC3339, ts)
		e.now = func() time.Time { return now }
		if err := e.Refresh(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	h, err := loadHistory(dir)
	if err != nil {
		t.Fatal(err)
	}
	var dates []string
	for _, d := range h.Days {
		dates = append(dates, d.Date)
	}
	if strings.Join(dates, ",") != "2026-11-21,2026-11-22" {
		t.Errorf("days = %v", dates)
	}
	if _, ok := h.Instances["api-prod-db"]; !ok || len(h.Instances) != 1 {
		t.Errorf("instances = %v", h.Instances)
	}
}

func TestDashboardNeedsThePassword(t *testing.T) {
	h := automation.Handler(newExplorer(t, Config{}, "hunter2", ""), []byte("whsec"))
	for _, c := range []struct{ user, pass string }{{"", ""}, {"massdriver", "wrong"}, {"admin", "hunter2"}} {
		for _, path := range []string{"/", "/api/costs.json"} {
			if rec := get(t, h, path, c.user, c.pass); rec.Code != http.StatusUnauthorized || rec.Header().Get("www-authenticate") == "" {
				t.Errorf("%s as %q/%q = %d", path, c.user, c.pass, rec.Code)
			}
		}
	}
	rec := get(t, h, "/", "massdriver", "hunter2")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "api/costs.json") {
		t.Errorf("dashboard = %d", rec.Code)
	}
	if rec := get(t, h, "/nope", "massdriver", "hunter2"); rec.Code != http.StatusNotFound {
		t.Errorf("unknown path = %d", rec.Code)
	}
}

func TestPublicDashboardNeedsNoPassword(t *testing.T) {
	h := automation.Handler(newExplorer(t, Config{Public: true}, "", ""), []byte("whsec"))
	if rec := get(t, h, "/api/costs.json", "", ""); rec.Code != http.StatusOK {
		t.Errorf("status = %d", rec.Code)
	}
}

func TestHandleAsksForARefreshOnProvisionAndDecommission(t *testing.T) {
	e := newExplorer(t, Config{}, "pw", "")
	for _, c := range []struct {
		typ, action string
		want        bool
	}{
		{"deployment.completed", "PLAN", false},
		{"deployment.failed", "PROVISION", false},
		{"deployment.completed", "PROVISION", true},
		{"deployment.completed", "DECOMMISSION", true},
	} {
		ev := automation.Event{Type: c.typ, Data: automation.EventData{Metadata: map[string]any{"deployment_action": c.action}}}
		if err := e.Handle(context.Background(), ev); err != nil {
			t.Fatal(err)
		}
		got := false
		select {
		case <-e.refresh:
			got = true
		default:
		}
		if got != c.want {
			t.Errorf("%s %s refresh = %v", c.typ, c.action, got)
		}
	}
}

func TestExampleConfigLoads(t *testing.T) {
	var cfg Config
	if err := automation.LoadConfigFile("config.example.yaml", &cfg); err != nil {
		t.Fatal(err)
	}
	if len(cfg.Dimensions) != 6 || time.Duration(cfg.Refresh) != time.Hour || cfg.HistoryDays != 90 {
		t.Errorf("cfg = %+v", cfg)
	}
}

func TestConfigRejectsBadValues(t *testing.T) {
	for _, c := range []Config{
		{Refresh: Duration(10 * time.Second)},
		{HistoryDays: 1000},
		{Dimensions: []string{"team", "team"}},
	} {
		if err := c.Validate(); err == nil {
			t.Errorf("%+v: want an error", c)
		}
	}
}
