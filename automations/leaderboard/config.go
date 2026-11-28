package leaderboard

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/massdriver-cloud/massdriver-automations/pkg/people"
	"gopkg.in/yaml.v3"
)

const minInterval = time.Minute

// Scores. Each counts only pull requests inside the route's window.
const (
	ScoreMerged = "merged" // merged pull requests
	ScoreOpened = "opened" // opened pull requests, in any state
	ScoreLines  = "lines"  // lines added and deleted by merged pull requests
)

// Config is the config file of leaderboard.
type Config struct {
	Interval Duration     `yaml:"interval"`
	Label    string       `yaml:"label"`
	GitHub   GitHubConfig `yaml:"github"`
	Routes   []Route      `yaml:"routes"`
}

// GitHubConfig is the GitHub GraphQL endpoint and the variable that holds the token.
type GitHubConfig struct {
	GraphQLURL string `yaml:"graphql_url"`
	TokenEnv   string `yaml:"token_env"`
}

// Route writes one leaderboard to one or more environments. Its players come from GitHub pull requests or from a
// fixed list.
type Route struct {
	Environments []string        `yaml:"environments"`
	Title        string          `yaml:"title"`
	MaxPlayers   int             `yaml:"max_players"`
	PullRequests *PullRequests   `yaml:"pull_requests"`
	Static       []people.Person `yaml:"static"`
}

// PullRequests selects the pull requests that score, and how they score.
type PullRequests struct {
	Repos []string `yaml:"repos"`
	// Base is the branch the pull requests target. Empty is any branch.
	Base string `yaml:"base"`
	// Labels keeps the pull requests that have any of them. Empty keeps all.
	Labels []string `yaml:"labels"`
	// Paths keeps the pull requests that change a file under any of these prefixes. Empty keeps all.
	Paths          []string `yaml:"paths"`
	Window         Duration `yaml:"window"`
	Score          string   `yaml:"score"`
	ExcludeAuthors []string `yaml:"exclude_authors"`
	IncludeBots    bool     `yaml:"include_bots"`
}

// Duration is a Go duration, such as 90s or 24h.
type Duration time.Duration

// UnmarshalYAML reads a duration string.
func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	v, err := time.ParseDuration(n.Value)
	if err != nil || v < 0 {
		return fmt.Errorf("line %d: a duration must look like 90s or 24h", n.Line)
	}
	*d = Duration(v)
	return nil
}

// GitHub owner and repository names: letters, digits, hyphens, underscores, and dots.
var repoPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

// usesGitHub reports whether a route reads pull requests, so a config with only static routes needs no token.
func (c *Config) usesGitHub() bool {
	for _, r := range c.Routes {
		if r.PullRequests != nil {
			return true
		}
	}
	return false
}

// Validate fills the defaults and checks the config. The bundles write a setting left empty as null, which
// decodes to the zero value, so a zero value takes the default too.
func (c *Config) Validate() error {
	var errs []error
	fail := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }

	if c.Interval == 0 {
		c.Interval = Duration(15 * time.Minute)
	}
	if c.Label == "" {
		c.Label = "Leaderboard"
	}
	if c.GitHub.GraphQLURL == "" {
		c.GitHub.GraphQLURL = "https://api.github.com/graphql"
	}
	if c.GitHub.TokenEnv == "" {
		c.GitHub.TokenEnv = "GITHUB_TOKEN" //nolint:gosec // The name of the variable, not a token.
	}
	c.GitHub.GraphQLURL = strings.TrimRight(c.GitHub.GraphQLURL, "/")

	if time.Duration(c.Interval) < minInterval {
		fail("interval must be at least %s", minInterval)
	}
	if len(c.Routes) == 0 {
		fail("add at least one route")
	}
	seen := map[string]bool{}
	for i := range c.Routes {
		r := &c.Routes[i]
		at := fmt.Sprintf("routes[%d]", i)
		if len(r.Environments) == 0 {
			fail("%s.environments must name at least one environment", at)
		}
		for j, env := range r.Environments {
			if !people.EnvironmentPattern.MatchString(env) {
				fail("%s.environments[%d] must be an environment identifier, such as billing-prod", at, j)
				continue
			}
			if seen[env] {
				fail("%s.environments[%d] is in more than one route", at, j)
			}
			seen[env] = true
		}
		if err := people.ValidateTitle(r.Title); err != nil {
			fail("%s.%v", at, err)
		}
		if r.MaxPlayers == 0 {
			r.MaxPlayers = 10
		}
		if r.MaxPlayers < 1 || r.MaxPlayers > people.MaxEntries {
			fail("%s.max_players must be from 1 to %d", at, people.MaxEntries)
		}
		switch {
		case r.PullRequests != nil && r.Static != nil, r.PullRequests == nil && r.Static == nil:
			fail("%s must set exactly one of pull_requests or static", at)
		case r.PullRequests != nil:
			errs = append(errs, r.PullRequests.validate(at+".pull_requests"))
		default:
			errs = append(errs, validateStatic(at+".static", r.Static))
		}
	}
	return errors.Join(errs...)
}

func (p *PullRequests) validate(at string) error {
	var errs []error
	fail := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }
	if p.Window == 0 {
		p.Window = Duration(30 * 24 * time.Hour)
	}
	if p.Score == "" {
		p.Score = ScoreMerged
	}
	if len(p.Repos) == 0 {
		fail("%s.repos must name at least one repository", at)
	}
	for j, r := range p.Repos {
		if !repoPattern.MatchString(r) {
			fail("%s.repos[%d] must look like owner/name", at, j)
		}
	}
	if p.Score != ScoreMerged && p.Score != ScoreOpened && p.Score != ScoreLines {
		fail("%s.score must be %s, %s, or %s", at, ScoreMerged, ScoreOpened, ScoreLines)
	}
	if time.Duration(p.Window) < time.Hour {
		fail("%s.window must be at least 1h", at)
	}
	for j, l := range p.Labels {
		if strings.TrimSpace(l) == "" {
			fail("%s.labels[%d] must not be empty", at, j)
		}
	}
	for j, path := range p.Paths {
		if strings.TrimSpace(path) == "" {
			fail("%s.paths[%d] must not be empty", at, j)
		}
	}
	return errors.Join(errs...)
}

func validateStatic(at string, list []people.Person) error {
	if len(list) < 1 || len(list) > people.MaxEntries {
		return fmt.Errorf("%s must have 1 to %d people", at, people.MaxEntries)
	}
	var errs []error
	for j, p := range list {
		if err := p.Validate(); err != nil {
			errs = append(errs, fmt.Errorf("%s[%d]: %w", at, j, err))
		}
	}
	return errors.Join(errs...)
}
