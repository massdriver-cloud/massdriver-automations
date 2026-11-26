package freezeguard

import (
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/massdriver-cloud/massdriver-automations/pkg/automation"
	"github.com/massdriver-cloud/massdriver-automations/pkg/slack"
)

// DefaultActions are the actions that change infrastructure. instance:deploy also gates approving and rejecting a
// proposal. instance:propose, instance:plan, and instance:configure stay allowed, so people can prepare changes
// during a freeze.
var DefaultActions = []string{"instance:deploy", "instance:decommission", "environment:deploy", "environment:decommission"}

// Window is one freeze. It is either weekly (recurs every week) or a fixed range between two times.
type Window struct {
	Name string
	// Conditions become the deny policy's conditions. Nil denies the actions on every entity.
	Conditions   map[string][]string
	Actions      []string
	ExemptGroups []string
	Slack        slack.Config
	// Weekly ranges are minutes since Sunday 00:00 in loc. A range whose end is before its start wraps past Saturday.
	weekly   [][2]int
	from, to time.Time
	loc      *time.Location
}

// values reads a YAML string or a list of strings.
type values []string

func (v *values) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		*v = values{n.Value}
		return nil
	}
	var list []string
	if err := n.Decode(&list); err != nil {
		return err
	}
	*v = list
	return nil
}

type fileWindow struct {
	Name         string            `yaml:"name"`
	Match        map[string]values `yaml:"match"`
	Actions      []string          `yaml:"actions"`
	ExemptGroups []string          `yaml:"exempt_groups"`
	Timezone     string            `yaml:"timezone"`
	Weekly       []struct {
		From string `yaml:"from"`
		To   string `yaml:"to"`
	} `yaml:"weekly"`
	From  string       `yaml:"from"`
	To    string       `yaml:"to"`
	Slack slack.Config `yaml:"slack"`
}

// Config is the config file of freeze-guard. No windows is valid: freeze-guard then removes the policies it made.
type Config struct {
	Timezone     string       `yaml:"timezone"`
	Actions      []string     `yaml:"actions"`
	ExemptGroups []string     `yaml:"exempt_groups"`
	Windows      []fileWindow `yaml:"windows"`
	// Slack is where announcements go when a window does not name its own channel.
	Slack slack.Config `yaml:"slack"`

	windows []Window
}

// ParseWindows decodes and checks a config document and returns its windows. YAML and JSON both parse.
func ParseWindows(b []byte) ([]Window, error) {
	var c Config
	if err := automation.DecodeConfig(b, &c); err != nil {
		return nil, err
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return c.windows, nil
}

// channels returns every channel that the config names.
func (c *Config) channels() []string {
	out := []string{c.Slack.Channel}
	for _, w := range c.Windows {
		out = append(out, w.Slack.Channel)
	}
	return out
}

// Validate checks every window and builds it.
func (c *Config) Validate() error {
	file := c
	var out []Window
	seen := map[string]bool{}
	for i, fw := range file.Windows {
		name := fw.Name
		if name == "" {
			return fmt.Errorf("windows[%d]: name is required", i)
		}
		if seen[name] {
			return fmt.Errorf("windows[%d].name is used by an earlier window", i)
		}
		seen[name] = true
		tz := fw.Timezone
		if tz == "" {
			tz = file.Timezone
		}
		if tz == "" {
			tz = "UTC"
		}
		loc, err := time.LoadLocation(tz)
		if err != nil {
			return fmt.Errorf("windows[%d].timezone is not an IANA zone", i)
		}

		actions := fw.Actions
		if len(actions) == 0 {
			actions = file.Actions
		}
		if len(actions) == 0 {
			actions = DefaultActions
		}
		for _, a := range actions {
			if entity, verb, ok := strings.Cut(a, ":"); !ok || entity == "" || verb == "" {
				return fmt.Errorf("windows[%d].actions: each action must be like instance:deploy", i)
			}
		}

		w := Window{
			Name:         name,
			Actions:      sortedUnique(actions),
			ExemptGroups: sortedUnique(append(slices.Clone(file.ExemptGroups), fw.ExemptGroups...)),
			Slack:        fw.Slack.Or(file.Slack),
			loc:          loc,
		}
		if len(fw.Match) > 0 {
			w.Conditions = map[string][]string{}
			for k, v := range fw.Match {
				if len(v) == 0 {
					return fmt.Errorf("windows[%d].match.%s has no value", i, k)
				}
				w.Conditions[k] = sortedUnique(v)
			}
		}

		fixed := fw.From != "" || fw.To != ""
		if fixed == (len(fw.Weekly) > 0) {
			return fmt.Errorf("windows[%d]: set either weekly or from and to", i)
		}
		if fixed {
			if w.from, err = parseTime(fw.From, loc); err != nil {
				return fmt.Errorf("windows[%d]: from: %w", i, err)
			}
			if w.to, err = parseTime(fw.To, loc); err != nil {
				return fmt.Errorf("windows[%d]: to: %w", i, err)
			}
			if !w.to.After(w.from) {
				return fmt.Errorf("windows[%d]: to must be after from", i)
			}
		}
		for _, r := range fw.Weekly {
			from, err := parseWeekTime(r.From)
			if err != nil {
				return fmt.Errorf("windows[%d]: weekly from: %w", i, err)
			}
			to, err := parseWeekTime(r.To)
			if err != nil {
				return fmt.Errorf("windows[%d]: weekly to: %w", i, err)
			}
			if from == to {
				return fmt.Errorf("windows[%d]: weekly from and to are the same", i)
			}
			w.weekly = append(w.weekly, [2]int{from, to})
		}
		out = append(out, w)
	}
	c.windows = out
	return nil
}

func sortedUnique(in []string) []string {
	out := slices.Clone(in)
	sort.Strings(out)
	return slices.Compact(out)
}

func parseTime(s string, loc *time.Location) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	for _, layout := range []string{"2006-01-02 15:04", "2006-01-02T15:04", "2006-01-02"} {
		if t, err := time.ParseInLocation(layout, s, loc); err == nil {
			return t, nil
		}
	}
	return time.Time{}, errors.New("want RFC 3339, YYYY-MM-DD HH:MM, or YYYY-MM-DD")
}

