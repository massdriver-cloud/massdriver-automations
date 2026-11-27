package stashsync

import (
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"sort"
	"strings"
	"time"
)

// Scenarios.
const (
	ScenarioSteady          = "steady"
	ScenarioIncident        = "incident"
	ScenarioPostgresCutover = "postgres-cutover"
	ScenarioRollout         = "rollout"
)

const (
	defaultScenarioDuration = 3 * time.Minute
	defaultMaxLag           = 120
	defaultAdvisory         = "Security advisory: php2shell remote code execution. A patch is on the way."
)

// SimulateConfig is the `simulate` section: which scenario runs, how fast, and which entities play its roles.
type SimulateConfig struct {
	Scenario string   `yaml:"scenario"`
	Duration Duration `yaml:"duration"`
	Loop     *bool    `yaml:"loop"`
	Seed     uint64   `yaml:"seed"`
	Roles    Roles    `yaml:"roles"`
}

// Roles are the environments and instances of a scenario. Each scenario reads only its own roles.
type Roles struct {
	Environment  string               `yaml:"environment"`
	Instances    []string             `yaml:"instances"`
	Instance     string               `yaml:"instance"`
	OldDatabase  string               `yaml:"old_database"`
	NewDatabase  string               `yaml:"new_database"`
	MaxLag       float64              `yaml:"max_lag"`
	Environments []RolloutEnvironment `yaml:"environments"`
	Advisory     string               `yaml:"advisory"`
}

// RolloutEnvironment is one environment of the rollout scenario, in rollout order.
type RolloutEnvironment struct {
	Environment string   `yaml:"environment"`
	Instances   []string `yaml:"instances"`
}

func (c *SimulateConfig) validate() error {
	var errs []error
	fail := func(format string, args ...any) { errs = append(errs, fmt.Errorf("simulate."+format, args...)) }
	if c.Duration == 0 {
		c.Duration = Duration(defaultScenarioDuration)
	}
	if time.Duration(c.Duration) < 10*time.Second {
		fail("duration must be at least 10s")
	}
	if c.Loop == nil {
		c.Loop = ptr(true)
	}
	r := &c.Roles
	need := func(name, v string) {
		if v == "" {
			fail("roles.%s is required for scenario %s", name, c.Scenario)
		}
	}
	switch c.Scenario {
	case ScenarioSteady:
		if len(r.Instances) == 0 {
			fail("roles.instances is required for scenario steady")
		}
	case ScenarioIncident:
		need("environment", r.Environment)
		need("instance", r.Instance)
	case ScenarioPostgresCutover:
		need("environment", r.Environment)
		need("old_database", r.OldDatabase)
		need("new_database", r.NewDatabase)
		if r.OldDatabase != "" && r.OldDatabase == r.NewDatabase {
			fail("roles.old_database and roles.new_database must differ")
		}
		if r.MaxLag == 0 {
			r.MaxLag = defaultMaxLag
		}
		if r.MaxLag < 0 {
			fail("roles.max_lag must be more than 0")
		}
	case ScenarioRollout:
		if len(r.Environments) == 0 {
			fail("roles.environments is required for scenario rollout")
		}
		for i, e := range r.Environments {
			if e.Environment == "" || len(e.Instances) == 0 {
				fail("roles.environments[%d] needs environment and instances", i)
			}
		}
		if r.Advisory == "" {
			r.Advisory = defaultAdvisory
		}
		if len(r.Advisory) > 280 {
			fail("roles.advisory must be at most 280 characters")
		}
	default:
		fail("scenario must be one of %s", strings.Join(ScenarioNames(), ", "))
	}
	for i, id := range r.Instances {
		switch id {
		case "":
			fail("roles.instances[%d] is empty", i)
		case r.Instance, r.OldDatabase, r.NewDatabase:
			fail("roles.instances[%d] already has another role", i)
		}
	}
	return errors.Join(errs...)
}

// ScenarioNames returns every scenario name, sorted.
func ScenarioNames() []string {
	out := []string{ScenarioSteady, ScenarioIncident, ScenarioPostgresCutover, ScenarioRollout}
	sort.Strings(out)
	return out
}

// simulator steps a scenario on a clock. One pass of the scenario takes `duration`.
type simulator struct {
	cfg      SimulateConfig
	scenario scenario
	start    time.Time
	tick     int
}

