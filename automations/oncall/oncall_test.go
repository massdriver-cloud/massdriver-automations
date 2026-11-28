package oncall

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/massdriver-cloud/massdriver-automations/internal/widgetschema"
	"github.com/massdriver-cloud/massdriver-automations/pkg/automation"
	"github.com/massdriver-cloud/massdriver-automations/pkg/massdriver"
)

var start = time.Date(2026, 11, 27, 9, 0, 0, 0, time.UTC)

// fakePagerDuty serves GET /oncalls from canned entries, filtered by schedule or escalation policy, in pages.
type fakePagerDuty struct {
	mu       sync.Mutex
	oncalls  []map[string]any
	pageSize int
	fail     int
	requests []*http.Request
}

func (f *fakePagerDuty) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, r)
	q := r.URL.Query()
	if r.URL.Path != "/oncalls" || q.Get("include[]") != "users" || q.Get("earliest") != "true" {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if f.fail != 0 {
		http.Error(w, "down", f.fail)
		return
	}
	var match []map[string]any
	for _, o := range f.oncalls {
		if ids := q["schedule_ids[]"]; ids != nil && !slices.Contains(ids, o["_schedule"].(string)) {
			continue
		}
		if ids := q["escalation_policy_ids[]"]; ids != nil && !slices.Contains(ids, o["_policy"].(string)) {
			continue
		}
		match = append(match, map[string]any{"escalation_level": o["escalation_level"], "user": o["user"]})
	}
	offset, _ := strconv.Atoi(q.Get("offset"))
	size := f.pageSize
	if size == 0 {
		size = 100
	}
	end := min(offset+size, len(match))
	page := match[offset:end]
	if page == nil {
		page = []map[string]any{}
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"oncalls": page, "more": end < len(match), "offset": offset})
}

func (f *fakePagerDuty) calls() []*http.Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*http.Request(nil), f.requests...)
}

func pdOnCall(level int, schedule, policy, id, name string) map[string]any {
	return map[string]any{
		"escalation_level": level, "_schedule": schedule, "_policy": policy,
		"user": map[string]any{
			"id": id, "type": "user", "summary": name, "name": name,
			"email":      strings.ToLower(strings.Fields(name)[0]) + "@tendwell.example",
			"avatar_url": "https://secure.gravatar.com/avatar/" + id + ".png?d=mm&r=PG",
		},
	}
}

// fakeIncidentIO serves the schedule and escalation path show endpoints.
type fakeIncidentIO struct {
	schedules map[string][]map[string]any
	paths     map[string][]map[string]any
	auth      string
}

func (f *fakeIncidentIO) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.auth = r.Header.Get("authorization")
	if id, ok := strings.CutPrefix(r.URL.Path, "/v2/schedules/"); ok {
		shifts := []map[string]any{}
		for _, u := range f.schedules[id] {
			shifts = append(shifts, map[string]any{"start_at": start.Format(time.RFC3339), "user": u})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"schedule": map[string]any{"id": id, "current_shifts": shifts}})
		return
	}
	if id, ok := strings.CutPrefix(r.URL.Path, "/v2/escalation_paths/"); ok {
		_ = json.NewEncoder(w).Encode(map[string]any{"escalation_path": map[string]any{"id": id,
			"current_responders": f.paths[id]}})
		return
	}
	http.NotFound(w, r)
}

func iioUser(id, name string) map[string]any {
	return map[string]any{"id": id, "name": name, "email": strings.ToLower(name) + "@tendwell.example",
		"role": "responder", "slack_user_id": "U" + id}
}

type setCall struct {
	Scope map[string]any
	Key   string
	Input map[string]any
}

// fakeAPI serves the stash mutation of the Massdriver API.
type fakeAPI struct {
	mu      sync.Mutex
	sets    []setCall
	invalid []error
}

