package scorecards

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/itchyny/gojq"
	"gopkg.in/yaml.v3"

	"github.com/massdriver-cloud/massdriver-automations/pkg/automation"
	"github.com/massdriver-cloud/massdriver-automations/pkg/massdriver"
)

const (
	defaultInterval = time.Hour
	minInterval     = time.Minute
	defaultWindow   = 30 * 24 * time.Hour
	maxChecks       = 50
	// A roll-up counts at most 10 levels, and one of them can be NoLevel.
	maxLevels = 9
)

// Inputs a check can read with `query`.
const (
	QueryInstance = "instance"
)

// Scopes a check can read a stash key from.
const (
	ScopeInstance    = "instance"
	ScopeDeployment  = "deployment"
	ScopeEnvironment = "environment"
	ScopeProject     = "project"
)

// Config is the config file of scorecards.
type Config struct {
	Interval    Duration          `yaml:"interval"`
	Delivery    WindowConfig      `yaml:"delivery"`
	Reliability WindowConfig      `yaml:"reliability"`
	Ownership   OwnershipConfig   `yaml:"ownership"`
	Parity      []ParityConfig    `yaml:"parity"`
	Scorecards  []ScorecardConfig `yaml:"scorecards"`
	// Results.Prefix replaces md/v1 in the keys that scorecards writes, until the server's widget catalog has them.
	Results massdriver.ResultsConfig `yaml:"results"`

	// checkKeys are the stash keys that any check reads.
	checkKeys map[string]bool
}

// WindowConfig is how far back a fact counts.
type WindowConfig struct {
	Window Duration `yaml:"window"`
}

// OwnershipConfig names the actions that make a group an owner.
type OwnershipConfig struct {
	EnvironmentAction string `yaml:"environment_action"`
	ProjectAction     string `yaml:"project_action"`
}

// ParityConfig compares an environment with its reference.
type ParityConfig struct {
	Environment string `yaml:"environment"`
	Reference   string `yaml:"reference"`
}

// ScorecardConfig is one scorecard: the instances it scores, its checks, and its levels.
type ScorecardConfig struct {
	Name   string         `yaml:"name"`
	Match  Attributes     `yaml:"match"`
	Checks []CheckConfig  `yaml:"checks"`
	Levels map[string]int `yaml:"levels"`

	levels []level
}

// CheckConfig is one check. It reads a stash entry or the instance, and passes when its jq expression is true.
type CheckConfig struct {
	ID     string    `yaml:"id"`
	Stash  *StashRef `yaml:"stash"`
	Query  string    `yaml:"query"`
	Pass   string    `yaml:"pass"`
	MaxAge Duration  `yaml:"max_age"`

	pass *gojq.Code
}

// StashRef is a stash key, on the instance or on its latest deployment, environment, or project.
type StashRef struct {
	Key   string `yaml:"key"`
	Scope string `yaml:"scope"`
}

// UnmarshalYAML reads a key on the instance, such as `md/v1/drift`, or `{key, scope}`.
func (s *StashRef) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		*s = StashRef{Key: n.Value}
		return nil
	}
	type plain StashRef
	var p plain
	if err := n.Decode(&p); err != nil {
		return err
	}
	*s = StashRef(p)
	return nil
}

type level struct {
	name      string
	threshold int
}

// Attributes are effective attributes and their allowed values. An instance matches when it has every key with
// one of its values.
type Attributes map[string]Values

// Matches reports whether attrs has every key with an allowed value. Empty Attributes match everything.
func (a Attributes) Matches(attrs map[string]any) bool {
	for k, allowed := range a {
		v, ok := attrs[k].(string)
		if !ok || !contains(allowed, v) {
			return false
		}
	}
	return true
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// Values is one attribute value or a list of allowed values.
type Values []string

// UnmarshalYAML reads a scalar or a sequence of scalars.
func (v *Values) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		*v = Values{n.Value}
		return nil
	}
	var list []string
	if err := n.Decode(&list); err != nil {
		return fmt.Errorf("line %d: an attribute value must be a string or a list of strings", n.Line)
	}
	*v = list
	return nil
}

