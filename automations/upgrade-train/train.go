// Package upgradetrain moves each new version of a bundle through stages of instances, such as dev, then staging,
// then prod. A stage deploys or proposes the version, waits a soak time with no firing alarms, and then the next
// stage starts. A failed deployment or a firing alarm stops the train.
package upgradetrain

import (
	"context"
	"crypto/subtle"
	_ "embed"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/massdriver-cloud/massdriver-automations/pkg/automation"
	"github.com/massdriver-cloud/massdriver-automations/pkg/massdriver"
	"github.com/massdriver-cloud/massdriver-automations/pkg/slack"
)

const (
	automationName = "upgrade-train"
	pageLimit      = 100
	// MessagePrefix starts the message of every deployment that upgrade-train creates.
	MessagePrefix = "upgrade-train: "
	// maxWaitTicks is how many ticks a stage waits for a busy instance before it leaves it out.
	maxWaitTicks = 60
	tickEvery    = time.Minute
)

var (
	//go:embed queries/bundle_versions.graphql
	bundleVersionsQuery string
	//go:embed queries/stage_instances.graphql
	stageInstancesQuery string
	//go:embed queries/instance.graphql
	instanceQuery string
	//go:embed queries/create_deployment.graphql
	createDeploymentMutation string
	//go:embed queries/propose_deployment.graphql
	proposeDeploymentMutation string
)

// Definition registers the automation.
var Definition = automation.Definition{
	Name:        automationName,
	Description: "Moves each new bundle version through stages of instances, with a soak time and alarm check between stages.",
	Events:      []string{"repo.published", "deployment.completed", "deployment.failed", "deployment.aborted", "deployment.rejected"},
	New: func() (automation.Automation, error) {
		var cfg Config
		if err := automation.LoadConfig(&cfg); err != nil {
			return nil, err
		}
		api, err := massdriver.FromEnv()
		if err != nil {
			return nil, err
		}
		sl := slack.FromEnv()
		sl.WarnIgnoredChannels(slog.Default(), cfg.channels()...)
		t, err := New(api, sl, &cfg, Options{
			RunToken: os.Getenv("UPGRADE_TRAIN_TOKEN"),
			StateDir: os.Getenv("STATE_DIR"),
		})
		if err != nil {
			return nil, err
		}
		go func() {
			for range time.Tick(tickEvery) {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
				t.Tick(ctx)
				cancel()
			}
		}()
		return t, nil
	},
}

// Options are the settings that come from the environment.
type Options struct {
	RunToken string
	StateDir string
}

// Trains runs the configured trains.
type Trains struct {
	api     *massdriver.Client
	results massdriver.Results
	slack   *slack.Client
	cfg     *Config
	opts    Options
	now     func() time.Time
	run     func(func())

	// mu serializes every change to the runs, including the API calls that decide it.
	mu sync.Mutex
	st state
}

// New returns the automation with its runs loaded from opts.StateDir.
func New(api *massdriver.Client, sl *slack.Client, cfg *Config, opts Options) (*Trains, error) {
	st, err := loadState(opts.StateDir)
	if err != nil {
		return nil, fmt.Errorf("state: %w", err)
	}
	return &Trains{
		api: api, results: massdriver.NewResults(api, cfg.Results), slack: sl, cfg: cfg, opts: opts,
		now: time.Now, run: func(f func()) { go f() }, st: st,
	}, nil
}

