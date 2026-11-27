package stashsync

import (
	"context"
	"encoding/json"
	"math/rand/v2"
	"reflect"
	"regexp"
	"testing"
	"time"
)

// run steps a scenario through one pass, one tick every 5 seconds of a 3 minute pass, and returns each tick's
// phase and entries.
func run(t *testing.T, doc string) ([]string, [][]Entry) {
	t.Helper()
	cfg, err := ParseConfig([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	sim := newSimulator(cfg.Simulate, rand.New(rand.NewPCG(7, 7)))
	var phases []string
	var ticks [][]Entry
	for now := start; now.Before(start.Add(3 * time.Minute)); now = now.Add(5 * time.Second) {
		phase, entries, done := sim.step(now)
		if done {
			t.Fatal("done before the end of the pass")
		}
		phases = append(phases, phase)
		ticks = append(ticks, entries)
	}
	return phases, ticks
}

var digits = regexp.MustCompile(`[0-9]+`)

// sequence is the distinct values in a row of one key on one scope: health text, or banner level and message
// with numbers replaced by N.
func sequence(ticks [][]Entry, scopeID, key string) []string {
	var out []string
	for _, entries := range ticks {
		for _, e := range entries {
			if e.Scope.ID != scopeID || e.Key != key {
				continue
			}
			var v string
			switch d := e.Data.(type) {
			case string:
				v = d
			case map[string]any:
				v = d["level"].(string) + ": " + digits.ReplaceAllString(d["message"].(string), "N")
			}
			if len(out) == 0 || out[len(out)-1] != v {
				out = append(out, v)
			}
		}
	}
	return out
}

func distinct(list []string) []string {
	var out []string
	for _, v := range list {
		if len(out) == 0 || out[len(out)-1] != v {
			out = append(out, v)
		}
	}
	return out
}

func fieldValues(ticks [][]Entry, scopeID, key, name string) []float64 {
	var out []float64
	for _, entries := range ticks {
		for _, e := range entries {
			if e.Scope.ID == scopeID && e.Key == key {
				out = append(out, e.Data.(map[string]any)[name].(float64))
			}
		}
	}
	return out
}

// checkValid checks every entry against the widget catalog.
func checkValid(t *testing.T, ticks [][]Entry) {
	t.Helper()
	var widgets []Widget
	var raw []struct {
		Key, Scope, Format string
		Schema             map[string]any
	}
	if err := json.Unmarshal(catalogJSON, &raw); err != nil {
		t.Fatal(err)
	}
	for _, w := range raw {
		widgets = append(widgets, Widget{Key: w.Key, Scope: lower(w.Scope), Format: w.Format, Schema: w.Schema})
	}
	c := newCatalog(widgets)
	for i, entries := range ticks {
		for _, e := range entries {
			if _, err := c.prepare(e); err != nil {
				t.Fatalf("tick %d: %s %s %v: %v", i, e.Scope, e.Key, e.Data, err)
			}
		}
	}
}

func lower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + 32
		}
	}
	return string(b)
}

func TestSteadyScenario(t *testing.T) {
	phases, ticks := run(t, `
mode: simulate
simulate:
  scenario: steady
  roles: {instances: [billing-dev-claims, billing-dev-app, billing-dev-ledger]}
`)
	checkValid(t, ticks)
	if got := distinct(phases); !reflect.DeepEqual(got, []string{"steady"}) {
		t.Errorf("phases = %v", got)
	}
	for _, id := range []string{"billing-dev-claims", "billing-dev-app", "billing-dev-ledger"} {
		if got := sequence(ticks, id, "md/v1/health"); !reflect.DeepEqual(got, []string{"healthy"}) {
			t.Errorf("%s health = %v", id, got)
		}
		rps := fieldValues(ticks, id, "md/v1/traffic", "rps")
		if len(rps) != 36 || len(distinct(fmtAll(rps))) < 10 {
			t.Errorf("%s rps does not change: %v", id, rps)
		}
		for _, u := range fieldValues(ticks, id, "md/v1/usage", "value") {
			if u < 35 || u > 60 {
				t.Errorf("%s usage %v is outside the steady range", id, u)
			}
		}
	}
}

func fmtAll(vs []float64) []string {
	out := make([]string, len(vs))
	for i, v := range vs {
		b, _ := json.Marshal(v)
		out[i] = string(b)
	}
	return out
}

