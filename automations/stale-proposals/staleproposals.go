// Package staleproposals posts a digest of the proposed deployments that have waited too long for approval, and
// can reject the oldest.
package staleproposals

import (
	"context"
	"crypto/subtle"
	_ "embed"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
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
	automationName = "stale-proposals"
	pageLimit      = 100
)

var (
	//go:embed queries/proposed_deployments.graphql
	proposedDeploymentsQuery string
	//go:embed queries/reject_deployment.graphql
	rejectDeploymentMutation string
)

// Definition registers the automation. It works from the API on a schedule. The event destination, which every
// automation has, sends it nothing: its condition is false.
var Definition = automation.Definition{
	Name:        automationName,
	Description: "Posts a digest of proposed deployments that wait too long for approval, and can reject the oldest.",
	Events:      []string{"deployment.proposed"},
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
		s := New(api, sl, &cfg, os.Getenv("STALE_PROPOSALS_RUN_TOKEN"))
		if cfg.Schedule == "" && s.runToken == "" {
			slog.Warn("stale-proposals will never run: set schedule or STALE_PROPOSALS_RUN_TOKEN")
		}
		if cfg.Schedule != "" {
			c := cron.New()
			if _, err := c.AddFunc(cfg.spec(), func() { s.runLogged(context.Background()) }); err != nil {
				return nil, err
			}
			c.Start()
		}
		return s, nil
	},
}

// Stale finds the proposals that wait too long.
type Stale struct {
	api      *massdriver.Client
	slack    *slack.Client
	cfg      *Config
	runToken string
	now      func() time.Time
	run      func(func())

	// mu keeps runs from overlapping, so a proposal is not rejected twice.
	mu sync.Mutex
}

// New returns the automation.
func New(api *massdriver.Client, sl *slack.Client, cfg *Config, runToken string) *Stale {
	return &Stale{api: api, slack: sl, cfg: cfg, runToken: runToken, now: time.Now, run: func(f func()) { go f() }}
}

// Handle ignores every event. Proposals are read from the API when a run starts.
func (s *Stale) Handle(context.Context, automation.Event) error { return nil }

// Routes serves POST /stale-proposals/run when a run token is set.
func (s *Stale) Routes(mux *http.ServeMux) {
	if s.runToken == "" {
		return
	}
	mux.HandleFunc("POST /stale-proposals/run", func(w http.ResponseWriter, r *http.Request) {
		want := []byte("Bearer " + s.runToken)
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), want) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		bg := context.WithoutCancel(r.Context())
		s.run(func() { s.runLogged(bg) })
		w.WriteHeader(http.StatusAccepted)
	})
}

func (s *Stale) runLogged(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	if err := s.Run(ctx); err != nil {
		automation.Logger(ctx).Error("stale proposals run failed", "err", err)
	}
}

type proposal struct {
	ID                  string         `json:"id"`
	CreatedAt           time.Time      `json:"createdAt"`
	Message             string         `json:"message"`
	DeployedBy          string         `json:"deployedBy"`
	EffectiveAttributes map[string]any `json:"effectiveAttributes"`

	age       time.Duration
	rejectErr error
}

func (p proposal) attr(k string) string {
	v, _ := p.EffectiveAttributes[k].(string)
	return v
}

// Run reads every proposal, rejects those past reject_after, and posts one digest of the stale ones.
func (s *Stale) Run(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	log := automation.Logger(ctx)

	all, err := s.proposals(ctx)
	if err != nil {
		return err
	}
	now := s.now()
	var waiting, rejecting []proposal
	for _, p := range all {
		if !s.cfg.Match.Matches(p.EffectiveAttributes) {
			continue
		}
		if len(s.cfg.Exclude) > 0 && s.cfg.Exclude.Matches(p.EffectiveAttributes) {
			continue
		}
		p.age = now.Sub(p.CreatedAt)
		switch {
		case s.cfg.RejectAfter > 0 && p.age >= time.Duration(s.cfg.RejectAfter):
			rejecting = append(rejecting, p)
		case p.age >= time.Duration(s.cfg.RemindAfter):
			waiting = append(waiting, p)
		}
	}

	if !s.cfg.DryRun {
		for i := range rejecting {
			p := &rejecting[i]
			p.rejectErr = s.reject(ctx, p.ID)
			if p.rejectErr != nil {
				log.Error("proposal reject failed", "deployment_id", p.ID, "err", p.rejectErr)
			} else {
				log.Info("proposal rejected", "deployment_id", p.ID, "instance", p.attr("md-instance"), "age", p.age.Round(time.Minute).String())
			}
		}
	}

	log.Info("stale proposals run finished", "proposed", len(all), "waiting", len(waiting), "rejecting", len(rejecting), "dry_run", s.cfg.DryRun)
	if len(waiting) == 0 && len(rejecting) == 0 {
		return nil
	}
	if !s.slack.Sends(s.cfg.Slack.Channel) {
		log.Info("digest logged", "text", s.digest(waiting, rejecting, false))
		return nil
	}
	if _, err := s.slack.Post(ctx, slack.Message{Channel: s.cfg.Slack.Channel, Text: s.digest(waiting, rejecting, true)}); err != nil {
		log.Error("slack post failed", "err", err)
	}
	return nil
}

