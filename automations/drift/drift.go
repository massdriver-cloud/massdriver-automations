// Package drift plans instances on a schedule, reads the plan logs for drift, alerts Slack, and can re-apply the
// deployed or saved configuration to remove the drift.
package drift

import (
	"bytes"
	"context"
	"crypto/subtle"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
	// Check timezones must resolve in a distroless image, which has no zoneinfo files.
	_ "time/tzdata"

	"github.com/robfig/cron/v3"

	"github.com/massdriver-cloud/massdriver-automations/pkg/automation"
	"github.com/massdriver-cloud/massdriver-automations/pkg/massdriver"
)

const (
	automationName = "drift"
	pageLimit      = 100
	// A plan with no terminal event after this long is given up on, so it stops holding a max_concurrent slot.
	planTimeout = 6 * time.Hour
)

var (
	//go:embed queries/matching_instances.graphql
	matchingInstancesQuery string
	//go:embed queries/instance.graphql
	instanceQuery string
	//go:embed queries/plan_logs.graphql
	planLogsQuery string
	//go:embed queries/plan_deployment.graphql
	planDeploymentMutation string
	//go:embed queries/rollback_deployment.graphql
	rollbackDeploymentMutation string
	//go:embed queries/approve_deployment.graphql
	approveDeploymentMutation string
	//go:embed queries/create_deployment.graphql
	createDeploymentMutation string
	//go:embed queries/propose_deployment.graphql
	proposeDeploymentMutation string
)

// Definition registers the automation.
var Definition = automation.Definition{
	Name:        automationName,
	Description: "Plans instances on a schedule, alerts Slack about drift, and can re-apply the deployed configuration.",
	Events:      []string{"deployment.completed", "deployment.failed", "deployment.aborted"},
	New: func() (automation.Automation, error) {
		api, err := massdriver.FromEnv()
		if err != nil {
			return nil, err
		}
		cfg, err := LoadConfig(automation.Env("DRIFT_CONFIG", "./drift.yaml"))
		if err != nil {
			return nil, err
		}
		d, err := New(api, cfg, Options{
			SlackWebhookURL: os.Getenv("SLACK_WEBHOOK_URL"),
			RunToken:        os.Getenv("DRIFT_RUN_TOKEN"),
			AppURL:          automation.Env("MASSDRIVER_APP_URL", "https://app.massdriver.cloud"),
			StateDir:        os.Getenv("STATE_DIR"),
		})
		if err != nil {
			return nil, err
		}
		c := cron.New()
		if err := d.Schedule(c); err != nil {
			return nil, err
		}
		c.Start()
		go d.Resume()
		go func() {
			for range time.Tick(5 * time.Minute) {
				d.Prune()
			}
		}()
		return d, nil
	},
}

// Options are the settings that come from the environment.
type Options struct {
	SlackWebhookURL string
	RunToken        string
	AppURL          string
	StateDir        string
}

// Drift runs the configured checks and tracks their plans.
type Drift struct {
	api  *massdriver.Client
	cfg  *Config
	opts Options
	now  func() time.Time
	run  func(func())

	// handleMu serializes plan results, so two deliveries of one event cannot both resolve.
	handleMu sync.Mutex
	// fillMu serializes plan starts, so max_concurrent holds.
	fillMu sync.Mutex
	mu     sync.Mutex
	st     state
}

// New returns the automation with its pending plans loaded from opts.StateDir.
func New(api *massdriver.Client, cfg *Config, opts Options) (*Drift, error) {
	st, err := loadState(opts.StateDir)
	if err != nil {
		return nil, fmt.Errorf("state: %w", err)
	}
	opts.AppURL = strings.TrimRight(opts.AppURL, "/")
	return &Drift{api: api, cfg: cfg, opts: opts, now: time.Now, run: func(f func()) { go f() }, st: st}, nil
}

// Schedule adds every check that has a schedule to c.
func (d *Drift) Schedule(c *cron.Cron) error {
	for _, ch := range d.cfg.Checks {
		if ch.Schedule == "" {
			continue
		}
		name := ch.Name
		if _, err := c.AddFunc(ch.spec(), func() { d.runLogged(context.Background(), name) }); err != nil {
			return fmt.Errorf("check %s: %w", name, err)
		}
	}
	return nil
}

// Resume starts the queued plans that a restart interrupted.
func (d *Drift) Resume() {
	for i := range d.cfg.Checks {
		d.fill(context.Background(), &d.cfg.Checks[i])
	}
}

