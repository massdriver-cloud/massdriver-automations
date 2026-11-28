// Package leaderboard ranks the authors of GitHub pull requests and writes the top players to the leaderboard
// widget of Massdriver environments on an interval. A route can also write a fixed list, to try the widget out.
package leaderboard

import (
	"cmp"
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/massdriver-cloud/massdriver-automations/pkg/automation"
	"github.com/massdriver-cloud/massdriver-automations/pkg/massdriver"
	"github.com/massdriver-cloud/massdriver-automations/pkg/people"
)

const automationName = "leaderboard"

// Key is the stash key of the environment leaderboard widget.
const Key = "md/v1/leaderboard"

const (
	// maxParallelQueries keeps a cycle well under GitHub's secondary rate limits.
	maxParallelQueries = 4
	queryTimeout       = time.Minute
)

// Definition registers the automation. It works on an interval. The event destination, which every automation
// has, sends it nothing: its condition is false.
var Definition = automation.Definition{
	Name:        automationName,
	Description: "Ranks the authors of GitHub pull requests and writes the top players to environment leaderboards.",
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
		var gh GitHub
		if cfg.usesGitHub() {
			v, err := automation.RequireEnv(cfg.GitHub.TokenEnv)
			if err != nil {
				return nil, err
			}
			gh = &GraphQLGitHub{URL: cfg.GitHub.GraphQLURL, Token: v[cfg.GitHub.TokenEnv], HTTP: automation.HTTPClient(0)}
		}
		l := New(&cfg, gh, api, time.Now)
		go l.Run(context.Background())
		return l, nil
	},
}

// Leaderboard polls GitHub and writes each route's leaderboard.
type Leaderboard struct {
	cfg *Config
	gh  GitHub
	out *people.Writer
	now func() time.Time
}

// New returns the automation. gh can be nil when no route reads pull requests. now is the clock.
func New(cfg *Config, gh GitHub, stash people.Stash, now func() time.Time) *Leaderboard {
	return &Leaderboard{cfg: cfg, gh: gh, now: now,
		out: people.NewWriter(stash, Key, cfg.Label, time.Duration(cfg.Interval))}
}

// Handle ignores every event. Players come from GitHub on each interval.
func (l *Leaderboard) Handle(context.Context, automation.Event) error { return nil }

