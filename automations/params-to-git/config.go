package paramstogit

import (
	"errors"
	"fmt"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

// Strategies, sensitive modes, decommission modes, and commit authors.
const (
	StrategyPush        = "push"
	StrategyPullRequest = "pull_request"
	StrategyStdout      = "stdout"
	StrategyDirectory   = "directory"

	SensitiveMask = "mask"
	SensitiveOmit = "omit"

	DecommissionDelete = "delete"
	DecommissionKeep   = "keep"

	AuthorActor = "actor"
	AuthorBot   = "bot"
)

const (
	defaultMessage  = `\(.data.attributes["md-instance"]): \(.data.metadata.deployment_action | ascii_downcase) deployment \(.data.entity.id)`
	defaultPRBranch = `massdriver/params/\(.data.attributes["md-project"])-\(.data.attributes["md-environment"])`
	defaultPRTitle  = `Massdriver params: \(.data.attributes["md-project"])-\(.data.attributes["md-environment"])`
	defaultEmail    = "noreply@massdriver.cloud"
)

// Config is the config file of params-to-git.
type Config struct {
	GitHub   GitHubConfig `yaml:"github"`
	Debug    DebugConfig  `yaml:"debug"`
	Defaults Target       `yaml:"defaults"`
	Mappings []Mapping    `yaml:"mappings"`

	routes       []*route
	defaultRoute *route
}

// GitHubConfig is the GitHub connection.
type GitHubConfig struct {
	APIURL   string `yaml:"api_url"`
	TokenEnv string `yaml:"token_env"`

	token string
}

// DebugConfig is where the directory strategy writes.
type DebugConfig struct {
	Directory string `yaml:"directory"`
}

// Mapping sends the instances whose deployment event has every attribute of Match to its own target. Each field of
// the target that the mapping leaves out comes from defaults.
type Mapping struct {
	Name   string            `yaml:"name"`
	Match  map[string]Values `yaml:"match"`
	Target `yaml:",inline"`
}

// Target is where and how one instance's params.json is written.
type Target struct {
	Repo           string      `yaml:"repo"`
	Branch         string      `yaml:"branch"`
	Root           string      `yaml:"root"`
	Strategy       string      `yaml:"strategy"`
	Sensitive      string      `yaml:"sensitive"`
	OnDecommission string      `yaml:"on_decommission"`
	Skip           *bool       `yaml:"skip"`
	Commit         Commit      `yaml:"commit"`
	PullRequest    PullRequest `yaml:"pull_request"`
}

// Commit sets the author and the message of each commit.
type Commit struct {
	Author  string `yaml:"author"`
	Email   string `yaml:"email"`
	Message string `yaml:"message"`
}

// PullRequest sets the branch, title, and reviewers of the pull_request strategy.
type PullRequest struct {
	Branch    string   `yaml:"branch"`
	Title     string   `yaml:"title"`
	Rolling   *bool    `yaml:"rolling"`
	Reviewers []string `yaml:"reviewers"`
}

// Values reads a YAML scalar or a list of scalars. Unquoted true and 1 read as the strings "true" and "1".
type Values []string

// UnmarshalYAML reads a scalar or a sequence of scalars.
func (v *Values) UnmarshalYAML(n *yaml.Node) error {
	switch n.Kind {
	case yaml.ScalarNode:
		*v = Values{n.Value}
		return nil
	case yaml.SequenceNode:
		out := make(Values, 0, len(n.Content))
		for _, item := range n.Content {
			if item.Kind != yaml.ScalarNode {
				return fmt.Errorf("line %d: want a value or a list of values", item.Line)
			}
			out = append(out, item.Value)
		}
		*v = out
		return nil
	default:
		return fmt.Errorf("line %d: want a value or a list of values", n.Line)
	}
}

// DefaultConfig uses github.com and GITHUB_TOKEN.
func DefaultConfig() Config {
	// TokenEnv names the variable that holds the token.
	return Config{GitHub: GitHubConfig{APIURL: "https://api.github.com", TokenEnv: "GITHUB_TOKEN"}} //nolint:gosec
}

// route is a target with every default filled and its expressions compiled.
type route struct {
	name           string
	match          map[string]Values
	skip           bool
	repo           string
	branch         string
	root           string
	strategy       string
	sensitive      string
	onDecommission string
	author         string
	email          string
	message        *text
	prBranch       *text
	prTitle        *text
	rolling        bool
	users          []string
	teams          []string
}

var (
	repoPattern     = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
	reviewerPattern = regexp.MustCompile(`^[A-Za-z0-9-]+(/[A-Za-z0-9_.-]+)?$`)
)

// Validate fills the defaults of every mapping and checks them.
func (c *Config) Validate() error {
	var errs []error
	fail := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }

	c.GitHub.APIURL = strings.TrimRight(c.GitHub.APIURL, "/")
	if c.GitHub.APIURL == "" {
		fail("github.api_url must not be empty")
	}
	if c.GitHub.TokenEnv == "" {
		fail("github.token_env must not be empty")
	}

	def, problems := build("defaults", nil, Target{}, c.Defaults)
	for _, p := range problems {
		fail("defaults%s", p)
	}
	c.defaultRoute = def

	names := map[string]bool{}
	c.routes = nil
	for i, m := range c.Mappings {
		if m.Name == "" {
			fail("mappings[%d].name is required", i)
		}
		if names[m.Name] {
			fail("mappings[%d].name is used by an earlier mapping", i)
		}
		names[m.Name] = true
		if len(m.Match) == 0 {
			fail("mappings[%d].match needs at least one attribute", i)
		}
		for k, v := range m.Match {
			if len(v) == 0 {
				fail("mappings[%d].match.%s has no values", i, k)
			}
		}
		r, problems := build(m.Name, m.Match, c.Defaults, m.Target)
		for _, p := range problems {
			fail("mappings[%d]%s", i, p)
		}
		c.routes = append(c.routes, r)
	}

	if c.uses(StrategyDirectory) {
		if c.Debug.Directory == "" {
			fail("debug.directory is required by the directory strategy")
		} else {
			c.Debug.Directory = filepath.Clean(c.Debug.Directory)
		}
	}
	return errors.Join(errs...)
}