// Routes serves POST /drift/run?check=<name> when a run token is set.
func (d *Drift) Routes(mux *http.ServeMux) {
	if d.opts.RunToken == "" {
		return
	}
	mux.HandleFunc("POST /drift/run", func(w http.ResponseWriter, r *http.Request) {
		want := []byte("Bearer " + d.opts.RunToken)
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), want) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		name := r.URL.Query().Get("check")
		if _, ok := d.cfg.find(name); !ok {
			http.Error(w, "unknown check", http.StatusNotFound)
			return
		}
		bg := context.WithoutCancel(r.Context())
		d.run(func() { d.runLogged(bg, name) })
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]string{"check": name})
	})
}

func (d *Drift) runLogged(ctx context.Context, name string) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	if err := d.RunCheck(ctx, name); err != nil {
		automation.Logger(ctx).Error("check run failed", "check", name, "err", err)
	}
}

// RunCheck queues every matching provisioned instance of the check and starts plans up to max_concurrent.
func (d *Drift) RunCheck(ctx context.Context, name string) error {
	ch, ok := d.cfg.find(name)
	if !ok {
		return fmt.Errorf("unknown check %q", name)
	}
	ids, err := d.matching(ctx, ch)
	if err != nil {
		return err
	}

	d.mu.Lock()
	if d.st.Runs[name] == nil {
		d.st.Runs[name] = &run{StartedAt: d.now()}
	}
	busy := map[string]bool{}
	for _, id := range d.st.Queues[name] {
		busy[id] = true
	}
	for _, p := range d.st.Plans {
		busy[p.Instance] = true
	}
	for _, id := range ids {
		if !busy[id] {
			d.st.Queues[name] = append(d.st.Queues[name], id)
		}
	}
	d.saveLocked()
	d.mu.Unlock()

	automation.Logger(ctx).Info("check started", "check", name, "matched", len(ids))
	d.fill(ctx, ch)
	return nil
}