// Run runs a cycle now and then on each interval, until ctx is done.
func (l *Leaderboard) Run(ctx context.Context) {
	slog.Default().Info("leaderboard started", "interval", time.Duration(l.cfg.Interval).String(),
		"routes", len(l.cfg.Routes))
	t := time.NewTicker(time.Duration(l.cfg.Interval))
	defer t.Stop()
	for {
		l.Cycle(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Cycle runs each distinct GitHub query once, then writes the leaderboards that changed.
func (l *Leaderboard) Cycle(ctx context.Context) {
	log := automation.Logger(ctx)
	now := l.now().UTC().Truncate(time.Second)
	budget := max(time.Duration(l.cfg.Interval), queryTimeout+10*time.Second)
	rctx, cancel := context.WithTimeout(ctx, budget)
	results := l.fetch(rctx, now)
	cancel()

	wctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	var written, unchanged, skipped, failed int
	for i, r := range l.cfg.Routes {
		entries, ok := r.Static, true
		if r.PullRequests != nil {
			entries, ok = l.players(r, now, results)
		}
		// A partial ranking would move players down for a repository that failed. The last entry stays until its
		// TTL instead.
		if !ok {
			skipped += len(r.Environments)
			log.Warn("leaderboard skipped", "route", i, "reason", "repo_query_failed")
			continue
		}
		list := people.List{Title: r.Title, Entries: entries}
		for _, env := range r.Environments {
			attrs := []any{"route", i, "environment", env}
			did, err := l.out.Write(wctx, now, env, list)
			switch {
			case errors.Is(err, people.ErrEmpty):
				skipped++
				log.Debug("leaderboard skipped", append(attrs, "reason", "no_players")...)
			case err != nil:
				failed++
				log.Error("leaderboard write failed", append(attrs, "err", err)...)
			case did:
				written++
				log.Debug("leaderboard set", append(attrs, "players", len(entries))...)
			default:
				unchanged++
			}
		}
	}
	level := slog.LevelDebug
	if written > 0 || failed > 0 {
		level = slog.LevelInfo
	}
	log.Log(ctx, level, "cycle finished", "routes", len(l.cfg.Routes), "written", written,
		"unchanged", unchanged, "skipped", skipped, "failed", failed)
}

type queryResult struct {
	prs []PullRequest
	err error
}

func (p *PullRequests) query(repo string, now time.Time) Query {
	q := Query{Repo: repo, Base: p.Base, Files: len(p.Paths) > 0, Since: now.Add(-time.Duration(p.Window))}
	if p.Score != ScoreOpened {
		q.States = []string{"MERGED"}
	}
	return q
}

// fetch runs each distinct query once, even when several routes need it.
func (l *Leaderboard) fetch(ctx context.Context, now time.Time) map[string]queryResult {
	log := automation.Logger(ctx)
	queries := map[string]Query{}
	for _, r := range l.cfg.Routes {
		if r.PullRequests == nil {
			continue
		}
		for _, repo := range r.PullRequests.Repos {
			q := r.PullRequests.query(repo, now)
			queries[q.key()] = q
		}
	}
	results := make(map[string]queryResult, len(queries))
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, maxParallelQueries)
	for k, q := range queries {
		wg.Go(func() {
			sem <- struct{}{}
			defer func() { <-sem }()
			qctx, cancel := context.WithTimeout(ctx, queryTimeout)
			defer cancel()
			prs, err := l.gh.PullRequests(qctx, q)
			if err != nil {
				log.Error("github query failed", "repo", q.Repo, "err", err)
			}
			mu.Lock()
			results[k] = queryResult{prs: prs, err: err}
			mu.Unlock()
		})
	}
	wg.Wait()
	return results
}

type player struct {
	author Author
	score  int
}

// players scores the route's pull requests by author, and returns the best first. It returns false when a
// repository failed.
func (l *Leaderboard) players(r Route, now time.Time, results map[string]queryResult) ([]people.Person, bool) {
	p := r.PullRequests
	since := now.Add(-time.Duration(p.Window))
	excluded := map[string]bool{}
	for _, a := range p.ExcludeAuthors {
		excluded[strings.ToLower(a)] = true
	}
	byLogin := map[string]*player{}
	for _, repo := range p.Repos {
		res := results[p.query(repo, now).key()]
		if res.err != nil {
			return nil, false
		}
		for _, pr := range res.prs {
			a := pr.Author
			if a == nil || excluded[strings.ToLower(a.Login)] || (a.Type == "Bot" && !p.IncludeBots) {
				continue
			}
			points := p.points(pr, since)
			if points == 0 {
				continue
			}
			pl := byLogin[a.Login]
			if pl == nil {
				pl = &player{author: *a}
				byLogin[a.Login] = pl
			}
			if pl.author.Name == "" {
				pl.author.Name = a.Name
			}
			pl.score += points
		}
	}
	ranked := make([]*player, 0, len(byLogin))
	for _, pl := range byLogin {
		ranked = append(ranked, pl)
	}
	slices.SortFunc(ranked, func(a, b *player) int {
		if c := cmp.Compare(b.score, a.score); c != 0 {
			return c
		}
		return cmp.Compare(strings.ToLower(a.author.Login), strings.ToLower(b.author.Login))
	})
	out := make([]people.Person, 0, r.MaxPlayers)
	for _, pl := range ranked {
		if len(out) == r.MaxPlayers {
			break
		}
		name := pl.author.Name
		if strings.TrimSpace(name) == "" {
			name = pl.author.Login
		}
		person, ok := people.Clean(people.Person{
			Name: name, ImageURL: pl.author.AvatarURL, Handles: []string{"github:" + pl.author.Login},
		})
		if ok {
			out = append(out, person)
		}
	}
	return out, true
}

// points is what one pull request adds to its author's score: 0 when it is outside the window or the filters.
func (p *PullRequests) points(pr PullRequest, since time.Time) int {
	if len(p.Labels) > 0 && !labeled(pr, p.Labels) {
		return 0
	}
	if len(p.Paths) > 0 && !touches(pr, p.Paths) {
		return 0
	}
	switch p.Score {
	case ScoreOpened:
		if pr.CreatedAt.Before(since) {
			return 0
		}
		return 1
	case ScoreLines:
		if pr.MergedAt == nil || pr.MergedAt.Before(since) {
			return 0
		}
		return pr.Additions + pr.Deletions
	default:
		if pr.MergedAt == nil || pr.MergedAt.Before(since) {
			return 0
		}
		return 1
	}
}

// touches reports whether the pull request changes a file under one of the prefixes. GitHub returns the first 100
// files of a pull request.
func touches(pr PullRequest, prefixes []string) bool {
	if pr.Files == nil {
		return false
	}
	for _, f := range pr.Files.Nodes {
		for _, prefix := range prefixes {
			if strings.HasPrefix(f.Path, prefix) {
				return true
			}
		}
	}
	return false
}

// labeled reports whether the pull request has any of the labels. The filter runs here, not in the query, so
// that the rule is any of them whatever GitHub's labels argument does with several. GitHub label names ignore
// case: a repository can't have both Fix and fix.
func labeled(pr PullRequest, labels []string) bool {
	for _, l := range pr.Labels.Nodes {
		if slices.ContainsFunc(labels, func(want string) bool { return strings.EqualFold(want, l.Name) }) {
			return true
		}
	}
	return false
}
