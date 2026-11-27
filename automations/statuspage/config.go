package statuspage

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/massdriver-cloud/massdriver-automations/pkg/automation"
)

// Config is the config file of statuspage.
type Config struct {
	Site                 SiteConfig     `yaml:"site"`
	HistoryDays          int            `yaml:"history_days"`
	DeploysAsMaintenance *bool          `yaml:"deploys_as_maintenance"`
	MinDuration          Duration       `yaml:"min_duration"`
	Attributes           AttributeKeys  `yaml:"attributes"`
	Announcements        []Announcement `yaml:"announcements"`
	Maintenance          []Maintenance  `yaml:"maintenance"`
	Pages                []PageConfig   `yaml:"pages"`
	Stash                StashConfig    `yaml:"stash"`
	// OutputDir is a directory that gets the static site after each change.
	OutputDir string `yaml:"output_dir"`

	location *time.Location
	template string
}

// SiteConfig holds what every page shares.
type SiteConfig struct {
	Title            string `yaml:"title"`
	BaseURL          string `yaml:"base_url"`
	Logo             string `yaml:"logo"`
	AccentColor      string `yaml:"accent_color"`
	Timezone         string `yaml:"timezone"`
	Template         string `yaml:"template"`
	ShowAlarmDetails bool   `yaml:"show_alarm_details"`
	FooterLinks      []Link `yaml:"footer_links"`
}

// StashConfig turns on the stash keys that change the page. Both are off by default.
type StashConfig struct {
	Health   bool   `yaml:"health"`
	Banners  bool   `yaml:"banners"`
	MinLevel string `yaml:"min_level"`
}

func (s StashConfig) enabled() bool { return s.Health || s.Banners }

// Link is a titled URL.
type Link struct {
	Title string `yaml:"title"`
	URL   string `yaml:"url"`
}

// AttributeKeys names the attributes that change how an instance appears.
type AttributeKeys struct {
	Title  string `yaml:"title"`
	Hidden string `yaml:"hidden"`
	Impact string `yaml:"impact"`
}

// Announcement is a banner at the top of a page.
type Announcement struct {
	Message  string     `yaml:"message"`
	Level    string     `yaml:"level"`
	URL      string     `yaml:"url"`
	Pages    []string   `yaml:"pages"`
	StartsAt *time.Time `yaml:"starts_at"`
	EndsAt   *time.Time `yaml:"ends_at"`
}

// Maintenance is a scheduled maintenance window.
type Maintenance struct {
	Title        string    `yaml:"title"`
	Description  string    `yaml:"description"`
	StartsAt     time.Time `yaml:"starts_at"`
	EndsAt       time.Time `yaml:"ends_at"`
	Environments []string  `yaml:"environments"`
	Components   []string  `yaml:"components"`
}

// PageConfig is one status page.
type PageConfig struct {
	Title        string      `yaml:"title"`
	Slug         string      `yaml:"slug"`
	Description  string      `yaml:"description"`
	Environments []EnvConfig `yaml:"environments"`
}

// EnvConfig is one environment section of a page.
type EnvConfig struct {
	ID                   string            `yaml:"id"`
	Title                string            `yaml:"title"`
	Description          string            `yaml:"description"`
	DeploysAsMaintenance *bool             `yaml:"deploys_as_maintenance"`
	Components           []ComponentConfig `yaml:"components"`
}

// ComponentConfig groups the instances whose attributes match into one row.
type ComponentConfig struct {
	Title       string            `yaml:"title"`
	Description string            `yaml:"description"`
	Match       map[string]string `yaml:"match"`
	Impact      string            `yaml:"impact"`
}

// Duration reads "90s", "5m", or "1h".
type Duration time.Duration

// UnmarshalYAML parses a Go duration string.
func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	v, err := time.ParseDuration(n.Value)
	if err != nil {
		return fmt.Errorf("line %d: want a duration, such as 2m", n.Line)
	}
	*d = Duration(v)
	return nil
}

var (
	slugPattern  = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)
	colorPattern = regexp.MustCompile(`^#[0-9a-fA-F]{3,8}$`)
	nonSlug      = regexp.MustCompile(`[^a-z0-9]+`)
	reserved     = map[string]bool{"api": true, "hooks": true, "healthz": true, "feed.atom": true, "badge.svg": true}
	impacts      = map[string]bool{degraded: true, partialOutage: true, majorOutage: true}
	levels       = map[string]bool{"info": true, "warning": true, "critical": true}
	bannerLevels = map[string]int{"info": 0, "warning": 1, "error": 2}
)

// LoadConfig reads and checks the config file at path, and the template file it names.
func LoadConfig(path string) (*Config, error) {
	var c Config
	if err := automation.LoadConfigFile(path, &c); err != nil {
		return nil, err
	}
	if c.Site.Template != "" {
		p := c.Site.Template
		if !filepath.IsAbs(p) {
			p = filepath.Join(filepath.Dir(path), p)
		}
		t, err := os.ReadFile(p)
		if err != nil {
			return nil, &automation.ConfigError{Path: path, Err: fmt.Errorf("site.template: %w", err)}
		}
		c.template = string(t)
	}
	return &c, nil
}

