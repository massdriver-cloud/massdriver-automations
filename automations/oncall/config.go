package oncall

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/massdriver-cloud/massdriver-automations/pkg/people"
	"gopkg.in/yaml.v3"
)

const minInterval = 30 * time.Second

// Config is the config file of oncall.
type Config struct {
	Interval   Duration         `yaml:"interval"`
	Label      string           `yaml:"label"`
	PagerDuty  PagerDutyConfig  `yaml:"pagerduty"`
	IncidentIO IncidentIOConfig `yaml:"incident_io"`
	Routes     []Route          `yaml:"routes"`
}

// PagerDutyConfig is the PagerDuty REST API and the variable that holds the API key.
type PagerDutyConfig struct {
	APIURL   string `yaml:"api_url"`
	TokenEnv string `yaml:"token_env"`
}

// IncidentIOConfig is the incident.io API and the variable that holds the API key.
type IncidentIOConfig struct {
	APIURL   string `yaml:"api_url"`
	TokenEnv string `yaml:"token_env"`
}

// Route writes who is on call to one or more environments. The people come from PagerDuty, from incident.io, or
// from a fixed list.
type Route struct {
	Environments []string         `yaml:"environments"`
	Title        string           `yaml:"title"`
	MaxPeople    int              `yaml:"max_people"`
	PagerDuty    *PagerDutyRoute  `yaml:"pagerduty"`
	IncidentIO   *IncidentIORoute `yaml:"incident_io"`
	Static       []people.Person  `yaml:"static"`
}

// PagerDutyRoute is the schedules and escalation policies whose on-call people a route lists.
type PagerDutyRoute struct {
	Schedules          []string `yaml:"schedules"`
	EscalationPolicies []string `yaml:"escalation_policies"`
}

// IncidentIORoute is the schedules and escalation paths whose on-call people a route lists, in this order.
type IncidentIORoute struct {
	Schedules       []string `yaml:"schedules"`
	EscalationPaths []string `yaml:"escalation_paths"`
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

// IDs of PagerDuty and incident.io objects: letters and digits.
var idPattern = regexp.MustCompile(`^[A-Za-z0-9]{1,64}$`)

func (c *Config) usesPagerDuty() bool {
	for _, r := range c.Routes {
		if r.PagerDuty != nil {
			return true
		}
	}
	return false
}

func (c *Config) usesIncidentIO() bool {
	for _, r := range c.Routes {
		if r.IncidentIO != nil {
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
		c.Interval = Duration(2 * time.Minute)
	}
	if c.Label == "" {
		c.Label = "On call"
	}
	if c.PagerDuty.APIURL == "" {
		c.PagerDuty.APIURL = "https://api.pagerduty.com"
	}
	if c.PagerDuty.TokenEnv == "" {
		c.PagerDuty.TokenEnv = "PAGERDUTY_TOKEN" //nolint:gosec // The name of the variable, not a token.
	}
	if c.IncidentIO.APIURL == "" {
		c.IncidentIO.APIURL = "https://api.incident.io"
	}
	if c.IncidentIO.TokenEnv == "" {
		c.IncidentIO.TokenEnv = "INCIDENT_IO_API_KEY" //nolint:gosec // The name of the variable, not a token.
	}
	c.PagerDuty.APIURL = strings.TrimRight(c.PagerDuty.APIURL, "/")
	c.IncidentIO.APIURL = strings.TrimRight(c.IncidentIO.APIURL, "/")

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
		if r.MaxPeople == 0 {
			r.MaxPeople = 10
		}
		if r.MaxPeople < 1 || r.MaxPeople > people.MaxEntries {
			fail("%s.max_people must be from 1 to %d", at, people.MaxEntries)
		}
		sources := 0
		for _, set := range []bool{r.PagerDuty != nil, r.IncidentIO != nil, r.Static != nil} {
			if set {
				sources++
			}
		}
		switch {
		case sources != 1:
			fail("%s must set exactly one of pagerduty, incident_io, or static", at)
		case r.PagerDuty != nil:
			errs = append(errs, validateIDs(at+".pagerduty", "schedules", r.PagerDuty.Schedules,
				"escalation_policies", r.PagerDuty.EscalationPolicies))
		case r.IncidentIO != nil:
			errs = append(errs, validateIDs(at+".incident_io", "schedules", r.IncidentIO.Schedules,
				"escalation_paths", r.IncidentIO.EscalationPaths))
		default:
			errs = append(errs, validateStatic(at+".static", r.Static))
		}
	}
	return errors.Join(errs...)
}

func validateIDs(at, aName string, a []string, bName string, b []string) error {
	var errs []error
	if len(a)+len(b) == 0 {
		errs = append(errs, fmt.Errorf("%s must list at least one of %s or %s", at, aName, bName))
	}
	for _, list := range []struct {
		name string
		ids  []string
	}{{aName, a}, {bName, b}} {
		for j, id := range list.ids {
			if !idPattern.MatchString(id) {
				errs = append(errs, fmt.Errorf("%s.%s[%d] must be an ID of letters and digits", at, list.name, j))
			}
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
