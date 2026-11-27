package statuspage

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const stashConfig = testConfig + `
stash:
  health: true
  banners: true
`

func newStashHarness(t *testing.T, config string, entries ...map[string]any) (*harness, *fakeStash) {
	t.Helper()
	fs := &fakeStash{}
	fs.set(entries...)
	return newHarnessWith(t, config, fakeAPI{stash: fs}, "", ""), fs
}

// poll is one refresh of the 30-second ticker.
func (h *harness) poll() {
	h.sp.refreshStash(context.Background())
	h.sp.render()
}

func health(id, value string, at time.Time) map[string]any {
	return stashEntryJSON("INSTANCE", id, healthKey, value, at, nil)
}

func stashEvent(key, kind, id string, value any, expires *time.Time) map[string]any {
	e := map[string]any{"key": key, "scope": map[string]any{"kind": kind, "id": id}, "format": "text", "value": value}
	if expires != nil {
		e["expires_at"] = expires.Format(time.RFC3339)
	}
	return e
}

func envAttrs(project, env string) map[string]any {
	return map[string]any{"md-project": project, "md-environment": env}
}

func TestStashHealthDownAndBackWithinOneRefresh(t *testing.T) {
	h, fs := newStashHarness(t, stashConfig, health("api-prod-database", "healthy", t0.Add(-time.Hour)))
	if got := summary(h.status())[1]; got != "foobar/Production/Database=operational" {
		t.Fatalf("at boot: %s", got)
	}

	// Database has impact partial_outage in the config, so down shows as that.
	fs.set(health("api-prod-database", "down", t0.Add(time.Minute)))
	h.clock = t0.Add(time.Minute)
	h.poll()
	if got := summary(h.status())[1]; got != "foobar/Production/Database=partial_outage" {
		t.Errorf("after down: %s", got)
	}
	inc := h.status().Pages[0].Incidents
	if len(inc) != 2 || inc[0].Title != "Database: partial outage" || !inc[0].StartedAt.Equal(t0.Add(time.Minute)) {
		t.Errorf("incidents = %+v", inc)
	}

	fs.set(health("api-prod-database", "healthy", t0.Add(11*time.Minute)))
	h.clock = t0.Add(11 * time.Minute)
	h.poll()
	if got := summary(h.status())[1]; got != "foobar/Production/Database=operational" {
		t.Errorf("after healthy: %s", got)
	}

	// Ten minutes of partial outage in 1000 observed minutes, as alarms count.
	h.advance(989 * time.Minute)
	if got := *h.status().Pages[0].Environments[0].Components[0].UptimePercent; got != 99 {
		t.Errorf("database uptime = %v, want 99", got)
	}
}

func TestStashHealthMapping(t *testing.T) {
	for value, want := range map[string]string{
		"healthy": "operational", "OK": "operational", "up": "operational",
		"degraded": "degraded", "warning": "degraded", " Recovering ": "degraded",
		"down": "major_outage", "failed": "major_outage", "error": "major_outage",
		"purple": "operational", "": "operational",
	} {
		// Staging has no components, so its row has no configured impact.
		h, _ := newStashHarness(t, stashConfig, health("api-staging-worker", value, t0))
		h.post("c1", "instance.created", t0, map[string]any{"identifier": "api-staging-worker", "status": "PROVISIONED"},
			attrs("api", "staging", "worker"), nil)
		h.poll()
		if got := summary(h.status())[5]; got != "internal/Staging/worker="+want {
			t.Errorf("%q: %s", value, got)
		}
	}
}

func TestStashHealthUsesInstanceImpact(t *testing.T) {
	h, fs := newStashHarness(t, stashConfig)
	h.post("c1", "instance.created", t0, map[string]any{"identifier": "api-staging-worker", "status": "PROVISIONED"},
		attrs("api", "staging", "worker"), nil)
	h.post("c2", "instance.updated", t0, map[string]any{"identifier": "api-staging-worker", "status": "PROVISIONED"},
		attrs("api", "staging", "worker", "status_impact", "degraded"), nil)
	fs.set(health("api-staging-worker", "down", t0))
	h.poll()
	if got := summary(h.status())[5]; got != "internal/Staging/worker=degraded" {
		t.Errorf("got %s", got)
	}
}

func TestAlarmBeatsStashHealthy(t *testing.T) {
	h, _ := newStashHarness(t, stashConfig, health("api-prod-api", "healthy", t0))
	if got := summary(h.status())[2]; got != "foobar/Production/API=degraded" {
		t.Errorf("got %s", got)
	}
}

func TestWorseOfAlarmAndStashWins(t *testing.T) {
	h, _ := newStashHarness(t, stashConfig, health("api-prod-api", "down", t0))
	if got := summary(h.status())[2]; got != "foobar/Production/API=major_outage" {
		t.Errorf("got %s", got)
	}
}