// scenario returns the phase and the entries at fraction f (0 to 1) of a pass, on the tick-th step.
type scenario interface {
	step(f float64, tick int) (phase string, entries []Entry)
}

func newSimulator(cfg SimulateConfig, r *rand.Rand) *simulator {
	s := &simulator{cfg: cfg}
	roles := cfg.Roles
	switch cfg.Scenario {
	case ScenarioSteady:
		s.scenario = &steady{gens: newGens(r, roles.Instances), r: r}
	case ScenarioIncident:
		s.scenario = &incident{roles: roles, r: r, gens: newGens(r, append([]string{roles.Instance}, roles.Instances...))}
	case ScenarioPostgresCutover:
		ids := append([]string{roles.OldDatabase, roles.NewDatabase}, roles.Instances...)
		s.scenario = &cutover{roles: roles, r: r, gens: newGens(r, ids),
			lag: walk{v: roles.MaxLag * 0.9, lo: roles.MaxLag * 0.8, hi: roles.MaxLag, step: roles.MaxLag * 0.03}}
	case ScenarioRollout:
		var ids []string
		for _, e := range roles.Environments {
			ids = append(ids, e.Instances...)
		}
		s.scenario = &rollout{roles: roles, r: r, gens: newGens(r, ids)}
	}
	return s
}

// step returns the entries for now. done is true once a scenario that does not loop has finished.
func (s *simulator) step(now time.Time) (phase string, entries []Entry, done bool) {
	if s.start.IsZero() {
		s.start = now
	}
	d := time.Duration(s.cfg.Duration)
	elapsed := now.Sub(s.start)
	if elapsed >= d && !*s.cfg.Loop {
		return "", nil, true
	}
	f := float64(elapsed%d) / float64(d)
	phase, entries = s.scenario.step(f, s.tick)
	s.tick++
	return phase, entries, false
}

// ---------------------------------------------------------------- generators

// walk is a random walk that stays between lo and hi.
type walk struct{ v, lo, hi, step float64 }

func (w *walk) next(r *rand.Rand) float64 {
	w.v = math.Max(w.lo, math.Min(w.hi, w.v+(r.Float64()*2-1)*w.step))
	return w.v
}

// burst is a spike of about height for length ticks out of every `every` ticks, and 0 otherwise.
type burst struct {
	every, length int
	height        float64
}

func (b burst) at(tick int, r *rand.Rand) float64 {
	if tick%b.every >= b.length {
		return 0
	}
	return b.height * (0.7 + 0.6*r.Float64())
}

// drift moves from `from` to `to` as f goes from 0 to 1.
func drift(from, to, f float64) float64 { return from + (to-from)*math.Max(0, math.Min(1, f)) }

// gen is the normal traffic and usage of one instance.
type gen struct {
	rps, p95, usage walk
	errors          burst
}

func newGens(r *rand.Rand, ids []string) map[string]*gen {
	out := map[string]*gen{}
	for _, id := range ids {
		base := 30 + r.Float64()*40
		out[id] = &gen{
			rps:    walk{v: base, lo: base * 0.8, hi: base * 1.2, step: base * 0.04},
			p95:    walk{v: 110, lo: 80, hi: 160, step: 8},
			usage:  walk{v: 45, lo: 35, hi: 60, step: 2},
			errors: burst{every: 15 + r.IntN(10), length: 1, height: 3},
		}
	}
	return out
}

func (g *gen) traffic(r *rand.Rand, tick int) map[string]any {
	return trafficData(g.rps.next(r), g.errors.at(tick, r), g.p95.next(r))
}

func (g *gen) cpu(r *rand.Rand) map[string]any { return usageData(g.usage.next(r), 100, "CPU %") }

func trafficData(rps, errors5m, p95 float64) map[string]any {
	m := map[string]any{"rps": round1(rps), "errors_5m": math.Round(errors5m)}
	if p95 > 0 {
		m["p95_ms"] = math.Round(p95)
	}
	return m
}

func idleTraffic() map[string]any { return map[string]any{"rps": 0.0, "errors_5m": 0.0} }

func usageData(value, limit float64, label string) map[string]any {
	return map[string]any{"value": round1(math.Max(0, value)), "max": limit, "label": label}
}

func round1(v float64) float64 { return math.Round(v*10) / 10 }

func instance(id, key string, data any) Entry {
	format := FormatJSON
	if _, ok := data.(string); ok {
		format = FormatText
	}
	return Entry{Scope: Scope{Kind: "instance", ID: id}, Key: key, Format: format, Data: data}
}

