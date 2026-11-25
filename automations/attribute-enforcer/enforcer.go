// Package attributeenforcer checks the attributes of new projects, environments, and components, sets defaults
// for missing keys, and reports the rest to Slack.
package attributeenforcer

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"slices"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/massdriver-cloud/massdriver-automations/pkg/automation"
	"github.com/massdriver-cloud/massdriver-automations/pkg/massdriver"
)

var (
	//go:embed queries/project.graphql
	projectQuery string
	//go:embed queries/environment.graphql
	environmentQuery string
	//go:embed queries/component.graphql
	componentQuery string
	//go:embed queries/update_project.graphql
	updateProjectMutation string
	//go:embed queries/update_environment.graphql
	updateEnvironmentMutation string
	//go:embed queries/update_component.graphql
	updateComponentMutation string
)

// Actions.
const (
	SetDefault = "set_default"
	Notify     = "notify"
)

type kind struct {
	name     string
	label    string
	query    string
	mutation string
	update   string
}

var kinds = map[string]kind{
	"project.created":     {"project", "Project", projectQuery, updateProjectMutation, "updateProject"},
	"project.cloned":      {"project", "Project", projectQuery, updateProjectMutation, "updateProject"},
	"environment.created": {"environment", "Environment", environmentQuery, updateEnvironmentMutation, "updateEnvironment"},
	"component.created":   {"component", "Component", componentQuery, updateComponentMutation, "updateComponent"},
}

// Definition registers the automation.
var Definition = automation.Definition{
	Name:        "attribute-enforcer",
	Description: "Sets default attributes on new projects, environments, and components, and reports missing ones to Slack.",
	Events:      []string{"project.created", "project.cloned", "environment.created", "component.created"},
	New: func() (automation.Automation, error) {
		path := automation.Env("ATTRIBUTE_ENFORCER_CONFIG", "/etc/attribute-enforcer/config.yaml")
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		cfg, err := ParseConfig(b)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}

		var api *massdriver.Client
		if cfg.has(SetDefault) {
			if api, err = massdriver.FromEnv(); err != nil {
				return nil, err
			}
		}
		slackURL := os.Getenv("SLACK_WEBHOOK_URL")
		if cfg.has(Notify) && slackURL == "" {
			return nil, errors.New("missing required env vars: [SLACK_WEBHOOK_URL]")
		}
		return New(cfg, api, slackURL), nil
	},
}

// Rule requires one attribute key.
type Rule struct {
	Key     string   `yaml:"key"`
	Default string   `yaml:"default"`
	Allowed []string `yaml:"allowed"`
}

// Config is the YAML configuration file.
type Config struct {
	Actions     []string `yaml:"actions"`
	Project     []Rule   `yaml:"project"`
	Environment []Rule   `yaml:"environment"`
	Component   []Rule   `yaml:"component"`
}

func (c Config) has(action string) bool { return slices.Contains(c.Actions, action) }

func (c Config) rules(kind string) []Rule {
	switch kind {
	case "project":
		return c.Project
	case "environment":
		return c.Environment
	default:
		return c.Component
	}
}

// ParseConfig reads and checks the YAML configuration.
func ParseConfig(b []byte) (Config, error) {
	var c Config
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return c, err
	}
	if len(c.Actions) == 0 {
		return c, errors.New("actions: list set_default, notify, or both")
	}
	for _, a := range c.Actions {
		if a != SetDefault && a != Notify {
			return c, fmt.Errorf("actions: %q is not set_default or notify", a)
		}
	}
	for name, rules := range map[string][]Rule{"project": c.Project, "environment": c.Environment, "component": c.Component} {
		for i, r := range rules {
			if r.Key == "" {
				return c, fmt.Errorf("%s[%d]: key is required", name, i)
			}
			if r.Default != "" && len(r.Allowed) > 0 && !slices.Contains(r.Allowed, r.Default) {
				return c, fmt.Errorf("%s[%d]: default %q is not in allowed", name, i, r.Default)
			}
		}
	}
	return c, nil
}

// Enforcer holds the configuration, the API client, and the Slack webhook.
type Enforcer struct {
	cfg      Config
	api      *massdriver.Client
	slackURL string
	http     *http.Client
	run      func(func())
}

// New returns the automation. api may be nil when the actions do not include set_default.
func New(cfg Config, api *massdriver.Client, slackURL string) *Enforcer {
	return &Enforcer{cfg: cfg, api: api, slackURL: slackURL, http: http.DefaultClient, run: func(f func()) { go f() }}
}

type finding struct {
	rule  Rule
	value string
	// set is the default written to the entity; err is why writing it failed.
	set string
	err error
}

func (f finding) missing() bool { return f.value == "" }

