// Package paramstogit writes the params of each successful deployment to a params.json file in a GitHub repository.
package paramstogit

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/massdriver-cloud/massdriver-automations/pkg/automation"
	"github.com/massdriver-cloud/massdriver-automations/pkg/massdriver"
)

//go:embed queries/instance.graphql
var instanceQuery string

const (
	fileName = "params.json"
	// Massdriver waits 10 seconds. The rest is left for the response, so a slow GitHub ends in a retry, not a
	// timeout that Massdriver cannot tell from a crash.
	handleTimeout = 8 * time.Second
	pullBody      = "Massdriver deployed these params. Merging this pull request records them and deploys nothing."
)

// MaskKeyEnv holds an optional key that turns each masked value into a fingerprint of the secret.
const MaskKeyEnv = "PARAMS_MASK_KEY"

// Definition registers the automation.
var Definition = automation.Definition{
	Name:        "params-to-git",
	Description: "Writes the params of each successful deployment to params.json in a GitHub repository.",
	Events:      []string{"deployment.completed"},
	New: func() (automation.Automation, error) {
		cfg := DefaultConfig()
		if err := automation.LoadConfig(&cfg); err != nil {
			return nil, err
		}
		if cfg.usesGitHub() {
			v, err := automation.RequireEnv(cfg.GitHub.TokenEnv)
			if err != nil {
				return nil, err
			}
			cfg.GitHub.token = v[cfg.GitHub.TokenEnv]
		}
		api, err := massdriver.FromEnv()
		if err != nil {
			return nil, err
		}
		return New(api, cfg, []byte(os.Getenv(MaskKeyEnv))), nil
	},
}

// ParamsToGit holds the API clients, the mappings, and one lock for each branch it writes.
type ParamsToGit struct {
	api     *massdriver.Client
	gh      *github
	cfg     Config
	maskKey []byte
	out     io.Writer

	mu    sync.Mutex
	locks map[string]chan struct{}
}

// New returns the automation. cfg must have passed Validate.
func New(api *massdriver.Client, cfg Config, maskKey []byte) *ParamsToGit {
	return &ParamsToGit{
		api:     api,
		gh:      &github{url: cfg.GitHub.APIURL, token: cfg.GitHub.token, http: automation.HTTPClient(0)},
		cfg:     cfg,
		maskKey: maskKey,
		out:     os.Stdout,
		locks:   map[string]chan struct{}{},
	}
}

type deploymentEntity struct {
	ID             string `json:"id"`
	DeployedByName string `json:"deployed_by_name"`
}

type instanceState struct {
	Status          string `json:"status"`
	LatestProvision *struct {
		ID     string          `json:"id"`
		Params json.RawMessage `json:"params"`
		Bundle struct {
			ParamsSchema map[string]any `json:"paramsSchema"`
		} `json:"bundle"`
	} `json:"latestProvision"`
}

