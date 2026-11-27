package stashsync

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/massdriver-cloud/massdriver-automations/pkg/automation"
)

// Modes.
const (
	ModeSync     = "sync"
	ModeSimulate = "simulate"
)

// Formats of a stash entry.
const (
	FormatText = "TEXT"
	FormatJSON = "JSON"
)

const (
	defaultSyncInterval     = time.Minute
	defaultSimulateInterval = 5 * time.Second
)

// Config is the config file of stash-sync.
type Config struct {
	Mode       string           `yaml:"mode"`
	Interval   Duration         `yaml:"interval"`
	CloudWatch CloudWatchConfig `yaml:"cloudwatch"`
	Prometheus PrometheusConfig `yaml:"prometheus"`
	Presets    []Preset         `yaml:"presets"`
	Rules      []Rule           `yaml:"rules"`
	Simulate   SimulateConfig   `yaml:"simulate"`

	// groups are the rules by stash entry, in config order. Validate builds them.
	groups []*group
}

// CloudWatchConfig is the `cloudwatch` section.
type CloudWatchConfig struct {
	Region string `yaml:"region"`
}

// PrometheusConfig is the `prometheus` section.
type PrometheusConfig struct {
	URL         string   `yaml:"url"`
	TokenEnv    string   `yaml:"token_env"`
	Username    string   `yaml:"username"`
	PasswordEnv string   `yaml:"password_env"`
	Timeout     Duration `yaml:"timeout"`
}

// Rule fills a stash entry, or one field of a JSON entry, from one query or a fixed value.
type Rule struct {
	Target     Target           `yaml:"target"`
	CloudWatch *CloudWatchQuery `yaml:"cloudwatch"`
	Prometheus *PrometheusQuery `yaml:"prometheus"`
	Value      any              `yaml:"value"`
	Scale      *float64         `yaml:"scale"`
	Default    *float64         `yaml:"default"`
	Text       []TextRule       `yaml:"text"`
}

// TextRule turns a number into text for a TEXT entry. The first rule whose `below` is more than the number
// applies. A rule without `below` applies to every number.
type TextRule struct {
	Below *float64 `yaml:"below"`
	Text  string   `yaml:"text"`
}

// Target is the stash entry that a rule writes: one scope, a key, and an optional field of a JSON value.
type Target struct {
	Project     string `yaml:"project"`
	Environment string `yaml:"environment"`
	Component   string `yaml:"component"`
	Instance    string `yaml:"instance"`
	Deployment  string `yaml:"deployment"`
	Key         string `yaml:"key"`
	Field       string `yaml:"field"`
	Format      string `yaml:"format"`
	Label       string `yaml:"label"`
}

// Scope is the entity that owns a stash.
type Scope struct {
	Kind string // project, environment, component, instance, or deployment
	ID   string
}

func (s Scope) String() string { return s.Kind + " " + s.ID }

func (t Target) scope() (Scope, error) {
	var set []Scope
	for _, s := range []Scope{
		{"project", t.Project}, {"environment", t.Environment}, {"component", t.Component},
		{"instance", t.Instance}, {"deployment", t.Deployment},
	} {
		if s.ID != "" {
			set = append(set, s)
		}
	}
	if len(set) != 1 {
		return Scope{}, errors.New("set exactly one of project, environment, component, instance, or deployment")
	}
	return set[0], nil
}

// Duration is a Go duration, such as 60s or 5m.
type Duration time.Duration

// UnmarshalYAML reads a duration string.
func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	v, err := time.ParseDuration(n.Value)
	if err != nil || v < 0 {
		return fmt.Errorf("line %d: a duration must look like 60s or 5m", n.Line)
	}
	*d = Duration(v)
	return nil
}

var keyPattern = regexp.MustCompile(`^[A-Za-z0-9_./-]{1,255}$`)

// group is every rule that writes one stash entry.
type group struct {
	scope  Scope
	key    string
	format string
	label  string
	rules  []Rule
	// whole is true when one rule writes the whole value, and false when each rule writes a field.
	whole bool
}

func (g *group) id() string { return g.scope.Kind + "/" + g.scope.ID + "/" + g.key }

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

