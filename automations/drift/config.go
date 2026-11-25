package drift

import (
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/robfig/cron/v3"
	"gopkg.in/yaml.v3"

	"github.com/massdriver-cloud/massdriver-automations/pkg/automation"
	"github.com/massdriver-cloud/massdriver-automations/pkg/massdriver"
	"github.com/massdriver-cloud/massdriver-automations/pkg/slack"
)

// Modes and resolve sources of a check.
const (
	ModeDetect  = "detect"
	ModeResolve = "resolve"
	ModePropose = "propose"

	ResolveDeployed = "deployed"
	ResolveSaved    = "saved"
)

// Default log patterns for OpenTofu and Terraform.
const (
	DefaultCleanPattern    = `No changes\.`
	DefaultDriftPattern    = `Plan: \d+ to \w+(?:, \d+ to \w+)*`
	DefaultResourcePattern = `(?m)^\s*# \S+.* (?:will be|must be) .*$`
)

// Config is the config file of drift.
type Config struct {
	// Slack is where alerts go when a check does not name its own channel.
	Slack      slack.Config         `yaml:"slack"`
	Massdriver massdriver.AppConfig `yaml:"massdriver"`
	Checks     []Check              `yaml:"checks"`
}

// Check is one set of instances to plan on a schedule.
type Check struct {
	Name          string            `yaml:"name"`
	Schedule      string            `yaml:"schedule"`
	Timezone      string            `yaml:"timezone"`
	Match         map[string]Values `yaml:"match"`
	Mode          string            `yaml:"mode"`
	ResolveWith   string            `yaml:"resolve_with"`
	Slack         slack.Config      `yaml:"slack"`
	NotifyClean   bool              `yaml:"notify_clean"`
	Summary       bool              `yaml:"summary"`
	MaxConcurrent int               `yaml:"max_concurrent"`
	Patterns      Patterns          `yaml:"patterns"`

	clean, drift, resource *regexp.Regexp
}

// Patterns are the regular expressions that read a plan's logs.
type Patterns struct {
	Clean    string `yaml:"clean"`
	Drift    string `yaml:"drift"`
	Resource string `yaml:"resource"`
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

// Validate checks every check and fills its defaults.
func (c *Config) Validate() error {
	var errs []error
	fail := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }

	if len(c.Checks) == 0 {
		fail("checks: at least one check is required")
	}
	names := map[string]bool{}
	for i := range c.Checks {
		ch := &c.Checks[i]
		if ch.Name == "" {
			fail("checks[%d].name is required", i)
		}
		if names[ch.Name] {
			fail("checks[%d].name %q is used twice", i, ch.Name)
		}
		names[ch.Name] = true

		if ch.Timezone == "" {
			ch.Timezone = "UTC"
		}
		if _, err := time.LoadLocation(ch.Timezone); err != nil {
			fail("checks[%d].timezone: %v", i, err)
		} else if ch.Schedule != "" {
			if _, err := cron.ParseStandard(ch.spec()); err != nil {
				fail("checks[%d].schedule: %v", i, err)
			}
		}
		for k, v := range ch.Match {
			if len(v) == 0 {
				fail("checks[%d].match.%s needs a value", i, k)
			}
		}

		if ch.Mode == "" {
			ch.Mode = ModeDetect
		}
		if ch.Mode != ModeDetect && ch.Mode != ModeResolve && ch.Mode != ModePropose {
			fail("checks[%d].mode must be detect, resolve, or propose", i)
		}
		if ch.ResolveWith == "" {
			ch.ResolveWith = ResolveDeployed
		}
		if ch.ResolveWith != ResolveDeployed && ch.ResolveWith != ResolveSaved {
			fail("checks[%d].resolve_with must be deployed or saved", i)
		}
		if ch.MaxConcurrent == 0 {
			ch.MaxConcurrent = 5
		}
		if ch.MaxConcurrent < 1 {
			fail("checks[%d].max_concurrent must be at least 1", i)
		}

		compile := func(field, src, fallback string) *regexp.Regexp {
			if src == "" {
				src = fallback
			}
			re, err := regexp.Compile(src)
			if err != nil {
				fail("checks[%d].patterns.%s: %v", i, field, err)
			}
			return re
		}
		ch.clean = compile("clean", ch.Patterns.Clean, DefaultCleanPattern)
		ch.drift = compile("drift", ch.Patterns.Drift, DefaultDriftPattern)
		ch.resource = compile("resource", ch.Patterns.Resource, DefaultResourcePattern)
	}
	return errors.Join(errs...)
}

// channels returns every channel that the config names.
func (c *Config) channels() []string {
	out := []string{c.Slack.Channel}
	for _, ch := range c.Checks {
		out = append(out, ch.Slack.Channel)
	}
	return out
}

func (ch *Check) spec() string { return "CRON_TZ=" + ch.Timezone + " " + ch.Schedule }

func (c *Config) find(name string) (*Check, bool) {
	for i := range c.Checks {
		if c.Checks[i].Name == name {
			return &c.Checks[i], true
		}
	}
	return nil, false
}
