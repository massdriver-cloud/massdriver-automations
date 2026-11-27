// Package prsync writes the pull requests of a project's GitHub repositories to one JSON entry in the project's
// stash on an interval, so a note on the project can list them with their review, check, and merge state.
package prsync

import (
	"cmp"
	"context"
	"encoding/json"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/massdriver-cloud/massdriver-automations/pkg/automation"
	"github.com/massdriver-cloud/massdriver-automations/pkg/massdriver"
)

const automationName = "pr-sync"

const (
	// maxParallelQueries keeps a cycle well under GitHub's secondary rate limits.
	maxParallelQueries = 4
	queryTimeout       = 30 * time.Second
)

// Definition registers the automation. It works on an interval. The event destination, which every automation
// has, sends it nothing: its condition is false.
var Definition = automation.Definition{
	Name:        automationName,
	Description: "Writes the pull requests of a project's GitHub repositories to the project's stash, for notes.",
	Events:      []string{"deployment.completed"},
	New: func() (automation.Automation, error) {
		var cfg Config
		if err := automation.LoadConfig(&cfg); err != nil {
			return nil, err
		}
		v, err := automation.RequireEnv(cfg.GitHub.TokenEnv)
		if err != nil {
			return nil, err
		}
		api, err := massdriver.FromEnv()
		if err != nil {
			return nil, err
		}
		gh := &GraphQLGitHub{URL: cfg.GitHub.GraphQLURL, Token: v[cfg.GitHub.TokenEnv], HTTP: automation.HTTPClient(0)}
		s := New(&cfg, gh, &GraphQLStash{API: api}, time.Now)
		go s.Run(context.Background())
		return s, nil
	},
}

// States of a pull request in the stash entry.
const (
	StateMerging  = "merging"
	StateChecks   = "checks"
	StateFailed   = "failed"
	StateApproved = "approved"
	StateReview   = "review"
	StateDraft    = "draft"
	StateMerged   = "merged"
)

// rank orders the entry: pull requests closest to merging first, then merged ones.
var rank = map[string]int{
	StateMerging: 0, StateChecks: 1, StateFailed: 2, StateApproved: 3, StateReview: 4, StateDraft: 5, StateMerged: 6,
}

// PR is one pull request in the stash entry.
type PR struct {
	Repo      string     `json:"repo"`
	Number    int        `json:"number"`
	Title     string     `json:"title"`
	Author    string     `json:"author"`
	URL       string     `json:"url"`
	State     string     `json:"state"`
	Draft     bool       `json:"draft"`
	Review    string     `json:"review"`
	Checks    string     `json:"checks"`
	UpdatedAt time.Time  `json:"updated_at"`
	MergedAt  *time.Time `json:"merged_at"`
}

