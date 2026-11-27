package stashsync

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/massdriver-cloud/massdriver-automations/pkg/automation"
	"github.com/massdriver-cloud/massdriver-automations/pkg/massdriver"
)

//go:embed testdata/widget_catalog.json
var catalogJSON []byte

var start = time.Date(2026, 11, 27, 9, 0, 0, 0, time.UTC)

type setCall struct {
	Scope map[string]any
	Key   string
	Input map[string]any
}

// fakeAPI serves the stash mutation and the widget catalog of the Massdriver API.
type fakeAPI struct {
	mu          sync.Mutex
	sets        []setCall
	catalogDown bool
	reject      string
}

var opName = regexp.MustCompile(`(?:query|mutation) (\w+)`)

func (f *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Query     string         `json:"query"`
		Variables map[string]any `json:"variables"`
	}
	b, _ := io.ReadAll(r.Body)
	_ = json.Unmarshal(b, &req)
	f.mu.Lock()
	defer f.mu.Unlock()
	switch opName.FindStringSubmatch(req.Query)[1] {
	case "WidgetCatalog":
		if f.catalogDown {
			http.Error(w, "unavailable", http.StatusBadRequest)
			return
		}
		_, _ = w.Write([]byte(`{"data":{"alphaWidgetCatalog":` + string(catalogJSON) + `}}`))
	case "SetStashEntry":
		if req.Variables["org"] != "tendwell-health" {
			http.Error(w, "wrong org", http.StatusBadRequest)
			return
		}
		call := setCall{Scope: req.Variables["scope"].(map[string]any), Key: req.Variables["key"].(string),
			Input: req.Variables["input"].(map[string]any)}
		if f.reject != "" {
			_, _ = w.Write([]byte(`{"data":{"alphaSetStashEntry":{"successful":false,"messages":[{"field":"value","message":"` + f.reject + `"}],"result":null}}}`))
			return
		}
		f.sets = append(f.sets, call)
		_, _ = w.Write([]byte(`{"data":{"alphaSetStashEntry":{"successful":true,"messages":[],"result":{"key":"` + call.Key + `"}}}}`))
	}
}

func (f *fakeAPI) calls() []setCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]setCall(nil), f.sets...)
}

// fakeSource returns numbers by query text: the PromQL, or the CloudWatch metric or alarm name.
type fakeSource struct {
	mu     sync.Mutex
	values map[string]float64
	errs   map[string]error
}

func (f *fakeSource) Query(_ context.Context, q Query) (float64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var name string
	switch q := q.(type) {
	case *PrometheusQuery:
		name = q.Query
	case *CloudWatchQuery:
		name = q.Metric + q.Alarm
	}
	if err, ok := f.errs[name]; ok {
		return 0, err
	}
	v, ok := f.values[name]
	if !ok {
		return 0, ErrNoData
	}
	return v, nil
}

func (f *fakeSource) set(name string, v float64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.values[name] = v
}

type env struct {
	api   *fakeAPI
	src   *fakeSource
	clock time.Time
	s     *Sync
}

func setup(t *testing.T, cfgYAML string) *env {
	t.Helper()
	cfg, err := ParseConfig([]byte(cfgYAML))
	if err != nil {
		t.Fatal(err)
	}
	api := &fakeAPI{}
	srv := httptest.NewServer(api)
	t.Cleanup(srv.Close)
	e := &env{api: api, src: &fakeSource{values: map[string]float64{}, errs: map[string]error{}}, clock: start}
	client := &massdriver.Client{Endpoint: srv.URL, OrganizationID: "tendwell-health", APIKey: "md_test"}
	sources := map[string]Source{"prometheus": e.src, "cloudwatch": e.src}
	e.s = New(cfg, &GraphQLStash{API: client}, sources, func() time.Time { return e.clock }, nil)
	return e
}

func (e *env) cycle(t *testing.T) {
	t.Helper()
	if done := e.s.Cycle(context.Background()); done {
		t.Fatal("cycle reported done")
	}
}