// Handle starts a run when a covered bundle publishes a version, and records the end of each deployment a run
// created.
func (t *Trains) Handle(ctx context.Context, ev automation.Event) error {
	if ev.Type == "repo.published" {
		var repo struct {
			Name string `json:"name"`
		}
		if err := ev.DecodeEntity(&repo); err != nil {
			return err
		}
		if repo.Name == "" || len(t.cfg.covering(repo.Name)) == 0 {
			return nil
		}
		bg := context.WithoutCancel(ctx)
		t.run(func() { t.published(bg, repo.Name, ev.Time) })
		return nil
	}

	var outcome string
	switch ev.Type {
	case "deployment.completed":
	case "deployment.failed":
		outcome = "failed"
	case "deployment.aborted":
		outcome = "was aborted"
	case "deployment.rejected":
		outcome = "was rejected"
	default:
		return nil
	}
	var dep struct {
		ID      string `json:"id"`
		Message string `json:"message"`
	}
	if err := ev.DecodeEntity(&dep); err != nil {
		return err
	}
	if !strings.HasPrefix(dep.Message, MessagePrefix) {
		return nil
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	r := t.runOf(dep.ID)
	if r == nil {
		return nil
	}
	tr, ok := t.cfg.find(r.Train)
	if !ok {
		return nil
	}
	instance := r.Deployments[dep.ID]
	delete(r.Deployments, dep.ID)
	log := automation.Logger(ctx).With("train", r.Train, "bundle", r.Bundle, "bundle_version", r.Version,
		"instance", instance, "deployment_id", dep.ID)
	if outcome != "" {
		log.Warn("train deployment ended badly", "outcome", outcome)
		t.haltLocked(ctx, tr, r, fmt.Sprintf("deployment %s of %s %s", dep.ID, instance, outcome))
		return nil
	}
	log.Info("train deployment completed")
	deployedAt := ev.Time
	if deployedAt.IsZero() {
		deployedAt = t.now()
	}
	t.results.Write(ctx, "instance", instance, "release", "Release", massdriver.Release{
		Version: r.Version, DeployedAt: massdriver.Timestamp(deployedAt),
	})
	r.Done = append(r.Done, instance)
	t.stageDoneLocked(ctx, tr, r)
	t.saveLocked()
	return nil
}

func (t *Trains) runOf(deploymentID string) *run {
	for _, r := range t.st.Runs {
		if _, ok := r.Deployments[deploymentID]; ok {
			return r
		}
	}
	return nil
}

func (t *Trains) published(ctx context.Context, repo string, at time.Time) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	log := automation.Logger(ctx).With("bundle", repo)
	v, err := t.publishedVersion(ctx, repo, at)
	if err != nil {
		log.Error("version lookup failed", "err", err)
		return
	}
	for _, tr := range t.cfg.covering(repo) {
		if isDevelopment(v) && !tr.IncludeDevelopment {
			log.Debug("train skipped", "train", tr.Name, "reason", "development_version", "bundle_version", v)
			continue
		}
		t.Start(ctx, tr.Name, repo, v)
	}
}

