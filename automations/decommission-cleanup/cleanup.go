// Package decommissioncleanup runs configured hooks when Massdriver tears down or deletes infrastructure, to clean up
// what the IaC does not manage.
package decommissioncleanup

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"text/template"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/massdriver-cloud/massdriver-automations/pkg/automation"
)

const (
	automationName = "decommission-cleanup"
	queueSize      = 100
	defaultTimeout = 30 * time.Second
	maxOutputBytes = 4096
)

// Definition registers the automation.
var Definition = automation.Definition{
	Name:        automationName,
	Description: "Runs HTTP and command hooks when instances are decommissioned or deleted.",
	Events:      []string{"deployment.completed", "instance.deleted", "environment.deleted"},
	New: func() (automation.Automation, error) {
		path := automation.Env("CLEANUP_HOOKS_FILE", "/etc/decommission-cleanup/hooks.yaml")
		hooks, err := LoadHooks(path)
		if err != nil {
			return nil, err
		}
		c := New(hooks)
		go c.Work()
		return c, nil
	},
}

// Hook is one cleanup action and the events that select it.
type Hook struct {
	Name    string            `yaml:"name"`
	Events  []string          `yaml:"events"`
	Match   map[string]string `yaml:"match"`
	Timeout time.Duration     `yaml:"timeout"`
	HTTP    *HTTPHook         `yaml:"http"`
	Exec    *ExecHook         `yaml:"exec"`

	templates map[string]*template.Template
}

// HTTPHook sends one request. URL, header values, and body are templates.
type HTTPHook struct {
	Method  string            `yaml:"method"`
	URL     string            `yaml:"url"`
	Headers map[string]string `yaml:"headers"`
	Body    string            `yaml:"body"`
}

// ExecHook runs one command without a shell. Each argument is a template.
type ExecHook struct {
	Command []string `yaml:"command"`
}

// LoadHooks reads and checks the hooks file, so a mistake stops the automation at start and not at a teardown.
func LoadHooks(path string) ([]*Hook, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("CLEANUP_HOOKS_FILE: %w", err)
	}
	return ParseHooks(b)
}

// ParseHooks parses the YAML of a hooks file.
func ParseHooks(b []byte) ([]*Hook, error) {
	var file struct {
		Hooks []*Hook `yaml:"hooks"`
	}
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&file); err != nil {
		return nil, fmt.Errorf("hooks file: %w", err)
	}
	if len(file.Hooks) == 0 {
		return nil, errors.New("hooks file has no hooks")
	}
	names := map[string]bool{}
	for i, h := range file.Hooks {
		if h.Name == "" {
			return nil, fmt.Errorf("hook %d has no name", i+1)
		}
		if names[h.Name] {
			return nil, fmt.Errorf("hook name %q is used twice", h.Name)
		}
		names[h.Name] = true
		if err := h.compile(); err != nil {
			return nil, fmt.Errorf("hook %q: %w", h.Name, err)
		}
	}
	return file.Hooks, nil
}

func (h *Hook) compile() error {
	if len(h.Events) == 0 {
		return errors.New("events is empty")
	}
	if (h.HTTP == nil) == (h.Exec == nil) {
		return errors.New("set exactly one of http or exec")
	}
	if h.Timeout <= 0 {
		h.Timeout = defaultTimeout
	}

	sources := map[string]string{}
	if h.HTTP != nil {
		if h.HTTP.URL == "" {
			return errors.New("http.url is empty")
		}
		if h.HTTP.Method == "" {
			h.HTTP.Method = http.MethodPost
		}
		sources["url"] = h.HTTP.URL
		sources["body"] = h.HTTP.Body
		for k, v := range h.HTTP.Headers {
			sources["header "+k] = v
		}
	} else {
		if len(h.Exec.Command) == 0 {
			return errors.New("exec.command is empty")
		}
		for i, arg := range h.Exec.Command {
			sources[fmt.Sprintf("arg %d", i)] = arg
		}
	}

	h.templates = map[string]*template.Template{}
	for key, src := range sources {
		t, err := template.New(key).Option("missingkey=error").Funcs(placeholderFuncs).Parse(src)
		if err != nil {
			return err
		}
		h.templates[key] = t
	}
	return nil
}

func (h *Hook) selects(ev automation.Event) bool {
	matched := slices.ContainsFunc(h.Events, func(e string) bool {
		if prefix, ok := strings.CutSuffix(e, "*"); ok {
			return strings.HasPrefix(ev.Type, prefix)
		}
		return e == ev.Type
	})
	if !matched {
		return false
	}
	for k, v := range h.Match {
		if ev.Attribute(k) != v {
			return false
		}
	}
	return true
}

// Cleanup queues the hooks that an event selects and runs them one event at a time.
type Cleanup struct {
	hooks []*Hook
	http  *http.Client
	queue chan job
}

type job struct {
	ev    automation.Event
	hooks []*Hook
}

// New returns the automation. Call Work to start running hooks.
func New(hooks []*Hook) *Cleanup {
	return &Cleanup{
		hooks: hooks,
		http:  &http.Client{},
		queue: make(chan job, queueSize),
	}
}