func decode(t *testing.T, s any) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(s.(string)), &m); err != nil {
		t.Fatalf("value %q: %v", s, err)
	}
	return m
}

const trafficRules = `
prometheus:
  url: http://prometheus:9090
rules:
  - target: {instance: billing-dev-claims, key: md/traffic, field: rps}
    prometheus: {query: rps}
  - target: {instance: billing-dev-claims, key: md/traffic, field: errors_5m}
    prometheus: {query: errors}
  - target: {instance: billing-dev-claims, key: md/traffic, field: p95_ms}
    prometheus: {query: p95}
    scale: 1000
`

func TestFieldsOfOneKeyAreOneWrite(t *testing.T) {
	e := setup(t, trafficRules)
	e.src.values = map[string]float64{"rps": 41.256, "errors": 2.6, "p95": 0.1234}
	e.cycle(t)

	calls := e.api.calls()
	if len(calls) != 1 {
		t.Fatalf("calls = %+v", calls)
	}
	c := calls[0]
	if !reflect.DeepEqual(c.Scope, map[string]any{"instance": "billing-dev-claims"}) || c.Key != "md/traffic" {
		t.Errorf("target = %v %s", c.Scope, c.Key)
	}
	if c.Input["format"] != "JSON" || c.Input["ttl"] != float64(180) {
		t.Errorf("input = %v", c.Input)
	}
	// errors_5m is an integer in the widget schema, so it is rounded.
	want := map[string]any{"rps": 41.26, "errors_5m": float64(3), "p95_ms": 123.4}
	if got := decode(t, c.Input["value"]); !reflect.DeepEqual(got, want) {
		t.Errorf("value = %v, want %v", got, want)
	}
}

func TestWritesOnlyWhenChangedAndBeforeExpiry(t *testing.T) {
	e := setup(t, trafficRules)
	e.src.values = map[string]float64{"rps": 10, "errors": 0, "p95": 0.1}
	e.cycle(t)
	e.clock = e.clock.Add(time.Minute)
	e.cycle(t)
	if n := len(e.api.calls()); n != 1 {
		t.Fatalf("unchanged value was written again: %d writes", n)
	}

	e.src.set("rps", 12)
	e.clock = e.clock.Add(time.Minute)
	e.cycle(t)
	calls := e.api.calls()
	if len(calls) != 2 || decode(t, calls[1].Input["value"])["rps"] != float64(12) {
		t.Fatalf("calls = %+v", calls)
	}

	// The entry expires three intervals after a write. It is written again two intervals after, unchanged.
	e.clock = e.clock.Add(time.Minute)
	e.cycle(t)
	if n := len(e.api.calls()); n != 2 {
		t.Fatalf("writes = %d after one interval, want 2", n)
	}
	e.clock = e.clock.Add(time.Minute)
	e.cycle(t)
	if n := len(e.api.calls()); n != 3 {
		t.Fatalf("writes = %d after two intervals, want 3", n)
	}
}

func TestUnchangedEntryIsRefreshedWhenQueriesTakeLessTime(t *testing.T) {
	e := setup(t, trafficRules)
	e.src.values = map[string]float64{"rps": 10, "errors": 0}
	// The first cycle's queries took 20 seconds; the next ones take 1 second.
	e.clock = start.Add(20 * time.Second)
	e.cycle(t)
	e.clock = start.Add(time.Minute + time.Second)
	e.cycle(t)
	e.clock = start.Add(2*time.Minute + time.Second)
	e.cycle(t)
	// The entry expires at 3m20s, so the cycle at 2m must write it again.
	if n := len(e.api.calls()); n != 2 {
		t.Errorf("writes = %d, want 2", n)
	}
}

func TestTTLIsThreeIntervals(t *testing.T) {
	e := setup(t, trafficRules+"interval: 20s\n")
	e.src.values = map[string]float64{"rps": 10, "errors": 0}
	e.cycle(t)
	if ttl := e.api.calls()[0].Input["ttl"]; ttl != float64(60) {
		t.Errorf("ttl = %v, want 60", ttl)
	}
}