func TestExpiredAndMissingHealthChangeNothing(t *testing.T) {
	past, soon := t0.Add(-time.Minute), t0.Add(5*time.Minute)
	h, fs := newStashHarness(t, stashConfig,
		stashEntryJSON("INSTANCE", "api-prod-database", healthKey, "down", t0.Add(-time.Hour), &past))
	want := []string{
		"foobar=degraded",
		"foobar/Production/Database=operational",
		"foobar/Production/API=degraded",
	}
	assertLines(t, summary(h.status())[:3], want)

	// An event that is already expired when it arrives changes nothing either.
	h.post("s1", "stash_entry.set", t0, stashEvent(healthKey, "instance", "api-prod-database", "down", &past),
		attrs("api", "prod", "database"), nil)
	assertLines(t, summary(h.status())[:3], want)

	fs.set(stashEntryJSON("INSTANCE", "api-prod-database", healthKey, "down", t0, &soon))
	h.poll()
	if got := summary(h.status())[1]; got != "foobar/Production/Database=partial_outage" {
		t.Fatalf("before expiry: %s", got)
	}
	// The entry expires with no event and no read.
	h.advance(6 * time.Minute)
	assertLines(t, summary(h.status())[:3], want)

	// A read that no longer returns the entry clears it.
	fs.set(health("api-prod-database", "down", t0.Add(6*time.Minute)))
	h.poll()
	fs.set()
	h.poll()
	assertLines(t, summary(h.status())[:3], want)
}

func TestStashEventsUpdateHealth(t *testing.T) {
	h, _ := newStashHarness(t, stashConfig)
	db := attrs("api", "prod", "database")

	h.post("s1", "stash_entry.set", t0, stashEvent(healthKey, "instance", "api-prod-database", "down", nil), db, nil)
	h.post("s1", "stash_entry.set", t0, stashEvent(healthKey, "instance", "api-prod-database", "down", nil), db, nil)
	if got := summary(h.status())[1]; got != "foobar/Production/Database=partial_outage" {
		t.Errorf("after set: %s", got)
	}
	if n := len(h.status().Pages[0].Incidents); n != 2 {
		t.Errorf("a repeated event opened %d incidents, want 2", n)
	}

	h.post("s2", "stash_entry.deleted", t0.Add(time.Minute), stashEvent(healthKey, "instance", "api-prod-database", nil, nil), db, nil)
	// The set that arrives after the delete is older, so it stays deleted.
	h.post("s1-late", "stash_entry.set", t0, stashEvent(healthKey, "instance", "api-prod-database", "down", nil), db, nil)
	if got := summary(h.status())[1]; got != "foobar/Production/Database=operational" {
		t.Errorf("after delete: %s", got)
	}

	// Other keys and environments that no page shows change nothing.
	h.post("s3", "stash_entry.set", t0.Add(2*time.Minute), stashEvent("md/v1/usage", "instance", "api-prod-database", "down", nil), db, nil)
	h.post("s4", "stash_entry.set", t0.Add(2*time.Minute), stashEvent(healthKey, "instance", "web-prod-db", "down", nil), attrs("web", "prod", "db"), nil)
	h.post("s5", "stash_entry.set", t0.Add(2*time.Minute), stashEvent(healthKey, "instance", "api-prod-database", nil, nil), db, nil)
	if got := summary(h.status())[1]; got != "foobar/Production/Database=operational" {
		t.Errorf("after other keys: %s", got)
	}
	if len(h.sp.health) != 1 {
		t.Errorf("health entries = %d, want 1", len(h.sp.health))
	}
}