// Handle writes or deletes the instance's params.json. It returns an error only when a retry can succeed: a
// Massdriver API error, a GitHub server error, a rate limit, or a branch that moved during the write.
func (p *ParamsToGit) Handle(ctx context.Context, ev automation.Event) error {
	action := ev.Metadata("deployment_action")
	if ev.Type != "deployment.completed" || (action != "PROVISION" && action != "DECOMMISSION") {
		return nil
	}
	instance := ev.Attribute("md-instance")
	log := automation.Logger(ctx).With("instance", instance, "deployment_action", action)
	project, environment, component := ev.Attribute("md-project"), ev.Attribute("md-environment"), ev.Attribute("md-component")
	if !segment(instance) || !segment(project) || !segment(environment) || !segment(component) {
		log.Debug("sync skipped", "reason", "missing_attributes")
		return nil
	}
	var dep deploymentEntity
	if err := ev.DecodeEntity(&dep); err != nil || dep.ID == "" {
		log.Debug("sync skipped", "reason", "no_deployment_id")
		return nil
	}
	log = log.With("deployment_id", dep.ID)

	rt := p.cfg.routeFor(ev.Data.Attributes)
	log = log.With("mapping", rt.name)
	if rt.skip {
		log.Debug("sync skipped", "reason", "mapping_skips")
		return nil
	}
	if action == "DECOMMISSION" && rt.onDecommission == DecommissionKeep {
		log.Debug("sync skipped", "reason", "decommission_keeps_file")
		return nil
	}

	ctx, cancel := context.WithTimeout(ctx, handleTimeout)
	defer cancel()

	data, err := eventData(ev)
	if err != nil {
		log.Error("event decode failed", "err", err)
		return nil
	}
	message, err := rt.message.eval(ctx, data)
	if err != nil {
		log.Error("commit message failed", "err", err)
		return nil
	}
	c := change{
		path:    joinPath(rt.root, project, environment, component, fileName),
		message: message,
		author:  p.author(rt, ev, dep),
	}

	// The instance is read under the branch's lock: a newer deployment's write that ran first is then visible, and
	// this one stands down instead of overwriting it.
	write := func(deliver func(change) error) func() error {
		return func() error {
			content, ok, err := p.content(ctx, log, rt, instance, action, dep.ID)
			if err != nil || !ok {
				return err
			}
			c.content = content
			return deliver(c)
		}
	}

	switch rt.strategy {
	case StrategyStdout:
		return p.locked(ctx, StrategyStdout, "", log, write(func(c change) error { p.toStdout(log, rt, instance, dep.ID, c); return nil }))
	case StrategyDirectory:
		return p.locked(ctx, StrategyDirectory, "", log, write(func(c change) error { p.toDirectory(log, c); return nil }))
	}
	if rt.strategy == StrategyPullRequest {
		head, err := rt.prBranch.eval(ctx, data)
		if err == nil && !rt.rolling {
			head += "-" + shortID(dep.ID)
		}
		if err != nil || head == "" || head == rt.branch {
			log.Error("pull request branch failed", "err", err)
			return nil
		}
		title, err := rt.prTitle.eval(ctx, data)
		if err != nil {
			log.Error("pull request title failed", "err", err)
			return nil
		}
		return p.locked(ctx, rt.repo, head, log, write(func(c change) error { return p.viaPull(ctx, log, rt, head, title, c) }))
	}
	return p.locked(ctx, rt.repo, rt.branch, log, write(func(c change) error { return p.viaPush(ctx, log, rt, c) }))
}

// content returns the file to write, or nil to delete it. ok is false when the event is out of date: events can
// arrive late or twice, and git follows the instance's current state.
func (p *ParamsToGit) content(ctx context.Context, log *slog.Logger, rt *route, instance, action, deploymentID string) ([]byte, bool, error) {
	var out struct {
		Instance *instanceState `json:"instance"`
	}
	if err := p.api.Do(ctx, instanceQuery, map[string]any{"org": p.api.OrganizationID, "id": instance}, &out); err != nil {
		return nil, false, err
	}
	inst := out.Instance

	if action == "DECOMMISSION" {
		if inst != nil && inst.Status == "PROVISIONED" {
			log.Debug("sync skipped", "reason", "redeployed")
			return nil, false, nil
		}
		return nil, true, nil
	}
	if inst == nil || inst.Status == "DECOMMISSIONED" || inst.Status == "INITIALIZED" {
		log.Debug("sync skipped", "reason", "instance_not_deployed")
		return nil, false, nil
	}
	if inst.LatestProvision == nil || inst.LatestProvision.ID != deploymentID {
		log.Debug("sync skipped", "reason", "newer_deployment")
		return nil, false, nil
	}
	red := &redactor{omit: rt.sensitive == SensitiveOmit, key: p.maskKey, scope: instance}
	b, err := render(inst.LatestProvision.Params, inst.LatestProvision.Bundle.ParamsSchema, red)
	if err != nil {
		log.Error("params render failed", "err", err)
		return nil, false, nil
	}
	return b, true, nil
}

// change is one file to write, or to delete when content is nil.
type change struct {
	path    string
	content []byte
	message string
	author  *person
}

func (p *ParamsToGit) viaPush(ctx context.Context, log *slog.Logger, rt *route, c change) error {
	done, err := p.write(ctx, rt.repo, rt.branch, c)
	if err != nil {
		return p.failed(log, err)
	}
	if !done {
		log.Debug("sync skipped", "reason", "unchanged")
		return nil
	}
	log.Info(c.verb(), "strategy", rt.strategy)
	return nil
}