func (d *Drift) matching(ctx context.Context, ch *Check) ([]string, error) {
	keys := make([]string, 0, len(ch.Match))
	for k := range ch.Match {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	attrs := make([]map[string]any, 0, len(keys))
	for _, k := range keys {
		if v := ch.Match[k]; len(v) == 1 {
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
		err := d.api.Do(ctx, matchingInstancesQuery, map[string]any{
			"org":        d.api.OrganizationID,
			"attributes": attrs,
			"cursor":     map[string]any{"limit": pageLimit, "next": next},
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

func (d *Drift) inflightLocked(check string) int {
	n := 0
	for _, p := range d.st.Plans {
		if p.Check == check {
			n++
		}
	}
	return n
}

func (d *Drift) runLocked(check string) *run {
	r := d.st.Runs[check]
	if r == nil {
		r = &run{StartedAt: d.now()}
		d.st.Runs[check] = r
	}
	return r
}

func (d *Drift) fill(ctx context.Context, ch *Check) {
	d.fillMu.Lock()
	defer d.fillMu.Unlock()
	log := automation.Logger(ctx).With("check", ch.Name)

	for {
		d.mu.Lock()
		queue := d.st.Queues[ch.Name]
		inflight := d.inflightLocked(ch.Name)
		if len(queue) == 0 || inflight >= ch.MaxConcurrent {
			finished := len(queue) == 0 && inflight == 0
			d.mu.Unlock()
			if finished {
				d.finishRun(ctx, ch)
			}
			return
		}
		id := queue[0]
		d.st.Queues[ch.Name] = queue[1:]
		d.saveLocked()
		d.mu.Unlock()

		p, planID, reason, err := d.startPlan(ctx, ch, id)

		d.mu.Lock()
		r := d.runLocked(ch.Name)
		switch {
		case err != nil:
			r.Failed++
			log.Error("plan start failed", "instance", id, "err", err)
		case p == nil:
			r.Skipped++
			log.Debug("plan skipped", "instance", id, "reason", reason)
		default:
			r.Planned++
			d.st.Plans[planID] = p
			log.Info("plan started", "instance", id, "deployment_id", planID, "source_deployment_id", p.Source)
		}
		d.saveLocked()
		d.mu.Unlock()
	}
}

type instance struct {
	ID                  string          `json:"id"`
	Status              string          `json:"status"`
	Params              json.RawMessage `json:"params"`
	ResolvedVersion     string          `json:"resolvedVersion"`
	ChangedDependencies []string        `json:"changedDependencies"`
	EffectiveAttributes map[string]any  `json:"effectiveAttributes"`
	StatePaths          []struct {
		StepName string `json:"stepName"`
	} `json:"statePaths"`
	LatestDeployment *struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	} `json:"latestDeployment"`
	LatestProvision *struct {
		ID      string `json:"id"`
		Version string `json:"version"`
	} `json:"latestProvision"`
}

func (d *Drift) instance(ctx context.Context, id string) (instance, error) {
	var out struct {
		Instance instance `json:"instance"`
	}
	err := d.api.Do(ctx, instanceQuery, map[string]any{"org": d.api.OrganizationID, "id": id}, &out)
	return out.Instance, err
}

var inProgress = []string{"PENDING", "APPROVED", "RUNNING"}

// startPlan plans the instance's latest completed PROVISION. It returns a nil plan and a reason when the instance
// cannot be checked now.
func (d *Drift) startPlan(ctx context.Context, ch *Check, id string) (*plan, string, string, error) {
	inst, err := d.instance(ctx, id)
	if err != nil {
		return nil, "", "", err
	}
	switch {
	case inst.Status != "PROVISIONED":
		return nil, "", "status is " + inst.Status, nil
	case inst.LatestDeployment != nil && slices.Contains(inProgress, inst.LatestDeployment.Status):
		return nil, "", "a deployment is in progress", nil
	case inst.LatestProvision == nil:
		return nil, "", "no completed PROVISION deployment", nil
	}

	// planDeployment plans the source's params with the instance's current bundle version and dependencies.
	// Any difference there from the source shows up in the plan without being drift.
	var undeployed []string
	if inst.ResolvedVersion != inst.LatestProvision.Version {
		undeployed = append(undeployed, fmt.Sprintf("bundle version %s to %s", inst.LatestProvision.Version, inst.ResolvedVersion))
	}
	if len(inst.ChangedDependencies) > 0 {
		undeployed = append(undeployed, "dependencies "+strings.Join(inst.ChangedDependencies, ", "))
	}

	planID, err := d.mutate(ctx, planDeploymentMutation, "planDeployment", map[string]any{
		"org": d.api.OrganizationID, "id": inst.LatestProvision.ID,
	})
	if err != nil {
		return nil, "", "", err
	}

	attr := func(k string) string {
		if v, ok := inst.EffectiveAttributes[k].(string); ok {
			return v
		}
		return ""
	}
	steps := make([]string, len(inst.StatePaths))
	for i, s := range inst.StatePaths {
		steps[i] = s.StepName
	}
	return &plan{
		Check:       ch.Name,
		Instance:    inst.ID,
		Source:      inst.LatestProvision.ID,
		Project:     attr("md-project"),
		Environment: attr("md-environment"),
		Component:   attr("md-component"),
		Steps:       steps,
		Undeployed:  undeployed,
		StartedAt:   d.now(),
	}, planID, "", nil
}

func (d *Drift) mutate(ctx context.Context, mutation, field string, vars map[string]any) (string, error) {
	var out map[string]massdriver.Payload[struct {
		ID string `json:"id"`
	}]
	if err := d.api.Do(ctx, mutation, vars, &out); err != nil {
		return "", err
	}
	p := out[field]
	if err := p.Err(); err != nil {
		return "", err
	}
	return p.Result.ID, nil
}

// Handle reads the result of a tracked plan when it completes, fails, or is aborted.
func (d *Drift) Handle(ctx context.Context, ev automation.Event) error {
	if ev.Metadata("deployment_action") != "PLAN" {
		return nil
	}
	if ev.Type != "deployment.completed" && ev.Type != "deployment.failed" && ev.Type != "deployment.aborted" {
		return nil
	}
	var dep struct {
		ID string `json:"id"`
	}
	if err := ev.DecodeEntity(&dep); err != nil {
		return err
	}

	d.handleMu.Lock()
	defer d.handleMu.Unlock()

	d.mu.Lock()
	p := d.st.Plans[dep.ID]
	d.mu.Unlock()
	if p == nil {
		return nil
	}
	ch, ok := d.cfg.find(p.Check)
	if !ok {
		d.finishPlan(p, dep.ID, "")
		return nil
	}
	log := automation.Logger(ctx).With("check", ch.Name, "instance", p.Instance, "deployment_id", dep.ID)

	var outcome, text string
	if ev.Type == "deployment.completed" {
		logs, err := d.planLogs(ctx, dep.ID)
		if err != nil {
			return err
		}
		res := Analyze(logs, p.Steps, ch)
		outcome = res.Status
		switch res.Status {
		case Drifted:
			log.Info("plan drifted", "steps", len(res.Steps), "resources", len(res.Resources))
		case Clean:
			log.Debug("plan clean")
		default:
			log.Warn("plan result unknown", "steps", len(res.Steps), "expected_steps", res.Expected)
		}
		if res.Status == Drifted && ch.Mode != ModeDetect && len(p.Undeployed) == 0 && p.Outcome == "" {
			id, note := d.resolve(ctx, ch, p, dep.ID)
			if id != "" {
				log.Info("resolution deployment created", "resolution_deployment_id", id, "mode", ch.Mode)
			} else {
				log.Error("drift resolution failed", "detail", note)
			}
			d.mu.Lock()
			p.Resolution, p.Outcome = id, note
			d.saveLocked()
			d.mu.Unlock()
		}
		text = d.resultMessage(ch, p, dep.ID, res)
	} else {
		outcome = "failed"
		log.Warn("plan failed", "status", strings.TrimPrefix(ev.Type, "deployment."))
		text = fmt.Sprintf("Drift check failed for %s (check %s): the plan was %s. Nothing was resolved.\nPlan: %s",
			p.Instance, ch.Name, strings.TrimPrefix(ev.Type, "deployment."), d.planLink(p, dep.ID))
	}

	if text != "" {
		if err := d.notify(ctx, ch, text); err != nil {
			return fmt.Errorf("slack: %w", err)
		}
	}
	d.finishPlan(p, dep.ID, outcome)
	bg := context.WithoutCancel(ctx)
	d.run(func() { d.fill(bg, ch) })
	return nil
}

func (d *Drift) finishPlan(p *plan, planID, outcome string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.st.Plans, planID)
	if r := d.st.Runs[p.Check]; r != nil {
		switch outcome {
		case Clean:
			r.Clean++
		case Drifted:
			r.Drifted++
		case Unknown:
			r.Unknown++
		case "failed":
			r.Failed++
		}
	}
	d.saveLocked()
}

func (d *Drift) finishRun(ctx context.Context, ch *Check) {
	d.mu.Lock()
	r := d.st.Runs[ch.Name]
	delete(d.st.Runs, ch.Name)
	d.saveLocked()
	d.mu.Unlock()
	if r == nil || !ch.Summary {
		return
	}
	text := fmt.Sprintf("Drift check %s finished: %d planned, %d drifted, %d clean, %d unknown, %d failed, %d skipped.",
		ch.Name, r.Planned, r.Drifted, r.Clean, r.Unknown, r.Failed, r.Skipped)
	if err := d.notify(ctx, ch, text); err != nil {
		automation.Logger(ctx).Error("slack post failed", "check", ch.Name, "err", err)
	}
}

// Prune gives up on plans that never reported a result.
func (d *Drift) Prune() {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	d.handleMu.Lock()
	d.mu.Lock()
	var stale []string
	for id, p := range d.st.Plans {
		if d.now().Sub(p.StartedAt) > planTimeout {
			stale = append(stale, id)
		}
	}
	d.mu.Unlock()

	checks := map[string]*Check{}
	for _, id := range stale {
		d.mu.Lock()
		p := d.st.Plans[id]
		d.mu.Unlock()
		if ch, ok := d.cfg.find(p.Check); ok {
			checks[ch.Name] = ch
			text := fmt.Sprintf("Drift check for %s (check %s) has no result: the plan did not finish within %s.\nPlan: %s",
				p.Instance, ch.Name, planTimeout, d.planLink(p, id))
			if err := d.notify(ctx, ch, text); err != nil {
				slog.Error("slack post failed", "check", ch.Name, "err", err)
			}
			slog.Warn("plan timed out", "check", ch.Name, "instance", p.Instance, "deployment_id", id)
		}
		d.finishPlan(p, id, "failed")
	}
	d.handleMu.Unlock()
	for _, ch := range checks {
		d.fill(ctx, ch)
	}
}

func (d *Drift) planLogs(ctx context.Context, id string) (string, error) {
	var out struct {
		Deployment struct {
			Logs []struct {
				Message string `json:"message"`
			} `json:"logs"`
		} `json:"deployment"`
	}
	if err := d.api.Do(ctx, planLogsQuery, map[string]any{"org": d.api.OrganizationID, "id": id}, &out); err != nil {
		return "", err
	}
	var b strings.Builder
	for _, l := range out.Deployment.Logs {
		b.WriteString(l.Message)
		b.WriteString("\n")
	}
	return b.String(), nil
}

// resolve removes the drift as the check's mode says. It returns the deployment it created, if any, and a sentence
// for the alert.
func (d *Drift) resolve(ctx context.Context, ch *Check, p *plan, planID string) (string, string) {
	if ch.ResolveWith == ResolveDeployed {
		id, err := d.mutate(ctx, rollbackDeploymentMutation, "rollbackDeployment", map[string]any{
			"org": d.api.OrganizationID, "id": p.Source,
		})
		if err != nil {
			return "", fmt.Sprintf("Not resolved: rollbackDeployment failed: %v", err)
		}
		if ch.Mode == ModePropose {
			return id, fmt.Sprintf("Proposed deployment %s re-applies deployment %s. It waits for approval.", id, p.Source)
		}
		if _, err := d.mutate(ctx, approveDeploymentMutation, "approveDeployment", map[string]any{
			"org": d.api.OrganizationID, "id": id,
		}); err != nil {
			return id, fmt.Sprintf("Proposed deployment %s re-applies deployment %s, but the approval failed: %v. Approve it in Massdriver.", id, p.Source, err)
		}
		return id, fmt.Sprintf("Resolving: deployment %s re-applies deployment %s.", id, p.Source)
	}

	inst, err := d.instance(ctx, p.Instance)
	if err != nil {
		return "", fmt.Sprintf("Not resolved: reading the saved configuration failed: %v", err)
	}
	params := string(inst.Params)
	if params == "" || params == "null" {
		params = "{}"
	}
	input := map[string]any{
		"action":  "PROVISION",
		"params":  params,
		"message": fmt.Sprintf("drift: %s resolving drift found by plan %s", ch.Name, planID),
	}
	vars := map[string]any{"org": d.api.OrganizationID, "id": p.Instance, "input": input}
	if ch.Mode == ModePropose {
		id, err := d.mutate(ctx, proposeDeploymentMutation, "proposeDeployment", vars)
		if err != nil {
			return "", fmt.Sprintf("Not resolved: proposeDeployment failed: %v", err)
		}
		return id, fmt.Sprintf("Proposed deployment %s applies the saved configuration. It waits for approval.", id)
	}
	id, err := d.mutate(ctx, createDeploymentMutation, "createDeployment", vars)
	if err != nil {
		return "", fmt.Sprintf("Not resolved: createDeployment failed: %v", err)
	}
	return id, fmt.Sprintf("Resolving: deployment %s applies the saved configuration.", id)
}

func (d *Drift) resultMessage(ch *Check, p *plan, planID string, res Result) string {
	var b strings.Builder
	switch res.Status {
	case Clean:
		if !ch.NotifyClean {
			return ""
		}
		fmt.Fprintf(&b, "No drift on %s (check %s).\n", p.Instance, ch.Name)
	case Unknown:
		fmt.Fprintf(&b, "Drift unknown on %s (check %s): the plan logs show a result for %d of %d steps. Nothing was resolved.\n",
			p.Instance, ch.Name, len(res.Steps), res.Expected)
	case Drifted:
		fmt.Fprintf(&b, "Drift on %s (check %s).\n", p.Instance, ch.Name)
		for _, s := range res.Steps {
			b.WriteString(s.String() + "\n")
		}
		if len(res.Resources) > 0 {
			b.WriteString("```\n" + strings.Join(res.Resources, "\n") + "\n```\n")
		}
		if len(p.Undeployed) > 0 {
			fmt.Fprintf(&b, "The plan also includes changes that were never deployed: %s.", strings.Join(p.Undeployed, "; "))
			if ch.Mode != ModeDetect {
				b.WriteString(" Nothing was resolved.")
			}
			b.WriteString("\n")
		}
		if p.Outcome != "" {
			b.WriteString(p.Outcome + "\n")
		}
	}
	b.WriteString("Plan: " + d.planLink(p, planID))
	return b.String()
}

// The deployment in the instance's history, the page that Massdriver's own notifications link to.
func (d *Drift) planLink(p *plan, planID string) string {
	u := fmt.Sprintf("%s/orgs/%s/projects/%s/environments/%s/instances/%s?%s",
		d.opts.AppURL,
		url.PathEscape(d.api.OrganizationID),
		url.PathEscape(p.Project),
		url.PathEscape(p.Environment),
		url.PathEscape(p.Component),
		url.Values{"tab": {"history"}, "deployment": {planID}}.Encode(),
	)
	return "<" + u + "|" + planID + ">"
}

func (d *Drift) notify(ctx context.Context, ch *Check, text string) error {
	hook := ch.SlackWebhookURL
	if hook == "" {
		hook = d.opts.SlackWebhookURL
	}
	if hook == "" {
		automation.Logger(ctx).Info("alert logged", "check", ch.Name, "text", text)
		return nil
	}
	body, err := json.Marshal(map[string]string{"text": text})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, hook, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("content-type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return automation.RedactURL(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return errors.New(resp.Status + ": " + string(b))
	}
	return nil
}

func (d *Drift) saveLocked() {
	if err := saveState(d.opts.StateDir, d.st); err != nil {
		slog.Error("state save failed", "err", err)
	}
}