// Handle checks the entity's attributes when the event recorded them, then fixes and reports after responding.
func (e *Enforcer) Handle(_ context.Context, ev automation.Event) error {
	k, ok := kinds[ev.Type]
	if !ok {
		return nil
	}
	var entity struct {
		Identifier string `json:"identifier"`
	}
	if err := ev.DecodeEntity(&entity); err != nil {
		return err
	}
	if entity.Identifier == "" {
		return fmt.Errorf("%s event has no entity identifier", ev.Type)
	}

	findings := check(e.cfg.rules(k.name), ev.Data.Attributes)
	if len(findings) == 0 {
		return nil
	}

	var actor struct {
		Name string `json:"name"`
	}
	_ = ev.DecodeActor(&actor)

	e.run(func() { e.enforce(ev.ID, k, entity.Identifier, actor.Name, findings) })
	return nil
}

// Attributes cascade, so a key set on the parent project counts for its environments and components.
func check(rules []Rule, effective map[string]any) []finding {
	var out []finding
	for _, r := range rules {
		v, _ := effective[r.Key].(string)
		if v == "" || (len(r.Allowed) > 0 && !slices.Contains(r.Allowed, v)) {
			out = append(out, finding{rule: r, value: v})
		}
	}
	return out
}

func (e *Enforcer) enforce(eventID string, k kind, id, actor string, findings []finding) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	log := slog.With("automation", Definition.Name, "event_id", eventID, k.name, id)

	if e.cfg.has(SetDefault) {
		findings = e.setDefaults(ctx, k, id, findings)
	}
	for _, f := range findings {
		switch {
		case f.set != "":
			log.Info("set default", "key", f.rule.Key, "value", f.set)
		case f.err != nil:
			log.Error("set default", "key", f.rule.Key, "error", f.err)
		default:
			log.Info("attribute not compliant", "key", f.rule.Key, "value", f.value)
		}
	}

	if e.cfg.has(Notify) && len(findings) > 0 {
		if err := e.notify(ctx, message(k, id, actor, findings)); err != nil {
			log.Error("notify", "error", err)
		}
	}
}

// setDefaults reads the entity again, so a key someone set after the event is left alone, and writes every
// missing key that has a default. The API replaces the whole attribute map, so the write includes the
// entity's own keys.
func (e *Enforcer) setDefaults(ctx context.Context, k kind, id string, findings []finding) []finding {
	if !slices.ContainsFunc(findings, settable) {
		return findings
	}

	var out map[string]struct {
		Attributes          map[string]any `json:"attributes"`
		EffectiveAttributes map[string]any `json:"effectiveAttributes"`
	}
	if err := e.api.Do(ctx, k.query, map[string]any{"org": e.api.OrganizationID, "id": id}, &out); err != nil {
		return result(findings, err)
	}
	current := out[k.name]
	findings = check(rulesOf(findings), current.EffectiveAttributes)

	attrs := map[string]any{}
	for key, v := range current.Attributes {
		attrs[key] = v
	}
	if !slices.ContainsFunc(findings, settable) {
		return findings
	}
	for _, f := range findings {
		if settable(f) {
			attrs[f.rule.Key] = f.rule.Default
		}
	}

	encoded, err := massdriver.JSONString(attrs)
	if err != nil {
		return result(findings, err)
	}
	var payload map[string]massdriver.Payload[struct {
		ID string `json:"id"`
	}]
	err = e.api.Do(ctx, k.mutation, map[string]any{
		"org":   e.api.OrganizationID,
		"id":    id,
		"input": map[string]any{"attributes": encoded},
	}, &payload)
	if err == nil {
		err = payload[k.update].Err()
	}
	return result(findings, err)
}

func settable(f finding) bool { return f.missing() && f.rule.Default != "" }

func result(findings []finding, err error) []finding {
	for i, f := range findings {
		if settable(f) {
			if err != nil {
				findings[i].err = err
			} else {
				findings[i].set = f.rule.Default
			}
		}
	}
	return findings
}

func rulesOf(findings []finding) []Rule {
	rules := make([]Rule, len(findings))
	for i, f := range findings {
		rules[i] = f.rule
	}
	return rules
}

func message(k kind, id, actor string, findings []finding) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s `%s` was created", k.label, id)
	if actor != "" {
		fmt.Fprintf(&b, " by %s", actor)
	}
	b.WriteString(" without the required attributes.")

	sort.Slice(findings, func(i, j int) bool { return findings[i].rule.Key < findings[j].rule.Key })
	for _, f := range findings {
		fmt.Fprintf(&b, "\n• `%s` ", f.rule.Key)
		switch {
		case f.set != "":
			fmt.Fprintf(&b, "was missing. Set it to `%s`.", f.set)
		case f.err != nil:
			fmt.Fprintf(&b, "is missing. Setting it to `%s` failed: %s", f.rule.Default, f.err)
		case f.missing():
			b.WriteString("is missing.")
		default:
			fmt.Fprintf(&b, "is `%s`. Allowed: `%s`.", f.value, strings.Join(f.rule.Allowed, "`, `"))
		}
	}
	return b.String()
}

func (e *Enforcer) notify(ctx context.Context, text string) error {
	body, _ := json.Marshal(map[string]string{"text": text})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.slackURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("content-type", "application/json")
	resp, err := e.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("slack: HTTP %d: %s", resp.StatusCode, b)
	}
	return nil
}
