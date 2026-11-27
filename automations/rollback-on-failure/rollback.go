// Package rollbackonfailure rolls an opted-in instance back to its last completed PROVISION when a PROVISION fails.
package rollbackonfailure

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/massdriver-cloud/massdriver-automations/pkg/automation"
	"github.com/massdriver-cloud/massdriver-automations/pkg/massdriver"
	"github.com/massdriver-cloud/massdriver-automations/pkg/slack"
)

// rollbackMessagePrefix starts the message that Massdriver gives every rollback deployment.
const rollbackMessagePrefix = "Rollback to deployment "

var (
	//go:embed queries/instance_deployments.graphql
	instanceDeploymentsQuery string
	//go:embed queries/rollback_deployment.graphql
	rollbackDeploymentMutation string
	//go:embed queries/approve_deployment.graphql
	approveDeploymentMutation string
)

// threadRetention is how long a notice waits for the outcome of its rollback. A rollback that waits for approval
// can wait days.
const threadRetention = 7 * 24 * time.Hour

// Definition registers the automation.
var Definition = automation.Definition{
	Name:        "rollback-on-failure",
	Description: "Rolls an opted-in instance back to its last completed deployment when a deployment fails.",
	Events:      []string{"deployment.failed", "deployment.completed"},
	New: func() (automation.Automation, error) {
		cfg := DefaultConfig()
		if err := automation.LoadConfig(&cfg); err != nil {
			return nil, err
		}
		api, err := massdriver.FromEnv()
		if err != nil {
			return nil, err
		}
		sl := slack.FromEnv()
		sl.WarnIgnoredChannels(slog.Default(), cfg.Slack.Channel)
		return New(api, sl, cfg), nil
	},
}

// Config is the config file of rollback-on-failure.
type Config struct {
	Attribute  Attribute                `yaml:"attribute"`
	Slack      slack.Config             `yaml:"slack"`
	Massdriver massdriver.AppConfig     `yaml:"massdriver"`
	Results    massdriver.ResultsConfig `yaml:"results"`
}

// Attribute is the attribute key and value that opt an instance in.
type Attribute struct {
	Key   string `yaml:"key"`
	Value string `yaml:"value"`
}

// DefaultConfig opts in the instances whose attribute autorollback is "true".
func DefaultConfig() Config {
	return Config{Attribute: Attribute{Key: "autorollback", Value: "true"}}
}

// Validate requires the opt-in attribute.
func (c *Config) Validate() error {
	if c.Attribute.Key == "" || c.Attribute.Value == "" {
		return errors.New("attribute.key and attribute.value must not be empty")
	}
	return nil
}

// Rollback holds the API client, the configuration, and the Slack notice of each rollback in progress.
type Rollback struct {
	api     *massdriver.Client
	results massdriver.Results
	slack   *slack.Client
	cfg     Config

	mu sync.Mutex
	// threads holds the notice of each rollback deployment, so its outcome is a reply to that notice.
	threads map[string]thread
}

type thread struct {
	channel, ts string
	created     time.Time
}

// New returns the automation.
func New(api *massdriver.Client, sl *slack.Client, cfg Config) *Rollback {
	return &Rollback{api: api, results: massdriver.NewResults(api, cfg.Results), slack: sl, cfg: cfg, threads: map[string]thread{}}
}

type failedDeployment struct {
	ID           string    `json:"id"`
	Message      string    `json:"message"`
	RollbackOfID string    `json:"rollback_of_deployment_id"`
	CreatedAt    time.Time `json:"created_at"`
}

type deployment struct {
	ID        string    `json:"id"`
	Status    string    `json:"status"`
	Version   string    `json:"version"`
	Message   string    `json:"message"`
	CreatedAt time.Time `json:"createdAt"`
}

type page struct {
	Items []deployment `json:"items"`
}

