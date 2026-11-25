// Package githubdeployments mirrors Massdriver deployments as GitHub Deployments and Deployment Statuses.
package githubdeployments

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/massdriver-cloud/massdriver-automations/pkg/automation"
	"github.com/massdriver-cloud/massdriver-automations/pkg/massdriver"
)

const (
	deploymentTask = "massdriver"
	maxDescription = 140
)

// Definition registers the automation.
var Definition = automation.Definition{
	Name:        "github-deployments",
	Description: "Mirrors Massdriver deployments as GitHub Deployments in the repo set on the instance.",
	Events:      []string{"deployment.started", "deployment.completed", "deployment.failed", "deployment.aborted"},
	New: func() (automation.Automation, error) {
		cfg := DefaultConfig()
		if err := automation.LoadConfig(&cfg); err != nil {
			return nil, err
		}
		v, err := automation.RequireEnv(cfg.GitHub.TokenEnv)
		if err != nil {
			return nil, err
		}
		cfg.GitHub.token = v[cfg.GitHub.TokenEnv]
		return New(cfg), nil
	},
}

// Config is the config file of github-deployments.
type Config struct {
	Massdriver massdriver.AppConfig `yaml:"massdriver"`
	GitHub     GitHubConfig         `yaml:"github"`
}

// GitHubConfig is the GitHub connection and the attribute keys that name the repo and the ref.
type GitHubConfig struct {
	APIURL          string `yaml:"api_url"`
	TokenEnv        string `yaml:"token_env"`
	RepoAttribute   string `yaml:"repo_attribute"`
	RefAttribute    string `yaml:"ref_attribute"`
	EnvironmentName string `yaml:"environment_name"`

	token string
}

// DefaultConfig uses github.com and the github_repo and github_ref attributes.
func DefaultConfig() Config {
	return Config{GitHub: GitHubConfig{
		APIURL:          "https://api.github.com",
		TokenEnv:        "GITHUB_TOKEN",
		RepoAttribute:   "github_repo",
		RefAttribute:    "github_ref",
		EnvironmentName: "environment",
	}}
}

// Validate checks the settings that have a fixed set of values.
func (c *Config) Validate() error {
	g := &c.GitHub
	if g.EnvironmentName != "environment" && g.EnvironmentName != "project-environment" {
		return fmt.Errorf("github.environment_name must be environment or project-environment, got %q", g.EnvironmentName)
	}
	for key, v := range map[string]string{"api_url": g.APIURL, "token_env": g.TokenEnv, "repo_attribute": g.RepoAttribute, "ref_attribute": g.RefAttribute} {
		if v == "" {
			return fmt.Errorf("github.%s must not be empty", key)
		}
	}
	g.APIURL = strings.TrimRight(g.APIURL, "/")
	return nil
}

// GitHubDeployments holds the GitHub client and the deployments it created.
type GitHubDeployments struct {
	cfg Config
	gh  *github

	// One event at a time, so a started and a completed event that arrive together create one GitHub deployment.
	mu          sync.Mutex
	deployments map[string]int64
	branches    map[string]string
}

// New returns the automation.
func New(cfg Config) *GitHubDeployments {
	return &GitHubDeployments{
		cfg:         cfg,
		gh:          &github{url: cfg.GitHub.APIURL, token: cfg.GitHub.token},
		deployments: map[string]int64{},
		branches:    map[string]string{},
	}
}

var verbs = map[string]string{
	"deployment.started":   "started",
	"deployment.completed": "completed",
	"deployment.failed":    "failed",
	"deployment.aborted":   "aborted",
}

var terminal = map[string]bool{"success": true, "failure": true, "error": true, "inactive": true}

type deployment struct {
	ID             string `json:"id"`
	Message        string `json:"message"`
	DeployedByName string `json:"deployed_by_name"`
}