// Start runs version v of repo through the train from its first stage. A run of the same version that is under
// way is left alone, and a halted one resumes at the stage where it stopped. A run of another version is replaced.
func (t *Trains) Start(ctx context.Context, train, repo, v string) {
	tr, ok := t.cfg.find(train)
	if !ok {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	log := automation.Logger(ctx).With("train", train, "bundle", repo, "bundle_version", v)

	old := t.st.Runs[runKey(train, repo)]
	switch {
	case old != nil && old.Version == v && old.Phase != PhaseHalted:
		log.Debug("train skipped", "reason", "already_running")
		return
	case old != nil && old.Version == v:
		log.Info("train resumed", "stage", tr.Stages[old.Stage].Name)
		old.Phase, old.Deployments, old.Waiting = PhaseDeploying, map[string]string{}, map[string]int{}
		t.startStageLocked(ctx, tr, old)
		return
	case old != nil && old.Phase != PhaseHalted:
		t.notify(ctx, tr, fmt.Sprintf("Upgrade train %s: %s@%s replaces %s@%s, which stops at stage %s.",
			train, repo, v, repo, old.Version, tr.Stages[old.Stage].Name))
	}
	r := &run{Train: train, Bundle: repo, Version: v, Phase: PhaseDeploying, StartedAt: t.now(),
		Deployments: map[string]string{}, Waiting: map[string]int{}}
	t.st.Runs[r.key()] = r
	log.Info("train started")
	t.startStageLocked(ctx, tr, r)
}

// Halt stops the runs of a train, or of one bundle on it when repo is set. It returns how many it stopped.
func (t *Trains) Halt(ctx context.Context, train, repo string) int {
	tr, ok := t.cfg.find(train)
	if !ok {
		return 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	n := 0
	for _, r := range t.sortedRuns() {
		if r.Train == train && (repo == "" || r.Bundle == repo) && r.Phase != PhaseHalted {
			t.haltLocked(ctx, tr, r, "stopped by POST /upgrade-train/halt")
			n++
		}
	}
	return n
}

type instance struct {
	ID                  string         `json:"id"`
	Status              string         `json:"status"`
	Version             string         `json:"version"`
	Params              map[string]any `json:"params"`
	ResolvedVersion     string         `json:"resolvedVersion"`
	DeployedVersion     *string        `json:"deployedVersion"`
	FiringAlarmCount    int            `json:"firingAlarmCount"`
	EffectiveAttributes map[string]any `json:"effectiveAttributes"`
	LatestDeployment    *struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	} `json:"latestDeployment"`
}

func (i instance) on(v string) bool { return i.DeployedVersion != nil && *i.DeployedVersion == v }

var inProgress = []string{"PENDING", "APPROVED", "RUNNING"}

func (i instance) busy() bool {
	return i.LatestDeployment != nil && slices.Contains(inProgress, i.LatestDeployment.Status)
}

// startStageLocked deploys the version to every instance of the run's stage that its version constraint allows.
func (t *Trains) startStageLocked(ctx context.Context, tr *Train, r *run) {
	st := tr.Stages[r.Stage]
	log := automation.Logger(ctx).With("train", r.Train, "bundle", r.Bundle, "bundle_version", r.Version, "stage", st.Name)
	r.Done = nil
	ids, err := t.stageInstances(ctx, r.Bundle, st)
	if err != nil {
		log.Error("stage instances lookup failed", "err", err)
		t.haltLocked(ctx, tr, r, fmt.Sprintf("listing the instances of stage %s failed: %v", st.Name, err))
		return
	}

	var deployed, pinned, waiting []string
	for _, id := range ids {
		outcome, err := t.upgradeLocked(ctx, tr, r, id)
		if err != nil {
			log.Error("stage deployment failed", "instance", id, "err", err)
			t.haltLocked(ctx, tr, r, fmt.Sprintf("deploying %s failed: %v", id, err))
			return
		}
		switch outcome {
		case outcomeDeployed:
			deployed = append(deployed, id)
		case outcomePinned:
			pinned = append(pinned, id)
		case outcomeWaiting:
			waiting = append(waiting, id)
		}
	}
	verb := map[string]string{ModeDeploy: "deploying", ModePropose: "proposing"}[st.Mode]
	text := fmt.Sprintf("Upgrade train %s: %s@%s stage %s (%d of %d): %s %d, %d already on it.",
		r.Train, r.Bundle, r.Version, st.Name, r.Stage+1, len(tr.Stages), verb, len(deployed), len(r.Done))
	if len(waiting) > 0 {
		text += fmt.Sprintf(" Waiting for deployments in progress on %s.", strings.Join(waiting, ", "))
	}
	if len(pinned) > 0 {
		text += fmt.Sprintf(" Left out, because their version does not resolve to %s: %s.", r.Version, strings.Join(pinned, ", "))
	}
	log.Info("stage started", "deploying", len(deployed), "already_on_version", len(r.Done), "pinned", len(pinned), "waiting", len(waiting))
	t.notify(ctx, tr, text)
	t.stageDoneLocked(ctx, tr, r)
	t.saveLocked()
}

const (
	outcomeDeployed = "deployed"
	outcomeDone     = "done"
	outcomePinned   = "pinned"
	outcomeWaiting  = "waiting"
)

// upgradeLocked brings one instance of the run's stage to the version, and records where it stands.
func (t *Trains) upgradeLocked(ctx context.Context, tr *Train, r *run, id string) (string, error) {
	st := tr.Stages[r.Stage]
	inst, err := t.instance(ctx, id)
	if err != nil {
		return "", err
	}
	switch {
	case inst.on(r.Version):
		delete(r.Waiting, id)
		r.Done = append(r.Done, id)
		return outcomeDone, nil
	case inst.ResolvedVersion != r.Version:
		delete(r.Waiting, id)
		return outcomePinned, nil
	case inst.busy():
		r.Waiting[id]++
		return outcomeWaiting, nil
	}
	delete(r.Waiting, id)

	params := "{}"
	if inst.Params != nil {
		if params, err = massdriver.JSONString(inst.Params); err != nil {
			return "", err
		}
	}
	mutation, field := createDeploymentMutation, "createDeployment"
	if st.Mode == ModePropose {
		mutation, field = proposeDeploymentMutation, "proposeDeployment"
	}
	var out map[string]massdriver.Payload[struct {
		ID string `json:"id"`
	}]
	err = t.api.Do(ctx, mutation, map[string]any{
		"org": t.api.OrganizationID,
		"id":  id,
		"input": map[string]any{
			"action":  "PROVISION",
			"params":  params,
			"message": fmt.Sprintf("%s%s %s@%s stage %s", MessagePrefix, r.Train, r.Bundle, r.Version, st.Name),
		},
	}, &out)
	if err != nil {
		return "", err
	}
	p := out[field]
	if err := p.Err(); err != nil {
		return "", fmt.Errorf("%s: %w", field, err)
	}
	r.Deployments[p.Result.ID] = id
	automation.Logger(ctx).Info("train deployment created", "train", r.Train, "instance", id, "deployment_id", p.Result.ID, "mode", st.Mode)
	t.results.Write(ctx, "deployment", p.Result.ID, "step/upgrade-train", "Upgrade train", massdriver.Step{
		Tool:       "upgrade-train",
		Status:     "succeeded",
		FinishedAt: massdriver.Timestamp(t.now()),
		Message:    fmt.Sprintf("Stage %s of train %s: %s@%s", st.Name, r.Train, r.Bundle, r.Version),
	})
	return outcomeDeployed, nil
}

// stageDoneLocked starts the soak when every deployment of the stage has completed.
func (t *Trains) stageDoneLocked(ctx context.Context, tr *Train, r *run) {
	if r.Phase != PhaseDeploying || len(r.Deployments) > 0 || len(r.Waiting) > 0 {
		return
	}
	st := tr.Stages[r.Stage]
	r.Phase, r.SoakStart = PhaseSoaking, t.now()
	automation.Logger(ctx).Info("stage soaking", "train", r.Train, "bundle", r.Bundle, "stage", st.Name, "instances", len(r.Done))
	if st.Soak.Duration > 0 {
		t.notify(ctx, tr, fmt.Sprintf("Upgrade train %s: %s@%s stage %s is on the version (%d instances). Soaking for %s.",
			r.Train, r.Bundle, r.Version, st.Name, len(r.Done), st.Soak.Duration))
	}
}

// Tick retries busy instances, checks the alarms of soaking stages, and starts the next stage after a soak.
func (t *Trains) Tick(ctx context.Context) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, r := range t.sortedRuns() {
		tr, ok := t.cfg.find(r.Train)
		if !ok || r.Stage >= len(tr.Stages) {
			delete(t.st.Runs, r.key())
			continue
		}
		switch r.Phase {
		case PhaseDeploying:
			t.retryLocked(ctx, tr, r)
		case PhaseSoaking:
			t.soakLocked(ctx, tr, r)
		}
	}
	t.saveLocked()
}

