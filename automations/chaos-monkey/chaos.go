// Package chaosmonkey deploys random params to instances that match a set of attributes: presets, merges of
// presets, mutated fields, and gibberish. It proposes, plans, or deploys them, and posts how each one went.
package chaosmonkey

import (
	"context"
	"crypto/subtle"
	_ "embed"
	"encoding/json"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/url"
	"os"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	// The schedule's timezone must resolve in a distroless image, which has no zoneinfo files.
	_ "time/tzdata"

	"github.com/robfig/cron/v3"

	"github.com/massdriver-cloud/massdriver-automations/pkg/automation"
	"github.com/massdriver-cloud/massdriver-automations/pkg/massdriver"
	"github.com/massdriver-cloud/massdriver-automations/pkg/slack"
)

const (
	automationName = "chaos-monkey"
	pageLimit      = 100
	// MessagePrefix starts the message of every deployment that chaos-monkey creates. The event destination's
	// condition and Handle use it to find them.
	MessagePrefix = "chaos-monkey: "
	maxMessage    = 240
)

var (
	//go:embed queries/matching_instances.graphql
	matchingInstancesQuery string
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
	Description: "Deploys presets, merged presets, mutated params, and gibberish to matching instances on a schedule.",
	Events:      []string{"deployment.completed", "deployment.failed", "deployment.aborted"},
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
		sl.WarnIgnoredChannels(slog.Default(), cfg.Slack.Channel)
		c := New(api, sl, &cfg, os.Getenv("CHAOS_MONKEY_RUN_TOKEN"))
		if cfg.Schedule != "" {
			cr := cron.New()
			if _, err := cr.AddFunc(cfg.spec(), func() { c.runLogged(context.Background(), "") }); err != nil {
				return nil, err
			}
			cr.Start()
		}
		return c, nil
	},
}

// Chaos picks instances and deploys random params to them.
type Chaos struct {
	api      *massdriver.Client
	slack    *slack.Client
	cfg      *Config
	runToken string
	run      func(func())

	// mu guards the random source, which is not safe for concurrent use, and keeps runs from overlapping.
	mu     sync.Mutex
	monkey *monkey
}

// New returns the automation. A seed in the config makes the runs repeat.
func New(api *massdriver.Client, sl *slack.Client, cfg *Config, runToken string) *Chaos {
	// The picks only need to look random, and a seed must repeat them.
	var src rand.Source
	if cfg.Seed != nil {
		src = rand.NewPCG(*cfg.Seed, *cfg.Seed)
	} else {
		src = rand.NewPCG(rand.Uint64(), rand.Uint64()) //nolint:gosec
	}
	return &Chaos{
		api: api, slack: sl, cfg: cfg, runToken: runToken,
		run:    func(f func()) { go f() },
		monkey: &monkey{r: rand.New(src), cfg: cfg}, //nolint:gosec
	}
}

// Routes serves POST /chaos-monkey/run when a run token is set. `?strategy=<name>` forces one strategy.
func (c *Chaos) Routes(mux *http.ServeMux) {
	if c.runToken == "" {
		return
	}
	mux.HandleFunc("POST /chaos-monkey/run", func(w http.ResponseWriter, r *http.Request) {
		want := []byte("Bearer " + c.runToken)
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), want) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		strategy := r.URL.Query().Get("strategy")
		if strategy != "" && !slices.Contains(strategies, strategy) {
			http.Error(w, "unknown strategy", http.StatusBadRequest)
			return
		}
		bg := context.WithoutCancel(r.Context())
		c.run(func() { c.runLogged(bg, strategy) })
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]string{"mode": c.cfg.Mode, "strategy": strategy})
	})
}

func (c *Chaos) runLogged(ctx context.Context, strategy string) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	if err := c.Run(ctx, strategy); err != nil {
		automation.Logger(ctx).Error("chaos run failed", "err", err)
	}
}