// Validate checks the config, fills its defaults, and groups the rules by stash entry.
func (c *Config) Validate() error {
	var errs []error
	fail := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }

	if c.Mode == "" {
		c.Mode = ModeSync
	}
	switch c.Mode {
	case ModeSync:
		if c.Interval == 0 {
			c.Interval = Duration(defaultSyncInterval)
		}
	case ModeSimulate:
		if c.Interval == 0 {
			c.Interval = Duration(defaultSimulateInterval)
		}
	default:
		fail("mode must be sync or simulate")
	}
	if c.Interval != 0 && time.Duration(c.Interval) < time.Second {
		fail("interval must be at least 1s")
	}
	if c.Prometheus.Timeout == 0 {
		c.Prometheus.Timeout = Duration(10 * time.Second)
	}

	if c.Mode == ModeSimulate {
		if err := c.Simulate.validate(); err != nil {
			errs = append(errs, err)
		}
		if len(c.Rules) > 0 || len(c.Presets) > 0 {
			fail("rules and presets are for mode sync; mode simulate writes only its scenario")
		}
		return errors.Join(errs...)
	}

	rules := make([]Rule, 0, len(c.Rules))
	where := make([]string, 0, len(c.Rules))
	for i, p := range c.Presets {
		expanded, err := p.rules()
		if err != nil {
			fail("presets[%d]: %v", i, err)
			continue
		}
		for range expanded {
			where = append(where, fmt.Sprintf("presets[%d]", i))
		}
		rules = append(rules, expanded...)
	}
	for i := range c.Rules {
		where = append(where, fmt.Sprintf("rules[%d]", i))
	}
	rules = append(rules, c.Rules...)
	if len(rules) == 0 && len(errs) == 0 {
		fail("add at least one rule or preset")
	}

	byID := map[string]*group{}
	for i, r := range rules {
		at := where[i]
		scope, err := r.Target.scope()
		if err != nil {
			fail("%s.target: %v", at, err)
			continue
		}
		if !keyPattern.MatchString(r.Target.Key) {
			fail("%s.target.key must be 1 to 255 letters, digits, and the characters _ . / -", at)
			continue
		}
		if err := r.validate(c); err != nil {
			fail("%s: %v", at, err)
			continue
		}
		format := strings.ToUpper(r.Target.Format)
		if format == "" {
			format = r.defaultFormat()
		}
		if format != FormatText && format != FormatJSON {
			fail("%s.target.format must be TEXT or JSON", at)
			continue
		}
		if format == FormatText && r.Target.Field != "" {
			fail("%s.target.field needs format JSON", at)
			continue
		}
		if format == FormatText && r.Target.Field == "" && r.defaultFormat() == FormatJSON {
			fail("%s.target: an object value needs format JSON", at)
			continue
		}
		if format == FormatJSON && r.Target.Field == "" && r.defaultFormat() != FormatJSON {
			fail("%s.target: a JSON entry without a field needs a fixed object value", at)
			continue
		}

		g := &group{scope: scope, key: r.Target.Key}
		if existing, ok := byID[g.id()]; ok {
			g = existing
		} else {
			g.format = format
			g.whole = r.Target.Field == ""
			byID[g.id()] = g
			c.groups = append(c.groups, g)
		}
		switch {
		case g.format != format:
			fail("%s.target: every rule for one key must have the same format", at)
			continue
		case g.whole || r.Target.Field == "":
			if len(g.rules) > 0 {
				fail("%s.target: a rule without a field writes the whole value, so it must be the only rule for its key", at)
				continue
			}
		}
		for _, other := range g.rules {
			if other.Target.Field == r.Target.Field {
				fail("%s.target.field %s is set by another rule for the same key", at, r.Target.Field)
			}
		}
		if r.Target.Label != "" {
			if g.label != "" && g.label != r.Target.Label {
				fail("%s.target.label differs from another rule for the same key", at)
			}
			g.label = r.Target.Label
		}
		g.rules = append(g.rules, r)
	}
	return errors.Join(errs...)
}

func (r Rule) validate(c *Config) error {
	n := 0
	if r.CloudWatch != nil {
		n++
		if err := r.CloudWatch.validate(); err != nil {
			return fmt.Errorf("cloudwatch: %w", err)
		}
	}
	if r.Prometheus != nil {
		n++
		if strings.TrimSpace(r.Prometheus.Query) == "" {
			return errors.New("prometheus.query is required")
		}
		if c.Prometheus.URL == "" {
			return errors.New("a prometheus rule needs prometheus.url")
		}
	}
	if r.Value != nil {
		n++
		if r.Scale != nil || r.Default != nil || len(r.Text) > 0 {
			return errors.New("a fixed value takes no scale, default, or text")
		}
	}
	if n != 1 {
		return errors.New("set exactly one of cloudwatch, prometheus, or value")
	}
	for i, t := range r.Text {
		if t.Text == "" {
			return fmt.Errorf("text[%d].text is required", i)
		}
		if t.Below == nil && i != len(r.Text)-1 {
			return fmt.Errorf("text[%d]: only the last text rule can leave out below", i)
		}
	}
	if len(r.Text) > 0 && r.Target.Field != "" {
		return errors.New("text needs a TEXT target without a field")
	}
	return nil
}

// defaultFormat is JSON for a field or a fixed object, and TEXT otherwise.
func (r Rule) defaultFormat() string {
	if r.Target.Field != "" {
		return FormatJSON
	}
	if _, ok := r.Value.(map[string]any); ok {
		return FormatJSON
	}
	return FormatText
}