// uses reports whether the defaults or a mapping that is not skipped sends instances by one of strategies.
func (c *Config) uses(strategies ...string) bool {
	for _, r := range append([]*route{c.defaultRoute}, c.routes...) {
		if r != nil && !r.skip && slices.Contains(strategies, r.strategy) {
			return true
		}
	}
	return false
}

// usesGitHub reports whether any instance can be sent to GitHub, which needs the token.
func (c *Config) usesGitHub() bool {
	return c.uses(StrategyPush, StrategyPullRequest)
}

// build lays t over base, fills what both leave out, and checks the result. Each problem starts with the field's
// path from the target. A skipped target needs nothing else.
func build(name string, match map[string]Values, base, t Target) (*route, []string) {
	pick := func(v, b, d string) string {
		if v != "" {
			return v
		}
		if b != "" {
			return b
		}
		return d
	}
	r := &route{
		name:           name,
		match:          match,
		repo:           pick(t.Repo, base.Repo, ""),
		branch:         pick(t.Branch, base.Branch, "main"),
		root:           pick(t.Root, base.Root, "/"),
		strategy:       pick(t.Strategy, base.Strategy, StrategyPush),
		sensitive:      pick(t.Sensitive, base.Sensitive, SensitiveMask),
		onDecommission: pick(t.OnDecommission, base.OnDecommission, DecommissionDelete),
		author:         pick(t.Commit.Author, base.Commit.Author, AuthorActor),
		email:          pick(t.Commit.Email, base.Commit.Email, defaultEmail),
		rolling:        true,
	}
	switch {
	case t.Skip != nil:
		r.skip = *t.Skip
	case base.Skip != nil:
		r.skip = *base.Skip
	}
	if r.skip {
		return r, nil
	}
	switch {
	case t.PullRequest.Rolling != nil:
		r.rolling = *t.PullRequest.Rolling
	case base.PullRequest.Rolling != nil:
		r.rolling = *base.PullRequest.Rolling
	}
	reviewers := base.PullRequest.Reviewers
	if t.PullRequest.Reviewers != nil {
		reviewers = t.PullRequest.Reviewers
	}

	var problems []string
	fail := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }

	toGitHub := r.strategy == StrategyPush || r.strategy == StrategyPullRequest
	// stdout and directory write nowhere on GitHub, so they print a repository only when one is set.
	if (toGitHub || r.repo != "") && !repoPattern.MatchString(r.repo) {
		fail(".repo must be owner/name")
	}
	if strings.HasPrefix(r.branch, "/") || strings.Contains(r.branch, "..") {
		fail(".branch is not a branch name")
	}
	root, err := cleanRoot(r.root)
	if err != nil {
		fail(".root %s", err)
	}
	r.root = root
	if !toGitHub && r.strategy != StrategyStdout && r.strategy != StrategyDirectory {
		fail(".strategy must be push, pull_request, stdout, or directory")
	}
	if r.sensitive != SensitiveMask && r.sensitive != SensitiveOmit {
		fail(".sensitive must be mask or omit")
	}
	if r.onDecommission != DecommissionDelete && r.onDecommission != DecommissionKeep {
		fail(".on_decommission must be delete or keep")
	}
	if r.author != AuthorActor && r.author != AuthorBot {
		fail(".commit.author must be actor or bot")
	}
	if !strings.Contains(r.email, "@") {
		fail(".commit.email must be an email address")
	}
	if r.message, err = compileText("commit.message", pick(t.Commit.Message, base.Commit.Message, defaultMessage)); err != nil {
		fail(".%s", err)
	}
	if r.prBranch, err = compileText("pull_request.branch", pick(t.PullRequest.Branch, base.PullRequest.Branch, defaultPRBranch)); err != nil {
		fail(".%s", err)
	}
	if r.prTitle, err = compileText("pull_request.title", pick(t.PullRequest.Title, base.PullRequest.Title, defaultPRTitle)); err != nil {
		fail(".%s", err)
	}
	for i, rv := range reviewers {
		if !reviewerPattern.MatchString(rv) {
			fail(".pull_request.reviewers[%d] must be a login or org/team", i)
			continue
		}
		if _, team, ok := strings.Cut(rv, "/"); ok {
			r.teams = append(r.teams, team)
		} else {
			r.users = append(r.users, rv)
		}
	}
	return r, problems
}

// cleanRoot turns root into a repository path without leading or trailing slashes. "/" is the top of the repository.
func cleanRoot(root string) (string, error) {
	p := strings.Trim(path.Clean("/"+root), "/")
	if slices.Contains(strings.Split(root, "/"), "..") {
		return "", errors.New("must not have a .. segment")
	}
	return p, nil
}

// routeFor returns the first mapping whose match the attributes have, or the defaults.
func (c *Config) routeFor(attrs map[string]any) *route {
	for _, r := range c.routes {
		if matches(r.match, attrs) {
			return r
		}
	}
	return c.defaultRoute
}

// matches reports whether every key of want is in attrs with one of its values. A missing key never matches.
func matches(want map[string]Values, attrs map[string]any) bool {
	for k, vals := range want {
		v, ok := attrs[k]
		if !ok || v == nil {
			return false
		}
		if !slices.Contains(vals, fmt.Sprint(v)) {
			return false
		}
	}
	return true
}