func (t *Trains) retryLocked(ctx context.Context, tr *Train, r *run) {
	ids := make([]string, 0, len(r.Waiting))
	for id := range r.Waiting {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var gaveUp []string
	for _, id := range ids {
		if r.Waiting[id] >= maxWaitTicks {
			delete(r.Waiting, id)
			gaveUp = append(gaveUp, id)
			continue
		}
		if _, err := t.upgradeLocked(ctx, tr, r, id); err != nil {
			t.haltLocked(ctx, tr, r, fmt.Sprintf("deploying %s failed: %v", id, err))
			return
		}
	}
	if len(gaveUp) > 0 {
		t.notify(ctx, tr, fmt.Sprintf("Upgrade train %s: %s@%s stage %s leaves out %s: a deployment stayed in progress for %d minutes.",
			r.Train, r.Bundle, r.Version, tr.Stages[r.Stage].Name, strings.Join(gaveUp, ", "), maxWaitTicks))
	}
	t.stageDoneLocked(ctx, tr, r)
}

func (t *Trains) soakLocked(ctx context.Context, tr *Train, r *run) {
	st := tr.Stages[r.Stage]
	for _, id := range r.Done {
		inst, err := t.instance(ctx, id)
		if err != nil {
			automation.Logger(ctx).Error("soak check failed", "train", r.Train, "instance", id, "err", err)
			return
		}
		switch {
		case inst.FiringAlarmCount > 0:
			t.haltLocked(ctx, tr, r, fmt.Sprintf("%s has %d firing alarms during the soak of stage %s",
				t.link(inst.EffectiveAttributes, id), inst.FiringAlarmCount, st.Name))
			return
		case !inst.on(r.Version):
			t.haltLocked(ctx, tr, r, fmt.Sprintf("%s is no longer on %s during the soak of stage %s",
				t.link(inst.EffectiveAttributes, id), r.Version, st.Name))
			return
		}
	}
	if t.now().Sub(r.SoakStart) < st.Soak.Duration {
		return
	}
	r.Stage++
	if r.Stage == len(tr.Stages) {
		delete(t.st.Runs, r.key())
		automation.Logger(ctx).Info("train finished", "train", r.Train, "bundle", r.Bundle, "bundle_version", r.Version)
		t.notify(ctx, tr, fmt.Sprintf("Upgrade train %s: %s@%s finished all %d stages.", r.Train, r.Bundle, r.Version, len(tr.Stages)))
		return
	}
	r.Phase, r.Deployments, r.Waiting = PhaseDeploying, map[string]string{}, map[string]int{}
	t.startStageLocked(ctx, tr, r)
}

func (t *Trains) haltLocked(ctx context.Context, tr *Train, r *run, reason string) {
	r.Phase = PhaseHalted
	stage := ""
	if r.Stage < len(tr.Stages) {
		stage = tr.Stages[r.Stage].Name
	}
	automation.Logger(ctx).Warn("train halted", "train", r.Train, "bundle", r.Bundle, "bundle_version", r.Version, "stage", stage)
	t.notify(ctx, tr, fmt.Sprintf("Upgrade train %s stopped %s@%s at stage %s: %s. Resume it with POST /upgrade-train/run.",
		r.Train, r.Bundle, r.Version, stage, reason))
	t.saveLocked()
}

func (t *Trains) sortedRuns() []*run {
	keys := make([]string, 0, len(t.st.Runs))
	for k := range t.st.Runs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]*run, len(keys))
	for i, k := range keys {
		out[i] = t.st.Runs[k]
	}
	return out
}

