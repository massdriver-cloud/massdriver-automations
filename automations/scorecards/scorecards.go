// Package scorecards computes facts about instances, such as their deployments, alarms, and bundle versions, and
// scores them against scorecards from the config. It writes the facts and the scores to the stash, where widgets
// show them. Each event and each interval reads the current state from the API and computes everything again, so
// a late or repeated event changes nothing.
package scorecards

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/massdriver-cloud/massdriver-automations/pkg/automation"
	"github.com/massdriver-cloud/massdriver-automations/pkg/massdriver"
)

const automationName = "scorecards"

// workers is how many jobs run at once.
const workers = 4

// jobTimeout bounds one job.
const jobTimeout = 5 * time.Minute

// Facts are the names of the fact keys, after the prefix.
const (
	FactDelivery    = "delivery"
	FactReliability = "reliability"
	FactCurrency    = "currency"
	FactPending     = "pending"
	FactParity      = "parity"
	FactOwnership   = "ownership"
)

var facts = []string{FactDelivery, FactReliability, FactCurrency, FactPending, FactParity, FactOwnership}

// The keys with the default prefix.
const (
	KeyDelivery    = massdriver.DefaultResultPrefix + "/" + FactDelivery
	KeyReliability = massdriver.DefaultResultPrefix + "/" + FactReliability
	KeyCurrency    = massdriver.DefaultResultPrefix + "/" + FactCurrency
	KeyPending     = massdriver.DefaultResultPrefix + "/" + FactPending
	KeyParity      = massdriver.DefaultResultPrefix + "/" + FactParity
	KeyOwnership   = massdriver.DefaultResultPrefix + "/" + FactOwnership
)

// Events are the event types that start a computation.
var Events = []string{
	"deployment.completed", "deployment.failed", "deployment.aborted", "deployment.rejected",
	"alarm_state.created", "alarm.created", "alarm.updated", "alarm.deleted",
	"instance.created", "instance.updated", "instance.deleted", "instance.moved", "instance.orphaned",
	"instance.copied",
	"repo.published",
	"group_membership.added", "group_membership.removed",
	"stash_entry.set", "stash_entry.deleted",
}

// Definition registers the automation.
var Definition = automation.Definition{
	Name:        automationName,
	Description: "Scores instances against scorecards, and writes the scores and the facts they use to the stash.",
	Events:      Events,
	New: func() (automation.Automation, error) {
		var cfg Config
		if err := automation.LoadConfig(&cfg); err != nil {
			return nil, err
		}
		client, err := massdriver.FromEnv()
		if err != nil {
			return nil, err
		}
		s := New(&cfg, &GraphQLAPI{Client: client}, &GraphQLStash{API: client}, time.Now)
		ctx := context.Background()
		for i := 0; i < workers; i++ {
			go s.queue.work(ctx, s.run)
		}
		go s.Run(ctx)
		return s, nil
	},
}

// Scorecards computes facts and scores.
type Scorecards struct {
	cfg   *Config
	api   API
	out   *writer
	now   func() time.Time
	queue *queue
}

// New returns the automation. now is the clock.
func New(cfg *Config, api API, stash Stash, now func() time.Time) *Scorecards {
	return &Scorecards{cfg: cfg, api: api, out: newWriter(stash, api, now, time.Duration(cfg.Interval)), now: now, queue: newQueue()}
}

// Job keys.
const (
	jobAll          = "all"
	jobOwnership    = "ownership"
	jobInstance     = "instance/"
	jobEnvironment  = "environment/"
	jobProject      = "project/"
	jobParity       = "parity/"
	jobRepo         = "repo/"
	jobEnvInstances = "environment-instances/"
	jobPrjInstances = "project-instances/"
)

