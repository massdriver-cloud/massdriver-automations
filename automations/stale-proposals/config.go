package staleproposals

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/robfig/cron/v3"
	"gopkg.in/yaml.v3"

	"github.com/massdriver-cloud/massdriver-automations/pkg/automation"
	"github.com/massdriver-cloud/massdriver-automations/pkg/massdriver"
	"github.com/massdriver-cloud/massdriver-automations/pkg/slack"
)

// Ways to group the digest.
const (
	GroupEnvironment = "environment"
	GroupProject     = "project"
)

// Config is the config file of stale-proposals.
type Config struct {
	Slack       slack.Config         `yaml:"slack"`
	Massdriver  massdriver.AppConfig `yaml:"massdriver"`
	Schedule    string               `yaml:"schedule"`
	Timezone    string               `yaml:"timezone"`
	RemindAfter Duration             `yaml:"remind_after"`
	RejectAfter Duration             `yaml:"reject_after"`
	DryRun      bool                 `yaml:"dry_run"`
	GroupBy     string               `yaml:"group_by"`
	Match       Attributes           `yaml:"match"`
	Exclude     Attributes           `yaml:"exclude"`
	Mentions    []Mention            `yaml:"mentions"`
}

// Mention adds Slack mentions to the proposals that match its attributes.
type Mention struct {
	Match    Attributes `yaml:"match"`
	Mentions []string   `yaml:"mentions"`
}

// Attributes are effective attributes and their allowed values. A proposal matches when it has every key with one
// of its values.
type Attributes map[string]Values

// Matches reports whether attrs has every key with an allowed value. Empty Attributes match everything.
func (a Attributes) Matches(attrs map[string]any) bool {
	for k, allowed := range a {
		v, ok := attrs[k].(string)
		if !ok {
			return false
		}
		found := false
		for _, x := range allowed {
			if x == v {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
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

// Duration is a Go duration, such as 36h, or a number of days, such as 7d.
type Duration time.Duration

// UnmarshalYAML reads a duration string.
func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	v, err := parseDuration(n.Value)
	if err != nil || v < 0 {
		return fmt.Errorf("line %d: a duration must look like 36h, 90m, or 7d", n.Line)
	}
	*d = Duration(v)
	return nil
}

func parseDuration(s string) (time.Duration, error) {
	if s == "" {
		return 0, nil
	}
	if days, ok := strings.CutSuffix(s, "d"); ok {
		n, err := strconv.ParseFloat(days, 64)
		return time.Duration(n * float64(24*time.Hour)), err
	}
	return time.ParseDuration(s)
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
	if c.RemindAfter == 0 {
		c.RemindAfter = Duration(24 * time.Hour)
	}
	if c.RejectAfter != 0 && c.RejectAfter < c.RemindAfter {
		fail("reject_after must not be shorter than remind_after")
	}
	if c.GroupBy == "" {
		c.GroupBy = GroupEnvironment
	}
	if c.GroupBy != GroupEnvironment && c.GroupBy != GroupProject {
		fail("group_by must be environment or project")
	}
	for name, a := range map[string]Attributes{"match": c.Match, "exclude": c.Exclude} {
		for k, v := range a {
			if len(v) == 0 {
				fail("%s.%s needs a value", name, k)
			}
		}
	}
	for i, m := range c.Mentions {
		if len(m.Mentions) == 0 {
			fail("mentions[%d].mentions needs at least one mention", i)
		}
		for k, v := range m.Match {
			if len(v) == 0 {
				fail("mentions[%d].match.%s needs a value", i, k)
			}
		}
	}
	return errors.Join(errs...)
}

func (c *Config) spec() string { return "CRON_TZ=" + c.Timezone + " " + c.Schedule }