// Handle rolls back a failed PROVISION. The API calls take well under the 10 seconds Massdriver waits, so the
// work runs before the response, and a transport error makes Massdriver retry the event.
func (r *Rollback) Handle(ctx context.Context, ev automation.Event) error {
	instance := ev.Attribute("md-instance")
	if ev.Metadata("deployment_action") != "PROVISION" || instance == "" {
		return nil
	}
	if ev.Attribute(r.cfg.Attribute.Key) != r.cfg.Attribute.Value {
		return nil
	}

	var failed failedDeployment
	if err := ev.DecodeEntity(&failed); err != nil {
		return err
	}
	log := automation.Logger(ctx).With("instance", instance, "deployment_id", failed.ID)

	switch ev.Type {
	case "deployment.completed":
		if th, ok := r.takeThread(failed.ID); ok {
			log.Info("rollback completed")
			r.notify(ctx, log, th, fmt.Sprintf("Rollback %s completed.", r.link(ev, failed.ID, failed.ID)))
		}
		return nil
	case "deployment.failed":
	default:
		return nil
	}

	if failed.RollbackOfID != "" || strings.HasPrefix(failed.Message, rollbackMessagePrefix) {
		log.Warn("rollback skipped", "reason", "failed_deployment_is_rollback")
		th, _ := r.takeThread(failed.ID)
		r.notify(ctx, log, th, fmt.Sprintf("Rollback %s of %s failed. No further rollback. The instance needs attention.",
			r.link(ev, failed.ID, failed.ID), r.link(ev, "", instance)))
		return nil
	}

	var deps struct {
		InProgress    page `json:"inProgress"`
		LastCompleted page `json:"lastCompleted"`
		Proposed      page `json:"proposed"`
	}
	if err := r.api.Do(ctx, instanceDeploymentsQuery, map[string]any{"org": r.api.OrganizationID, "instance": instance}, &deps); err != nil {
		return err
	}

	if len(deps.InProgress.Items) > 0 {
		busy := deps.InProgress.Items[0]
		log.Debug("rollback skipped", "reason", "deployment_in_progress", "busy_deployment_id", busy.ID, "status", busy.Status)
		return nil
	}
	if len(deps.LastCompleted.Items) == 0 {
		log.Debug("rollback skipped", "reason", "no_completed_provision")
		return nil
	}
	target := deps.LastCompleted.Items[0]
	if !failed.CreatedAt.IsZero() && target.CreatedAt.After(failed.CreatedAt) {
		log.Debug("rollback skipped", "reason", "later_provision_completed", "target_deployment_id", target.ID)
		return nil
	}
	log = log.With("target_deployment_id", target.ID)

	// A retry after a failed approval finds the proposal it already made, so it does not propose a second one.
	proposalID := ""
	for _, p := range deps.Proposed.Items {
		if strings.HasPrefix(p.Message, rollbackMessagePrefix+target.ID+" ") {
			proposalID = p.ID
			break
		}
	}
	if proposalID == "" {
		id, err := r.mutate(ctx, rollbackDeploymentMutation, "rollbackDeployment", target.ID)
		if err != nil {
			return err
		}
		proposalID = id
	}
	log = log.With("rollback_deployment_id", proposalID)
	step := massdriver.Step{
		Tool:       "rollback-on-failure",
		Status:     "succeeded",
		FinishedAt: massdriver.Timestamp(time.Now()),
		Message:    fmt.Sprintf("Rolls back failed deployment %s to deployment %s (v%s)", failed.ID, target.ID, target.Version),
	}

	if _, err := r.mutate(ctx, approveDeploymentMutation, "approveDeployment", proposalID); err != nil {
		var rejected *payloadError
		if !errors.As(err, &rejected) {
			return err
		}
		// Separation of duty stops the service account from approving its own proposal. A person approves it.
		log.Warn("rollback approval refused", "err", err)
		step.Message += ". Waits for approval."
		r.results.Write(ctx, "deployment", proposalID, "step/rollback", "Rollback", step)
		r.notifyRollback(ctx, log, proposalID, fmt.Sprintf("Deployment %s of %s failed. Rollback %s to deployment %s (v%s) waits for approval: %s",
			r.link(ev, failed.ID, failed.ID), r.link(ev, "", instance), r.link(ev, proposalID, proposalID),
			r.link(ev, target.ID, target.ID), target.Version, err))
		return nil
	}

	log.Info("rollback started")
	r.results.Write(ctx, "deployment", proposalID, "step/rollback", "Rollback", step)
	r.notifyRollback(ctx, log, proposalID, fmt.Sprintf("Deployment %s of %s failed. Rollback %s restores deployment %s (v%s).",
		r.link(ev, failed.ID, failed.ID), r.link(ev, "", instance), r.link(ev, proposalID, proposalID),
		r.link(ev, target.ID, target.ID), target.Version))
	return nil
}

type payloadError struct{ err error }

func (e *payloadError) Error() string { return e.err.Error() }

func (r *Rollback) mutate(ctx context.Context, mutation, field, id string) (string, error) {
	var out map[string]massdriver.Payload[struct {
		ID string `json:"id"`
	}]
	if err := r.api.Do(ctx, mutation, map[string]any{"org": r.api.OrganizationID, "id": id}, &out); err != nil {
		return "", err
	}
	payload := out[field]
	if err := payload.Err(); err != nil {
		return "", &payloadError{fmt.Errorf("%s: %w", field, err)}
	}
	return payload.Result.ID, nil
}

// link is a Slack link to the instance, or to one deployment in its history when deploymentID is set.
func (r *Rollback) link(ev automation.Event, deploymentID, text string) string {
	parts := strings.Split(ev.Subject, ":")
	if len(parts) < 2 || parts[0] != "mri" {
		return text
	}
	u := fmt.Sprintf("%s/orgs/%s/projects/%s/environments/%s/instances/%s",
		r.cfg.Massdriver.URL(),
		url.PathEscape(parts[1]),
		url.PathEscape(ev.Attribute("md-project")),
		url.PathEscape(ev.Attribute("md-environment")),
		url.PathEscape(ev.Attribute("md-component")),
	)
	if deploymentID != "" {
		u += "?" + url.Values{"tab": {"history"}, "deployment": {deploymentID}}.Encode()
	}
	return "<" + u + "|" + text + ">"
}

// notifyRollback posts the notice of a rollback and keeps where it landed, so the rollback's outcome is a reply.
func (r *Rollback) notifyRollback(ctx context.Context, log *slog.Logger, rollbackID, text string) {
	posted := r.notify(ctx, log, thread{}, text)
	if posted.TS == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	for id, th := range r.threads {
		if now.Sub(th.created) > threadRetention {
			delete(r.threads, id)
		}
	}
	r.threads[rollbackID] = thread{channel: posted.Channel, ts: posted.TS, created: now}
}

func (r *Rollback) takeThread(rollbackID string) (thread, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	th, ok := r.threads[rollbackID]
	delete(r.threads, rollbackID)
	return th, ok
}

// notify posts text, as a reply when th is a thread. A failed notice must not make Massdriver retry the event: the
// rollback already happened.
func (r *Rollback) notify(ctx context.Context, log *slog.Logger, th thread, text string) slack.Posted {
	channel := th.channel
	if channel == "" {
		channel = r.cfg.Slack.Channel
	}
	if !r.slack.Sends(channel) {
		return slack.Posted{}
	}
	posted, err := r.slack.Post(ctx, slack.Message{Channel: channel, ThreadTS: th.ts, Text: text})
	if err != nil {
		log.Error("slack post failed", "err", err)
	}
	return posted
}
