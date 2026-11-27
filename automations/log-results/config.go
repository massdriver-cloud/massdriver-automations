package logresults

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/itchyny/gojq"
	"gopkg.in/yaml.v3"

	"github.com/massdriver-cloud/massdriver-automations/pkg/automation"
)

// Defaults of the config file.
const (
	DefaultPrefix     = "results/v1"
	DefaultMaxLogMB   = 64
	DefaultMaxMatches = 1000

	ModeLast  = "last"
	ModeFirst = "first"
	ModeSum   = "sum"

	FormatText = "text"
	FormatJSON = "json"
)

// DefaultActions are the deployment actions whose logs are read when the config names none.
var DefaultActions = []string{"PROVISION", "PLAN"}

var (
	deploymentActions = []string{"PROVISION", "PLAN", "DECOMMISSION"}
	// The server's rule for stash keys.
	stashKey       = regexp.MustCompile(`^[a-zA-Z0-9_./-]+$`)
	maxStashKeyLen = 255
	// Variables that every value expression can read.
	valueVariables = []string{"$status", "$action", "$matches", "$deployment_id", "$attributes"}
)

// Config is the config file of log-results.
type Config struct {
	Actions    []string `yaml:"actions"`
	Results    Results  `yaml:"results"`
	MaxLogMB   int      `yaml:"max_log_mb"`
	MaxMatches int      `yaml:"max_matches"`
	Rules      []*Rule  `yaml:"rules"`
}

// Results is where the entries go.
type Results struct {
	// Prefix starts every key. The server rejects an md/ key that its widget catalog does not have.
	Prefix string `yaml:"prefix"`
}

// Rule reads one result from the logs and writes it as one stash entry.
type Rule struct {
	Name     string            `yaml:"name"`
	Match    map[string]Values `yaml:"match"`
	Actions  []string          `yaml:"actions"`
	Detect   string            `yaml:"detect"`
	Patterns []string          `yaml:"patterns"`
	Mode     string            `yaml:"mode"`
	Key      string            `yaml:"key"`
	Label    string            `yaml:"label"`
	Format   string            `yaml:"format"`
	Value    string            `yaml:"value"`

	detect   *regexp.Regexp
	patterns []*regexp.Regexp
	value    *gojq.Code
	// capture is the one named group of a text rule without a value.
	capture string
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

// Validate fills the defaults and compiles every rule, so a mistake stops the automation at start.
func (c *Config) Validate() error {
	var errs []error
	fail := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }

	if len(c.Actions) == 0 {
		c.Actions = slices.Clone(DefaultActions)
	}
	if err := normalizeActions(c.Actions); err != nil {
		fail("actions: %v", err)
	}
	c.Results.Prefix = strings.Trim(c.Results.Prefix, "/")
	if c.Results.Prefix == "" {
		c.Results.Prefix = DefaultPrefix
	}
	if !stashKey.MatchString(c.Results.Prefix) {
		fail("results.prefix may contain only letters, digits, _, ., /, and -")
	}
	if c.MaxLogMB == 0 {
		c.MaxLogMB = DefaultMaxLogMB
	}
	if c.MaxLogMB < 1 {
		fail("max_log_mb must be at least 1")
	}
	if c.MaxMatches == 0 {
		c.MaxMatches = DefaultMaxMatches
	}
	if c.MaxMatches < 1 {
		fail("max_matches must be at least 1")
	}

	if len(c.Rules) == 0 {
		fail("rules: at least one rule is required")
	}
	names := map[string]bool{}
	for i, r := range c.Rules {
		if r == nil {
			fail("rules[%d] is empty", i)
			continue
		}
		if r.Name == "" {
			fail("rules[%d].name is required", i)
		} else if names[r.Name] {
			fail("rules[%d].name is used by an earlier rule", i)
		}
		names[r.Name] = true
		for _, err := range r.compile(c) {
			fail("rules[%d].%v", i, err)
		}
	}
	return errors.Join(errs...)
}

func normalizeActions(actions []string) error {
	for i, a := range actions {
		actions[i] = strings.ToUpper(a)
		if !slices.Contains(deploymentActions, actions[i]) {
			return errors.New("use PROVISION, PLAN, or DECOMMISSION")
		}
	}
	return nil
}

// compile checks one rule. The messages name the field and never quote a value: a pattern is config, and config
// never reaches a log.
func (r *Rule) compile(c *Config) []error {
	var errs []error
	fail := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }

	for k, v := range r.Match {
		if len(v) == 0 {
			fail("match.%s needs a value", k)
		}
	}
	if err := normalizeActions(r.Actions); err != nil {
		fail("actions: %v", err)
	}
	for _, a := range r.Actions {
		if !slices.Contains(c.Actions, a) {
			fail("actions: %s is not in the top-level actions, so its events never arrive", a)
		}
	}
	if r.Detect != "" {
		re, err := regexp.Compile(r.Detect)
		if err != nil {
			fail("detect is not a valid regular expression")
		}
		r.detect = re
	}

	if len(r.Patterns) == 0 {
		fail("patterns: at least one pattern is required")
	}
	var captures []string
	r.patterns = nil
	for j, p := range r.Patterns {
		re, err := regexp.Compile(p)
		if err != nil {
			fail("patterns[%d] is not a valid regular expression", j)
			continue
		}
		r.patterns = append(r.patterns, re)
		for _, name := range re.SubexpNames() {
			if name != "" && !slices.Contains(captures, name) {
				captures = append(captures, name)
			}
		}
	}

	if r.Mode == "" {
		r.Mode = ModeLast
	}
	if r.Mode != ModeLast && r.Mode != ModeFirst && r.Mode != ModeSum {
		fail("mode must be last, first, or sum")
	}

	r.Key = strings.Trim(r.Key, "/")
	switch key := c.Results.Prefix + "/" + r.Key; {
	case r.Key == "":
		fail("key is required")
	case !stashKey.MatchString(r.Key):
		fail("key may contain only letters, digits, _, ., /, and -")
	case len(key) > maxStashKeyLen:
		fail("key is longer than %d characters with the prefix", maxStashKeyLen)
	}
	if r.Label == "" {
		r.Label = r.Name
	}

	r.Format = strings.ToLower(r.Format)
	if r.Format == "" {
		r.Format = FormatJSON
	}
	if r.Format != FormatJSON && r.Format != FormatText {
		fail("format must be text or json")
	}

	r.value, r.capture = nil, ""
	switch {
	case r.Value != "":
		code, err := compileValue(r.Value)
		if err != nil {
			fail("value does not compile as jq")
		}
		r.value = code
	case r.Format == FormatText && len(captures) != 1:
		fail("value is required for a text rule unless its patterns have exactly one named group")
	case r.Format == FormatText:
		r.capture = captures[0]
	}
	return errs
}

func compileValue(src string) (*gojq.Code, error) {
	q, err := gojq.Parse(src)
	if err != nil {
		return nil, err
	}
	// The default environment loader reads os.Environ, which holds the API key and the signing secret.
	return gojq.Compile(q, gojq.WithVariables(valueVariables), gojq.WithEnvironLoader(func() []string { return nil }))
}

// applies reports whether the rule reads the logs of a deployment with this action and these attributes.
func (r *Rule) applies(action string, attrs map[string]any) bool {
	if len(r.Actions) > 0 && !slices.Contains(r.Actions, action) {
		return false
	}
	return matches(r.Match, attrs)
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

func (c *Config) maxLogBytes() int64 { return int64(c.MaxLogMB) << 20 }
