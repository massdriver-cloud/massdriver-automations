package scorecards

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/massdriver-cloud/massdriver-automations/pkg/automation"
)

var now = time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)

func ago(d time.Duration) time.Time { return now.Add(-d) }

func tp(t time.Time) *time.Time { return &t }

const day = 24 * time.Hour

const readiness = `
scorecards:
  - name: production-readiness
    match: {md-environment: prod}
    checks:
      - id: no-drift
        stash: md/v1/drift
        pass: .status == "none"
        max_age: 2d
      - id: has-alarms
        stash: md/v1/reliability
        pass: .alarms > 0
      - id: tests-passed
        stash: {key: md/v1/tests, scope: deployment}
        pass: .failed == 0
      - id: not-firing
        query: instance
        pass: .firingAlarmCount == 0
`

type env struct {
	md *fakeMassdriver
	s  *Scorecards
}

func setup(t *testing.T, cfgYAML string) *env {
	t.Helper()
	cfg, err := ParseConfig([]byte(cfgYAML))
	if err != nil {
		t.Fatal(err)
	}
	md := newFake()
	return &env{md: md, s: New(cfg, md, md, func() time.Time { return now })}
}

func (e *env) handle(t *testing.T, ev automation.Event) {
	t.Helper()
	if err := e.s.Handle(context.Background(), ev); err != nil {
		t.Fatal(err)
	}
	e.s.Drain(context.Background())
}