func TestFailedWriteIsRetriedNextCycle(t *testing.T) {
	e := setup(t, trafficRules)
	e.src.values = map[string]float64{"rps": 10, "errors": 0}
	e.api.reject = "stash is full"
	e.cycle(t)
	e.api.reject = ""
	e.clock = e.clock.Add(time.Minute)
	e.cycle(t)
	if n := len(e.api.calls()); n != 1 {
		t.Errorf("writes = %d, want 1", n)
	}
}

func TestFailingQueryDoesNotStopOtherEntries(t *testing.T) {
	e := setup(t, `
prometheus:
  url: http://prometheus:9090
rules:
  - target: {instance: billing-dev-claims, key: md/traffic, field: rps}
    prometheus: {query: claims_rps}
  - target: {instance: billing-dev-claims, key: md/traffic, field: errors_5m}
    prometheus: {query: claims_errors}
    default: 0
  - target: {instance: billing-dev-claims, key: md/traffic, field: p95_ms}
    prometheus: {query: claims_p95}
  - target: {instance: billing-dev-ledger, key: md/traffic, field: rps}
    prometheus: {query: ledger_rps}
  - target: {instance: billing-dev-ledger, key: md/traffic, field: errors_5m}
    prometheus: {query: ledger_errors}
  - target: {instance: billing-dev-app, key: queue.depth}
    prometheus: {query: queue}
`)
	// claims: p95 fails, which is optional; errors has no data and uses its default.
	// ledger: rps fails, which md/traffic requires, so ledger is skipped.
	e.src.values = map[string]float64{"claims_rps": 5, "ledger_errors": 1, "queue": 17}
	e.src.errs = map[string]error{"claims_p95": errors.New("timeout"), "ledger_rps": errors.New("timeout")}
	e.cycle(t)

	got := map[string]any{}
	for _, c := range e.api.calls() {
		got[c.Scope["instance"].(string)+" "+c.Key] = c.Input["value"]
	}
	if len(got) != 2 {
		t.Fatalf("writes = %v", got)
	}
	if v := decode(t, got["billing-dev-claims md/traffic"]); !reflect.DeepEqual(v, map[string]any{"rps": float64(5), "errors_5m": float64(0)}) {
		t.Errorf("claims = %v", v)
	}
	if got["billing-dev-app queue.depth"] != "17" {
		t.Errorf("queue = %v", got["billing-dev-app queue.depth"])
	}
}

func TestTextRulesMapNumbersToHealth(t *testing.T) {
	e := setup(t, `
rules:
  - target: {instance: billing-dev-app, key: md/health}
    cloudwatch: {namespace: AWS/ApplicationELB, metric: UnHealthyHostCount, stat: Maximum}
    default: 0
    text:
      - {below: 1, text: healthy}
      - {below: 3, text: degraded}
      - {text: down}
`)
	var states []string
	for _, n := range []float64{-1, 0, 2, 5} {
		if n >= 0 {
			e.src.set("UnHealthyHostCount", n)
		}
		e.cycle(t)
		e.clock = e.clock.Add(time.Minute)
	}
	for _, c := range e.api.calls() {
		if c.Input["format"] != "TEXT" {
			t.Errorf("format = %v", c.Input["format"])
		}
		states = append(states, c.Input["value"].(string))
	}
	// No data uses the default, 0, which is healthy. The second healthy is unchanged and not written.
	if want := []string{"healthy", "degraded", "down"}; !reflect.DeepEqual(states, want) {
		t.Errorf("states = %v, want %v", states, want)
	}
}