// Validate checks the config and fills its defaults.
func (c *Config) Validate() error {
	var errs []error
	fail := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }

	if c.Site.Title == "" {
		c.Site.Title = "Status"
	}
	c.Site.BaseURL = strings.TrimRight(c.Site.BaseURL, "/")
	if c.Site.AccentColor == "" {
		c.Site.AccentColor = "#2563eb"
	}
	if !colorPattern.MatchString(c.Site.AccentColor) {
		fail("site.accent_color must be a hex color such as #2563eb")
	}
	if c.Site.Timezone == "" {
		c.Site.Timezone = "UTC"
	}
	loc, err := time.LoadLocation(c.Site.Timezone)
	if err != nil {
		fail("site.timezone is not an IANA zone")
		loc = time.UTC
	}
	c.location = loc
	if c.HistoryDays == 0 {
		c.HistoryDays = 90
	}
	if c.HistoryDays < 1 || c.HistoryDays > 365 {
		fail("history_days must be between 1 and 365")
	}
	if c.DeploysAsMaintenance == nil {
		t := true
		c.DeploysAsMaintenance = &t
	}
	if c.MinDuration < 0 {
		fail("min_duration must not be negative")
	}
	if c.Attributes.Title == "" {
		c.Attributes.Title = "status_title"
	}
	if c.Attributes.Hidden == "" {
		c.Attributes.Hidden = "status_hidden"
	}
	if c.Attributes.Impact == "" {
		c.Attributes.Impact = "status_impact"
	}

	if len(c.Pages) == 0 {
		fail("pages: at least one page is required")
	}
	slugs := map[string]bool{}
	envs := map[string]bool{}
	for i := range c.Pages {
		p := &c.Pages[i]
		if p.Title == "" {
			fail("pages[%d].title is required", i)
		}
		if p.Slug == "" {
			p.Slug = slugify(p.Title)
		}
		if !slugPattern.MatchString(p.Slug) || reserved[p.Slug] {
			fail("pages[%d].slug must be lowercase letters, digits, and hyphens, and not one of api, hooks, healthz", i)
		}
		if slugs[p.Slug] {
			fail("pages[%d].slug is used by an earlier page", i)
		}
		slugs[p.Slug] = true
		if len(p.Environments) == 0 {
			fail("pages[%d].environments: at least one environment is required", i)
		}
		for j, e := range p.Environments {
			if e.ID == "" {
				fail("pages[%d].environments[%d].id is required", i, j)
			}
			// The page shows only configured titles, so an environment without one would expose its identifier.
			if e.Title == "" {
				fail("pages[%d].environments[%d].title is required", i, j)
			}
			envs[e.ID] = true
			for k, comp := range e.Components {
				if comp.Title == "" {
					fail("pages[%d].environments[%d].components[%d].title is required", i, j, k)
				}
				if len(comp.Match) == 0 {
					fail("pages[%d].environments[%d].components[%d].match needs at least one attribute", i, j, k)
				}
				if comp.Impact != "" && !impacts[comp.Impact] {
					fail("pages[%d].environments[%d].components[%d].impact must be degraded, partial_outage, or major_outage", i, j, k)
				}
			}
		}
	}

	if c.Stash.MinLevel == "" {
		c.Stash.MinLevel = "info"
	}
	if _, ok := bannerLevels[c.Stash.MinLevel]; !ok {
		fail("stash.min_level must be info, warning, or error")
	}

	for i := range c.Announcements {
		a := &c.Announcements[i]
		if a.Message == "" {
			fail("announcements[%d].message is required", i)
		}
		if a.Level == "" {
			a.Level = "info"
		}
		if !levels[a.Level] {
			fail("announcements[%d].level must be info, warning, or critical", i)
		}
		for j, s := range a.Pages {
			if !slugs[s] {
				fail("announcements[%d].pages[%d] is not the slug of a page", i, j)
			}
		}
	}
	for i, m := range c.Maintenance {
		if m.Title == "" {
			fail("maintenance[%d].title is required", i)
		}
		if m.StartsAt.IsZero() || !m.EndsAt.After(m.StartsAt) {
			fail("maintenance[%d] needs starts_at before ends_at", i)
		}
		if len(m.Environments) == 0 {
			fail("maintenance[%d].environments: at least one environment is required", i)
		}
		for j, e := range m.Environments {
			if !envs[e] {
				fail("maintenance[%d].environments[%d] is not on any page", i, j)
			}
		}
	}
	return errors.Join(errs...)
}

// environmentIDs lists each configured environment once.
func (c *Config) environmentIDs() []string {
	seen := map[string]bool{}
	var ids []string
	for _, p := range c.Pages {
		for _, e := range p.Environments {
			if !seen[e.ID] {
				seen[e.ID] = true
				ids = append(ids, e.ID)
			}
		}
	}
	return ids
}

func (c *Config) deploysAsMaintenance(e EnvConfig) bool {
	if e.DeploysAsMaintenance != nil {
		return *e.DeploysAsMaintenance
	}
	return *c.DeploysAsMaintenance
}

func slugify(s string) string {
	return strings.Trim(nonSlug.ReplaceAllString(strings.ToLower(s), "-"), "-")
}