func TestIncidentScenario(t *testing.T) {
	phases, ticks := run(t, `
mode: simulate
simulate:
  scenario: incident
  roles:
    environment: billing-dev
    instance: billing-dev-app
    instances: [billing-dev-claims, billing-dev-ledger]
`)
	checkValid(t, ticks)
	if got, want := distinct(phases), []string{"normal", "degrading", "outage", "recovering", "resolved"}; !reflect.DeepEqual(got, want) {
		t.Errorf("phases = %v, want %v", got, want)
	}
	if got, want := sequence(ticks, "billing-dev-app", "md/v1/health"), []string{"healthy", "degraded", "down", "recovering", "healthy"}; !reflect.DeepEqual(got, want) {
		t.Errorf("health = %v, want %v", got, want)
	}
	want := []string{
		"warning: Elevated latency on billing-dev-app.",
		"error: billing-dev-app is failing requests.",
		"info: billing-dev-app is recovering. Monitoring.",
		"info: Resolved: billing-dev-app is healthy.",
	}
	if got := sequence(ticks, "billing-dev", "md/v1/banner"); !reflect.DeepEqual(got, want) {
		t.Errorf("banner = %v, want %v", got, want)
	}
	if got := sequence(ticks, "billing-dev-claims", "md/v1/health"); !reflect.DeepEqual(got, []string{"healthy"}) {
		t.Errorf("claims health = %v", got)
	}

	var peakErrors, peakUsage float64
	var past80 bool
	for i, p := range phases {
		for _, e := range ticks[i] {
			if e.Scope.ID != "billing-dev-app" {
				continue
			}
			d, _ := e.Data.(map[string]any)
			switch e.Key {
			case "md/v1/traffic":
				if p == "outage" {
					peakErrors = max(peakErrors, d["errors_5m"].(float64))
				}
			case "md/v1/usage":
				v := d["value"].(float64)
				peakUsage = max(peakUsage, v)
				past80 = past80 || (v > 80 && v < 95)
			}
		}
	}
	if peakErrors < 40 || peakUsage < 95 || !past80 {
		t.Errorf("errors peak %v, usage peak %v, usage between 80 and 95: %v", peakErrors, peakUsage, past80)
	}
}

func TestPostgresCutoverScenario(t *testing.T) {
	phases, ticks := run(t, `
mode: simulate
simulate:
  scenario: postgres-cutover
  roles:
    environment: billing-dev
    old_database: billing-dev-claims
    new_database: billing-dev-ledger
    instances: [billing-dev-app]
`)
	checkValid(t, ticks)
	if got, want := distinct(phases), []string{"replicating", "catching-up", "cutover", "complete"}; !reflect.DeepEqual(got, want) {
		t.Errorf("phases = %v, want %v", got, want)
	}
	want := []string{
		"info: Replicating billing-dev-claims to billing-dev-ledger.",
		"warning: Catching up: lag Ns.",
		"warning: Cutover in progress.",
		"info: Cutover complete.",
	}
	if got := sequence(ticks, "billing-dev", "md/v1/banner"); !reflect.DeepEqual(got, want) {
		t.Errorf("banner = %v, want %v", got, want)
	}
	if got, want := sequence(ticks, "billing-dev-claims", "md/v1/health"), []string{"healthy", "draining", "idle"}; !reflect.DeepEqual(got, want) {
		t.Errorf("old health = %v, want %v", got, want)
	}
	if got, want := sequence(ticks, "billing-dev-ledger", "md/v1/health"), []string{"replicating", "catching up", "cutover", "healthy"}; !reflect.DeepEqual(got, want) {
		t.Errorf("new health = %v, want %v", got, want)
	}

	lag := fieldValues(ticks, "billing-dev-ledger", "md/v1/usage", "value")
	if lag[0] < 90 || lag[len(lag)-1] != 0 {
		t.Errorf("lag = %v", lag)
	}
	var catchingUp []float64
	for i, p := range phases {
		if p == "catching-up" {
			catchingUp = append(catchingUp, lag[i])
		}
	}
	if catchingUp[0] < 90 || catchingUp[len(catchingUp)-1] > 15 {
		t.Errorf("lag while catching up = %v", catchingUp)
	}

	oldRPS := fieldValues(ticks, "billing-dev-claims", "md/v1/traffic", "rps")
	newRPS := fieldValues(ticks, "billing-dev-ledger", "md/v1/traffic", "rps")
	last := len(oldRPS) - 1
	if oldRPS[0] == 0 || newRPS[0] != 0 || oldRPS[last] != 0 || newRPS[last] == 0 {
		t.Errorf("old rps = %v\nnew rps = %v", oldRPS, newRPS)
	}
}