var weekdays = map[string]int{"sun": 0, "mon": 1, "tue": 2, "wed": 3, "thu": 4, "fri": 5, "sat": 6}

// parseWeekTime reads "Fri 17:00" as minutes since Sunday 00:00.
func parseWeekTime(s string) (int, error) {
	day, clock, ok := strings.Cut(strings.TrimSpace(s), " ")
	d, known := weekdays[strings.ToLower(day)[:min(3, len(day))]]
	if !ok || !known {
		return 0, errors.New(`want a day and a time, such as "Fri 17:00"`)
	}
	t, err := time.Parse("15:04", strings.TrimSpace(clock))
	if err != nil {
		return 0, errors.New(`want a day and a time, such as "Fri 17:00"`)
	}
	return d*24*60 + t.Hour()*60 + t.Minute(), nil
}

// Contains reports whether t is inside the window.
func (w Window) Contains(t time.Time) bool {
	if len(w.weekly) == 0 {
		return !t.Before(w.from) && t.Before(w.to)
	}
	l := t.In(w.loc)
	m := int(l.Weekday())*24*60 + l.Hour()*60 + l.Minute()
	for _, r := range w.weekly {
		if r[0] < r[1] && m >= r[0] && m < r[1] {
			return true
		}
		if r[0] > r[1] && (m >= r[0] || m < r[1]) {
			return true
		}
	}
	return false
}

// NextStart returns the first start of the window after t. A fixed window does not start again, so it returns the
// window's end.
func (w Window) NextStart(t time.Time) time.Time {
	if len(w.weekly) == 0 {
		return w.to
	}
	l := t.In(w.loc)
	var next time.Time
	for _, r := range w.weekly {
		day := l.Day() - int(l.Weekday()) + r[0]/(24*60)
		s := time.Date(l.Year(), l.Month(), day, (r[0]%(24*60))/60, r[0]%60, 0, 0, w.loc)
		if !s.After(t) {
			s = time.Date(l.Year(), l.Month(), day+7, (r[0]%(24*60))/60, r[0]%60, 0, 0, w.loc)
		}
		if next.IsZero() || s.Before(next) {
			next = s
		}
	}
	return next
}

// Exempt reports whether the window leaves a group unfrozen. An exempt group is named by its name or its ID.
func (w Window) Exempt(id, name string) bool {
	return slices.Contains(w.ExemptGroups, id) || slices.Contains(w.ExemptGroups, name)
}

// conditionsInput is the `conditions` of createGroupPolicy: "*" or a JSON object.
func (w Window) conditionsInput() string {
	if len(w.Conditions) == 0 {
		return "*"
	}
	m := make(map[string]any, len(w.Conditions))
	for k, v := range w.Conditions {
		if len(v) == 1 && v[0] == "*" {
			m[k] = "*"
		} else {
			m[k] = v
		}
	}
	s, _ := jsonString(m)
	return s
}

// describeConditions renders the conditions for a Slack message.
func (w Window) describeConditions() string {
	if len(w.Conditions) == 0 {
		return "on every instance and environment"
	}
	keys := make([]string, 0, len(w.Conditions))
	for k := range w.Conditions {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		v := w.Conditions[k]
		switch {
		case len(v) == 1 && v[0] == "*":
			parts[i] = k + " has any value"
		case len(v) == 1:
			parts[i] = k + " is " + v[0]
		default:
			parts[i] = k + " is one of " + strings.Join(v, ", ")
		}
	}
	return "where " + strings.Join(parts, " and ")
}