func TestStashBanners(t *testing.T) {
	soon := t0.Add(5 * time.Minute)
	h, fs := newStashHarness(t, stashConfig,
		stashEntryJSON("ENVIRONMENT", "api-prod", bannerKey, `{"message":"Payments are slow.","level":"warning"}`, t0, &soon))

	notice := `<div class="notice warning" role="note"><b>Production:</b> Payments are slow.</div>`
	if _, html := h.get("/foobar/"); !strings.Contains(html, notice) {
		t.Errorf("foobar page is missing the banner")
	}
	if _, html := h.get("/internal/"); strings.Contains(html, "Payments are slow.") {
		t.Errorf("internal page shows a banner of an environment it does not have")
	}
	// A banner is a notice and does not change status.
	if got := summary(h.status())[1]; got != "foobar/Production/Database=operational" {
		t.Errorf("got %s", got)
	}

	h.advance(6 * time.Minute)
	if _, html := h.get("/foobar/"); strings.Contains(html, "Payments are slow.") {
		t.Errorf("expired banner still shows")
	}

	env := envAttrs("api", "prod")
	h.post("b1", "stash_entry.set", h.clock, stashEvent(bannerKey, "environment", "api-prod", `{"message":"Checkout is down.","level":"error"}`, nil), env, nil)
	if _, html := h.get("/foobar/"); !strings.Contains(html, `<div class="notice critical" role="note"><b>Production:</b> Checkout is down.</div>`) {
		t.Errorf("error banner does not show as critical")
	}
	h.post("b2", "stash_entry.deleted", h.clock.Add(time.Second), stashEvent(bannerKey, "environment", "api-prod", nil, nil), env, nil)
	if _, html := h.get("/foobar/"); strings.Contains(html, "Checkout is down.") {
		t.Errorf("deleted banner still shows")
	}

	for _, bad := range []string{`{"level":"warning"}`, `{"message":"Hi","level":"loud"}`, `not json`} {
		fs.set(stashEntryJSON("ENVIRONMENT", "api-prod", bannerKey, bad, h.clock.Add(time.Minute), nil))
		h.poll()
		if _, html := h.get("/foobar/"); strings.Contains(html, "<b>Production:</b>") {
			t.Errorf("banner %s shows", bad)
		}
	}
}

func TestStashBannerMinLevel(t *testing.T) {
	cfg := testConfig + "stash:\n  banners: true\n  min_level: warning\n"
	h, fs := newStashHarness(t, cfg,
		stashEntryJSON("ENVIRONMENT", "api-prod", bannerKey, `{"message":"New dashboard."}`, t0, nil))
	if _, html := h.get("/foobar/"); strings.Contains(html, "New dashboard.") {
		t.Errorf("info banner shows below min_level")
	}
	fs.set(stashEntryJSON("ENVIRONMENT", "api-prod", bannerKey, `{"message":"Slow writes.","level":"warning"}`, t0, nil))
	h.poll()
	if _, html := h.get("/foobar/"); !strings.Contains(html, "Slow writes.") {
		t.Errorf("warning banner does not show")
	}
	// Banners only: no instance scopes are read.
	if len(fs.scopes) != 1 {
		t.Errorf("scopes = %v", fs.scopes)
	}
}

func TestStashOffByDefault(t *testing.T) {
	fs := &fakeStash{}
	fs.set(health("api-prod-database", "down", t0))
	h := newHarnessWith(t, testConfig, fakeAPI{stash: fs}, "", "")
	h.post("s1", "stash_entry.set", t0, stashEvent(healthKey, "instance", "api-prod-database", "down", nil), attrs("api", "prod", "database"), nil)
	h.post("b1", "stash_entry.set", t0, stashEvent(bannerKey, "environment", "api-prod", `{"message":"Stash banner text","level":"error"}`, nil), envAttrs("api", "prod"), nil)
	h.poll()
	if fs.reads != 0 {
		t.Errorf("stash reads = %d, want 0", fs.reads)
	}
	if got := summary(h.status())[1]; got != "foobar/Production/Database=operational" {
		t.Errorf("got %s", got)
	}
	if _, html := h.get("/foobar/"); strings.Contains(html, "Stash banner text") {
		t.Errorf("banner shows with stash off")
	}
}

func TestStashReadFailureKeepsLastState(t *testing.T) {
	h, fs := newStashHarness(t, stashConfig, health("api-prod-database", "down", t0))
	fs.mu.Lock()
	fs.fail = true
	fs.mu.Unlock()
	h.poll()
	if got := summary(h.status())[1]; got != "foobar/Production/Database=partial_outage" {
		t.Errorf("got %s", got)
	}
}

func TestStashReadsEachEnvironmentWithItsInstances(t *testing.T) {
	_, fs := newStashHarness(t, stashConfig)
	// Bootstrap reads api-prod, then api-staging.
	if fs.reads != 2 {
		t.Errorf("reads = %d, want 2", fs.reads)
	}
	var got []string
	for _, sc := range fs.scopes {
		for k, v := range sc.(map[string]any) {
			got = append(got, k+"="+v.(string))
		}
	}
	assertLines(t, got, []string{"environment=api-staging", "instance=api-staging-api"})
}

func TestStashMinLevelIsChecked(t *testing.T) {
	path := filepath.Join(t.TempDir(), "statuspage.yaml")
	_ = os.WriteFile(path, []byte("stash: {min_level: critical}\npages: [{title: A, environments: [{id: a-b, title: B}]}]\n"), 0o644)
	if _, err := LoadConfig(path); err == nil || !strings.Contains(err.Error(), "stash.min_level must be info, warning, or error") {
		t.Errorf("err = %v", err)
	}
}