func TestWidgetValuesAreCheckedAgainstTheCatalog(t *testing.T) {
	e := setup(t, `
rules:
  - target: {instance: billing-dev-app, key: md/usage}
    value: {value: 10, max: 0}
  - target: {environment: billing-dev, key: md/health}
    value: healthy
  - target: {instance: billing-dev-app, key: md/unknown}
    value: "1"
  - target: {environment: billing-dev, key: md/banner}
    value: {message: "Deploys are paused", level: notice}
  - target: {environment: billing-dev, key: md/links}
    value: {links: [{label: Runbook, url: "not a url"}]}
  - target: {instance: billing-dev-ledger, key: md/usage}
    value: {value: 10, max: 100, label: connections}
`)
	e.cycle(t)
	var keys []string
	for _, c := range e.api.calls() {
		for _, id := range c.Scope {
			keys = append(keys, id.(string)+" "+c.Key)
		}
	}
	want := []string{"billing-dev-ledger md/usage"}
	if !reflect.DeepEqual(keys, want) {
		t.Errorf("written = %v, want %v", keys, want)
	}
}

func TestValidLinksAreWritten(t *testing.T) {
	e := setup(t, `
rules:
  - target: {environment: billing-dev, key: md/links, label: Links}
    value: {links: [{label: Runbook, url: "https://wiki.example.com/runbook"}]}
`)
	e.cycle(t)
	calls := e.api.calls()
	if len(calls) != 1 || calls[0].Input["label"] != "Links" || calls[0].Input["format"] != "JSON" {
		t.Fatalf("calls = %+v", calls)
	}
}

func TestCatalogIsReadAgainAfterAFailure(t *testing.T) {
	e := setup(t, `
rules:
  - target: {instance: billing-dev-app, key: md/health}
    value: healthy
  - target: {instance: billing-dev-app, key: owner}
    value: billing-team
`)
	e.api.catalogDown = true
	e.cycle(t)
	if calls := e.api.calls(); len(calls) != 1 || calls[0].Key != "owner" {
		t.Fatalf("calls = %+v", calls)
	}
	e.api.catalogDown = false
	e.clock = e.clock.Add(time.Minute)
	e.cycle(t)
	if calls := e.api.calls(); len(calls) != 2 || calls[1].Key != "md/health" {
		t.Fatalf("calls = %+v", calls)
	}
}

