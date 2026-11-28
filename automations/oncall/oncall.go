// Package oncall writes who is on call now, from PagerDuty or incident.io, to the on-call widget of Massdriver
// environments on an interval. A route can also write a fixed list, to try the widget out.
package oncall

import (
	"cmp"
	"context"
	"errors"
	"log/slog"
	"slices"
	"time"

	"github.com/massdriver-cloud/massdriver-automations/pkg/automation"
	"github.com/massdriver-cloud/massdriver-automations/pkg/massdriver"
	"github.com/massdriver-cloud/massdriver-automations/pkg/people"
)

const automationName = "oncall"

// Key is the stash key of the environment on-call widget.
const Key = "md/v1/oncall"

const queryTimeout = 30 * time.Second

// Definition registers the automation. It works on an interval. The event destination, which every automation
// has, sends it nothing: its condition is false.
var Definition = automation.Definition{
	Name:        automationName,
	Description: "Writes who is on call now, from PagerDuty or incident.io, to environment on-call cards.",
	Events:      []string{"deployment.completed"},
	New: func() (automation.Automation, error) {
		var cfg Config
		if err := automation.LoadConfig(&cfg); err != nil {
			return nil, err
		}
		api, err := massdriver.FromEnv()
		if err != nil {
			return nil, err
		}
		var pd, iio Provider
		if cfg.usesPagerDuty() {
			v, err := automation.RequireEnv(cfg.PagerDuty.TokenEnv)
			if err != nil {
				return nil, err
			}
			pd = &PagerDuty{URL: cfg.PagerDuty.APIURL, Token: v[cfg.PagerDuty.TokenEnv], HTTP: automation.HTTPClient(0)}
		}
		if cfg.usesIncidentIO() {
			v, err := automation.RequireEnv(cfg.IncidentIO.TokenEnv)
			if err != nil {
				return nil, err
			}
			iio = &IncidentIO{URL: cfg.IncidentIO.APIURL, Token: v[cfg.IncidentIO.TokenEnv], HTTP: automation.HTTPClient(0)}
		}
		o := New(&cfg, pd, iio, api, time.Now)
		go o.Run(context.Background())
		return o, nil
	},
}

// OnCall polls the providers and writes each route's list.
type OnCall struct {
	cfg        *Config
	pagerDuty  Provider
	incidentIO Provider
	out        *people.Writer
	now        func() time.Time
}

// New returns the automation. A provider can be nil when no route uses it. now is the clock.
func New(cfg *Config, pagerDuty, incidentIO Provider, stash people.Stash, now func() time.Time) *OnCall {
	return &OnCall{cfg: cfg, pagerDuty: pagerDuty, incidentIO: incidentIO, now: now,
		out: people.NewWriter(stash, Key, cfg.Label, time.Duration(cfg.Interval))}
}

// Handle ignores every event. People come from the providers on each interval.
func (o *OnCall) Handle(context.Context, automation.Event) error { return nil }

// Run runs a cycle now and then on each interval, until ctx is done.
func (o *OnCall) Run(ctx context.Context) {
	slog.Default().Info("oncall started", "interval", time.Duration(o.cfg.Interval).String(),
		"routes", len(o.cfg.Routes))
	t := time.NewTicker(time.Duration(o.cfg.Interval))
	defer t.Stop()
	for {
		o.Cycle(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Cycle reads each route's people and writes the lists that changed.
func (o *OnCall) Cycle(ctx context.Context) {
	log := automation.Logger(ctx)
	now := o.now().UTC().Truncate(time.Second)
	budget := max(time.Duration(o.cfg.Interval), queryTimeout+10*time.Second)
	cctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	var written, unchanged, skipped, failed int
	for i, r := range o.cfg.Routes {
		entries := r.Static
		if entries == nil {
			var err error
			entries, err = o.people(cctx, r)
			// A failed read would show nobody on call. The last entry stays until its TTL instead.
			if err != nil {
				skipped += len(r.Environments)
				log.Error("oncall query failed", "route", i, "err", err)
				continue
			}
		}
		list := people.List{Title: r.Title, Entries: entries}
		for _, env := range r.Environments {
			attrs := []any{"route", i, "environment", env}
			did, err := o.out.Write(cctx, now, env, list)
			switch {
			case errors.Is(err, people.ErrEmpty):
				skipped++
				log.Debug("oncall skipped", append(attrs, "reason", "nobody_on_call")...)
			case err != nil:
				failed++
				log.Error("oncall write failed", append(attrs, "err", err)...)
			case did:
				written++
				log.Debug("oncall set", append(attrs, "people", len(entries))...)
			default:
				unchanged++
			}
		}
	}
	level := slog.LevelDebug
	if written > 0 || failed > 0 {
		level = slog.LevelInfo
	}
	log.Log(ctx, level, "cycle finished", "routes", len(o.cfg.Routes), "written", written,
		"unchanged", unchanged, "skipped", skipped, "failed", failed)
}

// people reads the route's responders and returns each person once, at their lowest level, up to MaxPeople.
func (o *OnCall) people(ctx context.Context, r Route) ([]people.Person, error) {
	p := o.pagerDuty
	if r.IncidentIO != nil {
		p = o.incidentIO
	}
	qctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()
	responders, err := p.OnCall(qctx, r)
	if err != nil {
		return nil, err
	}
	slices.SortStableFunc(responders, func(a, b Responder) int { return cmp.Compare(a.Level, b.Level) })
	seen := map[string]bool{}
	out := make([]people.Person, 0, r.MaxPeople)
	for _, res := range responders {
		if seen[res.key] || len(out) == r.MaxPeople {
			continue
		}
		seen[res.key] = true
		person, ok := people.Clean(people.Person{Name: res.Name, Email: res.Email, ImageURL: res.AvatarURL,
			Handles: []string{res.Handle}})
		if ok {
			out = append(out, person)
		}
	}
	return out, nil
}