// Handle queues the work an event calls for, and returns at once.
func (s *Scorecards) Handle(ctx context.Context, ev automation.Event) error {
	log := automation.Logger(ctx)
	instance := ev.Attribute("md-instance")
	env := environmentID(ev.Attribute("md-project"), ev.Attribute("md-environment"))
	switch {
	case ev.Type == "instance.deleted":
		// The instance is gone, so only the roll-ups change.
		s.queueRollups(env, ev.Attribute("md-project"))
	case strings.HasPrefix(ev.Type, "deployment."), strings.HasPrefix(ev.Type, "alarm"),
		strings.HasPrefix(ev.Type, "instance."):
		if instance != "" {
			s.queue.add(jobInstance + instance)
		}
	case ev.Type == "repo.published":
		var repo struct {
			Name string `json:"name"`
		}
		if err := ev.DecodeEntity(&repo); err == nil && repo.Name != "" {
			s.queue.add(jobRepo + repo.Name)
		}
	case strings.HasPrefix(ev.Type, "group_membership."):
		s.queue.add(jobOwnership)
	case strings.HasPrefix(ev.Type, "stash_entry."):
		var entry struct {
			Key   string `json:"key"`
			Scope struct {
				Kind string `json:"kind"`
				ID   string `json:"id"`
			} `json:"scope"`
		}
		if err := ev.DecodeEntity(&entry); err != nil || s.cfg.ownKey(entry.Key) {
			log.Debug("event skipped", "reason", "own_key")
			return nil
		}
		// Other automations write keys often, such as traffic every minute. Only the keys a check reads matter.
		if !s.cfg.checkKeys[entry.Key] {
			log.Debug("event skipped", "reason", "key_not_checked")
			return nil
		}
		switch strings.ToLower(entry.Scope.Kind) {
		case "instance":
			s.queue.add(jobInstance + entry.Scope.ID)
		case "deployment":
			if instance != "" {
				s.queue.add(jobInstance + instance)
			}
		case "environment":
			s.queue.add(jobEnvInstances + entry.Scope.ID)
		case "project":
			s.queue.add(jobPrjInstances + entry.Scope.ID)
		}
	}
	return nil
}

// live reports whether an instance has infrastructure to score: it was deployed and not decommissioned. An empty
// status is an instance from a list that does not say, which counts.
func live(status string) bool {
	switch status {
	case "INITIALIZED", "DECOMMISSIONED":
		return false
	}
	return true
}

// environmentID is an environment's ID from its project and its identifier in the project.
func environmentID(project, env string) string {
	if project == "" || env == "" {
		return ""
	}
	return project + "-" + env
}

func (s *Scorecards) queueRollups(env, project string) {
	if env != "" {
		s.queue.add(jobEnvironment + env)
		if s.cfg.referenceOf(env) != "" {
			s.queue.add(jobParity + env)
		}
		for _, p := range s.cfg.Parity {
			if p.Reference == env {
				s.queue.add(jobParity + p.Environment)
			}
		}
	}
	if project != "" {
		s.queue.add(jobProject + project)
	}
}