func TestConfigErrors(t *testing.T) {
	cases := map[string]string{
		"no rules": `interval: 60s`,
		"two scopes": `
rules: [{target: {instance: a, environment: b, key: k}, value: x}]`,
		"no scope": `
rules: [{target: {key: k}, value: x}]`,
		"bad key": `
rules: [{target: {instance: a, key: "has space"}, value: x}]`,
		"text with a field": `
rules: [{target: {instance: a, key: k, field: f, format: TEXT}, value: 1}]`,
		"a query as a whole JSON value": `
rules: [{target: {instance: a, key: k, format: JSON}, cloudwatch: {alarm: x}}]`,
		"an object as a TEXT value": `
rules: [{target: {instance: a, key: k, format: TEXT}, value: {a: 1}}]`,
		"a string as a whole JSON value": `
rules: [{target: {instance: a, key: k, format: JSON}, value: x}]`,
		"two whole values": `
rules:
  - {target: {instance: a, key: k}, value: x}
  - {target: {instance: a, key: k}, value: y}`,
		"whole value and a field": `
rules:
  - {target: {instance: a, key: k, field: f}, value: 1}
  - {target: {instance: a, key: k}, value: y}`,
		"same field twice": `
rules:
  - {target: {instance: a, key: k, field: f}, value: 1}
  - {target: {instance: a, key: k, field: f}, value: 2}`,
		"two labels": `
rules:
  - {target: {instance: a, key: k, field: f, label: A}, value: 1}
  - {target: {instance: a, key: k, field: g, label: B}, value: 2}`,
		"two sources": `
prometheus: {url: http://p}
rules: [{target: {instance: a, key: k}, value: x, prometheus: {query: up}}]`,
		"prometheus without url": `
rules: [{target: {instance: a, key: k}, prometheus: {query: up}}]`,
		"cloudwatch without metric": `
rules: [{target: {instance: a, key: k}, cloudwatch: {namespace: AWS/RDS}}]`,
		"cloudwatch period": `
rules: [{target: {instance: a, key: k}, cloudwatch: {namespace: AWS/RDS, metric: CPUUtilization, period: 90s}}]`,
		"text before the end without below": `
rules: [{target: {instance: a, key: k}, cloudwatch: {alarm: x}, text: [{text: a}, {below: 1, text: b}]}]`,
		"unknown preset": `
presets: [{preset: aws-lambda, instance: a}]`,
		"preset without its setting": `
presets: [{preset: aws-rds-connections, instance: a, with: {db_instance: db}}]`,
		"preset with an unknown setting": `
presets: [{preset: aws-alarm-health, instance: a, with: {alarm: x, region: y}}]`,
		"unknown mode": `
mode: replay
rules: [{target: {instance: a, key: k}, value: x}]`,
		"interval too short": `
interval: 100ms
rules: [{target: {instance: a, key: k}, value: x}]`,
		"unknown key": `
rules: [{target: {instance: a, key: k}, value: x, every: 5m}]`,
		"simulate without a scenario": `
mode: simulate`,
		"simulate without roles": `
mode: simulate
simulate: {scenario: postgres-cutover, roles: {environment: e}}`,
		"simulate with rules": `
mode: simulate
simulate: {scenario: steady, roles: {instances: [a]}}
rules: [{target: {instance: a, key: k}, value: x}]`,
	}
	for name, doc := range cases {
		if _, err := ParseConfig([]byte(doc)); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

func TestConfigDefaults(t *testing.T) {
	cfg, err := ParseConfig([]byte(`
rules: [{target: {instance: a, key: k, field: f}, value: 1}]`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Mode != ModeSync || time.Duration(cfg.Interval) != time.Minute || cfg.groups[0].format != FormatJSON {
		t.Errorf("cfg = %+v", cfg)
	}
	sim, err := ParseConfig([]byte(`
mode: simulate
simulate: {scenario: steady, roles: {instances: [a]}}`))
	if err != nil {
		t.Fatal(err)
	}
	if time.Duration(sim.Interval) != 5*time.Second || time.Duration(sim.Simulate.Duration) != 3*time.Minute ||
		!*sim.Simulate.Loop {
		t.Errorf("simulate = %+v", sim)
	}
}

func TestConfigExampleLoads(t *testing.T) {
	var cfg Config
	if err := automation.LoadConfigFile("config.example.yaml", &cfg); err != nil {
		t.Fatal(err)
	}
	keys := map[string]bool{}
	for _, g := range cfg.groups {
		keys[g.scope.String()+" "+g.key] = true
	}
	for _, k := range []string{
		"instance billing-prod-ledger md/usage", "instance billing-prod-app md/traffic",
		"instance billing-prod-ledger md/health", "instance billing-prod-claims md/traffic",
		"instance billing-prod-claims md/usage", "instance billing-prod-app md/health",
		"environment billing-prod billing.p99_ms", "environment billing-prod md/links",
	} {
		if !keys[k] {
			t.Errorf("missing %s in %v", k, keys)
		}
	}
}

func TestPresetsExpandToRules(t *testing.T) {
	cfg, err := ParseConfig([]byte(`
presets:
  - preset: aws-rds-connections
    instance: billing-dev-ledger
    with: {db_instance: ledger-pg, max: 200}
  - preset: aws-alb-traffic
    instance: billing-dev-app
    with: {load_balancer: app/billing/1, target_group: targetgroup/billing/2}
  - preset: aws-alarm-health
    instance: billing-dev-claims
    with: {alarm: claims-cpu}
  - preset: prometheus-traffic
    instance: billing-dev-claims
    with: {job: claims}
prometheus: {url: http://prometheus:9090}
`))
	if err != nil {
		t.Fatal(err)
	}
	type rule struct {
		Field, Query string
		Value        any
		Scale        float64
		Default      bool
	}
	got := map[string][]rule{}
	for _, g := range cfg.groups {
		for _, r := range g.rules {
			x := rule{Field: r.Target.Field, Value: r.Value, Default: r.Default != nil}
			if r.Scale != nil {
				x.Scale = *r.Scale
			}
			switch q := r.query().(type) {
			case *CloudWatchQuery:
				x.Query = q.Alarm
				if q.Alarm == "" {
					x.Query = q.Namespace + " " + q.Metric + " " + q.Stat + " " + time.Duration(q.Period).String()
					for _, k := range sortedKeys(q.Dimensions) {
						x.Query += " " + k + "=" + q.Dimensions[k]
					}
				}
			case *PrometheusQuery:
				x.Query = q.Query
			}
			got[g.scope.String()+" "+g.key] = append(got[g.scope.String()+" "+g.key], x)
		}
	}
	want := map[string][]rule{
		"instance billing-dev-ledger md/usage": {
			{Field: "value", Query: "AWS/RDS DatabaseConnections Average 1m0s DBInstanceIdentifier=ledger-pg"},
			{Field: "max", Value: float64(200)},
			{Field: "label", Value: "connections"},
		},
		"instance billing-dev-app md/traffic": {
			{Field: "rps", Query: "AWS/ApplicationELB RequestCount Sum 1m0s LoadBalancer=app/billing/1 TargetGroup=targetgroup/billing/2", Scale: 1.0 / 60, Default: true},
			{Field: "errors_5m", Query: "AWS/ApplicationELB HTTPCode_Target_5XX_Count Sum 5m0s LoadBalancer=app/billing/1 TargetGroup=targetgroup/billing/2", Default: true},
			{Field: "p95_ms", Query: "AWS/ApplicationELB TargetResponseTime p95 1m0s LoadBalancer=app/billing/1 TargetGroup=targetgroup/billing/2", Scale: 1000},
		},
		"instance billing-dev-claims md/health": {{Query: "claims-cpu"}},
		"instance billing-dev-claims md/traffic": {
			{Field: "rps", Query: `sum(rate(http_requests_total{job="claims"}[5m]))`, Default: true},
			{Field: "errors_5m", Query: `sum(increase(http_requests_total{job="claims",code=~"5.."}[5m]))`, Default: true},
			{Field: "p95_ms", Query: `histogram_quantile(0.95, sum by (le) (rate(http_request_duration_seconds_bucket{job="claims"}[5m]))) * 1000`},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("rules =\n%+v\nwant\n%+v", got, want)
	}

	health := cfg.groups[2].rules[0]
	var states []string
	for _, v := range []float64{AlarmOK, AlarmInsufficientData, AlarmAlarm} {
		states = append(states, text(health.Text, v))
	}
	if want := []string{"healthy", "degraded", "down"}; !reflect.DeepEqual(states, want) {
		t.Errorf("alarm states = %v, want %v", states, want)
	}
}

func TestPrometheusPresetSelector(t *testing.T) {
	cfg, err := ParseConfig([]byte(`
prometheus: {url: http://prometheus:9090}
presets:
  - preset: prometheus-traffic
    instance: billing-dev-app
    with: {selector: 'service="app",namespace="billing"', requests_metric: requests_total, code_label: status, duration_metric: latency_seconds}
`))
	if err != nil {
		t.Fatal(err)
	}
	var queries []string
	for _, r := range cfg.groups[0].rules {
		queries = append(queries, r.Prometheus.Query)
	}
	want := []string{
		`sum(rate(requests_total{service="app",namespace="billing"}[5m]))`,
		`sum(increase(requests_total{service="app",namespace="billing",status=~"5.."}[5m]))`,
		`histogram_quantile(0.95, sum by (le) (rate(latency_seconds_bucket{service="app",namespace="billing"}[5m]))) * 1000`,
	}
	if !reflect.DeepEqual(queries, want) {
		t.Errorf("queries = %q", queries)
	}
	if _, err := ParseConfig([]byte(`
prometheus: {url: http://p}
presets: [{preset: prometheus-traffic, instance: a, with: {job: a, selector: 'job="a"'}}]`)); err == nil ||
		!strings.Contains(err.Error(), "not both") {
		t.Errorf("job and selector: %v", err)
	}
}

func TestValidateSchema(t *testing.T) {
	var widgets []struct {
		Key    string         `json:"key"`
		Schema map[string]any `json:"schema"`
	}
	if err := json.Unmarshal(catalogJSON, &widgets); err != nil {
		t.Fatal(err)
	}
	schemas := map[string]map[string]any{}
	for _, w := range widgets {
		schemas[w.Key] = w.Schema
	}
	cases := []struct {
		key   string
		value string
		valid bool
	}{
		{"md/health", `"healthy"`, true},
		{"md/health", `""`, false},
		{"md/health", `"` + strings.Repeat("x", 65) + `"`, false},
		{"md/usage", `{"value": 0, "max": 1}`, true},
		{"md/usage", `{"value": 1, "max": 0}`, false},
		{"md/usage", `{"value": -1, "max": 1}`, false},
		{"md/usage", `{"value": 1, "max": 2, "unit": "GB"}`, false},
		{"md/usage", `{"value": 1}`, false},
		{"md/traffic", `{"rps": 1.5, "errors_5m": 2, "p95_ms": 3.5}`, true},
		{"md/traffic", `{"rps": 1.5, "errors_5m": 2.5}`, false},
		{"md/traffic", `{"rps": "1", "errors_5m": 2}`, false},
		{"md/banner", `{"message": "Hi", "level": "warning"}`, true},
		{"md/banner", `{"message": "Hi", "level": "debug"}`, false},
		{"md/banner", `{"message": ""}`, false},
		{"md/links", `{"links": [{"label": "Runbook", "url": "https://wiki.example.com"}]}`, true},
		{"md/links", `{"links": [{"label": "Runbook", "url": "https://wiki.example.com/a b"}]}`, false},
		{"md/links", `{"links": [{"label": "Runbook", "url": "ftp://wiki.example.com"}]}`, false},
		{"md/links", `{"links": []}`, false},
	}
	for _, c := range cases {
		var v any
		if err := json.Unmarshal([]byte(c.value), &v); err != nil {
			t.Fatal(err)
		}
		err := validateSchema(schemas[c.key], v, "value")
		if (err == nil) != c.valid {
			t.Errorf("%s %s: err = %v, want valid %v", c.key, c.value, err, c.valid)
		}
	}
}

func TestGraphQLStashSendsTheEntry(t *testing.T) {
	api := &fakeAPI{}
	srv := httptest.NewServer(api)
	defer srv.Close()
	stash := &GraphQLStash{API: &massdriver.Client{Endpoint: srv.URL, OrganizationID: "tendwell-health", APIKey: "md_test"}}
	err := stash.Set(context.Background(), Write{Scope: Scope{Kind: "environment", ID: "billing-dev"}, Key: "md/banner",
		Format: FormatJSON, Value: `{"message":"Hi"}`, TTL: 3 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	want := setCall{Scope: map[string]any{"environment": "billing-dev"}, Key: "md/banner",
		Input: map[string]any{"value": `{"message":"Hi"}`, "format": "JSON", "ttl": float64(180)}}
	if got := api.calls(); len(got) != 1 || !reflect.DeepEqual(got[0], want) {
		t.Errorf("calls = %+v", got)
	}
	api.reject = "md/banner can be set only on an environment"
	if err := stash.Set(context.Background(), Write{Scope: Scope{Kind: "instance", ID: "x"}, Key: "md/banner",
		Format: FormatJSON, Value: "{}"}); err == nil || !strings.Contains(err.Error(), "only on an environment") {
		t.Errorf("err = %v", err)
	}

	widgets, err := stash.WidgetCatalog(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(widgets) != 5 || widgets[0].Key != "md/banner" || widgets[0].Scope != "environment" || widgets[0].Format != "JSON" {
		t.Errorf("widgets = %+v", widgets)
	}
}

func TestHandleIgnoresEvents(t *testing.T) {
	e := setup(t, trafficRules)
	if err := e.s.Handle(context.Background(), automation.Event{ID: "1", Type: "deployment.completed"}); err != nil {
		t.Fatal(err)
	}
	if n := len(e.api.calls()); n != 0 {
		t.Errorf("writes = %d", n)
	}
}