func (t *Trains) stageInstances(ctx context.Context, repo string, st Stage) ([]string, error) {
	keys := make([]string, 0, len(st.Match))
	for k := range st.Match {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	attrs := make([]map[string]any, 0, len(keys))
	for _, k := range keys {
		if v := st.Match[k]; len(v) == 1 {
			attrs = append(attrs, map[string]any{"key": k, "eq": v[0]})
		} else {
			attrs = append(attrs, map[string]any{"key": k, "in": []string(v)})
		}
	}
	var ids []string
	var next *string
	for {
		var out struct {
			Instances struct {
				Cursor struct {
					Next *string `json:"next"`
				} `json:"cursor"`
				Items []struct {
					ID string `json:"id"`
				} `json:"items"`
			} `json:"instances"`
		}
		err := t.api.Do(ctx, stageInstancesQuery, map[string]any{
			"org": t.api.OrganizationID, "repo": repo, "attributes": attrs,
			"cursor": map[string]any{"limit": pageLimit, "next": next},
		}, &out)
		if err != nil {
			return nil, err
		}
		for _, it := range out.Instances.Items {
			ids = append(ids, it.ID)
		}
		if next = out.Instances.Cursor.Next; next == nil {
			return ids, nil
		}
	}
}

func (t *Trains) instance(ctx context.Context, id string) (instance, error) {
	var out struct {
		Instance instance `json:"instance"`
	}
	err := t.api.Do(ctx, instanceQuery, map[string]any{"org": t.api.OrganizationID, "id": id}, &out)
	return out.Instance, err
}

// The repo.published event carries the repo, not the version. The published version is the newest one that
// existed when the event occurred.
func (t *Trains) publishedVersion(ctx context.Context, repo string, at time.Time) (string, error) {
	var out struct {
		OciRepo *struct {
			Tags struct {
				Items []struct {
					Tag       string    `json:"tag"`
					CreatedAt time.Time `json:"createdAt"`
				} `json:"items"`
			} `json:"tags"`
		} `json:"ociRepo"`
	}
	if err := t.api.Do(ctx, bundleVersionsQuery, map[string]any{"org": t.api.OrganizationID, "repo": repo}, &out); err != nil {
		return "", err
	}
	if out.OciRepo == nil {
		return "", fmt.Errorf("repo %s not found", repo)
	}
	for _, v := range out.OciRepo.Tags.Items {
		if at.IsZero() || !v.CreatedAt.After(at) {
			return v.Tag, nil
		}
	}
	return "", fmt.Errorf("repo %s has no version published before %s", repo, at.Format(time.RFC3339))
}

func isDevelopment(tag string) bool { return strings.Contains(tag, "-") }

// Routes serves POST /upgrade-train/run and POST /upgrade-train/halt when a token is set.
func (t *Trains) Routes(mux *http.ServeMux) {
	if t.opts.RunToken == "" {
		return
	}
	auth := func(w http.ResponseWriter, r *http.Request) (*Train, string, bool) {
		want := []byte("Bearer " + t.opts.RunToken)
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), want) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return nil, "", false
		}
		tr, ok := t.cfg.find(r.URL.Query().Get("train"))
		if !ok {
			http.Error(w, "unknown train", http.StatusNotFound)
			return nil, "", false
		}
		repo := r.URL.Query().Get("bundle")
		if repo != "" && !slices.Contains(tr.Bundles, repo) {
			http.Error(w, "the train does not cover that bundle", http.StatusBadRequest)
			return nil, "", false
		}
		return tr, repo, true
	}
	reply := func(w http.ResponseWriter, code int, body map[string]any) {
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(body)
	}

	mux.HandleFunc("POST /upgrade-train/run", func(w http.ResponseWriter, r *http.Request) {
		tr, repo, ok := auth(w, r)
		if !ok {
			return
		}
		if repo == "" {
			if len(tr.Bundles) > 1 {
				http.Error(w, "bundle is required for a train with more than one bundle", http.StatusBadRequest)
				return
			}
			repo = tr.Bundles[0]
		}
		v := r.URL.Query().Get("version")
		bg := context.WithoutCancel(r.Context())
		t.run(func() {
			ctx, cancel := context.WithTimeout(bg, 5*time.Minute)
			defer cancel()
			version := v
			if version == "" {
				var err error
				if version, err = t.publishedVersion(ctx, repo, time.Time{}); err != nil {
					automation.Logger(ctx).Error("version lookup failed", "bundle", repo, "err", err)
					return
				}
			}
			t.Start(ctx, tr.Name, repo, version)
		})
		reply(w, http.StatusAccepted, map[string]any{"train": tr.Name, "bundle": repo, "version": v})
	})

	mux.HandleFunc("POST /upgrade-train/halt", func(w http.ResponseWriter, r *http.Request) {
		tr, repo, ok := auth(w, r)
		if !ok {
			return
		}
		n := t.Halt(r.Context(), tr.Name, repo)
		reply(w, http.StatusOK, map[string]any{"train": tr.Name, "halted": n})
	})
}