func (f *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Variables map[string]any `json:"variables"`
	}
	b, _ := io.ReadAll(r.Body)
	_ = json.Unmarshal(b, &req)
	f.mu.Lock()
	defer f.mu.Unlock()
	c := setCall{Scope: req.Variables["scope"].(map[string]any), Key: req.Variables["key"].(string),
		Input: req.Variables["input"].(map[string]any)}
	f.sets = append(f.sets, c)
	for scope := range c.Scope {
		if err := widgetschema.Check(scope, c.Key, c.Input["format"].(string), c.Input["value"].(string)); err != nil {
			f.invalid = append(f.invalid, err)
		}
	}
	_, _ = w.Write([]byte(`{"data":{"alphaSetStashEntry":{"successful":true,"messages":[],"result":{"key":"x"}}}}`))
}

func (f *fakeAPI) calls() []setCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]setCall(nil), f.sets...)
}

type env struct {
	pd    *fakePagerDuty
	iio   *fakeIncidentIO
	api   *fakeAPI
	clock time.Time
	o     *OnCall
}

func setup(t *testing.T, cfgYAML string) *env {
	t.Helper()
	var cfg Config
	if err := automation.DecodeConfig([]byte(cfgYAML), &cfg); err != nil {
		t.Fatal(err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	e := &env{pd: &fakePagerDuty{}, iio: &fakeIncidentIO{schedules: map[string][]map[string]any{},
		paths: map[string][]map[string]any{}}, api: &fakeAPI{}, clock: start}
	pdSrv := httptest.NewServer(e.pd)
	t.Cleanup(pdSrv.Close)
	iioSrv := httptest.NewServer(e.iio)
	t.Cleanup(iioSrv.Close)
	apiSrv := httptest.NewServer(e.api)
	t.Cleanup(apiSrv.Close)
	t.Cleanup(func() {
		for _, err := range e.api.invalid {
			t.Errorf("the server would reject this write: %v", err)
		}
	})
	client := &massdriver.Client{Endpoint: apiSrv.URL, OrganizationID: "tendwell-health", APIKey: "md_test"}
	e.o = New(&cfg,
		&PagerDuty{URL: pdSrv.URL, Token: "pd_test", HTTP: pdSrv.Client()},
		&IncidentIO{URL: iioSrv.URL, Token: "iio_test", HTTP: iioSrv.Client()},
		client, func() time.Time { return e.clock })
	return e
}

func (e *env) cycle() { e.o.Cycle(context.Background()) }

type value struct {
	Title   string           `json:"title"`
	Entries []map[string]any `json:"entries"`
}

func decode(t *testing.T, c setCall) value {
	t.Helper()
	var v value
	if err := json.Unmarshal([]byte(c.Input["value"].(string)), &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func names(v value) []string {
	out := make([]string, len(v.Entries))
	for i, p := range v.Entries {
		out[i], _ = p["name"].(string)
	}
	return out
}

func TestPagerDutyListsPeopleByLevelOnce(t *testing.T) {
	e := setup(t, `
routes:
  - environments: [billing-prod, billing-staging]
    title: Billing on call
    pagerduty:
      schedules: [PSCHED1]
      escalation_policies: [PPOLICY]
`)
	e.pd.oncalls = []map[string]any{
		pdOnCall(2, "PSCHED1", "PPOLICY", "PLEE", "Lee Chen"),
		pdOnCall(1, "PSCHED1", "PPOLICY", "PJO", "Jo Park"),
		pdOnCall(3, "", "PPOLICY", "PANA", "Ana Ruiz"),
		pdOnCall(1, "PSCHED2", "POTHER", "PSAM", "Sam Wu"),
		pdOnCall(3, "PSCHED1", "PPOLICY", "PJO", "Jo Park"),
	}
	e.cycle()

	calls := e.api.calls()
	if len(calls) != 2 || calls[0].Scope["environment"] != "billing-prod" || calls[1].Scope["environment"] != "billing-staging" {
		t.Fatalf("calls = %+v", calls)
	}
	c := calls[0]
	if c.Key != "md/v1/oncall" || c.Input["format"] != "JSON" || c.Input["label"] != "On call" ||
		c.Input["ttl"] != float64(360) {
		t.Errorf("write = %s %v", c.Key, c.Input)
	}
	v := decode(t, c)
	if v.Title != "Billing on call" {
		t.Errorf("title = %q", v.Title)
	}
	if got := names(v); !reflect.DeepEqual(got, []string{"Jo Park", "Lee Chen", "Ana Ruiz"}) {
		t.Errorf("people = %v", got)
	}
	want := map[string]any{"name": "Jo Park", "email": "jo@tendwell.example",
		"image_url": "https://secure.gravatar.com/avatar/PJO.png?d=mm&r=PG", "handles": []any{"pagerduty:PJO"}}
	if !reflect.DeepEqual(v.Entries[0], want) {
		t.Errorf("jo = %v", v.Entries[0])
	}
	reqs := e.pd.calls()
	if len(reqs) != 2 {
		t.Fatalf("requests = %d, want one for schedules and one for escalation policies", len(reqs))
	}
	if q := reqs[0].URL.Query(); !reflect.DeepEqual(q["schedule_ids[]"], []string{"PSCHED1"}) ||
		q["escalation_policy_ids[]"] != nil {
		t.Errorf("first query = %v", q)
	}
	if q := reqs[1].URL.Query(); !reflect.DeepEqual(q["escalation_policy_ids[]"], []string{"PPOLICY"}) {
		t.Errorf("second query = %v", q)
	}
	if got := reqs[0].Header.Get("authorization"); got != "Token token=pd_test" {
		t.Errorf("authorization = %q", got)
	}
}

func TestPagerDutyPagesAndMaxPeople(t *testing.T) {
	e := setup(t, `
routes:
  - environments: [billing-prod]
    max_people: 3
    pagerduty: {escalation_policies: [PPOLICY]}
`)
	e.pd.pageSize = 2
	for i, n := range []string{"E One", "D Two", "C Three", "B Four", "A Five"} {
		e.pd.oncalls = append(e.pd.oncalls, pdOnCall(5-i, "S", "PPOLICY", "P"+strconv.Itoa(i), n))
	}
	e.cycle()
	if got := names(decode(t, e.api.calls()[0])); !reflect.DeepEqual(got, []string{"A Five", "B Four", "C Three"}) {
		t.Errorf("people = %v", got)
	}
	if n := len(e.pd.calls()); n != 3 {
		t.Errorf("pages = %d, want 3", n)
	}
}

func TestIncidentIOSchedulesThenPaths(t *testing.T) {
	e := setup(t, `
routes:
  - environments: [visits-prod]
    incident_io:
      schedules: [01SCHEDPRIMARY, 01SCHEDSECOND]
      escalation_paths: [01PATH]
`)
	e.iio.schedules["01SCHEDPRIMARY"] = []map[string]any{iioUser("01JO", "Jo")}
	e.iio.schedules["01SCHEDSECOND"] = []map[string]any{iioUser("01LEE", "Lee"), iioUser("01JO", "Jo")}
	e.iio.paths["01PATH"] = []map[string]any{iioUser("01ANA", "Ana")}
	e.cycle()
	v := decode(t, e.api.calls()[0])
	if got := names(v); !reflect.DeepEqual(got, []string{"Jo", "Lee", "Ana"}) {
		t.Errorf("people = %v", got)
	}
	want := map[string]any{"name": "Jo", "email": "jo@tendwell.example", "handles": []any{"incidentio:01JO"}}
	if !reflect.DeepEqual(v.Entries[0], want) {
		t.Errorf("jo = %v", v.Entries[0])
	}
	if e.iio.auth != "Bearer iio_test" {
		t.Errorf("authorization = %q", e.iio.auth)
	}
}

func TestFailedProviderKeepsTheLastEntry(t *testing.T) {
	e := setup(t, `
routes:
  - environments: [billing-prod]
    pagerduty: {schedules: [PSCHED1]}
  - environments: [sandbox-dev]
    static: [{name: Jo Park}]
`)
	e.pd.fail = http.StatusTooManyRequests
	e.cycle()
	calls := e.api.calls()
	if len(calls) != 1 || calls[0].Scope["environment"] != "sandbox-dev" {
		t.Errorf("calls = %+v", calls)
	}
}

func TestNobodyOnCallWritesNothing(t *testing.T) {
	e := setup(t, "routes:\n  - environments: [billing-prod]\n    pagerduty: {schedules: [PSCHED1]}\n")
	e.cycle()
	if n := len(e.api.calls()); n != 0 {
		t.Errorf("writes = %d", n)
	}
}

func TestChangesAreWrittenAndUnchangedListsRefreshBeforeExpiry(t *testing.T) {
	e := setup(t, "routes:\n  - environments: [billing-prod]\n    pagerduty: {schedules: [PSCHED1]}\n")
	e.pd.oncalls = []map[string]any{pdOnCall(1, "PSCHED1", "P", "PJO", "Jo Park")}
	e.cycle()
	e.clock = e.clock.Add(2 * time.Minute)
	e.cycle()
	if n := len(e.api.calls()); n != 1 {
		t.Fatalf("writes = %d, want 1", n)
	}
	e.pd.mu.Lock()
	e.pd.oncalls = []map[string]any{pdOnCall(1, "PSCHED1", "P", "PLEE", "Lee Chen")}
	e.pd.mu.Unlock()
	e.clock = e.clock.Add(2 * time.Minute)
	e.cycle()
	calls := e.api.calls()
	if len(calls) != 2 || names(decode(t, calls[1]))[0] != "Lee Chen" {
		t.Fatalf("calls = %+v", calls)
	}
	e.clock = e.clock.Add(4 * time.Minute)
	e.cycle()
	if n := len(e.api.calls()); n != 3 {
		t.Errorf("writes = %d after two intervals, want 3", n)
	}
}

func TestStaticRouteNeedsNoProvider(t *testing.T) {
	var cfg Config
	doc := "routes:\n  - environments: [sandbox-dev]\n    title: Demo\n" +
		"    static:\n      - {name: Jo Park, handles: ['pagerduty:jo@tendwell.example']}\n"
	if err := automation.DecodeConfig([]byte(doc), &cfg); err != nil {
		t.Fatal(err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if cfg.usesPagerDuty() || cfg.usesIncidentIO() {
		t.Error("a static route needs no provider token")
	}
	api := &fakeAPI{}
	srv := httptest.NewServer(api)
	t.Cleanup(srv.Close)
	client := &massdriver.Client{Endpoint: srv.URL, OrganizationID: "tendwell-health", APIKey: "md_test"}
	New(&cfg, nil, nil, client, func() time.Time { return start }).Cycle(context.Background())
	calls := api.calls()
	if len(calls) != 1 {
		t.Fatalf("calls = %+v", calls)
	}
	if got := calls[0].Input["value"]; got != `{"title":"Demo","entries":[{"name":"Jo Park","handles":["pagerduty:jo@tendwell.example"]}]}` {
		t.Errorf("value = %s", got)
	}
}

func TestHandleIgnoresEvents(t *testing.T) {
	e := setup(t, "routes:\n  - environments: [a]\n    static: [{name: Jo}]\n")
	if err := e.o.Handle(context.Background(), automation.Event{Type: "deployment.completed"}); err != nil {
		t.Fatal(err)
	}
	if n := len(e.api.calls()); n != 0 {
		t.Errorf("writes = %d", n)
	}
}

func TestConfigErrors(t *testing.T) {
	pd := "    pagerduty: {schedules: [PSCHED1]}\n"
	for name, tc := range map[string]struct{ yaml, want string }{
		"no routes":       {"interval: 2m\n", "add at least one route"},
		"short interval":  {"interval: 10s\nroutes:\n  - environments: [a]\n" + pd, "interval must be at least 30s"},
		"no environments": {"routes:\n  -\n" + pd, "must name at least one environment"},
		"shared env":      {"routes:\n  - environments: [a]\n" + pd + "  - environments: [a]\n" + pd, "more than one route"},
		"two sources": {"routes:\n  - environments: [a]\n" + pd + "    incident_io: {schedules: [X]}\n",
			"exactly one of pagerduty, incident_io, or static"},
		"no source":   {"routes:\n  - environments: [a]\n", "exactly one of"},
		"no ids":      {"routes:\n  - environments: [a]\n    pagerduty: {}\n", "at least one of schedules or escalation_policies"},
		"no iio ids":  {"routes:\n  - environments: [a]\n    incident_io: {}\n", "at least one of schedules or escalation_paths"},
		"bad id":      {"routes:\n  - environments: [a]\n    pagerduty: {schedules: ['P 1']}\n", "schedules[0] must be an ID"},
		"max_people":  {"routes:\n  - environments: [a]\n    max_people: 30\n" + pd, "max_people must be from 1 to 25"},
		"bad static":  {"routes:\n  - environments: [a]\n    static: [{name: ' '}]\n", "name must be"},
		"many static": {"routes:\n  - environments: [a]\n    static: [" + strings.Repeat("{name: x},", 26) + "]\n", "1 to 25 people"},
	} {
		t.Run(name, func(t *testing.T) {
			var cfg Config
			if err := automation.DecodeConfig([]byte(tc.yaml), &cfg); err != nil {
				t.Fatal(err)
			}
			err := cfg.Validate()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestNullSettingsTakeDefaults(t *testing.T) {
	var cfg Config
	doc := "interval: null\nlabel: null\npagerduty: {api_url: null, token_env: null}\n" +
		"incident_io: {api_url: null, token_env: null}\nroutes:\n  - environments: [a]\n    max_people: null\n" +
		"    pagerduty: {schedules: [P1]}\n"
	if err := automation.DecodeConfig([]byte(doc), &cfg); err != nil {
		t.Fatal(err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if time.Duration(cfg.Interval) != 2*time.Minute || cfg.Label != "On call" || cfg.Routes[0].MaxPeople != 10 ||
		cfg.PagerDuty.APIURL != "https://api.pagerduty.com" || cfg.PagerDuty.TokenEnv != "PAGERDUTY_TOKEN" ||
		cfg.IncidentIO.APIURL != "https://api.incident.io" || cfg.IncidentIO.TokenEnv != "INCIDENT_IO_API_KEY" {
		t.Errorf("config = %+v", cfg)
	}
}

func TestExampleConfigLoads(t *testing.T) {
	var cfg Config
	if err := automation.LoadConfigFile("config.example.yaml", &cfg); err != nil {
		t.Fatal(err)
	}
	if len(cfg.Routes) != 3 || cfg.Routes[0].PagerDuty == nil || cfg.Routes[1].IncidentIO == nil ||
		cfg.Routes[2].Static == nil {
		t.Errorf("config = %+v", cfg)
	}
}

// Fields the server would reject are dropped, so the rest of the person still shows. setup checks each write
// against the server's schema.
func TestProviderPeopleAreTrimmedToTheWidget(t *testing.T) {
	e := setup(t, `
routes:
  - environments: [billing-prod]
    max_people: 25
    pagerduty: {escalation_policies: [PPOLICY]}
`)
	for i := range 30 {
		o := pdOnCall(1, "S", "PPOLICY", "P"+strconv.Itoa(i), "Person "+strconv.Itoa(i)+" "+strings.Repeat("x", 80))
		u := o["user"].(map[string]any)
		u["email"], u["avatar_url"] = "a@b.test, c@d.test", "ftp://x.test/a.png"
		e.pd.oncalls = append(e.pd.oncalls, o)
	}
	e.cycle()
	v := decode(t, e.api.calls()[0])
	if len(v.Entries) != 25 {
		t.Fatalf("entries = %d", len(v.Entries))
	}
	for _, p := range v.Entries {
		if p["email"] != nil || p["image_url"] != nil || len([]rune(p["name"].(string))) != 64 {
			t.Errorf("person = %v", p)
		}
	}
}
