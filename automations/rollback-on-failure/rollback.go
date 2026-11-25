// Package rollbackonfailure rolls an opted-in instance back to its last completed PROVISION when a PROVISION fails.
package rollbackonfailure

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/massdriver-cloud/massdriver-automations/pkg/automation"
	"github.com/massdriver-cloud/massdriver-automations/pkg/massdriver"
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

// Definition registers the automation.
var Definition = automation.Definition{
	Name:        "rollback-on-failure",
	Description: "Rolls an opted-in instance back to its last completed deployment when a deployment fails.",
	Events:      []string{"deployment.failed"},
	New: func() (automation.Automation, error) {
		api, err := massdriver.FromEnv()
		if err != nil {
			return nil, err
		}
		return New(api, Config{
			AttrKey:         automation.Env("AUTO_ROLLBACK_ATTR_KEY", "autorollback"),
			AttrValue:       automation.Env("AUTO_ROLLBACK_ATTR_VALUE", "true"),
			SlackWebhookURL: automation.Env("SLACK_WEBHOOK_URL", ""),
			AppURL:          automation.Env("MASSDRIVER_APP_URL", "https://app.massdriver.cloud"),
		}), nil
	},
}

// Config is the opt-in attribute and the optional Slack notice.
type Config struct {
	AttrKey         string
	AttrValue       string
	SlackWebhookURL string
	AppURL          string
}

// Rollback holds the API client and the configuration.
type Rollback struct {
	api  *massdriver.Client
	cfg  Config
	http *http.Client
}

// New returns the automation.
func New(api *massdriver.Client, cfg Config) *Rollback {
	cfg.AppURL = strings.TrimRight(cfg.AppURL, "/")
	return &Rollback{api: api, cfg: cfg, http: http.DefaultClient}
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
	if ev.Type != "deployment.failed" || ev.Metadata("deployment_action") != "PROVISION" || instance == "" {
		return nil
	}
	if ev.Attribute(r.cfg.AttrKey) != r.cfg.AttrValue {
		return nil
	}

	var failed failedDeployment
	if err := ev.DecodeEntity(&failed); err != nil {
		return err
	}
	log := slog.With("automation", Definition.Name, "event_id", ev.ID, "instance", instance, "failed_deployment", failed.ID)

	if failed.RollbackOfID != "" || strings.HasPrefix(failed.Message, rollbackMessagePrefix) {
		log.Warn("skip: the failed deployment is a rollback")
		r.notify(ctx, log, fmt.Sprintf("Rollback %s of %s failed. No further rollback. The instance needs attention.",
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
		log.Info("skip: a deployment is in progress", "deployment", busy.ID, "status", busy.Status)
		return nil
	}
	if len(deps.LastCompleted.Items) == 0 {
		log.Info("skip: no completed PROVISION to roll back to")
		return nil
	}
	target := deps.LastCompleted.Items[0]
	if !failed.CreatedAt.IsZero() && target.CreatedAt.After(failed.CreatedAt) {
		log.Info("skip: a later PROVISION completed", "deployment", target.ID)
		return nil
	}
	log = log.With("rollback_to", target.ID)

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
	log = log.With("rollback_deployment", proposalID)

	if _, err := r.mutate(ctx, approveDeploymentMutation, "approveDeployment", proposalID); err != nil {
		var rejected *payloadError
		if !errors.As(err, &rejected) {
			return err
		}
		// Separation of duty stops the service account from approving its own proposal. A person approves it.
		log.Warn("rollback proposed, approval refused", "error", err)
		r.notify(ctx, log, fmt.Sprintf("Deployment %s of %s failed. Rollback %s to deployment %s (v%s) waits for approval: %s",
			r.link(ev, failed.ID, failed.ID), r.link(ev, "", instance), r.link(ev, proposalID, proposalID),
			r.link(ev, target.ID, target.ID), target.Version, err))
		return nil
	}

	log.Info("rolled back")
	r.notify(ctx, log, fmt.Sprintf("Deployment %s of %s failed. Rollback %s restores deployment %s (v%s).",
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
		r.cfg.AppURL,
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

// A failed notice must not make Massdriver retry the event: the rollback already happened.
func (r *Rollback) notify(ctx context.Context, log *slog.Logger, text string) {
	if r.cfg.SlackWebhookURL == "" {
		return
	}
	if err := r.postSlack(ctx, text); err != nil {
		log.Error("slack webhook", "error", err)
	}
}

func (r *Rollback) postSlack(ctx context.Context, text string) error {
	body, err := json.Marshal(map[string]string{"text": text})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.cfg.SlackWebhookURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("content-type", "application/json")
	resp, err := r.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return errors.New(resp.Status + ": " + string(b))
	}
	return nil
}