// Run queues a pass over every instance now and then on each interval, until ctx is done. The pass catches what
// time changes, such as deployments that leave the window, and events that never arrived.
func (s *Scorecards) Run(ctx context.Context) {
	slog.Default().Info("scorecards started", "interval", time.Duration(s.cfg.Interval).String(),
		"scorecards", len(s.cfg.Scorecards))
	t := time.NewTicker(time.Duration(s.cfg.Interval))
	defer t.Stop()
	for {
		s.queue.add(jobAll)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Drain runs the queued work, one job at a time, until there is none. Tests call it after Handle.
func (s *Scorecards) Drain(ctx context.Context) { s.queue.drain(ctx, s.run) }

// Pass queues a pass over every instance.
func (s *Scorecards) Pass() { s.queue.add(jobAll) }

func (s *Scorecards) run(ctx context.Context, job string) {
	ctx, cancel := context.WithTimeout(ctx, jobTimeout)
	defer cancel()
	log := automation.Logger(ctx).With("job", job)
	ctx = automation.WithLogger(ctx, log)
	var err error
	switch {
	case job == jobAll:
		err = s.all(ctx)
	case job == jobOwnership:
		err = s.ownership(ctx)
	case strings.HasPrefix(job, jobInstance):
		err = s.instance(ctx, strings.TrimPrefix(job, jobInstance))
	case strings.HasPrefix(job, jobEnvironment):
		err = s.rollups(ctx, Scope{Kind: "environment", ID: strings.TrimPrefix(job, jobEnvironment)})
	case strings.HasPrefix(job, jobProject):
		err = s.rollups(ctx, Scope{Kind: "project", ID: strings.TrimPrefix(job, jobProject)})
	case strings.HasPrefix(job, jobParity):
		err = s.parity(ctx, strings.TrimPrefix(job, jobParity))
	case strings.HasPrefix(job, jobRepo):
		err = s.queueInstances(ctx, InstanceFilter{Repo: strings.TrimPrefix(job, jobRepo)})
	case strings.HasPrefix(job, jobEnvInstances):
		err = s.queueInstances(ctx, InstanceFilter{Environment: strings.TrimPrefix(job, jobEnvInstances)})
	case strings.HasPrefix(job, jobPrjInstances):
		err = s.queueInstances(ctx, InstanceFilter{Project: strings.TrimPrefix(job, jobPrjInstances)})
	}
	if err != nil {
		log.Error("job failed", "err", err)
	}
}

// all queues every instance that a scorecard matches, ownership, and parity.
func (s *Scorecards) all(ctx context.Context) error {
	s.queue.add(jobOwnership)
	for _, p := range s.cfg.Parity {
		s.queue.add(jobParity + p.Environment)
	}
	return s.queueInstances(ctx, InstanceFilter{})
}

func (s *Scorecards) queueInstances(ctx context.Context, f InstanceFilter) error {
	refs, err := s.api.Instances(ctx, f)
	if err != nil {
		return err
	}
	// An instance that is not deployed is queued too, so that what it had when it was is deleted.
	for _, r := range refs {
		if len(s.cfg.matching(r.EffectiveAttributes)) > 0 {
			s.queue.add(jobInstance + r.ID)
		}
	}
	return nil
}

// instance computes an instance's facts and scores, and queues the roll-ups of its environment and project.
func (s *Scorecards) instance(ctx context.Context, id string) error {
	log := automation.Logger(ctx)
	inst, err := s.api.Instance(ctx, id)
	if errors.Is(err, ErrNotFound) {
		log.Debug("instance skipped", "reason", "not_found")
		return nil
	}
	if err != nil {
		return err
	}
	scope := Scope{Kind: "instance", ID: inst.ID}
	cards := s.cfg.matching(inst.EffectiveAttributes)
	if !live(inst.Status) {
		// An instance that was decommissioned, or reset, keeps nothing that describes infrastructure it no longer
		// has.
		s.clear(ctx, scope, s.instanceKeys(nil))
		s.queueRollups(inst.Environment.ID, inst.Environment.Project.ID)
		log.Debug("instance skipped", "reason", "not_deployed")
		return nil
	}
	// A scorecard that no longer matches the instance's attributes leaves no score behind.
	s.clear(ctx, scope, s.unmatchedKeys(cards))
	if len(cards) == 0 {
		log.Debug("instance skipped", "reason", "no_scorecard")
		return nil
	}
	now := s.now().UTC().Truncate(time.Second)

	facts := s.facts(ctx, now, inst)
	keys := sortedKeys(facts)
	for _, key := range keys {
		s.write(ctx, scope, key, facts[key])
	}

	in, err := s.inputs(ctx, now, inst, cards, facts)
	if err != nil {
		return err
	}
	for _, sc := range cards {
		s.write(ctx, scope, s.cfg.scorecardKey(sc.Name), score(ctx, sc, in))
	}
	s.queueRollups(inst.Environment.ID, inst.Environment.Project.ID)
	return nil
}

// facts computes each fact of an instance. A fact whose reads fail is left out, and the others are still written.
func (s *Scorecards) facts(ctx context.Context, now time.Time, inst *Instance) map[string]any {
	log := automation.Logger(ctx)
	out := map[string]any{}
	failed := func(fact string, err error) { log.Warn("fact skipped", "fact", fact, "err", err) }

	window := time.Duration(s.cfg.Delivery.Window)
	deps, err := s.api.Deployments(ctx, inst.ID, now.Add(-window))
	if err == nil {
		var created []AuditEvent
		created, err = s.api.AuditEvents(ctx, inst.ID, []string{"deployment.created", "deployment.proposed"},
			"SERVICE_ACCOUNT", now.Add(-window))
		automated := map[string]bool{}
		for _, e := range created {
			if id := entityString(e.Data, "id"); id != "" {
				automated[id] = true
			}
		}
		if err == nil {
			out[s.cfg.key(FactDelivery)] = delivery(window, deps, automated, inst.LatestProvision)
		}
	}
	if err != nil {
		failed("delivery", err)
	}

	window = time.Duration(s.cfg.Reliability.Window)
	alarms, err := s.api.Alarms(ctx, inst.ID)
	if err == nil {
		var changes []AuditEvent
		changes, err = s.api.AuditEvents(ctx, inst.ID, []string{"alarm_state.created"}, "", now.Add(-window))
		if err == nil {
			out[s.cfg.key(FactReliability)] = reliability(now, window, alarms, changes)
		}
	}
	if err != nil {
		failed("reliability", err)
	}

	if inst.Bundle != nil && inst.DeployedVersion != "" {
		tags, err := s.api.Tags(ctx, inst.Bundle.Name, "STABLE")
		if err == nil && strings.Contains(inst.DeployedVersion, "-") {
			var dev []Tag
			dev, err = s.api.Tags(ctx, inst.Bundle.Name, "DEV")
			tags = append(tags, dev...)
		}
		if err != nil {
			failed("currency", err)
		} else if c, ok := currency(now, inst.DeployedVersion, tags); ok {
			out[s.cfg.key(FactCurrency)] = c
		}
	}

	out[s.cfg.key(FactPending)] = pending(inst)
	return out
}

// inputs reads the stash keys that the scorecards check, on the instance, its latest deployment, its environment,
// and its project. The facts just computed stand in for what the stash has, which can be older.
func (s *Scorecards) inputs(ctx context.Context, now time.Time, inst *Instance, cards []*ScorecardConfig, facts map[string]any) (inputs, error) {
	in := inputs{now: now, stash: map[string]StashValue{}}
	owner := map[string]string{"instance": inst.ID, "environment": inst.Environment.ID,
		"project": inst.Environment.Project.ID}
	if inst.LatestProvision != nil {
		owner["deployment"] = inst.LatestProvision.ID
	}
	keySet := map[string]bool{}
	scopeSet := map[string]bool{}
	needInstance := false
	for _, sc := range cards {
		for _, ch := range sc.Checks {
			if ch.Stash == nil {
				needInstance = true
				continue
			}
			if owner[ch.Stash.Scope] != "" {
				keySet[ch.Stash.Key] = true
				scopeSet[ch.Stash.Scope] = true
			}
		}
	}
	if len(keySet) > 0 {
		var scopes []Scope
		for _, kind := range sortedKeys(scopeSet) {
			scopes = append(scopes, Scope{Kind: kind, ID: owner[kind]})
		}
		entries, err := s.api.StashEntries(ctx, scopes, sortedKeys(keySet))
		if err != nil {
			return in, err
		}
		for _, e := range entries {
			kind := strings.ToLower(e.Scope.Kind)
			if owner[kind] == e.Scope.ID {
				in.stash[stashID(kind, e.Key)] = e
			}
		}
	}
	for key, v := range facts {
		b, err := json.Marshal(v)
		if err != nil {
			continue
		}
		value := string(b)
		e := StashValue{Key: key, Value: &value, Format: "JSON", UpdatedAt: now}
		in.stash[stashID("instance", key)] = e
	}
	if needInstance {
		in.instance = instanceDocument(inst)
	}
	return in, nil
}

// instanceDocument is the input of a `query: instance` check.
func instanceDocument(inst *Instance) any {
	b, err := json.Marshal(map[string]any{
		"id":                  inst.ID,
		"status":              inst.Status,
		"deployedVersion":     inst.DeployedVersion,
		"resolvedVersion":     inst.ResolvedVersion,
		"firingAlarmCount":    inst.FiringAlarmCount,
		"effectiveAttributes": inst.EffectiveAttributes,
		"params":              inst.Params,
		"environment":         inst.Environment.ID,
		"project":             inst.Environment.Project.ID,
	})
	if err != nil {
		return nil
	}
	var doc any
	_ = json.Unmarshal(b, &doc)
	return doc
}

// rollups writes each scorecard's roll-up on an environment or a project, from the scorecards of its instances.
func (s *Scorecards) rollups(ctx context.Context, scope Scope) error {
	f := InstanceFilter{Environment: scope.ID}
	if scope.Kind == "project" {
		f = InstanceFilter{Project: scope.ID}
	}
	refs, err := s.api.Instances(ctx, f)
	if err != nil {
		return err
	}
	now := s.now().UTC().Truncate(time.Second)
	for i := range s.cfg.Scorecards {
		sc := &s.cfg.Scorecards[i]
		var scopes []Scope
		for _, r := range refs {
			if live(r.Status) && sc.Match.Matches(r.EffectiveAttributes) {
				scopes = append(scopes, Scope{Kind: "instance", ID: r.ID})
			}
		}
		if len(scopes) == 0 {
			s.clear(ctx, scope, []string{s.cfg.scorecardKey(sc.Name)})
			continue
		}
		entries, err := s.api.StashEntries(ctx, scopes, []string{s.cfg.scorecardKey(sc.Name)})
		if err != nil {
			return err
		}
		var scores []InstanceScore
		for _, e := range entries {
			var v InstanceScore
			if e.Value != nil && json.Unmarshal([]byte(*e.Value), &v) == nil && v.Level != "" {
				scores = append(scores, v)
			}
		}
		if r, ok := rollup(now, sc, scores); ok {
			s.write(ctx, scope, s.cfg.scorecardKey(sc.Name), r)
		} else {
			s.clear(ctx, scope, []string{s.cfg.scorecardKey(sc.Name)})
		}
	}
	return nil
}

// parity compares an environment with its reference.
func (s *Scorecards) parity(ctx context.Context, env string) error {
	reference := s.cfg.referenceOf(env)
	if reference == "" {
		return nil
	}
	comparisons, err := s.api.CompareEnvironments(ctx, env, reference)
	if err != nil {
		return err
	}
	s.write(ctx, Scope{Kind: "environment", ID: env}, s.cfg.key(FactParity), parity(s.now().UTC().Truncate(time.Second), reference, comparisons))
	return nil
}

// ownership writes the owners of each environment and project that has a scored instance. A group owns an
// environment when its policies allow the environment action there, and a project when they allow the project
// action. Organization admin groups can do everything, so they do not count as owners.
func (s *Scorecards) ownership(ctx context.Context) error {
	refs, err := s.api.Instances(ctx, InstanceFilter{})
	if err != nil {
		return err
	}
	envsByProject := map[string]map[string]bool{}
	for _, r := range refs {
		if !live(r.Status) || len(s.cfg.matching(r.EffectiveAttributes)) == 0 {
			continue
		}
		p := r.Environment.Project.ID
		if envsByProject[p] == nil {
			envsByProject[p] = map[string]bool{}
		}
		envsByProject[p][r.Environment.ID] = true
	}
	if len(envsByProject) == 0 {
		return nil
	}
	groups, err := s.api.Groups(ctx)
	if err != nil {
		return err
	}
	var custom []Group
	for _, g := range groups {
		if g.Role != "ORGANIZATION_ADMIN" {
			custom = append(custom, g)
		}
	}
	members := map[string][]string{}
	membersOf := func(g Group) ([]string, error) {
		if m, ok := members[g.ID]; ok {
			return m, nil
		}
		m, err := s.api.GroupMembers(ctx, g.ID)
		members[g.ID] = m
		return m, err
	}
	owners := func(gs []Group) (Ownership, error) {
		o := Ownership{Tool: tool, Groups: []GroupOwner{}}
		seen := map[string]bool{}
		for _, g := range gs {
			if len(o.Groups) < maxItems {
				o.Groups = append(o.Groups, GroupOwner{ID: g.ID, Name: clip(g.Name, 255)})
			}
			m, err := membersOf(g)
			if err != nil {
				return o, err
			}
			for _, id := range m {
				seen[id] = true
			}
		}
		o.Members = len(seen)
		return o, nil
	}

	for _, project := range sortedKeys(envsByProject) {
		projectOwners := map[string]Group{}
		for _, env := range sortedKeys(envsByProject[project]) {
			var envOwners []Group
			for _, g := range custom {
				actions, err := s.api.EnvironmentActions(ctx, env, g.ID)
				if err != nil {
					return err
				}
				if contains(actions, s.cfg.Ownership.EnvironmentAction) {
					envOwners = append(envOwners, g)
				}
				if contains(actions, s.cfg.Ownership.ProjectAction) {
					projectOwners[g.ID] = g
				}
			}
			o, err := owners(envOwners)
			if err != nil {
				return err
			}
			s.write(ctx, Scope{Kind: "environment", ID: env}, s.cfg.key(FactOwnership), o)
		}
		var pg []Group
		for _, id := range sortedKeys(projectOwners) {
			pg = append(pg, projectOwners[id])
		}
		o, err := owners(pg)
		if err != nil {
			return err
		}
		s.write(ctx, Scope{Kind: "project", ID: project}, s.cfg.key(FactOwnership), o)
	}
	return nil
}

// instanceKeys are the keys that scorecards writes on an instance: its facts, and the scorecards in cards, or
// every scorecard when cards is nil.
func (s *Scorecards) instanceKeys(cards []*ScorecardConfig) []string {
	keys := []string{s.cfg.key(FactDelivery), s.cfg.key(FactReliability), s.cfg.key(FactCurrency),
		s.cfg.key(FactPending)}
	if cards == nil {
		for i := range s.cfg.Scorecards {
			cards = append(cards, &s.cfg.Scorecards[i])
		}
	}
	for _, sc := range cards {
		keys = append(keys, s.cfg.scorecardKey(sc.Name))
	}
	return keys
}

// unmatchedKeys are the keys of the scorecards that are not in cards.
func (s *Scorecards) unmatchedKeys(cards []*ScorecardConfig) []string {
	var keys []string
	for i := range s.cfg.Scorecards {
		if !slices.Contains(cards, &s.cfg.Scorecards[i]) {
			keys = append(keys, s.cfg.scorecardKey(s.cfg.Scorecards[i].Name))
		}
	}
	return keys
}

// clear deletes entries that scorecards wrote and that no longer apply. A failure is logged; the next event or
// interval tries again.
func (s *Scorecards) clear(ctx context.Context, scope Scope, keys []string) {
	if len(keys) == 0 {
		return
	}
	log := automation.Logger(ctx).With("scope", scope.Kind, "scope_id", scope.ID)
	n, err := s.out.clear(ctx, scope, keys)
	if err != nil {
		log.Error("stash entry delete failed", "err", err)
	}
	if n > 0 {
		log.Info("stash entries deleted", "entries", n)
	}
}

// write sets an entry when it changed. A failed write is logged; the next event or interval writes it again.
func (s *Scorecards) write(ctx context.Context, scope Scope, key string, v any) {
	log := automation.Logger(ctx).With("scope", scope.Kind, "scope_id", scope.ID, "key", key)
	wrote, err := s.out.write(ctx, scope, key, strings.TrimPrefix(key, s.cfg.key("")), v)
	switch {
	case err != nil:
		log.Error("stash entry write failed", "err", err)
	case wrote:
		log.Info("stash entry set")
	default:
		log.Debug("stash entry unchanged")
	}
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