// link is a Slack link to the instance, when its attributes place it.
func (t *Trains) link(attrs map[string]any, id string) string {
	attr := func(k string) string {
		s, _ := attrs[k].(string)
		return s
	}
	if attr("md-project") == "" || attr("md-environment") == "" || attr("md-component") == "" {
		return id
	}
	u := fmt.Sprintf("%s/orgs/%s/projects/%s/environments/%s/instances/%s",
		t.cfg.Massdriver.URL(),
		url.PathEscape(t.api.OrganizationID),
		url.PathEscape(attr("md-project")),
		url.PathEscape(attr("md-environment")),
		url.PathEscape(attr("md-component")),
	)
	return "<" + u + "|" + id + ">"
}

// notify posts to the train's channel, or logs the text without one. A failed post is logged: the train has
// already acted, and a retry must not act again.
func (t *Trains) notify(ctx context.Context, tr *Train, text string) {
	channel := tr.Slack.Or(t.cfg.Slack).Channel
	log := automation.Logger(ctx)
	if !t.slack.Sends(channel) {
		log.Info("train report logged", "train", tr.Name, "text", text)
		return
	}
	if _, err := t.slack.Post(ctx, slack.Message{Channel: channel, Text: text}); err != nil {
		log.Error("slack post failed", "train", tr.Name, "err", err)
	}
}

func (t *Trains) saveLocked() {
	if err := saveState(t.opts.StateDir, t.st); err != nil {
		slog.Error("state save failed", "err", err)
	}
}