func (s *Stale) proposals(ctx context.Context) ([]proposal, error) {
	var out []proposal
	var next *string
	for {
		var page struct {
			Deployments struct {
				Cursor struct {
					Next *string `json:"next"`
				} `json:"cursor"`
				Items []proposal `json:"items"`
			} `json:"deployments"`
		}
		err := s.api.Do(ctx, proposedDeploymentsQuery, map[string]any{
			"org":    s.api.OrganizationID,
			"cursor": map[string]any{"limit": pageLimit, "next": next},
		}, &page)
		if err != nil {
			return nil, err
		}
		out = append(out, page.Deployments.Items...)
		if next = page.Deployments.Cursor.Next; next == nil {
			return out, nil
		}
	}
}

func (s *Stale) reject(ctx context.Context, id string) error {
	var out struct {
		RejectDeployment massdriver.Payload[struct {
			ID string `json:"id"`
		}] `json:"rejectDeployment"`
	}
	if err := s.api.Do(ctx, rejectDeploymentMutation, map[string]any{"org": s.api.OrganizationID, "id": id}, &out); err != nil {
		return err
	}
	return out.RejectDeployment.Err()
}

// digest is the Slack text. Mentions come from the config, so the logged digest leaves them out.
func (s *Stale) digest(waiting, rejecting []proposal, mentions bool) string {
	var b strings.Builder
	n := len(waiting)
	switch {
	case n == 1:
		fmt.Fprintf(&b, "1 proposed deployment has waited more than %s for approval.\n", age(time.Duration(s.cfg.RemindAfter)))
	case n > 1:
		fmt.Fprintf(&b, "%d proposed deployments have waited more than %s for approval.\n", n, age(time.Duration(s.cfg.RemindAfter)))
	}
	for _, g := range s.group(waiting) {
		fmt.Fprintf(&b, "\n*%s*\n", g.name)
		for _, p := range g.items {
			b.WriteString(s.line(p, "waiting", mentions) + "\n")
		}
	}

	if len(rejecting) > 0 {
		after := age(time.Duration(s.cfg.RejectAfter))
		if s.cfg.DryRun {
			fmt.Fprintf(&b, "\n*Would reject: waited more than %s (dry run)*\n", after)
		} else {
			fmt.Fprintf(&b, "\n*Rejected: waited more than %s*\n", after)
		}
		for _, g := range s.group(rejecting) {
			for _, p := range g.items {
				line := s.line(p, "waited", mentions)
				if p.rejectErr != nil {
					line += fmt.Sprintf(" Not rejected: %v", p.rejectErr)
				}
				b.WriteString(line + "\n")
			}
		}
	}
	return strings.TrimSpace(b.String())
}

type group struct {
	name  string
	items []proposal
}

// group sorts proposals into projects or environments, the oldest first in each, and the groups by name.
func (s *Stale) group(list []proposal) []group {
	byName := map[string][]proposal{}
	for _, p := range list {
		name := p.attr("md-project")
		if s.cfg.GroupBy == GroupEnvironment {
			name += " / " + p.attr("md-environment")
		}
		byName[name] = append(byName[name], p)
	}
	out := make([]group, 0, len(byName))
	for name, items := range byName {
		sort.SliceStable(items, func(i, j int) bool { return items[i].age > items[j].age })
		out = append(out, group{name: name, items: items})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out
}

func (s *Stale) line(p proposal, verb string, mentions bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "- %s %s %s", s.link(p), verb, age(p.age))
	if p.DeployedBy != "" {
		fmt.Fprintf(&b, ", proposed by %s", p.DeployedBy)
	}
	if msg := strings.TrimSpace(p.Message); msg != "" {
		fmt.Fprintf(&b, ": %q", firstLine(msg))
	}
	if mentions {
		if m := s.mentions(p); len(m) > 0 {
			b.WriteString(" " + strings.Join(m, " "))
		}
	}
	return b.String()
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	const limit = 120
	if len(line) > limit {
		line = strings.ToValidUTF8(line[:limit-3], "") + "..."
	}
	return line
}

// mentions are the mentions of every rule that matches the proposal, each once, in config order.
func (s *Stale) mentions(p proposal) []string {
	seen := map[string]bool{}
	var out []string
	for _, rule := range s.cfg.Mentions {
		if !rule.Match.Matches(p.EffectiveAttributes) {
			continue
		}
		for _, m := range rule.Mentions {
			if !seen[m] {
				seen[m] = true
				out = append(out, m)
			}
		}
	}
	return out
}

// link points at the proposal in the instance's history.
func (s *Stale) link(p proposal) string {
	text := p.attr("md-instance")
	if text == "" {
		text = p.ID
	}
	if p.attr("md-project") == "" || p.attr("md-environment") == "" || p.attr("md-component") == "" {
		return text
	}
	u := fmt.Sprintf("%s/orgs/%s/projects/%s/environments/%s/instances/%s?%s",
		s.cfg.Massdriver.URL(),
		url.PathEscape(s.api.OrganizationID),
		url.PathEscape(p.attr("md-project")),
		url.PathEscape(p.attr("md-environment")),
		url.PathEscape(p.attr("md-component")),
		url.Values{"tab": {"history"}, "deployment": {p.ID}}.Encode(),
	)
	return "<" + u + "|" + text + ">"
}

// age is a duration in days and hours, or hours and minutes under a day.
func age(d time.Duration) string {
	days := int(d / (24 * time.Hour))
	hours := int(d % (24 * time.Hour) / time.Hour)
	switch {
	case days > 0 && hours > 0:
		return fmt.Sprintf("%dd %dh", days, hours)
	case days > 0:
		return fmt.Sprintf("%dd", days)
	case hours > 0:
		if m := int(d % time.Hour / time.Minute); m > 0 {
			return fmt.Sprintf("%dh %dm", hours, m)
		}
		return fmt.Sprintf("%dh", hours)
	}
	return fmt.Sprintf("%dm", int(d/time.Minute))
}