func (e *env) decode(t *testing.T, kind, id, key string) map[string]any {
	t.Helper()
	v, ok := e.md.value(kind, id, key)
	if !ok {
		t.Fatalf("no %s on %s %s", key, kind, id)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(v), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func deployed(instance string) automation.Event {
	return automation.Event{ID: "evt-1", Type: "deployment.completed",
		Data: automation.EventData{Attributes: map[string]any{"md-instance": instance, "md-project": "billing",
			"md-environment": "prod"}}}
}

// prodApp is billing-prod-app: deployed, with one alarm, a passing test run, and no drift.
func prodApp(e *env) *Instance {
	app := e.md.addInstance("billing", "prod", "app", nil)
	app.DeployedVersion, app.ResolvedVersion = "1.2.0", "1.2.0"
	app.Bundle = &struct {
		Name string `json:"name"`
	}{Name: "aws-ecs-service"}
	app.Params = map[string]any{"replicas": float64(2)}
	app.LatestProvision = &Deployment{ID: "dep-3", Status: "COMPLETED", Action: "PROVISION", Version: "1.2.0",
		Params: map[string]any{"replicas": float64(2)}, CreatedAt: ago(day), LastTransitionedAt: tp(ago(day - 5*time.Minute))}
	e.md.deployments[app.ID] = []Deployment{
		*app.LatestProvision,
		{ID: "dep-2", Status: "FAILED", Action: "PROVISION", Version: "1.2.0", CreatedAt: ago(2 * day), ElapsedTime: 60},
		{ID: "dep-1", Status: "COMPLETED", Action: "PROVISION", Version: "1.1.0", CreatedAt: ago(3 * day), ElapsedTime: 120,
			LastTransitionedAt: tp(ago(3*day - 2*time.Minute))},
	}
	e.md.deployments[app.ID][0].ElapsedTime = 300
	e.md.audit = append(e.md.audit, fakeAudit{instance: app.ID, actorType: "SERVICE_ACCOUNT",
		event: AuditEvent{Type: "deployment.created", OccurredAt: ago(day), Data: map[string]any{"entity": map[string]any{"id": "dep-3"}}}})
	e.md.alarms[app.ID] = []Alarm{{DisplayName: "High CPU", CurrentState: &AlarmState{Status: "OK", OccurredAt: ago(day)}}}
	e.md.tags["aws-ecs-service"] = []Tag{{Tag: "1.3.0", CreatedAt: ago(10 * day)}, {Tag: "1.2.0", CreatedAt: ago(40 * day)}}
	e.md.put("instance", app.ID, "md/v1/drift", `{"status":"none","checked_at":"2026-11-27T06:00:00Z"}`, ago(6*time.Hour))
	e.md.put("deployment", "dep-3", "md/v1/tests", `{"tool":"go","passed":12,"failed":0}`, ago(day))
	return app
}

func TestDeploymentEventWritesFactsAndScores(t *testing.T) {
	e := setup(t, readiness)
	prodApp(e)
	e.handle(t, deployed("billing-prod-app"))

	delivery := e.decode(t, "instance", "billing-prod-app", KeyDelivery)
	want := map[string]any{"tool": "scorecards", "window_days": float64(30), "deployments": float64(3),
		"failed": float64(1), "rolled_back": float64(0), "automated": float64(1), "failure_rate": 0.3333,
		"median_duration_seconds": float64(120), "last_status": "completed",
		"last_completed_at": "2026-11-26T12:05:00Z"}
	if !reflect.DeepEqual(delivery, want) {
		t.Errorf("delivery = %v\nwant %v", delivery, want)
	}
	currency := e.decode(t, "instance", "billing-prod-app", KeyCurrency)
	if want := map[string]any{"tool": "scorecards", "version": "1.2.0", "latest": "1.3.0", "behind": float64(1),
		"latest_published_at": "2026-11-17T12:00:00Z", "days_behind": float64(10)}; !reflect.DeepEqual(currency, want) {
		t.Errorf("currency = %v", currency)
	}
	pending := e.decode(t, "instance", "billing-prod-app", KeyPending)
	if want := map[string]any{"tool": "scorecards", "changed": false, "deployed_at": "2026-11-26T12:05:00Z"}; !reflect.DeepEqual(pending, want) {
		t.Errorf("pending = %v", pending)
	}

	card := e.decode(t, "instance", "billing-prod-app", "md/v1/scorecard/production-readiness")
	if card["level"] != "gold" || card["score"] != float64(100) || card["passed"] != float64(4) ||
		card["evaluated_at"] != "2026-11-27T12:00:00Z" {
		t.Errorf("scorecard = %v", card)
	}
	rollup := e.decode(t, "environment", "billing-prod", "md/v1/scorecard/production-readiness")
	if want := map[string]any{"tool": "scorecards", "level": "gold", "score": float64(100), "instances": float64(1),
		"levels": map[string]any{"gold": float64(1)}, "lowest_level": "gold",
		"evaluated_at": "2026-11-27T12:00:00Z"}; !reflect.DeepEqual(rollup, want) {
		t.Errorf("environment rollup = %v", rollup)
	}
	if _, ok := e.md.value("project", "billing", "md/v1/scorecard/production-readiness"); !ok {
		t.Error("no project rollup")
	}
}

func TestRedeliveredEventWritesNothing(t *testing.T) {
	e := setup(t, readiness)
	prodApp(e)
	e.handle(t, deployed("billing-prod-app"))
	first := len(e.md.written())
	e.handle(t, deployed("billing-prod-app"))
	if n := len(e.md.written()); n != first {
		t.Errorf("writes = %d after the repeat, want %d", n, first)
	}
}

func TestRestartWritesNothingThatDidNotChange(t *testing.T) {
	e := setup(t, readiness)
	prodApp(e)
	e.s.Pass()
	e.s.Drain(context.Background())
	first := len(e.md.written())

	restarted := New(e.s.cfg, e.md, e.md, func() time.Time { return now.Add(time.Hour) })
	restarted.Pass()
	restarted.Drain(context.Background())
	if n := len(e.md.written()); n != first {
		t.Errorf("writes = %d after a restart, want %d", n, first)
	}
}

func TestMissingStaleAndExpiredInputsAreUnknown(t *testing.T) {
	e := setup(t, readiness)
	app := prodApp(e)
	// The drift check is older than max_age, and the latest provision has no test results.
	e.md.put("instance", app.ID, "md/v1/drift", `{"status":"detected"}`, ago(3*day))
	app.LatestProvision.ID = "dep-4"
	e.handle(t, deployed(app.ID))

	card := e.decode(t, "instance", app.ID, "md/v1/scorecard/production-readiness")
	checks := map[string]string{}
	for _, c := range card["checks"].([]any) {
		m := c.(map[string]any)
		checks[m["id"].(string)] = m["status"].(string)
	}
	want := map[string]string{"no-drift": "unknown", "has-alarms": "passed", "tests-passed": "unknown", "not-firing": "passed"}
	if !reflect.DeepEqual(checks, want) {
		t.Errorf("checks = %v", checks)
	}
	// Unknown checks do not count: 2 of 2 known checks passed.
	if card["score"] != float64(100) || card["unknown"] != float64(2) || card["level"] != "gold" {
		t.Errorf("scorecard = %v", card)
	}

	expired := now.Add(-time.Minute)
	v := `{"status":"none"}`
	e.md.stash["instance/"+app.ID+"/md/v1/drift"] = StashValue{Key: "md/v1/drift", Value: &v, Format: "JSON",
		UpdatedAt: now, ExpiresAt: &expired}
	sc := &e.s.cfg.Scorecards[0]
	in := inputs{now: now, stash: map[string]StashValue{"instance/md/v1/drift": e.md.stash["instance/"+app.ID+"/md/v1/drift"]}}
	if r := runCheck(context.Background(), &sc.Checks[0], in); r.Status != StatusUnknown || !strings.Contains(r.Message, "expired") {
		t.Errorf("expired = %+v", r)
	}
}

func TestScoreAndLevels(t *testing.T) {
	cfg, err := ParseConfig([]byte(`
scorecards:
  - name: basics
    checks:
      - {id: a, stash: a, pass: "true"}
      - {id: b, stash: b, pass: ". == 1"}
      - {id: c, stash: c, pass: .missing.deeper}
      - {id: d, stash: d, pass: "error(\"boom\")"}
      - {id: e, stash: e, pass: "null"}
    levels: {gold: 100, silver: 60, bronze: 30}
`))
	if err != nil {
		t.Fatal(err)
	}
	sc := &cfg.Scorecards[0]
	val := func(s string) StashValue { return StashValue{Value: &s, Format: "JSON", UpdatedAt: now} }
	in := inputs{now: now, stash: map[string]StashValue{
		"instance/a": val(`{}`), "instance/b": val(`2`), "instance/c": val(`{"missing":{"deeper":1}}`),
		"instance/d": val(`{}`), "instance/e": val(`{}`),
	}}
	s := score(context.Background(), sc, in)
	// a and c pass; b and e fail; d's expression fails, so it is unknown.
	if s.Passed != 2 || s.Failed != 2 || s.Unknown != 1 || s.Score != 50 || s.Level != "bronze" {
		t.Errorf("score = %+v", s)
	}
	if s.Checks[3].Status != StatusUnknown || !strings.Contains(s.Checks[3].Message, "boom") {
		t.Errorf("check d = %+v", s.Checks[3])
	}

	none := score(context.Background(), sc, inputs{now: now, stash: map[string]StashValue{}})
	if none.Level != NoLevel || none.Score != 0 || none.Unknown != 5 {
		t.Errorf("all unknown = %+v", none)
	}
	for score, want := range map[float64]string{100: "gold", 99.9: "silver", 60: "silver", 30: "bronze", 29: NoLevel} {
		if got := sc.levelOf(score); got != want {
			t.Errorf("levelOf(%v) = %s, want %s", score, got, want)
		}
	}
}

func TestRollupCountsLevelsAndLowest(t *testing.T) {
	cfg, err := ParseConfig([]byte(`
scorecards:
  - name: basics
    checks: [{id: a, stash: a, pass: "true"}]
`))
	if err != nil {
		t.Fatal(err)
	}
	sc := &cfg.Scorecards[0]
	r, ok := rollup(now, sc, []InstanceScore{
		{Level: "gold", Score: 100}, {Level: "silver", Score: 80}, {Level: "gold", Score: 100}, {Level: NoLevel, Score: 0},
	})
	if !ok {
		t.Fatal("no rollup")
	}
	want := Rollup{Tool: "scorecards", Level: "bronze", Score: 70, Instances: 4,
		Levels: map[string]int{"gold": 2, "silver": 1, "none": 1}, LowestLevel: "none", EvaluatedAt: "2026-11-27T12:00:00Z"}
	if !reflect.DeepEqual(r, want) {
		t.Errorf("rollup = %+v", r)
	}
	if _, ok := rollup(now, sc, nil); ok {
		t.Error("rollup of no instances")
	}
}

func TestEnvironmentRollupOverInstances(t *testing.T) {
	e := setup(t, readiness)
	prodApp(e)
	// billing-prod-ledger has no alarms, drift, or tests: has-alarms fails, and the rest is unknown but not-firing.
	ledger := e.md.addInstance("billing", "prod", "ledger", nil)
	ledger.Params = map[string]any{}
	// billing-dev-app is not in prod, so the scorecard does not match it.
	e.md.addInstance("billing", "dev", "app", nil)

	e.s.Pass()
	e.s.Drain(context.Background())

	rollup := e.decode(t, "environment", "billing-prod", "md/v1/scorecard/production-readiness")
	if rollup["instances"] != float64(2) || rollup["lowest_level"] != "bronze" ||
		!reflect.DeepEqual(rollup["levels"], map[string]any{"gold": float64(1), "bronze": float64(1)}) {
		t.Errorf("rollup = %v", rollup)
	}
	if _, ok := e.md.value("instance", "billing-dev-app", "md/v1/scorecard/production-readiness"); ok {
		t.Error("billing-dev-app was scored")
	}
	if _, ok := e.md.value("instance", "billing-dev-app", KeyDelivery); ok {
		t.Error("billing-dev-app has facts")
	}
}

func TestStashEventsThatStartWork(t *testing.T) {
	e := setup(t, readiness)
	prodApp(e)
	stashEvent := func(kind, id, key string) automation.Event {
		entity, _ := json.Marshal(map[string]any{"key": key, "scope": map[string]any{"kind": kind, "id": id}})
		return automation.Event{ID: key, Type: "stash_entry.set", Data: automation.EventData{Entity: entity,
			Attributes: map[string]any{"md-instance": "billing-prod-app", "md-project": "billing", "md-environment": "prod"}}}
	}
	for _, ev := range []automation.Event{
		stashEvent("instance", "billing-prod-app", "md/v1/scorecard/production-readiness"),
		stashEvent("instance", "billing-prod-app", "md/v1/delivery"),
		stashEvent("instance", "billing-prod-app", "md/v1/traffic"),
	} {
		e.handle(t, ev)
	}
	if n := len(e.md.written()); n != 0 {
		t.Errorf("writes = %d for keys no check reads", n)
	}
	e.handle(t, stashEvent("deployment", "dep-3", "md/v1/tests"))
	if _, ok := e.md.value("instance", "billing-prod-app", "md/v1/scorecard/production-readiness"); !ok {
		t.Error("a checked key on the deployment did not score the instance")
	}
}

func TestOtherEventsQueueTheirWork(t *testing.T) {
	e := setup(t, readiness)
	app := prodApp(e)
	repo, _ := json.Marshal(map[string]any{"name": "aws-ecs-service"})
	e.handle(t, automation.Event{ID: "r", Type: "repo.published", Data: automation.EventData{Entity: repo}})
	if _, ok := e.md.value("instance", app.ID, KeyCurrency); !ok {
		t.Error("repo.published did not compute the instance")
	}

	delete(e.md.instances, app.ID)
	e.md.stash = map[string]StashValue{}
	e.handle(t, automation.Event{ID: "d", Type: "instance.deleted", Data: automation.EventData{
		Attributes: map[string]any{"md-instance": app.ID, "md-project": "billing", "md-environment": "prod"}}})
	if n := len(e.md.stash); n != 0 {
		t.Errorf("a deleted instance's roll-up with no instances was written: %v", e.md.stash)
	}
}

func TestFailedFactIsSkippedAndTheRestIsWritten(t *testing.T) {
	e := setup(t, readiness)
	app := prodApp(e)
	e.md.failing["Tags"] = errors.New("timeout")
	e.handle(t, deployed(app.ID))
	if _, ok := e.md.value("instance", app.ID, KeyCurrency); ok {
		t.Error("currency was written")
	}
	for _, key := range []string{KeyDelivery, KeyReliability, KeyPending, "md/v1/scorecard/production-readiness"} {
		if _, ok := e.md.value("instance", app.ID, key); !ok {
			t.Errorf("no %s", key)
		}
	}
}

func TestOwnershipAndParity(t *testing.T) {
	e := setup(t, readiness+`
parity:
  - {environment: billing-prod, reference: billing-staging}
`)
	prodApp(e)
	e.md.groups = []Group{{ID: "g1", Name: "Billing", Role: "CUSTOM"}, {ID: "g2", Name: "Admins", Role: "ORGANIZATION_ADMIN"},
		{ID: "g3", Name: "Readers", Role: "CUSTOM"}}
	e.md.members = map[string][]string{"g1": {"a1", "a2"}, "g2": {"a9"}, "g3": {"a2", "a3"}}
	e.md.actions = map[string][]string{
		"billing-prod/g1": {"environment:deploy", "project:update"},
		"billing-prod/g2": {"environment:deploy", "project:update"},
		"billing-prod/g3": {"project:view"},
	}
	v12, v11 := "1.2.0", "1.1.0"
	cmp := InstanceComparison{Equal: false}
	cmp.Component.ID = "billing-app"
	cmp.Source = &struct {
		ID string `json:"id"`
	}{ID: "billing-prod-app"}
	cmp.Target = &struct {
		ID string `json:"id"`
	}{ID: "billing-staging-app"}
	cmp.Version.Source, cmp.Version.Target = &v12, &v11
	cmp.Params = []struct {
		Path  string `json:"path"`
		Equal bool   `json:"equal"`
	}{{Path: ".replicas", Equal: false}, {Path: ".image", Equal: true}}
	only := InstanceComparison{}
	only.Component.ID = "billing-cache"
	only.Target = &struct {
		ID string `json:"id"`
	}{ID: "billing-staging-cache"}
	e.md.compare["billing-prod/billing-staging"] = []InstanceComparison{cmp, only}

	e.s.Pass()
	e.s.Drain(context.Background())

	owners := e.decode(t, "environment", "billing-prod", KeyOwnership)
	if want := map[string]any{"tool": "scorecards", "members": float64(2),
		"groups": []any{map[string]any{"id": "g1", "name": "Billing"}}}; !reflect.DeepEqual(owners, want) {
		t.Errorf("environment ownership = %v", owners)
	}
	if p := e.decode(t, "project", "billing", KeyOwnership); p["members"] != float64(2) {
		t.Errorf("project ownership = %v", p)
	}
	parity := e.decode(t, "environment", "billing-prod", KeyParity)
	want := map[string]any{"tool": "scorecards", "reference_environment": "billing-staging", "differences": float64(3),
		"checked_at": "2026-11-27T12:00:00Z", "items": []any{
			map[string]any{"instance": "billing-prod-app", "field": "version", "message": "Version 1.2.0 here, 1.1.0 in billing-staging."},
			map[string]any{"instance": "billing-prod-app", "field": ".replicas", "message": "Differs from billing-staging."},
			map[string]any{"instance": "billing-cache", "field": "instance", "message": "Only in billing-staging."},
		}}
	if !reflect.DeepEqual(parity, want) {
		t.Errorf("parity = %v\nwant %v", parity, want)
	}
}

func TestConfigErrors(t *testing.T) {
	cases := map[string]string{
		"no scorecards": `interval: 1h`,
		"bad name":      `scorecards: [{name: Prod Ready, checks: [{id: a, stash: a, pass: "true"}]}]`,
		"no checks":     `scorecards: [{name: a, checks: []}]`,
		"two inputs":    `scorecards: [{name: a, checks: [{id: a, stash: a, query: instance, pass: "true"}]}]`,
		"no input":      `scorecards: [{name: a, checks: [{id: a, pass: "true"}]}]`,
		"bad query":     `scorecards: [{name: a, checks: [{id: a, query: deployments, pass: "true"}]}]`,
		"bad jq":        `scorecards: [{name: a, checks: [{id: a, stash: a, pass: ".a ==="}]}]`,
		"no pass":       `scorecards: [{name: a, checks: [{id: a, stash: a}]}]`,
		"bad scope":     `scorecards: [{name: a, checks: [{id: a, stash: {key: a, scope: component}, pass: "true"}]}]`,
		"same id":       `scorecards: [{name: a, checks: [{id: a, stash: a, pass: "true"}, {id: a, stash: b, pass: "true"}]}]`,
		"same name": `scorecards: [{name: a, checks: [{id: a, stash: a, pass: "true"}]},
                       {name: a, checks: [{id: a, stash: a, pass: "true"}]}]`,
		"level over 100":   `scorecards: [{name: a, checks: [{id: a, stash: a, pass: "true"}], levels: {gold: 101}}]`,
		"level none":       `scorecards: [{name: a, checks: [{id: a, stash: a, pass: "true"}], levels: {none: 10}}]`,
		"max_age on query": `scorecards: [{name: a, checks: [{id: a, query: instance, pass: "true", max_age: 1h}]}]`,
		"short interval":   "interval: 10s\nscorecards: [{name: a, checks: [{id: a, stash: a, pass: \"true\"}]}]",
		"partial window":   "delivery: {window: 36h}\nscorecards: [{name: a, checks: [{id: a, stash: a, pass: \"true\"}]}]",
		"parity to self":   "parity: [{environment: a, reference: a}]\nscorecards: [{name: a, checks: [{id: a, stash: a, pass: \"true\"}]}]",
		"unknown key":      "scorecards: [{name: a, owner: me, checks: [{id: a, stash: a, pass: \"true\"}]}]",
	}
	for name, doc := range cases {
		if _, err := ParseConfig([]byte(doc)); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

func TestConfigDefaults(t *testing.T) {
	cfg, err := ParseConfig([]byte(`scorecards: [{name: a, checks: [{id: a, stash: md/v1/drift, pass: "true"}]}]`))
	if err != nil {
		t.Fatal(err)
	}
	sc := cfg.Scorecards[0]
	if time.Duration(cfg.Interval) != time.Hour || time.Duration(cfg.Delivery.Window) != 30*day ||
		cfg.Ownership.EnvironmentAction != "environment:deploy" || cfg.Ownership.ProjectAction != "project:update" ||
		sc.Checks[0].Stash.Scope != ScopeInstance || !reflect.DeepEqual(sc.Levels, map[string]int{"gold": 100, "silver": 80, "bronze": 50}) {
		t.Errorf("cfg = %+v", cfg)
	}
}

func TestConfigExampleLoads(t *testing.T) {
	var cfg Config
	if err := automation.LoadConfigFile("config.example.yaml", &cfg); err != nil {
		t.Fatal(err)
	}
	if len(cfg.Scorecards) != 1 || len(cfg.Scorecards[0].Checks) != 5 {
		t.Errorf("cfg = %+v", cfg)
	}
}

func TestChecksCannotReadTheEnvironment(t *testing.T) {
	t.Setenv("MASSDRIVER_API_KEY", "md_secret")
	cfg, err := ParseConfig([]byte(`scorecards: [{name: a, checks: [{id: a, stash: a, pass: "$ENV.MASSDRIVER_API_KEY == \"md_secret\""}]}]`))
	if err != nil {
		t.Fatal(err)
	}
	v := `{}`
	r := runCheck(context.Background(), &cfg.Scorecards[0].Checks[0],
		inputs{now: now, stash: map[string]StashValue{"instance/a": {Value: &v, Format: "JSON", UpdatedAt: now}}})
	if r.Status != StatusFailed {
		t.Errorf("check = %+v", r)
	}
}

func TestDestinationConditionDropsOwnKeys(t *testing.T) {
	for key, own := range map[string]bool{
		"md/v1/scorecard/production-readiness": true, "md/v1/delivery": true, "md/v1/ownership": true,
		"md/v1/drift": false, "md/v1/tests": false, "md/v1/delivery-extra": false,
	} {
		if ownKey.MatchString(key) != own {
			t.Errorf("%s own = %v", key, !own)
		}
	}
}
