package prsync

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	// maxPRsLimit is also the page size of each GitHub query, so one repository can fill a whole entry.
	maxPRsLimit = 50
	minInterval = 30 * time.Second
)

// Config is the config file of pr-sync.
type Config struct {
	Interval Duration `yaml:"interval"`
	Key      string   `yaml:"key"`
	Label    string   `yaml:"label"`
	// MergedWindow is a pointer so that an explicit 0s, which lists only open pull requests, differs from unset.
	MergedWindow *Duration    `yaml:"merged_window"`
	MaxPRs       int          `yaml:"max_prs"`
	GitHub       GitHubConfig `yaml:"github"`
	Projects     []Project    `yaml:"projects"`
}

// GitHubConfig is the GitHub GraphQL endpoint and the variable that holds the token.
type GitHubConfig struct {
	GraphQLURL string `yaml:"graphql_url"`
	TokenEnv   string `yaml:"token_env"`
}

// Project maps the repositories whose pull requests go into one project's stash.
type Project struct {
	Project string   `yaml:"project"`
	Repos   []string `yaml:"repos"`
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

var (
	keyPattern     = regexp.MustCompile(`^[A-Za-z0-9_./-]{1,255}$`)
	projectPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)
	// GitHub owner and repository names: letters, digits, hyphens, underscores, and dots.
	repoPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
)

// Validate fills the defaults and checks the config. The bundles write a setting left empty as null, which
// decodes to the zero value, so a zero value takes the default too.
func (c *Config) Validate() error {
	var errs []error
	fail := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }

	if c.Interval == 0 {
		c.Interval = Duration(2 * time.Minute)
	}
	if c.Key == "" {
		c.Key = "github/prs"
	}
	if c.Label == "" {
		c.Label = "Pull requests"
	}
	if c.MergedWindow == nil {
		d := Duration(24 * time.Hour)
		c.MergedWindow = &d
	}
	if c.MaxPRs == 0 {
		c.MaxPRs = 20
	}
	if c.GitHub.GraphQLURL == "" {
		c.GitHub.GraphQLURL = "https://api.github.com/graphql"
	}
	if c.GitHub.TokenEnv == "" {
		c.GitHub.TokenEnv = "GITHUB_TOKEN" //nolint:gosec // The name of the variable, not a token.
	}

	if time.Duration(c.Interval) < minInterval {
		fail("interval must be at least %s", minInterval)
	}
	if !keyPattern.MatchString(c.Key) {
		fail("key must be 1 to 255 letters, digits, and the characters _ . / -")
	}
	if c.MaxPRs < 1 || c.MaxPRs > maxPRsLimit {
		fail("max_prs must be from 1 to %d", maxPRsLimit)
	}
	c.GitHub.GraphQLURL = strings.TrimRight(c.GitHub.GraphQLURL, "/")
	if len(c.Projects) == 0 {
		fail("add at least one project")
	}
	seen := map[string]bool{}
	for i, p := range c.Projects {
		if !projectPattern.MatchString(p.Project) {
			fail("projects[%d].project must be a project identifier, such as visits", i)
			continue
		}
		if seen[p.Project] {
			fail("projects[%d].project appears more than once", i)
		}
		seen[p.Project] = true
		if len(p.Repos) == 0 {
			fail("projects[%d].repos must name at least one repository", i)
		}
		for j, r := range p.Repos {
			if !repoPattern.MatchString(r) {
				fail("projects[%d].repos[%d] must look like owner/name", i, j)
			}
		}
	}
	return errors.Join(errs...)
}
