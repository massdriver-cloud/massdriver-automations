package autodeploydownstream

import (
	"fmt"
	"slices"

	"gopkg.in/yaml.v3"
)

// Actions of a rule.
const (
	ActionDeploy  = "deploy"
	ActionPropose = "propose"
)

// Rule selects downstream instances to deploy.
type Rule struct {
	Name string `yaml:"name"`
	// Downstream holds the effective attributes that a downstream instance must have. Each key must match one of
	// its values.
	Downstream map[string]Values `yaml:"downstream"`
	// Upstream holds the attributes that the upstream's deployment event must have.
	Upstream map[string]Values `yaml:"upstream"`
	Action   string            `yaml:"action"`
}

// Values reads a YAML scalar or a list of scalars. Unquoted true and 1 read as the strings "true" and "1".
type Values []string

func (v *Values) UnmarshalYAML(n *yaml.Node) error {
	switch n.Kind {
	case yaml.ScalarNode:
		*v = Values{n.Value}
		return nil
	case yaml.SequenceNode:
		out := make(Values, 0, len(n.Content))
		for _, item := range n.Content {
			if item.Kind != yaml.ScalarNode {
				return fmt.Errorf("line %d: want a value or a list of values", item.Line)
			}
			out = append(out, item.Value)
		}
		*v = out
		return nil
	default:
		return fmt.Errorf("line %d: want a value or a list of values", n.Line)
	}
}

// Config is the config file of autodeploy-downstream.
type Config struct {
	Rules []Rule `yaml:"rules"`
}

// DefaultRule deploys the instances whose attribute autodeploy is "true". It applies when the config has no rules.
var DefaultRule = Rule{Name: "autodeploy=true", Downstream: map[string]Values{"autodeploy": {"true"}}, Action: ActionDeploy}

// Validate checks the rules and fills their defaults.
func (c *Config) Validate() error {
	if len(c.Rules) == 0 {
		c.Rules = []Rule{DefaultRule}
		return nil
	}
	seen := map[string]bool{}
	for i := range c.Rules {
		r := &c.Rules[i]
		if r.Name == "" {
			return fmt.Errorf("rules[%d]: name is required", i)
		}
		if seen[r.Name] {
			return fmt.Errorf("rules[%d].name is used by an earlier rule", i)
		}
		seen[r.Name] = true
		if len(r.Downstream) == 0 {
			return fmt.Errorf("rules[%d].downstream needs at least one attribute", i)
		}
		for k, v := range r.Downstream {
			if len(v) == 0 {
				return fmt.Errorf("rules[%d].downstream.%s has no values", i, k)
			}
		}
		for k, v := range r.Upstream {
			if len(v) == 0 {
				return fmt.Errorf("rules[%d].upstream.%s has no values", i, k)
			}
		}
		if r.Action == "" {
			r.Action = ActionDeploy
		}
		if r.Action != ActionDeploy && r.Action != ActionPropose {
			return fmt.Errorf("rules[%d].action: want deploy or propose", i)
		}
	}
	return nil
}

// matches reports whether every key of want is in attrs with one of its values. A missing key never matches.
func matches(want map[string]Values, attrs map[string]any) bool {
	for k, vals := range want {
		v, ok := attrs[k]
		if !ok || v == nil {
			return false
		}
		if !slices.Contains(vals, fmt.Sprint(v)) {
			return false
		}
	}
	return true
}
