package router

import (
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/massdriver-cloud/massdriver-automations/pkg/automation"
)

const (
	// Massdriver waits 10 seconds for a response. A sync route must finish before that, with room for the response.
	syncLimit           = 8 * time.Second
	defaultAsyncTimeout = time.Minute
)

// Config is the router configuration file.
type Config struct {
	// Engine is the expression language of every expression in the file: "jq" (default) or "go".
	Engine string   `yaml:"engine"`
	Routes []*Route `yaml:"routes"`
}

// Route selects events and runs its steps in order.
type Route struct {
	Name    string            `yaml:"name"`
	Events  []string          `yaml:"events"`
	Match   map[string]string `yaml:"match"`
	When    string            `yaml:"when"`
	Timeout time.Duration     `yaml:"timeout"`
	Async   bool              `yaml:"async"`
	Do      []*Step           `yaml:"do"`

	when *program
}

// Step is one action. Exactly one of its fields is set.
type Step struct {
	HTTP       *HTTPStep       `yaml:"http"`
	Exec       []string        `yaml:"exec"`
	Massdriver *MassdriverStep `yaml:"massdriver"`

	args []*program
}

// HTTPStep sends one request.
type HTTPStep struct {
	Method  string            `yaml:"method"`
	URL     string            `yaml:"url"`
	Headers map[string]string `yaml:"headers"`
	Body    string            `yaml:"body"`

	url     *program
	headers map[string]*program
	body    *program
}

// MassdriverStep runs one GraphQL operation against the Massdriver V2 API.
type MassdriverStep struct {
	Query     string `yaml:"query"`
	Variables string `yaml:"variables"`

	variables *program
}

func (s *Step) kind() string {
	switch {
	case s.HTTP != nil:
		return "http"
	case s.Exec != nil:
		return "exec"
	default:
		return "massdriver"
	}
}

var eventPattern = regexp.MustCompile(`^[a-z_]+\.([a-z_]+|\*)$`)

// ParseConfig parses and compiles the YAML of a configuration file.
func ParseConfig(b []byte) (*Config, error) {
	var cfg Config
	if err := automation.DecodeConfig(b, &cfg); err != nil {
		return nil, err
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// Validate compiles every route, so a mistake stops the router at start and not at an event.
func (cfg *Config) Validate() error {
	if cfg.Engine == "" {
		cfg.Engine = engineJQ
	}
	if cfg.Engine != engineJQ && cfg.Engine != engineGo {
		return fmt.Errorf("engine %q: use jq or go", cfg.Engine)
	}
	if len(cfg.Routes) == 0 {
		return errors.New("routes: at least one route is required")
	}
	c := newCompiler(cfg.Engine)
	names := map[string]bool{}
	for i, r := range cfg.Routes {
		if r.Name == "" {
			return fmt.Errorf("route %d has no name", i+1)
		}
		if names[r.Name] {
			return fmt.Errorf("route name %q is used twice", r.Name)
		}
		names[r.Name] = true
		if err := r.compile(c); err != nil {
			return fmt.Errorf("route %q: %w", r.Name, err)
		}
	}
	return nil
}

// EventTypes returns the event types of every route, for the event destination.
func (cfg *Config) EventTypes() []string {
	var types []string
	for _, r := range cfg.Routes {
		for _, e := range r.Events {
			if !slices.Contains(types, e) {
				types = append(types, e)
			}
		}
	}
	return types
}

func (cfg *Config) usesMassdriver() bool {
	for _, r := range cfg.Routes {
		for _, s := range r.Do {
			if s.Massdriver != nil {
				return true
			}
		}
	}
	return false
}

func (r *Route) compile(c *compiler) error {
	if len(r.Events) == 0 {
		return errors.New("events is empty")
	}
	for _, e := range r.Events {
		if !eventPattern.MatchString(e) {
			return fmt.Errorf("event %q: use an event type, such as deployment.failed, or <entity>.*", e)
		}
	}
	switch {
	case r.Timeout < 0:
		return errors.New("timeout is negative")
	case r.Timeout == 0 && r.Async:
		r.Timeout = defaultAsyncTimeout
	case r.Timeout == 0:
		r.Timeout = syncLimit
	case !r.Async && r.Timeout > syncLimit:
		return fmt.Errorf("timeout %s is longer than %s; set async: true", r.Timeout, syncLimit)
	}
	if r.When != "" {
		p, err := c.value("when", r.When)
		if err != nil {
			return err
		}
		r.when = p
	}
	if len(r.Do) == 0 {
		return errors.New("do is empty")
	}
	for i, s := range r.Do {
		if err := s.compile(c); err != nil {
			return fmt.Errorf("step %d: %w", i+1, err)
		}
	}
	return nil
}

func (s *Step) compile(c *compiler) error {
	set := 0
	for _, ok := range []bool{s.HTTP != nil, s.Exec != nil, s.Massdriver != nil} {
		if ok {
			set++
		}
	}
	if set != 1 {
		return errors.New("set exactly one of http, exec, or massdriver")
	}
	var err error
	switch {
	case s.HTTP != nil:
		h := s.HTTP
		if h.URL == "" {
			return errors.New("http.url is empty")
		}
		if h.Method == "" {
			h.Method = http.MethodPost
		}
		h.Method = strings.ToUpper(h.Method)
		if h.url, err = c.text("http.url", h.URL); err != nil {
			return err
		}
		h.headers = map[string]*program{}
		for k, v := range h.Headers {
			if h.headers[k], err = c.text("http.headers."+k, v); err != nil {
				return err
			}
		}
		if h.Body != "" {
			if h.body, err = c.value("http.body", h.Body); err != nil {
				return err
			}
		}
	case s.Exec != nil:
		if len(s.Exec) == 0 {
			return errors.New("exec is empty")
		}
		s.args = make([]*program, len(s.Exec))
		for i, a := range s.Exec {
			if s.args[i], err = c.text(fmt.Sprintf("exec[%d]", i), a); err != nil {
				return err
			}
		}
	default:
		m := s.Massdriver
		if strings.TrimSpace(m.Query) == "" {
			return errors.New("massdriver.query is empty")
		}
		if m.Variables != "" {
			if m.variables, err = c.value("massdriver.variables", m.Variables); err != nil {
				return err
			}
		}
	}
	return nil
}