// Duration is a Go duration, such as 90m, or a number of days, such as 30d.
type Duration time.Duration

// UnmarshalYAML reads a duration string.
func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	v, err := parseDuration(n.Value)
	if err != nil || v < 0 {
		return fmt.Errorf("line %d: a duration must look like 90m, 1h, or 30d", n.Line)
	}
	*d = Duration(v)
	return nil
}

func parseDuration(s string) (time.Duration, error) {
	if days, ok := strings.CutSuffix(s, "d"); ok {
		var n int
		if _, err := fmt.Sscanf(days, "%d", &n); err != nil || fmt.Sprint(n) != days {
			return 0, errors.New("not a number of days")
		}
		return time.Duration(n) * 24 * time.Hour, nil
	}
	return time.ParseDuration(s)
}

// The segment rule of md/v1/scorecard/{scorecard}.
var scorecardName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)

var stashKey = regexp.MustCompile(`^[A-Za-z0-9_./-]{1,255}$`)

// ParseConfig decodes and checks a config document.
func ParseConfig(b []byte) (*Config, error) {
	var c Config
	if err := automation.DecodeConfig(b, &c); err != nil {
		return nil, err
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

// Validate checks the config, fills its defaults, and compiles the checks.
func (c *Config) Validate() error {
	var errs []error
	fail := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }

	if c.Interval == 0 {
		c.Interval = Duration(defaultInterval)
	}
	if time.Duration(c.Interval) < minInterval {
		fail("interval must be at least 1m")
	}
	for name, w := range map[string]*WindowConfig{"delivery": &c.Delivery, "reliability": &c.Reliability} {
		if w.Window == 0 {
			w.Window = Duration(defaultWindow)
		}
		if time.Duration(w.Window) < 24*time.Hour || time.Duration(w.Window)%(24*time.Hour) != 0 {
			fail("%s.window must be a whole number of days, such as 30d", name)
		}
	}
	if c.Ownership.EnvironmentAction == "" {
		c.Ownership.EnvironmentAction = "environment:deploy"
	}
	if c.Ownership.ProjectAction == "" {
		c.Ownership.ProjectAction = "project:update"
	}
	for i, p := range c.Parity {
		if p.Environment == "" || p.Reference == "" {
			fail("parity[%d] needs environment and reference", i)
		} else if p.Environment == p.Reference {
			fail("parity[%d].reference must be another environment", i)
		}
	}

	if len(c.Scorecards) == 0 {
		fail("add at least one scorecard")
	}
	names := map[string]bool{}
	c.checkKeys = map[string]bool{}
	for i := range c.Scorecards {
		s := &c.Scorecards[i]
		at := fmt.Sprintf("scorecards[%d]", i)
		if !scorecardName.MatchString(s.Name) {
			fail("%s.name must be 1 to 63 lowercase letters, digits, _, and -, and start with a letter or digit", at)
		}
		if names[s.Name] {
			fail("%s.name %s is used by another scorecard", at, s.Name)
		}
		names[s.Name] = true
		for k, v := range s.Match {
			if len(v) == 0 {
				fail("%s.match.%s needs a value", at, k)
			}
		}
		if len(s.Checks) == 0 || len(s.Checks) > maxChecks {
			fail("%s.checks must have 1 to %d checks", at, maxChecks)
		}
		ids := map[string]bool{}
		for j := range s.Checks {
			if err := s.Checks[j].compile(); err != nil {
				fail("%s.checks[%d]: %v", at, j, err)
			}
			if s.Checks[j].Stash != nil {
				c.checkKeys[s.Checks[j].Stash.Key] = true
			}
			if ids[s.Checks[j].ID] {
				fail("%s.checks[%d].id %s is used by another check", at, j, s.Checks[j].ID)
			}
			ids[s.Checks[j].ID] = true
		}
		if len(s.Levels) == 0 {
			s.Levels = map[string]int{"gold": 100, "silver": 80, "bronze": 50}
		}
		if len(s.Levels) > maxLevels {
			fail("%s.levels has more than %d levels", at, maxLevels)
		}
		s.levels = s.levels[:0]
		for name, threshold := range s.Levels {
			if name == "" || len(name) > 32 || name == NoLevel {
				fail("%s.levels: a level name is 1 to 32 characters, and not %s", at, NoLevel)
			}
			if threshold < 0 || threshold > 100 {
				fail("%s.levels.%s must be from 0 to 100", at, name)
			}
			s.levels = append(s.levels, level{name: name, threshold: threshold})
		}
		// Highest threshold first, so the first level a score reaches is the highest.
		sort.Slice(s.levels, func(a, b int) bool {
			if s.levels[a].threshold != s.levels[b].threshold {
				return s.levels[a].threshold > s.levels[b].threshold
			}
			return s.levels[a].name < s.levels[b].name
		})
	}
	return errors.Join(errs...)
}

func (ch *CheckConfig) compile() error {
	if ch.ID == "" || len(ch.ID) > 64 {
		return errors.New("id must be 1 to 64 characters")
	}
	switch {
	case ch.Stash != nil && ch.Query != "":
		return errors.New("set stash or query, not both")
	case ch.Stash != nil:
		if !stashKey.MatchString(ch.Stash.Key) {
			return errors.New("stash.key must be 1 to 255 letters, digits, and the characters _ . / -")
		}
		if ch.Stash.Scope == "" {
			ch.Stash.Scope = ScopeInstance
		}
		switch ch.Stash.Scope {
		case ScopeInstance, ScopeDeployment, ScopeEnvironment, ScopeProject:
		default:
			return errors.New("stash.scope must be instance, deployment, environment, or project")
		}
	case ch.Query != "":
		if ch.Query != QueryInstance {
			return errors.New("query must be instance")
		}
		if ch.MaxAge != 0 {
			return errors.New("max_age applies to stash checks only")
		}
	default:
		return errors.New("set stash or query")
	}
	if ch.Pass == "" {
		return errors.New("pass is required")
	}
	q, err := gojq.Parse(ch.Pass)
	if err != nil {
		return fmt.Errorf("pass is not a jq expression: %w", err)
	}
	// A check sees only its input: no environment variables, which hold the automation's credentials.
	code, err := gojq.Compile(q, gojq.WithEnvironLoader(func() []string { return nil }))
	if err != nil {
		return fmt.Errorf("pass is not a jq expression: %w", err)
	}
	ch.pass = code
	return nil
}

// key is the stash key that scorecards writes for a fact or a scorecard, such as delivery or scorecard/gold.
func (c *Config) key(name string) string {
	return massdriver.Results{Prefix: c.Results.Prefix}.Key(name)
}

// scorecardKey is the stash key of a scorecard.
func (c *Config) scorecardKey(name string) string { return c.key("scorecard/" + name) }

// ownKey reports whether scorecards writes key. A change to one of them is not a reason to compute again.
func (c *Config) ownKey(key string) bool {
	name, ok := strings.CutPrefix(key, c.key(""))
	if !ok {
		return false
	}
	if sc, ok := strings.CutPrefix(name, "scorecard/"); ok {
		return sc != ""
	}
	return slices.Contains(facts, name)
}

// matching returns the scorecards whose match the attributes meet.
func (c *Config) matching(attrs map[string]any) []*ScorecardConfig {
	var out []*ScorecardConfig
	for i := range c.Scorecards {
		if c.Scorecards[i].Match.Matches(attrs) {
			out = append(out, &c.Scorecards[i])
		}
	}
	return out
}

// referenceOf returns the reference environment of env, or "".
func (c *Config) referenceOf(env string) string {
	for _, p := range c.Parity {
		if p.Environment == env {
			return p.Reference
		}
	}
	return ""
}