// viaPull commits to head and opens a pull request into the branch. With an open pull request, the commit adds to
// it. Without one, head starts again from the branch, so a merged or closed pull request's commits do not return.
func (p *ParamsToGit) viaPull(ctx context.Context, log *slog.Logger, rt *route, head, title string, c change) error {
	pr, err := p.gh.openPull(ctx, rt.repo, head, rt.branch)
	if err != nil {
		return p.failed(log, err)
	}
	if pr == nil {
		f, err := p.gh.getFile(ctx, rt.repo, rt.branch, c.path)
		if err != nil {
			return p.failed(log, err)
		}
		if same(f, c.content) {
			log.Debug("sync skipped", "reason", "unchanged")
			return nil
		}
		base, err := p.gh.branchSHA(ctx, rt.repo, rt.branch)
		if err == nil && base == "" {
			err = &githubError{op: "get ref", status: 404, message: "branch not found"}
		}
		if err != nil {
			return p.failed(log, err)
		}
		cur, err := p.gh.branchSHA(ctx, rt.repo, head)
		switch {
		case err != nil:
		case cur == "":
			err = p.gh.createBranch(ctx, rt.repo, head, base)
		case cur != base:
			err = p.gh.resetBranch(ctx, rt.repo, head, base)
		}
		if err != nil {
			return p.failed(log, err)
		}
	}

	done, err := p.write(ctx, rt.repo, head, c)
	if err != nil {
		return p.failed(log, err)
	}
	if pr == nil {
		pr, err = p.gh.createPull(ctx, rt.repo, head, rt.branch, title, pullBody)
		if err != nil {
			return p.failed(log, err)
		}
		log.Info("pull request opened", "pull_request", pr.Number)
		if len(rt.users)+len(rt.teams) > 0 {
			if err := p.gh.requestReviewers(ctx, rt.repo, pr.Number, rt.users, rt.teams); err != nil {
				// The pull request exists. A retry would find it open and not ask again, so only log.
				log.Warn("reviewer request failed", "pull_request", pr.Number, "err", err)
			}
		}
	}
	if !done {
		log.Debug("sync skipped", "reason", "unchanged", "pull_request", pr.Number)
		return nil
	}
	log.Info(c.verb(), "strategy", rt.strategy, "pull_request", pr.Number)
	return nil
}

// write commits the change to branch when the file there differs. It reports whether it committed.
func (p *ParamsToGit) write(ctx context.Context, repo, branch string, c change) (bool, error) {
	f, err := p.gh.getFile(ctx, repo, branch, c.path)
	if err != nil {
		return false, err
	}
	if same(f, c.content) {
		return false, nil
	}
	if c.content == nil {
		return true, p.gh.deleteFile(ctx, repo, branch, c.path, f.sha, c.message, c.author)
	}
	return true, p.gh.putFile(ctx, repo, branch, c.path, f.sha, c.message, c.content, c.author)
}

func same(f file, content []byte) bool {
	if content == nil {
		return !f.exists
	}
	return f.exists && bytes.Equal(f.content, content)
}

func (c change) verb() string {
	if c.content == nil {
		return "params deleted"
	}
	return "params committed"
}

// failed logs err, and returns it when Massdriver should send the event again.
func (p *ParamsToGit) failed(log *slog.Logger, err error) error {
	if retryable(err) {
		return err
	}
	log.Error("sync failed", "err", err)
	return nil
}

// locked runs f alone for repo and branch, so two deployments do not race on one branch and commits land in the
// order the events arrive. A wait longer than the handler's time ends in a retry.
func (p *ParamsToGit) locked(ctx context.Context, repo, branch string, log *slog.Logger, f func() error) error {
	key := repo + "\x00" + branch
	p.mu.Lock()
	l, ok := p.locks[key]
	if !ok {
		l = make(chan struct{}, 1)
		p.locks[key] = l
	}
	p.mu.Unlock()
	select {
	case l <- struct{}{}:
	case <-ctx.Done():
		log.Warn("branch busy", "err", ctx.Err())
		return errors.New("timed out waiting for another write to the branch")
	}
	defer func() { <-l }()
	return f()
}

// author is the person or service account that ran the deployment, or nil for the token's own identity.
func (p *ParamsToGit) author(rt *route, ev automation.Event, dep deploymentEntity) *person {
	if rt.author == AuthorBot {
		return nil
	}
	var actor struct {
		Name string `json:"name"`
	}
	_ = ev.DecodeActor(&actor)
	name := strings.TrimSpace(actor.Name)
	if name == "" {
		name = strings.TrimSpace(dep.DeployedByName)
	}
	if name == "" {
		return nil
	}
	return &person{Name: name, Email: rt.email}
}

func eventData(ev automation.Event) (any, error) {
	raw := ev.Raw
	if len(raw) == 0 {
		b, err := json.Marshal(ev)
		if err != nil {
			return nil, err
		}
		raw = b
	}
	var data any
	err := json.Unmarshal(raw, &data)
	return data, err
}

// segment reports whether an identifier is safe as one directory of the path.
func segment(s string) bool {
	return s != "" && s != "." && s != ".." && !strings.ContainsAny(s, "/\\")
}

func joinPath(parts ...string) string {
	var out []string
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, "/")
}

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}