func TestRolloutScenario(t *testing.T) {
	phases, ticks := run(t, `
mode: simulate
simulate:
  scenario: rollout
  roles:
    advisory: "Security advisory: remote code execution in the PHP runtime."
    environments:
      - {environment: billing-dev, instances: [billing-dev-claims, billing-dev-app]}
      - {environment: billing-staging, instances: [billing-staging-app]}
`)
	checkValid(t, ticks)
	wantPhases := []string{"billing-dev vulnerable", "billing-dev patching", "billing-staging vulnerable",
		"billing-staging patching", "patched"}
	if got := distinct(phases); !reflect.DeepEqual(got, wantPhases) {
		t.Errorf("phases = %v, want %v", got, wantPhases)
	}
	for env, n := range map[string]string{"billing-dev": "N of N", "billing-staging": "N of N"} {
		want := []string{
			"warning: Security advisory: remote code execution in the PHP runtime.",
			"warning: Patching " + env + ": " + n + " instances patched.",
			"info: Patched: " + env + " has the fix.",
		}
		if got := sequence(ticks, env, "md/v1/banner"); !reflect.DeepEqual(got, want) {
			t.Errorf("%s banner = %v, want %v", env, got, want)
		}
	}
	for _, id := range []string{"billing-dev-claims", "billing-dev-app", "billing-staging-app"} {
		if got, want := sequence(ticks, id, "md/v1/health"), []string{"vulnerable", "patching", "patched"}; !reflect.DeepEqual(got, want) {
			t.Errorf("%s health = %v, want %v", id, got, want)
		}
	}
	// billing-dev's instances patch one after the other.
	claims := healthAt(ticks, "billing-dev-claims")
	app := healthAt(ticks, "billing-dev-app")
	if first(claims, "patched") >= first(app, "patching")+1 || first(claims, "patching") >= first(app, "patching") {
		t.Errorf("claims %v\napp %v", claims, app)
	}
	staging := healthAt(ticks, "billing-staging-app")
	if first(staging, "patching") <= first(app, "patched") {
		t.Errorf("staging patched before dev finished: dev %v staging %v", app, staging)
	}
}

func healthAt(ticks [][]Entry, id string) []string {
	var out []string
	for _, entries := range ticks {
		for _, e := range entries {
			if e.Scope.ID == id && e.Key == "md/v1/health" {
				out = append(out, e.Data.(string))
			}
		}
	}
	return out
}

func first(list []string, v string) int {
	for i, x := range list {
		if x == v {
			return i
		}
	}
	return len(list)
}

func TestSimulatorIsDeterministic(t *testing.T) {
	doc := `
mode: simulate
simulate:
  scenario: incident
  roles: {environment: billing-dev, instance: billing-dev-app}
`
	_, a := run(t, doc)
	_, b := run(t, doc)
	if !reflect.DeepEqual(a, b) {
		t.Error("the same seed made different values")
	}
}

func TestScenarioLoopsOrStops(t *testing.T) {
	cfg, err := ParseConfig([]byte(`
mode: simulate
simulate:
  scenario: incident
  duration: 60s
  roles: {environment: billing-dev, instance: billing-dev-app}
`))
	if err != nil {
		t.Fatal(err)
	}
	sim := newSimulator(cfg.Simulate, rand.New(rand.NewPCG(1, 1)))
	sim.step(start)
	if phase, _, done := sim.step(start.Add(65 * time.Second)); done || phase != "normal" {
		t.Errorf("loop: phase %q, done %v", phase, done)
	}

	cfg.Simulate.Loop = ptr(false)
	stop := newSimulator(cfg.Simulate, rand.New(rand.NewPCG(1, 1)))
	stop.step(start)
	if _, entries, done := stop.step(start.Add(60 * time.Second)); !done || entries != nil {
		t.Errorf("stop: done %v, entries %v", done, entries)
	}
}

func TestSimulateModeWritesAndStops(t *testing.T) {
	e := setup(t, `
mode: simulate
interval: 10s
simulate:
  scenario: postgres-cutover
  duration: 30s
  loop: false
  seed: 3
  roles: {environment: billing-dev, old_database: billing-dev-claims, new_database: billing-dev-ledger}
`)
	for i := 0; i < 3; i++ {
		e.cycle(t)
		e.clock = e.clock.Add(10 * time.Second)
	}
	if done := e.s.Cycle(context.Background()); !done {
		t.Error("the scenario did not stop")
	}
	calls := e.api.calls()
	if len(calls) < 6 {
		t.Fatalf("writes = %d", len(calls))
	}
	for _, c := range calls {
		if c.Input["ttl"] != float64(30) {
			t.Errorf("ttl = %v", c.Input["ttl"])
		}
	}
}