// Value is the JSON value of the stash entry.
type Value struct {
	PRs       []PR      `json:"prs"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Sync polls GitHub and writes each project's entry.
type Sync struct {
	cfg   *Config
	gh    GitHub
	stash Stash
	now   func() time.Time
	last  map[string]sent
}

type sent struct {
	prs string
	at  time.Time
}

// New returns the automation. now is the clock.
func New(cfg *Config, gh GitHub, stash Stash, now func() time.Time) *Sync {
	return &Sync{cfg: cfg, gh: gh, stash: stash, now: now, last: map[string]sent{}}
}

// Handle ignores every event. Pull requests come from GitHub on each interval.
func (s *Sync) Handle(context.Context, automation.Event) error { return nil }

// Run runs a cycle now and then on each interval, until ctx is done.
func (s *Sync) Run(ctx context.Context) {
	slog.Default().Info("pr sync started", "interval", time.Duration(s.cfg.Interval).String(),
		"projects", len(s.cfg.Projects))
	t := time.NewTicker(time.Duration(s.cfg.Interval))
	defer t.Stop()
	for {
		s.Cycle(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// ttl is three intervals: when the automation stops, its entries expire after three missed cycles.
func (s *Sync) ttl() time.Duration { return 3 * time.Duration(s.cfg.Interval) }

// refreshAfter is when an unchanged entry is written again, so it never reaches its TTL while the automation runs.
func (s *Sync) refreshAfter() time.Duration { return time.Duration(s.cfg.Interval) * 3 / 2 }

// Cycle reads each repository once, and writes the entries of the projects whose pull requests changed.
func (s *Sync) Cycle(ctx context.Context) {
	log := automation.Logger(ctx)
	budget := max(time.Duration(s.cfg.Interval), queryTimeout+10*time.Second)
	rctx, cancel := context.WithTimeout(ctx, budget)
	results := s.fetch(rctx)
	cancel()

	wctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	now := s.now().UTC().Truncate(time.Second)
	var written, unchanged, skipped, failed int
	for _, p := range s.cfg.Projects {
		attrs := []any{"project", p.Project, "key", s.cfg.Key}
		var prs []repoPR
		ok := true
		for _, repo := range p.Repos {
			r := results[repo]
			if r.err != nil {
				ok = false
				break
			}
			for _, pr := range r.prs {
				prs = append(prs, repoPR{Repo: repo, PullRequest: pr})
			}
		}
		// A partial list would show the pull requests of a failed repository as gone. The last entry stays until
		// its TTL instead.
		if !ok {
			skipped++
			log.Warn("stash entry skipped", append(attrs, "reason", "repo_query_failed")...)
			continue
		}

		list := build(prs, now, time.Duration(*s.cfg.MergedWindow), s.cfg.MaxPRs)
		listJSON, err := json.Marshal(list)
		if err != nil {
			failed++
			log.Error("stash entry encode failed", append(attrs, "err", err)...)
			continue
		}
		if last, ok := s.last[p.Project]; ok && last.prs == string(listJSON) && now.Sub(last.at) < s.refreshAfter() {
			unchanged++
			continue
		}
		value, err := json.Marshal(Value{PRs: list, UpdatedAt: now})
		if err != nil {
			failed++
			log.Error("stash entry encode failed", append(attrs, "err", err)...)
			continue
		}
		w := Write{Project: p.Project, Key: s.cfg.Key, Label: s.cfg.Label, Value: string(value), TTL: s.ttl()}
		if err := s.stash.Set(wctx, w); err != nil {
			failed++
			log.Error("stash entry write failed", append(attrs, "err", err)...)
			continue
		}
		written++
		s.last[p.Project] = sent{prs: string(listJSON), at: now}
		log.Debug("stash entry set", append(attrs, "prs", len(list))...)
	}
	level := slog.LevelDebug
	if written > 0 || failed > 0 || skipped > 0 {
		level = slog.LevelInfo
	}
	log.Log(ctx, level, "cycle finished", "projects", len(s.cfg.Projects), "written", written,
		"unchanged", unchanged, "skipped", skipped, "failed", failed)
}

type repoResult struct {
	prs []PullRequest
	err error
}

// fetch queries each repository once, even when several projects list it.
func (s *Sync) fetch(ctx context.Context) map[string]repoResult {
	log := automation.Logger(ctx)
	var repos []string
	for _, p := range s.cfg.Projects {
		repos = append(repos, p.Repos...)
	}
	slices.Sort(repos)
	repos = slices.Compact(repos)

	results := make(map[string]repoResult, len(repos))
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, maxParallelQueries)
	for _, repo := range repos {
		wg.Go(func() {
			sem <- struct{}{}
			defer func() { <-sem }()
			qctx, cancel := context.WithTimeout(ctx, queryTimeout)
			defer cancel()
			prs, err := s.gh.PullRequests(qctx, repo)
			if err != nil {
				log.Error("github query failed", "repo", repo, "err", err)
			}
			mu.Lock()
			results[repo] = repoResult{prs: prs, err: err}
			mu.Unlock()
		})
	}
	wg.Wait()
	return results
}

// repoPR is a pull request and the repository it came from, as owner/name.
type repoPR struct {
	Repo string
	PullRequest
}

// build turns pull requests into the entry's list: open ones by state and then by last update, then the ones
// merged within window, newest first, up to limit.
func build(in []repoPR, now time.Time, window time.Duration, limit int) []PR {
	out := make([]PR, 0, len(in))
	for _, p := range in {
		if p.MergedAt != nil && now.Sub(*p.MergedAt) > window {
			continue
		}
		author := "ghost"
		if p.Author != nil {
			author = p.Author.Login
		}
		_, short, _ := strings.Cut(p.Repo, "/")
		out = append(out, PR{
			Repo:      short,
			Number:    p.Number,
			Title:     p.Title,
			Author:    author,
			URL:       p.URL,
			State:     state(p.PullRequest),
			Draft:     p.IsDraft,
			Review:    lowerOr(p.ReviewDecision, "none"),
			Checks:    lowerOr(p.checks(), "none"),
			UpdatedAt: p.UpdatedAt.UTC(),
			MergedAt:  utc(p.MergedAt),
		})
	}
	slices.SortFunc(out, func(a, b PR) int {
		if c := cmp.Compare(rank[a.State], rank[b.State]); c != 0 {
			return c
		}
		if a.State == StateMerged {
			if c := b.MergedAt.Compare(*a.MergedAt); c != 0 {
				return c
			}
		} else if c := b.UpdatedAt.Compare(a.UpdatedAt); c != 0 {
			return c
		}
		if c := cmp.Compare(a.Repo, b.Repo); c != 0 {
			return c
		}
		return cmp.Compare(a.Number, b.Number)
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

// state is the one word a note shows. A pull request in the merge queue is merging even while its checks run.
// Failing checks or requested changes block auto-merge, so they come before it.
func state(p PullRequest) string {
	checks := p.checks()
	switch {
	case p.MergedAt != nil:
		return StateMerged
	case p.IsDraft:
		return StateDraft
	case p.IsInMergeQueue:
		return StateMerging
	case p.ReviewDecision == "CHANGES_REQUESTED", checks == "FAILURE", checks == "ERROR":
		return StateFailed
	case p.AutoMergeRequest != nil:
		return StateMerging
	case checks == "PENDING", checks == "EXPECTED":
		return StateChecks
	case p.ReviewDecision == "APPROVED":
		return StateApproved
	default:
		return StateReview
	}
}

func lowerOr(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return strings.ToLower(s)
}

func utc(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}
