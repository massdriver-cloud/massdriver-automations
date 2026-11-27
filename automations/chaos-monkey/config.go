package chaosmonkey

import (
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/robfig/cron/v3"
	"gopkg.in/yaml.v3"

	"github.com/massdriver-cloud/massdriver-automations/pkg/automation"
	"github.com/massdriver-cloud/massdriver-automations/pkg/massdriver"
	"github.com/massdriver-cloud/massdriver-automations/pkg/slack"
)

// Modes say what a run does with the params it makes.
const (
	ModePropose = "propose"
	ModePlan    = "plan"
	ModeDeploy  = "deploy"
)

// Strategies make the params of a run.
const (
	StrategyPreset    = "preset"
	StrategyMerge     = "merge"
	StrategyMutate    = "mutate"
	StrategyGibberish = "gibberish"
)

var strategies = []string{StrategyPreset, StrategyMerge, StrategyMutate, StrategyGibberish}

var statuses = []string{"INITIALIZED", "PROVISIONED", "FAILED", "DECOMMISSIONED"}

// Config is the config file of chaos-monkey.
type Config struct {
	Slack      slack.Config         `yaml:"slack"`
	Massdriver massdriver.AppConfig `yaml:"massdriver"`
	Schedule   string               `yaml:"schedule"`
	Timezone   string               `yaml:"timezone"`
	Mode       string               `yaml:"mode"`
	Match      map[string]Values    `yaml:"match"`
	Status     []string             `yaml:"status"`
	Instances  int                  `yaml:"instances"`
	Strategies map[string]int       `yaml:"strategies"`
	Mutate     Mutate               `yaml:"mutate"`
	Gibberish  Gibberish            `yaml:"gibberish"`
	Seed       *uint64              `yaml:"seed"`
	// Signature is a node, not a string, so that an explicit null, which turns signatures off, differs from no key.
	Signature yaml.Node `yaml:"signature"`

	signatures []string
}

// DefaultSignatures are the sign-offs that end deployment messages and Slack posts when the config has no
// signature. Each deployment gets one at random.
var DefaultSignatures = []string{
	"Hey, thanks for monkeyin' around. 🐒🤙🏼🏄‍♂️",
	"No bananas were harmed in this deployment. 🍌",
	"Ook ook. Ship it. 🐵🚀",
	"Swinging from vine to vine, one PROVISION at a time. 🌴",
	"Your infrastructure has been lovingly flung. 🐒💩",
	"Peeled, planned, and pushed. 🍌📦",
	"Monkey see, monkey deploy. 🙈",
	"Chaos is a ladder. Also a tree. 🌳🐒",
	"Barrel of monkeys, fresh out of the barrel. 🛢️",
	"Hang loose, the pager probably won't go off. 🤙📟",
}

func (c *Config) parseSignature() error {
	n := &c.Signature
	switch {
	case n.Kind == 0:
		c.signatures = DefaultSignatures
	case n.Tag == "!!null":
		c.signatures = nil
	case n.Kind == yaml.ScalarNode:
		c.signatures = nil
		if n.Value != "" {
			c.signatures = []string{n.Value}
		}
	case n.Kind == yaml.SequenceNode:
		var list []string
		if err := n.Decode(&list); err != nil {
			return fmt.Errorf("line %d: signature must be a string, a list of strings, or null", n.Line)
		}
		c.signatures = slices.DeleteFunc(list, func(s string) bool { return s == "" })
	default:
		return fmt.Errorf("line %d: signature must be a string, a list of strings, or null", n.Line)
	}
	return nil
}

// Mutate tunes the mutate strategy.
type Mutate struct {
	Rate float64 `yaml:"rate"`
}

// Gibberish tunes the gibberish strategy.
type Gibberish struct {
	MaxFields int `yaml:"max_fields"`
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

// Validate checks the config and fills its defaults.
func (c *Config) Validate() error {
	var errs []error
	fail := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }

	// An empty match would target every instance in the organization.
	if len(c.Match) == 0 {
		fail("match: at least one attribute is required")
	}
	for k, v := range c.Match {
		if len(v) == 0 {
			fail("match.%s needs a value", k)
		}
	}

	if c.Timezone == "" {
		c.Timezone = "UTC"
	}
	if _, err := time.LoadLocation(c.Timezone); err != nil {
		fail("timezone is not an IANA zone")
	} else if c.Schedule != "" {
		if _, err := cron.ParseStandard(c.spec()); err != nil {
			fail("schedule is not a cron schedule")
		}
	}

	if c.Mode == "" {
		c.Mode = ModePropose
	}
	if c.Mode != ModePropose && c.Mode != ModePlan && c.Mode != ModeDeploy {
		fail("mode must be propose, plan, or deploy")
	}

	if len(c.Status) == 0 {
		c.Status = []string{"PROVISIONED"}
	}
	for i, s := range c.Status {
		if !slices.Contains(statuses, s) {
			fail("status[%d] must be INITIALIZED, PROVISIONED, FAILED, or DECOMMISSIONED", i)
		}
	}

	if c.Instances == 0 {
		c.Instances = 1
	}
	if c.Instances < 1 {
		fail("instances must be at least 1")
	}

	if c.Strategies == nil {
		c.Strategies = map[string]int{}
		for _, s := range strategies {
			c.Strategies[s] = 1
		}
	}
	total := 0
	for k, w := range c.Strategies {
		if !slices.Contains(strategies, k) {
			fail("strategies.%s is not a strategy: use preset, merge, mutate, or gibberish", k)
		}
		if w < 0 {
			fail("strategies.%s must not be negative", k)
		}
		total += w
	}
	if total == 0 {
		fail("strategies: at least one strategy needs a weight above 0")
	}

	if c.Mutate.Rate == 0 {
		c.Mutate.Rate = 0.3
	}
	if c.Mutate.Rate < 0 || c.Mutate.Rate > 1 {
		fail("mutate.rate must be between 0 and 1")
	}
	if c.Gibberish.MaxFields == 0 {
		c.Gibberish.MaxFields = 3
	}
	if c.Gibberish.MaxFields < 1 {
		fail("gibberish.max_fields must be at least 1")
	}
	if err := c.parseSignature(); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

func (c *Config) spec() string { return "CRON_TZ=" + c.Timezone + " " + c.Schedule }