// Run picks up to `instances` matching instances at random and sends each one a deployment of random params. An
// empty strategy picks one per instance by weight.
func (c *Chaos) Run(ctx context.Context, strategy string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	log := automation.Logger(ctx)

	ids, err := c.matching(ctx)
	if err != nil {
		return err
	}
	c.monkey.r.Shuffle(len(ids), func(i, j int) { ids[i], ids[j] = ids[j], ids[i] })
	log.Info("chaos run started", "matched", len(ids), "mode", c.cfg.Mode)

	hit := 0
	for _, id := range ids {
		if hit >= c.cfg.Instances {
			break
		}
		done, err := c.strike(ctx, id, strategy)
		if err != nil {
			log.Error("chaos strike failed", "instance", id, "err", err)
			continue
		}
		if done {
			hit++
		}
	}
	if hit == 0 {
		log.Info("chaos run found nothing to do", "matched", len(ids))
	}
	return nil
}

func (c *Chaos) matching(ctx context.Context) ([]string, error) {
	keys := make([]string, 0, len(c.cfg.Match))
	for k := range c.cfg.Match {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	attrs := make([]map[string]any, 0, len(keys))
	for _, k := range keys {
		if v := c.cfg.Match[k]; len(v) == 1 {
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
		err := c.api.Do(ctx, matchingInstancesQuery, map[string]any{
			"org":        c.api.OrganizationID,
			"attributes": attrs,
			"status":     c.cfg.Status,
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

type instance struct {
	ID                  string         `json:"id"`
	Status              string         `json:"status"`
	Params              map[string]any `json:"params"`
	ParamsSchema        map[string]any `json:"paramsSchema"`
	EffectiveAttributes map[string]any `json:"effectiveAttributes"`
	LatestDeployment    *struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	} `json:"latestDeployment"`
	MostRecentProposedDeployment *struct {
		ID string `json:"id"`
	} `json:"mostRecentProposedDeployment"`
}

var inProgress = []string{"PENDING", "APPROVED", "RUNNING"}

// strike sends one deployment to the instance. It returns false when the instance was skipped.
func (c *Chaos) strike(ctx context.Context, id, strategy string) (bool, error) {
	log := automation.Logger(ctx).With("instance", id)
	var out struct {
		Instance instance `json:"instance"`
	}
	if err := c.api.Do(ctx, instanceQuery, map[string]any{"org": c.api.OrganizationID, "id": id}, &out); err != nil {
		return false, err
	}
	inst := out.Instance
	switch {
	case inst.LatestDeployment != nil && slices.Contains(inProgress, inst.LatestDeployment.Status):
		log.Debug("chaos skipped", "reason", "deployment_in_progress")
		return false, nil
	case c.cfg.Mode == ModePropose && inst.MostRecentProposedDeployment != nil:
		log.Debug("chaos skipped", "reason", "proposal_waiting")
		return false, nil
	}

	var ms mess
	if strategy != "" {
		if !fits(strategy, inst.ParamsSchema) {
			log.Debug("chaos skipped", "reason", "strategy_does_not_fit", "strategy", strategy)
			return false, nil
		}
		ms = c.monkey.run(strategy, inst.ParamsSchema, inst.Params)
	} else {
		var ok bool
		if ms, ok = c.monkey.Make(inst.ParamsSchema, inst.Params); !ok {
			log.Debug("chaos skipped", "reason", "no_strategy_fits")
			return false, nil
		}
	}

	params, err := massdriver.JSONString(ms.Params)
	if err != nil {
		return false, err
	}
	sig := c.signature()
	message := deploymentMessage(MessagePrefix+ms.Strategy+": "+ms.Detail, sig)
	action := "PROVISION"
	if c.cfg.Mode == ModePlan {
		action = "PLAN"
	}
	mutation, field := createDeploymentMutation, "createDeployment"
	if c.cfg.Mode == ModePropose {
		mutation, field = proposeDeploymentMutation, "proposeDeployment"
	}
	vars := map[string]any{
		"org":   c.api.OrganizationID,
		"id":    inst.ID,
		"input": map[string]any{"action": action, "params": params, "message": message},
	}
	var res map[string]massdriver.Payload[struct {
		ID string `json:"id"`
	}]
	if err := c.api.Do(ctx, mutation, vars, &res); err != nil {
		return false, err
	}
	log = log.With("strategy", ms.Strategy, "mode", c.cfg.Mode)
	link := func(deploymentID, text string) string { return c.link(inst.EffectiveAttributes, deploymentID, text) }

	p := res[field]
	if rejected := p.Err(); rejected != nil {
		log.Info("chaos rejected", "detail", ms.Detail)
		c.notify(ctx, fmt.Sprintf("Chaos monkey tried %s with %s: %s. Massdriver rejected the params: %s",
			link("", inst.ID), ms.Strategy, ms.Detail, rejected), sig)
		return true, nil
	}
	verb := map[string]string{ModePropose: "proposed", ModePlan: "planned", ModeDeploy: "deployed"}[c.cfg.Mode]
	log.Info("chaos deployment created", "deployment_id", p.Result.ID, "detail", ms.Detail)
	text := fmt.Sprintf("Chaos monkey %s %s with %s: %s. Deployment %s.",
		verb, link("", inst.ID), ms.Strategy, ms.Detail, link(p.Result.ID, p.Result.ID))
	if ms.Strategy == StrategyGibberish {
		text += " Massdriver accepted gibberish."
	}
	c.notify(ctx, text, sig)
	return true, nil
}

// signature picks a sign-off for one deployment, or "" when signatures are off.
func (c *Chaos) signature() string {
	list := c.cfg.signatures
	if len(list) == 0 {
		return ""
	}
	return list[c.monkey.r.IntN(len(list))]
}

// deploymentMessage shortens body so that it and the signature fit in one deployment message.
func deploymentMessage(body, sig string) string {
	limit := maxMessage
	if sig != "" {
		limit -= len(sig) + 1
	}
	// A signature that leaves no room for what the deployment does is dropped.
	if limit < 40 {
		sig, limit = "", maxMessage
	}
	if len(body) > limit {
		body = strings.ToValidUTF8(body[:limit-3], "") + "..."
	}
	if sig == "" {
		return body
	}
	return body + " " + sig
}

// Handle posts the outcome of each deployment that chaos-monkey created.
func (c *Chaos) Handle(ctx context.Context, ev automation.Event) error {
	var outcome string
	switch ev.Type {
	case "deployment.completed":
		outcome = "completed"
	case "deployment.failed":
		outcome = "failed"
	case "deployment.aborted":
		outcome = "was aborted"
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
	attrs := map[string]any{
		"md-project":     ev.Attribute("md-project"),
		"md-environment": ev.Attribute("md-environment"),
		"md-component":   ev.Attribute("md-component"),
	}
	instance := ev.Attribute("md-instance")
	automation.Logger(ctx).Info("chaos deployment finished", "instance", instance, "deployment_id", dep.ID, "outcome", outcome)
	c.notify(ctx, fmt.Sprintf("Chaos deployment %s of %s %s: %s",
		c.link(attrs, dep.ID, dep.ID), c.link(attrs, "", instance), outcome,
		strings.TrimPrefix(dep.Message, MessagePrefix)), "")
	return nil
}

// link is a Slack link to the instance, or to one deployment in its history when deploymentID is set.
func (c *Chaos) link(attrs map[string]any, deploymentID, text string) string {
	attr := func(k string) string {
		s, _ := attrs[k].(string)
		return s
	}
	if attr("md-project") == "" || attr("md-environment") == "" || attr("md-component") == "" {
		return text
	}
	u := fmt.Sprintf("%s/orgs/%s/projects/%s/environments/%s/instances/%s",
		c.cfg.Massdriver.URL(),
		url.PathEscape(c.api.OrganizationID),
		url.PathEscape(attr("md-project")),
		url.PathEscape(attr("md-environment")),
		url.PathEscape(attr("md-component")),
	)
	if deploymentID != "" {
		u += "?" + url.Values{"tab": {"history"}, "deployment": {deploymentID}}.Encode()
	}
	return "<" + u + "|" + text + ">"
}

// notify posts text and its signature to the channel, or logs them without one. A failed post is logged: the
// deployment already exists, and a retry must not create another. An outcome's deployment message already ends
// with its signature, so Handle passes none.
func (c *Chaos) notify(ctx context.Context, text, sig string) {
	log := automation.Logger(ctx)
	if sig != "" {
		text += "\n" + sig
	}
	if !c.slack.Sends(c.cfg.Slack.Channel) {
		log.Info("chaos report logged", "text", text)
		return
	}
	if _, err := c.slack.Post(ctx, slack.Message{Channel: c.cfg.Slack.Channel, Text: text}); err != nil {
		log.Error("slack post failed", "err", err)
	}
}
