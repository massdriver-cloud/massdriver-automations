package costexplorer

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the config file of cost-explorer.
type Config struct {
	// Title names the dashboard.
	Title string `yaml:"title"`
	// Dimensions are the instance attributes to group and filter by. "bundle" is the bundle name.
	Dimensions []string `yaml:"dimensions"`
	// Refresh is how often costs are read from Massdriver.
	Refresh Duration `yaml:"refresh"`
	// HistoryDays is how many days of daily snapshots are kept.
	HistoryDays int `yaml:"history_days"`
	// Public serves the dashboard without a password.
	Public bool `yaml:"public"`
	// Username is the basic auth user. The password is COST_EXPLORER_PASSWORD.
	Username   string `yaml:"username"`
	Massdriver struct {
		AppURL string `yaml:"app_url"`
	} `yaml:"massdriver"`
}

// DefaultDimensions are the dimensions without a config.
var DefaultDimensions = []string{"md-project", "md-environment", "md-component", "bundle"}

// Validate checks c and fills its defaults.
func (c *Config) Validate() error {
	var errs []error
	if c.Title == "" {
		c.Title = "Cloud costs"
	}
	if len(c.Dimensions) == 0 {
		c.Dimensions = DefaultDimensions
	}
	seen := map[string]bool{}
	for _, d := range c.Dimensions {
		if strings.TrimSpace(d) == "" {
			errs = append(errs, errors.New("dimensions: a dimension is empty"))
		}
		if seen[d] {
			errs = append(errs, fmt.Errorf("dimensions: %s is listed twice", d))
		}
		seen[d] = true
	}
	if c.Refresh == 0 {
		c.Refresh = Duration(time.Hour)
	}
	if time.Duration(c.Refresh) < time.Minute {
		errs = append(errs, errors.New("refresh: must be at least 1m"))
	}
	if c.HistoryDays == 0 {
		c.HistoryDays = 90
	}
	if c.HistoryDays < 1 || c.HistoryDays > 731 {
		errs = append(errs, errors.New("history_days: must be from 1 to 731"))
	}
	if c.Username == "" {
		c.Username = "massdriver"
	}
	if c.Massdriver.AppURL == "" {
		c.Massdriver.AppURL = "https://app.massdriver.cloud"
	}
	c.Massdriver.AppURL = strings.TrimSuffix(c.Massdriver.AppURL, "/")
	return errors.Join(errs...)
}

// Duration reads "30m" or "1h".
type Duration time.Duration

// UnmarshalYAML parses a Go duration string.
func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	v, err := time.ParseDuration(n.Value)
	if err != nil {
		return fmt.Errorf("line %d: want a duration, such as 1h", n.Line)
	}
	*d = Duration(v)
	return nil
}