func health(id, state string) Entry { return instance(id, "md/v1/health", state) }

func banner(env, level, message string) Entry {
	return Entry{Scope: Scope{Kind: "environment", ID: env}, Key: "md/v1/banner", Format: FormatJSON,
		Data: map[string]any{"message": message, "level": level}}
}

// normal is the healthy dot, traffic, and usage of one instance.
func normal(id string, g *gen, r *rand.Rand, tick int) []Entry {
	return []Entry{health(id, "healthy"), instance(id, "md/v1/traffic", g.traffic(r, tick)), instance(id, "md/v1/usage", g.cpu(r))}
}

// phase is a named part of a pass that ends at fraction `until`.
type phase struct {
	name  string
	until float64
}

// phaseAt returns the phase at fraction f, and how far through that phase f is, from 0 to 1.
func phaseAt(phases []phase, f float64) (int, float64) {
	from := 0.0
	for i, p := range phases {
		if f < p.until || i == len(phases)-1 {
			return i, (f - from) / (p.until - from)
		}
		from = p.until
	}
	return len(phases) - 1, 1
}

// ---------------------------------------------------------------- steady

type steady struct {
	gens map[string]*gen
	r    *rand.Rand
}

func (s *steady) step(_ float64, tick int) (string, []Entry) {
	var out []Entry
	for _, id := range sortedKeys(s.gens) {
		out = append(out, normal(id, s.gens[id], s.r, tick)...)
	}
	return "steady", out
}

// ---------------------------------------------------------------- incident

var incidentPhases = []phase{
	{"normal", 0.15}, {"degrading", 0.35}, {"outage", 0.55}, {"recovering", 0.75}, {"resolved", 1},
}

type incident struct {
	roles Roles
	gens  map[string]*gen
	r     *rand.Rand
}

func (s *incident) step(f float64, tick int) (string, []Entry) {
	id := s.roles.Instance
	g := s.gens[id]
	i, lf := phaseAt(incidentPhases, f)
	var out []Entry
	switch incidentPhases[i].name {
	case "normal":
		out = normal(id, g, s.r, tick)
	case "degrading":
		out = []Entry{
			health(id, "degraded"),
			instance(id, "md/v1/traffic", trafficData(g.rps.next(s.r), drift(1, 8, lf), drift(150, 700, lf))),
			instance(id, "md/v1/usage", usageData(drift(60, 90, lf)+s.r.Float64(), 100, "CPU %")),
			banner(s.roles.Environment, "warning", "Elevated latency on "+id+"."),
		}
	case "outage":
		spike := burst{every: 3, length: 2, height: 60}
		out = []Entry{
			health(id, "down"),
			instance(id, "md/v1/traffic", trafficData(g.rps.next(s.r)*0.7, 40+spike.at(tick, s.r), 1200+s.r.Float64()*600)),
			instance(id, "md/v1/usage", usageData(math.Min(100, drift(95, 99, lf)+s.r.Float64()), 100, "CPU %")),
			banner(s.roles.Environment, "error", id+" is failing requests."),
		}
	case "recovering":
		out = []Entry{
			health(id, "recovering"),
			instance(id, "md/v1/traffic", trafficData(g.rps.next(s.r), drift(40, 1, lf), drift(900, 150, lf))),
			instance(id, "md/v1/usage", usageData(drift(97, 55, lf), 100, "CPU %")),
			banner(s.roles.Environment, "info", id+" is recovering. Monitoring."),
		}
	default:
		out = append(normal(id, g, s.r, tick), banner(s.roles.Environment, "info", "Resolved: "+id+" is healthy."))
	}
	for _, other := range s.roles.Instances {
		out = append(out, normal(other, s.gens[other], s.r, tick)...)
	}
	return incidentPhases[i].name, out
}

// ---------------------------------------------------------------- postgres-cutover

var cutoverPhases = []phase{{"replicating", 0.2}, {"catching-up", 0.55}, {"cutover", 0.7}, {"complete", 1}}

type cutover struct {
	roles Roles
	gens  map[string]*gen
	lag   walk
	r     *rand.Rand
}