// Handle queues the hooks that the event selects.
func (c *Cleanup) Handle(_ context.Context, ev automation.Event) error {
	// A PROVISION or PLAN that completes is not a teardown.
	if strings.HasPrefix(ev.Type, "deployment.") && ev.Metadata("deployment_action") != "DECOMMISSION" {
		return nil
	}
	var selected []*Hook
	for _, h := range c.hooks {
		if h.selects(ev) {
			selected = append(selected, h)
		}
	}
	if len(selected) == 0 {
		return nil
	}
	select {
	case c.queue <- job{ev: ev, hooks: selected}:
		return nil
	default:
		// A 500 makes Massdriver retry the event later, when the queue has room.
		return errors.New("hook queue is full")
	}
}

// Work runs queued hooks until the process exits.
func (c *Cleanup) Work() {
	for j := range c.queue {
		c.process(j)
	}
}

func (c *Cleanup) process(j job) {
	var data map[string]any
	if err := json.Unmarshal(j.ev.Raw, &data); err != nil {
		slog.Error("decode event", "automation", automationName, "event_id", j.ev.ID, "error", err)
		return
	}
	for _, h := range j.hooks {
		log := slog.With("automation", automationName, "event_id", j.ev.ID, "type", j.ev.Type, "hook", h.Name)
		start := time.Now()
		result, err := c.run(h, j.ev, data)
		if err != nil {
			log.Error("hook failed", "error", err, "output", result, "duration", time.Since(start))
			continue
		}
		log.Info("hook succeeded", "output", result, "duration", time.Since(start))
	}
}

func (c *Cleanup) run(h *Hook, ev automation.Event, data map[string]any) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), h.Timeout)
	defer cancel()
	render := func(key string) (string, error) {
		t, err := h.templates[key].Clone()
		if err != nil {
			return "", err
		}
		var b strings.Builder
		if err := t.Funcs(funcs(ev)).Execute(&b, data); err != nil {
			return "", err
		}
		return b.String(), nil
	}
	if h.HTTP != nil {
		return c.runHTTP(ctx, h, render)
	}
	return runExec(ctx, h, ev, render)
}

func (c *Cleanup) runHTTP(ctx context.Context, h *Hook, render func(string) (string, error)) (string, error) {
	url, err := render("url")
	if err != nil {
		return "", err
	}
	body, err := render("body")
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, h.HTTP.Method, url, strings.NewReader(body))
	if err != nil {
		return "", err
	}
	for k := range h.HTTP.Headers {
		v, err := render("header " + k)
		if err != nil {
			return "", err
		}
		req.Header.Set(k, v)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(io.LimitReader(resp.Body, maxOutputBytes))
	result := fmt.Sprintf("HTTP %d: %s", resp.StatusCode, bytes.TrimSpace(out))
	if resp.StatusCode/100 != 2 {
		return result, fmt.Errorf("%s %s: HTTP %d", h.HTTP.Method, req.URL.Redacted(), resp.StatusCode)
	}
	return result, nil
}

func runExec(ctx context.Context, h *Hook, ev automation.Event, render func(string) (string, error)) (string, error) {
	args := make([]string, len(h.Exec.Command))
	for i := range h.Exec.Command {
		arg, err := render(fmt.Sprintf("arg %d", i))
		if err != nil {
			return "", err
		}
		args[i] = arg
	}
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	cmd.Env = commandEnv(ev)
	cmd.Stdin = bytes.NewReader(ev.Raw)
	cmd.WaitDelay = 5 * time.Second
	var out limitedBuffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	if ctx.Err() != nil {
		err = fmt.Errorf("timed out after %s", h.Timeout)
	}
	return strings.TrimSpace(out.String()), err
}

var nonEnvChars = regexp.MustCompile(`[^A-Z0-9]+`)

func envKey(prefix, key string) string {
	return prefix + nonEnvChars.ReplaceAllString(strings.ToUpper(key), "_")
}

// A hook command gets the event, but not the credentials of the automation.
func commandEnv(ev automation.Event) []string {
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "MASSDRIVER_SIGNING_SECRET=") && !strings.HasPrefix(kv, "MASSDRIVER_API_KEY=") {
			env = append(env, kv)
		}
	}
	env = append(env,
		"MD_EVENT_ID="+ev.ID,
		"MD_EVENT_TYPE="+ev.Type,
		"MD_EVENT_SUBJECT="+ev.Subject,
		"MD_EVENT_TIME="+ev.Time.UTC().Format(time.RFC3339),
	)
	for k := range ev.Data.Attributes {
		env = append(env, envKey("MD_ATTR_", k)+"="+ev.Attribute(k))
	}
	for k := range ev.Data.Metadata {
		env = append(env, envKey("MD_METADATA_", k)+"="+ev.Metadata(k))
	}
	return env
}

var placeholderFuncs = template.FuncMap{
	"attr": func(string) (string, error) { return "", nil },
	"json": func(any) (string, error) { return "", nil },
	"env":  os.Getenv,
}

func funcs(ev automation.Event) template.FuncMap {
	return template.FuncMap{
		"attr": func(key string) (string, error) {
			if _, ok := ev.Data.Attributes[key]; !ok {
				return "", fmt.Errorf("event has no attribute %q", key)
			}
			return ev.Attribute(key), nil
		},
		"json": func(v any) (string, error) {
			b, err := json.Marshal(v)
			return string(b), err
		},
		"env": os.Getenv,
	}
}

type limitedBuffer struct{ bytes.Buffer }

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if room := maxOutputBytes - b.Len(); room > 0 {
		b.Buffer.Write(p[:min(len(p), room)])
	}
	return len(p), nil
}