type deploymentPayload struct {
	MassdriverDeploymentID string `json:"massdriver_deployment_id"`
	Instance               string `json:"instance"`
	Bundle                 string `json:"bundle,omitempty"`
	Action                 string `json:"action"`
}

// Handle creates or finds the GitHub deployment and posts its status. A GitHub error is returned, so Massdriver
// sends the event again.
func (g *GitHubDeployments) Handle(ctx context.Context, ev automation.Event) error {
	repo := ev.Attribute(g.cfg.GitHub.RepoAttribute)
	action := ev.Metadata("deployment_action")
	if verbs[ev.Type] == "" || action == "PLAN" || !validRepo(repo) || ev.Attribute("md-environment") == "" {
		return nil
	}
	var dep deployment
	if err := ev.DecodeEntity(&dep); err != nil {
		return err
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	// Massdriver waits 10 seconds for a response.
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	log := automation.Logger(ctx).With("deployment_id", dep.ID, "repo", repo)

	ghID, err := g.githubDeployment(ctx, ev, dep, repo, action)
	if err != nil {
		return err
	}

	state := stateFor(ev.Type, action)
	// Events arrive in any order. A started event that arrives after the end must not reopen the deployment.
	if state == "in_progress" {
		latest, err := g.gh.latestState(ctx, repo, ghID)
		if err != nil {
			return err
		}
		if terminal[latest] {
			log.Debug("status skipped", "reason", "deployment_ended", "github_deployment_id", ghID, "state", latest)
			return nil
		}
	}

	status := map[string]any{
		"state":       state,
		"log_url":     g.logURL(ev, dep.ID),
		"description": truncate(fmt.Sprintf("%s of %s %s", noun(action), ev.Attribute("md-instance"), verbs[ev.Type]), maxDescription),
		"environment": g.environment(ev),
	}
	if err := g.gh.do(ctx, http.MethodPost, fmt.Sprintf("/repos/%s/deployments/%d/statuses", repo, ghID), status, nil); err != nil {
		return err
	}
	log.Info("status posted", "github_deployment_id", ghID, "state", state)
	return nil
}

func validRepo(repo string) bool {
	owner, name, ok := strings.Cut(repo, "/")
	return ok && owner != "" && name != "" && !strings.Contains(name, "/")
}

func stateFor(eventType, action string) string {
	switch eventType {
	case "deployment.started":
		return "in_progress"
	case "deployment.completed":
		if action == "DECOMMISSION" {
			return "inactive"
		}
		return "success"
	case "deployment.failed":
		return "failure"
	default:
		return "error"
	}
}

func (g *GitHubDeployments) environment(ev automation.Event) string {
	if g.cfg.GitHub.EnvironmentName == "project-environment" {
		return ev.Attribute("md-project") + "-" + ev.Attribute("md-environment")
	}
	return ev.Attribute("md-environment")
}

// The deployment in the instance's history, the page that Massdriver's own notifications link to.
func (g *GitHubDeployments) logURL(ev automation.Event, deploymentID string) string {
	parts := strings.Split(ev.Subject, ":")
	if len(parts) < 2 || parts[0] != "mri" {
		return ""
	}
	return fmt.Sprintf("%s/orgs/%s/projects/%s/environments/%s/instances/%s?%s",
		g.cfg.Massdriver.URL(),
		url.PathEscape(parts[1]),
		url.PathEscape(ev.Attribute("md-project")),
		url.PathEscape(ev.Attribute("md-environment")),
		url.PathEscape(ev.Attribute("md-component")),
		url.Values{"tab": {"history"}, "deployment": {deploymentID}}.Encode(),
	)
}

func noun(action string) string {
	if action == "DECOMMISSION" {
		return "Decommission"
	}
	return "Deploy"
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

func description(ev automation.Event, dep deployment, action string) string {
	if dep.Message != "" {
		return dep.Message
	}
	d := fmt.Sprintf("%s of %s", noun(action), ev.Attribute("md-instance"))
	if b := ev.Attribute("md-bundle"); b != "" {
		d += " (" + b + ")"
	}
	if dep.DeployedByName != "" {
		d += " by " + dep.DeployedByName
	}
	return d
}

// After a restart the in-memory map is empty, so the automation searches GitHub for the deployment whose
// payload names the Massdriver deployment before it creates one.
func (g *GitHubDeployments) githubDeployment(ctx context.Context, ev automation.Event, dep deployment, repo, action string) (int64, error) {
	key := repo + "#" + dep.ID
	if id, ok := g.deployments[key]; ok {
		return id, nil
	}

	environment := g.environment(ev)
	id, err := g.gh.findDeployment(ctx, repo, environment, dep.ID)
	if err != nil {
		return 0, err
	}
	if id != 0 {
		g.deployments[key] = id
		return id, nil
	}

	ref := ev.Attribute(g.cfg.GitHub.RefAttribute)
	if ref == "" {
		if ref, err = g.defaultBranch(ctx, repo); err != nil {
			return 0, err
		}
	}

	var out struct {
		ID int64 `json:"id"`
	}
	err = g.gh.do(ctx, http.MethodPost, "/repos/"+repo+"/deployments", map[string]any{
		"ref":         ref,
		"task":        deploymentTask,
		"environment": environment,
		"description": description(ev, dep, action),
		"payload": deploymentPayload{
			MassdriverDeploymentID: dep.ID,
			Instance:               ev.Attribute("md-instance"),
			Bundle:                 ev.Attribute("md-bundle"),
			Action:                 action,
		},
		// Massdriver already deployed. GitHub must not merge the default branch or wait on commit checks.
		"auto_merge":        false,
		"required_contexts": []string{},
	}, &out)
	if err != nil {
		return 0, err
	}
	g.deployments[key] = out.ID
	return out.ID, nil
}

func (g *GitHubDeployments) defaultBranch(ctx context.Context, repo string) (string, error) {
	if b, ok := g.branches[repo]; ok {
		return b, nil
	}
	var out struct {
		DefaultBranch string `json:"default_branch"`
	}
	if err := g.gh.do(ctx, http.MethodGet, "/repos/"+repo, nil, &out); err != nil {
		return "", err
	}
	g.branches[repo] = out.DefaultBranch
	return out.DefaultBranch, nil
}

type github struct {
	url   string
	token string
}

func (c *github) findDeployment(ctx context.Context, repo, environment, massdriverID string) (int64, error) {
	q := url.Values{"task": {deploymentTask}, "environment": {environment}, "per_page": {"100"}}
	var out []struct {
		ID      int64           `json:"id"`
		Payload json.RawMessage `json:"payload"`
	}
	if err := c.do(ctx, http.MethodGet, "/repos/"+repo+"/deployments?"+q.Encode(), nil, &out); err != nil {
		return 0, err
	}
	for _, d := range out {
		var p deploymentPayload
		if json.Unmarshal(d.Payload, &p) == nil && p.MassdriverDeploymentID == massdriverID {
			return d.ID, nil
		}
	}
	return 0, nil
}

func (c *github) latestState(ctx context.Context, repo string, id int64) (string, error) {
	var out []struct {
		State string `json:"state"`
	}
	if err := c.do(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/deployments/%d/statuses?per_page=1", repo, id), nil, &out); err != nil {
		return "", err
	}
	if len(out) == 0 {
		return "", nil
	}
	return out[0].State, nil
}

func (c *github) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.url+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("accept", "application/vnd.github+json")
	req.Header.Set("x-github-api-version", "2022-11-28")
	req.Header.Set("authorization", "Bearer "+c.token)
	if in != nil {
		req.Header.Set("content-type", "application/json")
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("github: %s %s: HTTP %d: %s", method, path, resp.StatusCode, b)
	}
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("github: decode %s: %w", path, err)
	}
	return nil
}