func (s *cutover) step(f float64, tick int) (string, []Entry) {
	old, next, env := s.roles.OldDatabase, s.roles.NewDatabase, s.roles.Environment
	limit := s.roles.MaxLag
	og, ng := s.gens[old], s.gens[next]
	lagUsage := func(lag float64) Entry {
		return instance(next, "md/v1/usage", usageData(math.Round(lag), limit, "replication lag (s)"))
	}
	i, lf := phaseAt(cutoverPhases, f)
	var out []Entry
	switch cutoverPhases[i].name {
	case "replicating":
		out = []Entry{
			banner(env, "info", fmt.Sprintf("Replicating %s to %s.", old, next)),
			health(old, "healthy"), health(next, "replicating"),
			instance(old, "md/v1/traffic", og.traffic(s.r, tick)), instance(next, "md/v1/traffic", idleTraffic()),
			lagUsage(s.lag.next(s.r)),
		}
	case "catching-up":
		lag := math.Max(1, drift(limit*0.9, 0, lf)+(s.r.Float64()*2-1)*limit*0.02)
		out = []Entry{
			banner(env, "warning", fmt.Sprintf("Catching up: lag %.0fs.", math.Round(lag))),
			health(old, "healthy"), health(next, "catching up"),
			instance(old, "md/v1/traffic", og.traffic(s.r, tick)), instance(next, "md/v1/traffic", idleTraffic()),
			lagUsage(lag),
		}
	case "cutover":
		ot, nt := og.traffic(s.r, tick), ng.traffic(s.r, tick)
		ot["rps"] = round1(ot["rps"].(float64) * (1 - lf))
		nt["rps"] = round1(nt["rps"].(float64) * lf)
		out = []Entry{
			banner(env, "warning", "Cutover in progress."),
			health(old, "draining"), health(next, "cutover"),
			instance(old, "md/v1/traffic", ot), instance(next, "md/v1/traffic", nt),
			lagUsage(0),
		}
	default:
		out = []Entry{
			banner(env, "info", "Cutover complete."),
			health(old, "idle"), health(next, "healthy"),
			instance(old, "md/v1/traffic", idleTraffic()), instance(next, "md/v1/traffic", ng.traffic(s.r, tick)),
			lagUsage(0),
		}
	}
	for _, other := range s.roles.Instances {
		out = append(out, normal(other, s.gens[other], s.r, tick)...)
	}
	return cutoverPhases[i].name, out
}

// ---------------------------------------------------------------- rollout

// The rollout starts after the lead-in and ends before the tail, so every environment shows the advisory first
// and the fix last.
const (
	rolloutLeadIn = 0.1
	rolloutTail   = 0.1
	// patchShare is the part of an environment's turn spent patching. The rest shows it patched.
	patchShare = 0.6
)

type rollout struct {
	roles Roles
	gens  map[string]*gen
	r     *rand.Rand
}

func (s *rollout) step(f float64, tick int) (string, []Entry) {
	envs := s.roles.Environments
	turn := (1 - rolloutLeadIn - rolloutTail) / float64(len(envs))
	var out []Entry
	// The phase is the state of the first environment that is not patched.
	current := ""
	for ei, e := range envs {
		start := rolloutLeadIn + float64(ei)*turn
		progress := (f - start) / (turn * patchShare)
		n := len(e.Instances)
		switch {
		case progress < 0:
			out = append(out, banner(e.Environment, "warning", s.roles.Advisory))
			if current == "" {
				current = e.Environment + " vulnerable"
			}
		case progress < 1:
			done := int(progress * float64(n))
			out = append(out, banner(e.Environment, "warning",
				fmt.Sprintf("Patching %s: %d of %d instances patched.", e.Environment, done, n)))
			if current == "" {
				current = e.Environment + " patching"
			}
		default:
			out = append(out, banner(e.Environment, "info", "Patched: "+e.Environment+" has the fix."))
		}
		for ii, id := range e.Instances {
			g := s.gens[id]
			// Each instance takes an equal share of the environment's patching time.
			pos := progress*float64(n) - float64(ii)
			switch {
			case pos < 0:
				out = append(out, health(id, "vulnerable"), instance(id, "md/v1/traffic", g.traffic(s.r, tick)))
			case pos < 1:
				out = append(out, health(id, "patching"),
					instance(id, "md/v1/traffic", trafficData(g.rps.next(s.r)*0.8, 10+burst{every: 2, length: 1, height: 20}.at(tick, s.r), g.p95.next(s.r)*2)))
			default:
				out = append(out, health(id, "patched"), instance(id, "md/v1/traffic", g.traffic(s.r, tick)))
			}
		}
	}
	if current == "" {
		current = "patched"
	}
	return current, out
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
