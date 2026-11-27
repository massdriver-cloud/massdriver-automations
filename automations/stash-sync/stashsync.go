// Package stashsync copies cloud metrics into Massdriver stash entries on an interval, so the widget keys under
// md/v1/ show health, usage, traffic, and banners on the canvas. In simulate mode it writes changing values from a
// scenario, with no cloud access.
package stashsync

import (
	"context"
	"log/slog"
	"math/rand/v2"
	"os"
	"time"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"

	"github.com/massdriver-cloud/massdriver-automations/pkg/automation"
	"github.com/massdriver-cloud/massdriver-automations/pkg/massdriver"
)

const automationName = "stash-sync"

// Definition registers the automation. It works on an interval. The event destination, which every automation
// has, sends it nothing: its condition is false.
var Definition = automation.Definition{
	Name:        automationName,
	Description: "Copies cloud metrics into Massdriver stash entries, so widgets show health, usage, and traffic.",
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
		sources, err := newSources(&cfg)
		if err != nil {
			return nil, err
		}
		s := New(&cfg, &GraphQLStash{API: api}, sources, time.Now, nil)
		go s.Run(context.Background())
		return s, nil
	},
}

// newSources sets up the sources that the rules use. CloudWatch uses the default AWS credential chain.
func newSources(cfg *Config) (map[string]Source, error) {
	sources := map[string]Source{}
	for _, g := range cfg.groups {
		for _, r := range g.rules {
			q := r.query()
			if q == nil || sources[q.source()] != nil {
				continue
			}
			switch q.(type) {
			case *CloudWatchQuery:
				var opts []func(*awsconfig.LoadOptions) error
				if cfg.CloudWatch.Region != "" {
					opts = append(opts, awsconfig.WithRegion(cfg.CloudWatch.Region))
				}
				awsCfg, err := awsconfig.LoadDefaultConfig(context.Background(), opts...)
				if err != nil {
					return nil, err
				}
				sources[q.source()] = NewCloudWatch(awsCfg)
			case *PrometheusQuery:
				p := cfg.Prometheus
				sources[q.source()] = NewPrometheus(p.URL, envOf(p.TokenEnv), p.Username, envOf(p.PasswordEnv),
					automation.HTTPClient(time.Duration(p.Timeout)))
			}
		}
	}
	return sources, nil
}

func envOf(name string) string {
	if name == "" {
		return ""
	}
	return os.Getenv(name)
}

// Sync runs one cycle on each interval.
type Sync struct {
	cfg       *Config
	sources   map[string]Source
	publisher *publisher
	sim       *simulator
	now       func() time.Time
	phase     string
}

// New returns the automation. now is the clock, and r drives the simulator: nil seeds it from simulate.seed, or
// from the clock when the seed is 0.
func New(cfg *Config, stash Stash, sources map[string]Source, now func() time.Time, r *rand.Rand) *Sync {
	s := &Sync{cfg: cfg, sources: sources, now: now,
		publisher: newPublisher(stash, time.Duration(cfg.Interval), now)}
	if cfg.Mode == ModeSimulate {
		if r == nil {
			seed := cfg.Simulate.Seed
			if seed == 0 {
				seed = uint64(now().UnixNano()) //nolint:gosec
			}
			r = rand.New(rand.NewPCG(seed, seed)) //nolint:gosec
		}
		s.sim = newSimulator(cfg.Simulate, r)
	}
	return s
}

// Handle ignores every event. Values come from the sources on each interval.
func (s *Sync) Handle(context.Context, automation.Event) error { return nil }

// Run runs a cycle now and then on each interval, until ctx is done or a scenario that does not loop ends.
func (s *Sync) Run(ctx context.Context) {
	log := slog.Default()
	log.Info("stash sync started", "mode", s.cfg.Mode, "interval", time.Duration(s.cfg.Interval).String(),
		"entries", len(s.cfg.groups))
	t := time.NewTicker(time.Duration(s.cfg.Interval))
	defer t.Stop()
	for {
		if done := s.Cycle(ctx); done {
			log.Info("scenario finished", "scenario", s.cfg.Simulate.Scenario)
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Cycle reads every source, or steps the scenario, and writes the entries that changed. It returns true when a
// scenario that does not loop has ended.
func (s *Sync) Cycle(ctx context.Context) bool {
	log := automation.Logger(ctx)
	// Reads and writes each get their own deadline, so slow queries do not use up the time for writes.
	budget := max(time.Duration(s.cfg.Interval), queryTimeout+10*time.Second)

	var entries []Entry
	if s.sim != nil {
		phase, simEntries, done := s.sim.step(s.now())
		if done {
			return true
		}
		if phase != s.phase {
			log.Info("scenario phase", "scenario", s.cfg.Simulate.Scenario, "phase", phase)
			s.phase = phase
		}
		entries = simEntries
	} else {
		rctx, cancel := context.WithTimeout(ctx, budget)
		entries = readings(rctx, s.cfg.groups, s.sources)
		cancel()
	}

	wctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	st := s.publisher.publish(wctx, entries)
	level := slog.LevelDebug
	if st.written > 0 || st.failed > 0 || st.invalid > 0 {
		level = slog.LevelInfo
	}
	log.Log(ctx, level, "cycle finished", "entries", len(entries), "written", st.written,
		"unchanged", st.unchanged, "invalid", st.invalid, "failed", st.failed)
	return false
}
