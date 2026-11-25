package autodeploydownstream

import (
	"bytes"
	"errors"
	"fmt"
	"os"
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

// LoadConfig reads the rules file.
func LoadConfig(path string) ([]Rule, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("AUTODEPLOY_CONFIG: %w", err)
	}
	rules, err := ParseConfig(b)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return rules, nil
}

// ParseConfig parses the YAML of a rules file.
func ParseConfig(b []byte) ([]Rule, error) {
	var file struct {
		Rules []Rule `yaml:"rules"`
	}
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&file); err != nil {
		return nil, err
	}
	if len(file.Rules) == 0 {
		return nil, errors.New("rules: at least one rule is required")
	}
	seen := map[string]bool{}
	for i := range file.Rules {
		r := &file.Rules[i]
		if r.Name == "" {
			return nil, fmt.Errorf("rules[%d]: name is required", i)
		}
		if seen[r.Name] {
			return nil, fmt.Errorf("%s: name is used by two rules", r.Name)
		}
		seen[r.Name] = true
		if len(r.Downstream) == 0 {
			return nil, fmt.Errorf("%s: downstream needs at least one attribute", r.Name)
		}
		for k, v := range r.Downstream {
			if len(v) == 0 {
				return nil, fmt.Errorf("%s: downstream %s has no values", r.Name, k)
			}
		}
		for k, v := range r.Upstream {
			if len(v) == 0 {
				return nil, fmt.Errorf("%s: upstream %s has no values", r.Name, k)
			}
		}
		if r.Action == "" {
			r.Action = ActionDeploy
		}
		if r.Action != ActionDeploy && r.Action != ActionPropose {
			return nil, fmt.Errorf("%s: action %q: want deploy or propose", r.Name, r.Action)
		}
	}
	return file.Rules, nil
}

// RulesFromEnv returns the rules file at AUTODEPLOY_CONFIG, or one deploy rule built from AUTO_DEPLOY_ATTR_KEY and
// AUTO_DEPLOY_ATTR_VALUE. getenv is os.Getenv outside tests.
func RulesFromEnv(getenv func(string) string) ([]Rule, error) {
	path, key, value := getenv("AUTODEPLOY_CONFIG"), getenv("AUTO_DEPLOY_ATTR_KEY"), getenv("AUTO_DEPLOY_ATTR_VALUE")
	if path != "" {
		if key != "" || value != "" {
			return nil, errors.New("set AUTODEPLOY_CONFIG or AUTO_DEPLOY_ATTR_KEY and AUTO_DEPLOY_ATTR_VALUE, not both")
		}
		return LoadConfig(path)
	}
	if key == "" {
		key = "autodeploy"
	}
	if value == "" {
		value = "true"
	}
	return []Rule{{
		Name:       key + "=" + value,
		Downstream: map[string]Values{key: {value}},
		Action:     ActionDeploy,
	}}, nil
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
