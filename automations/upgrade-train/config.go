package upgradetrain

import (
	"errors"
	"fmt"
	"slices"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/massdriver-cloud/massdriver-automations/pkg/automation"
	"github.com/massdriver-cloud/massdriver-automations/pkg/massdriver"
	"github.com/massdriver-cloud/massdriver-automations/pkg/slack"
)

// Modes of a stage.
const (
	ModeDeploy  = "deploy"
	ModePropose = "propose"
)

// DefaultSoak is how long a stage waits, with no firing alarms, before the next stage starts.
const DefaultSoak = time.Hour

// Config is the config file of upgrade-train.
type Config struct {
	Slack      slack.Config         `yaml:"slack"`
	Massdriver massdriver.AppConfig `yaml:"massdriver"`
	Trains     []Train              `yaml:"trains"`
}

// Train moves each new version of its bundles through its stages in order.
type Train struct {
	Name               string       `yaml:"name"`
	Bundles            []string     `yaml:"bundles"`
	IncludeDevelopment bool         `yaml:"include_development"`
	Stages             []Stage      `yaml:"stages"`
	Slack              slack.Config `yaml:"slack"`
}

// Stage is one set of instances that upgrades together.
type Stage struct {
	Name  string            `yaml:"name"`
	Match map[string]Values `yaml:"match"`
	Mode  string            `yaml:"mode"`
	Soak  Duration          `yaml:"soak"`
}

// Duration is a Go duration, such as 30m or 24h.
type Duration struct {
	time.Duration
	set bool
}

// UnmarshalYAML reads a duration string.
func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	v, err := time.ParseDuration(n.Value)
	if err != nil || v < 0 {
		return fmt.Errorf("line %d: a duration must look like 30m or 24h", n.Line)
	}
	d.Duration, d.set = v, true
	return nil
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

// Validate checks every train and fills its defaults.
func (c *Config) Validate() error {
	var errs []error
	fail := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }

	if len(c.Trains) == 0 {
		fail("trains: at least one train is required")
	}
	names := map[string]bool{}
	for i := range c.Trains {
		t := &c.Trains[i]
		if t.Name == "" {
			fail("trains[%d].name is required", i)
		}
		if names[t.Name] {
			fail("trains[%d].name is used by an earlier train", i)
		}
		names[t.Name] = true
		if len(t.Bundles) == 0 {
			fail("trains[%d].bundles needs at least one bundle", i)
		}
		if len(t.Stages) == 0 {
			fail("trains[%d].stages needs at least one stage", i)
		}
		stages := map[string]bool{}
		for j := range t.Stages {
			s := &t.Stages[j]
			if s.Name == "" {
				fail("trains[%d].stages[%d].name is required", i, j)
			}
			if stages[s.Name] {
				fail("trains[%d].stages[%d].name is used by an earlier stage", i, j)
			}
			stages[s.Name] = true
			if len(s.Match) == 0 {
				fail("trains[%d].stages[%d].match needs at least one attribute", i, j)
			}
			for k, v := range s.Match {
				if len(v) == 0 {
					fail("trains[%d].stages[%d].match.%s needs a value", i, j, k)
				}
			}
			if s.Mode == "" {
				s.Mode = ModePropose
			}
			if s.Mode != ModeDeploy && s.Mode != ModePropose {
				fail("trains[%d].stages[%d].mode must be deploy or propose", i, j)
			}
			if !s.Soak.set {
				s.Soak.Duration = DefaultSoak
			}
		}
	}
	return errors.Join(errs...)
}

func (c *Config) find(name string) (*Train, bool) {
	for i := range c.Trains {
		if c.Trains[i].Name == name {
			return &c.Trains[i], true
		}
	}
	return nil, false
}

// covering returns the trains whose bundles include repo.
func (c *Config) covering(repo string) []*Train {
	var out []*Train
	for i := range c.Trains {
		if slices.Contains(c.Trains[i].Bundles, repo) {
			out = append(out, &c.Trains[i])
		}
	}
	return out
}

func (c *Config) channels() []string {
	out := []string{c.Slack.Channel}
	for _, t := range c.Trains {
		out = append(out, t.Slack.Channel)
	}
	return out
}
