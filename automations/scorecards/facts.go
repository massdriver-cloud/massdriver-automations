package scorecards

import (
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

// tool is the `tool` field of every value scorecards writes.
const tool = "scorecards"

// Delivery is the value of md/v1/delivery.
type Delivery struct {
	Tool                  string   `json:"tool"`
	WindowDays            int      `json:"window_days"`
	Deployments           int      `json:"deployments"`
	Failed                int      `json:"failed"`
	RolledBack            int      `json:"rolled_back"`
	Automated             int      `json:"automated"`
	FailureRate           *float64 `json:"failure_rate,omitempty"`
	MedianDurationSeconds *float64 `json:"median_duration_seconds,omitempty"`
	LastStatus            string   `json:"last_status,omitempty"`
	LastCompletedAt       string   `json:"last_completed_at,omitempty"`
}

// Reliability is the value of md/v1/reliability.
type Reliability struct {
	Tool                  string   `json:"tool"`
	WindowDays            int      `json:"window_days"`
	Alarms                int      `json:"alarms"`
	Firing                int      `json:"firing"`
	Incidents             int      `json:"incidents"`
	SecondsInAlarm        float64  `json:"seconds_in_alarm"`
	MedianRecoverySeconds *float64 `json:"median_recovery_seconds,omitempty"`
	LastFiredAt           string   `json:"last_fired_at,omitempty"`
}

// Currency is the value of md/v1/currency.
type Currency struct {
	Tool              string `json:"tool"`
	Version           string `json:"version"`
	Latest            string `json:"latest"`
	Behind            int    `json:"behind"`
	LatestPublishedAt string `json:"latest_published_at,omitempty"`
	DaysBehind        *int   `json:"days_behind,omitempty"`
}

// Pending is the value of md/v1/pending.
type Pending struct {
	Tool       string `json:"tool"`
	Changed    bool   `json:"changed"`
	DeployedAt string `json:"deployed_at,omitempty"`
}

// Parity is the value of md/v1/parity.
type Parity struct {
	Tool                 string       `json:"tool"`
	ReferenceEnvironment string       `json:"reference_environment"`
	Differences          int          `json:"differences"`
	CheckedAt            string       `json:"checked_at"`
	Items                []ParityItem `json:"items,omitempty"`
}

// ParityItem is one difference between an environment and its reference.
type ParityItem struct {
	Instance string `json:"instance"`
	Field    string `json:"field"`
	Message  string `json:"message"`
}

// Ownership is the value of md/v1/ownership.
type Ownership struct {
	Tool    string       `json:"tool"`
	Groups  []GroupOwner `json:"groups"`
	Members int          `json:"members"`
}

// GroupOwner is a group that owns a project or an environment.
type GroupOwner struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

const maxItems = 50

func timestamp(t time.Time) string { return t.UTC().Format(time.RFC3339) }

func windowDays(w time.Duration) int { return int(w / (24 * time.Hour)) }

// delivery counts the deployments that ran in the window. A rejected deployment never ran, so it counts only as
// the last status. automated holds the IDs of the deployments that a service account started.
func delivery(window time.Duration, deps []Deployment, automated map[string]bool, latest *Deployment) Delivery {
	d := Delivery{Tool: tool, WindowDays: windowDays(window)}
	var durations []float64
	// deps are newest first; a rollback is a completed provision of a lower version than the completed provision
	// before it.
	var completed []Deployment
	for _, dep := range deps {
		if d.LastStatus == "" && terminal(dep.Status) {
			d.LastStatus = strings.ToLower(dep.Status)
		}
		switch dep.Status {
		case "COMPLETED", "FAILED", "ABORTED":
		default:
			continue
		}
		d.Deployments++
		durations = append(durations, float64(dep.ElapsedTime))
		if automated[dep.ID] {
			d.Automated++
		}
		switch dep.Status {
		case "FAILED":
			d.Failed++
		case "COMPLETED":
			if d.LastCompletedAt == "" && dep.LastTransitionedAt != nil {
				d.LastCompletedAt = timestamp(*dep.LastTransitionedAt)
			}
			if dep.Action == "PROVISION" {
				completed = append(completed, dep)
			}
		}
	}
	for i := 0; i+1 < len(completed); i++ {
		if compareVersions(completed[i].Version, completed[i+1].Version) < 0 {
			d.RolledBack++
		}
	}
	if d.Deployments > 0 {
		rate := round(float64(d.Failed)/float64(d.Deployments), 4)
		d.FailureRate = &rate
		m := median(durations)
		d.MedianDurationSeconds = &m
	}
	if d.LastCompletedAt == "" && latest != nil && latest.Status == "COMPLETED" && latest.LastTransitionedAt != nil {
		d.LastCompletedAt = timestamp(*latest.LastTransitionedAt)
	}
	return d
}

func terminal(status string) bool {
	switch status {
	case "COMPLETED", "FAILED", "ABORTED", "REJECTED":
		return true
	}
	return false
}

// reliability reads the alarm state changes of the window, oldest first. Each change names its alarm. A change to
// OK as the first change of an alarm in the window means the alarm fired before the window started.
func reliability(now time.Time, window time.Duration, alarms []Alarm, changes []AuditEvent) Reliability {
	r := Reliability{Tool: tool, WindowDays: windowDays(window), Alarms: len(alarms)}
	start := now.Add(-window)
	var lastFired time.Time
	for _, a := range alarms {
		if a.CurrentState != nil && a.CurrentState.Status == "ALARM" {
			r.Firing++
			if a.CurrentState.OccurredAt.After(lastFired) {
				lastFired = a.CurrentState.OccurredAt
			}
		}
	}

	byAlarm := map[string][]AuditEvent{}
	var names []string
	for _, c := range changes {
		name := entityString(c.Data, "alarm_id")
		if _, ok := byAlarm[name]; !ok {
			names = append(names, name)
		}
		byAlarm[name] = append(byAlarm[name], c)
	}
	var recoveries []float64
	var inAlarm time.Duration
	for _, name := range names {
		var firedAt time.Time
		firing := false
		for i, c := range byAlarm[name] {
			switch entityString(c.Data, "status") {
			case "ALARM":
				if !firing {
					firing, firedAt = true, c.OccurredAt
					r.Incidents++
					if c.OccurredAt.After(lastFired) {
						lastFired = c.OccurredAt
					}
				}
			default:
				switch {
				case firing:
					inAlarm += c.OccurredAt.Sub(firedAt)
					if entityString(c.Data, "status") == "OK" {
						recoveries = append(recoveries, c.OccurredAt.Sub(firedAt).Seconds())
					}
					firing = false
				case i == 0 && previousState(c.Data) == "ALARM":
					inAlarm += c.OccurredAt.Sub(start)
				}
			}
		}
		if firing {
			inAlarm += now.Sub(firedAt)
		}
	}
	// An alarm that has fired since before the window, with no change in it, was in alarm the whole window.
	for _, a := range alarms {
		if _, changed := byAlarm[a.ID]; changed || a.CurrentState == nil {
			continue
		}
		if a.CurrentState.Status == "ALARM" && a.CurrentState.OccurredAt.Before(start) {
			inAlarm += window
		}
	}
	r.SecondsInAlarm = math.Round(inAlarm.Seconds())
	if len(recoveries) > 0 {
		m := median(recoveries)
		r.MedianRecoverySeconds = &m
	}
	if !lastFired.IsZero() {
		r.LastFiredAt = timestamp(lastFired)
	}
	return r
}

// previousState is the state an alarm left, from the CloudWatch notification of the change, or "" when the
// notification does not say.
func previousState(data map[string]any) string {
	entity, _ := data["entity"].(map[string]any)
	notification, _ := entity["notification"].(map[string]any)
	message, _ := notification["Message"].(string)
	var m struct {
		OldStateValue string `json:"OldStateValue"`
	}
	if json.Unmarshal([]byte(message), &m) != nil {
		return ""
	}
	return m.OldStateValue
}

func entityString(data map[string]any, key string) string {
	entity, _ := data["entity"].(map[string]any)
	s, _ := entity[key].(string)
	return s
}

// currency compares the version an instance runs with the published versions of its bundle. A stable version is
// compared with the stable versions by semantic version. A development build is compared by publish time with every
// version published after it, because a build of 1.2.0-dev is newer work than the 1.2.0 it is named after. ok is
// false for an instance that was never deployed or a bundle with no version.
func currency(now time.Time, deployed string, tags []Tag) (Currency, bool) {
	if deployed == "" || len(tags) == 0 {
		return Currency{}, false
	}
	newest := tags[0]
	for _, t := range tags {
		if t.CreatedAt.After(newest.CreatedAt) {
			newest = t
		}
	}
	newer := func(t Tag) bool { return compareVersions(t.Tag, deployed) > 0 }
	if i := slices.IndexFunc(tags, func(t Tag) bool { return t.Tag == deployed }); i >= 0 && strings.Contains(deployed, "-") {
		at := tags[i].CreatedAt
		newer = func(t Tag) bool { return t.CreatedAt.After(at) }
	} else {
		for _, t := range tags {
			if compareVersions(t.Tag, newest.Tag) > 0 {
				newest = t
			}
		}
	}
	c := Currency{Tool: tool, Version: clip(deployed, 128), Latest: clip(newest.Tag, 128),
		LatestPublishedAt: timestamp(newest.CreatedAt)}
	var oldestNewer time.Time
	for _, t := range tags {
		if newer(t) {
			c.Behind++
			if oldestNewer.IsZero() || t.CreatedAt.Before(oldestNewer) {
				oldestNewer = t.CreatedAt
			}
		}
	}
	// days_behind is how long a newer version has been out: since the first version after the deployed one.
	days := 0
	if c.Behind > 0 {
		days = max(0, int(now.Sub(oldestNewer)/(24*time.Hour)))
	}
	c.DaysBehind = &days
	return c, true
}

// pending reports whether the next deployment would change the instance: its params differ from those of its
// latest provision, or it resolves to another bundle version than the one deployed.
func pending(inst *Instance) Pending {
	p := Pending{Tool: tool}
	latest := inst.LatestProvision
	if latest == nil {
		p.Changed = inst.Params != nil
		return p
	}
	if latest.LastTransitionedAt != nil && latest.Status == "COMPLETED" {
		p.DeployedAt = timestamp(*latest.LastTransitionedAt)
	}
	p.Changed = !reflect.DeepEqual(normalizeParams(inst.Params), normalizeParams(latest.Params)) ||
		(inst.DeployedVersion != "" && inst.ResolvedVersion != inst.DeployedVersion)
	return p
}

// normalizeParams treats no params and empty params as the same, and leaves out what Massdriver adds.
func normalizeParams(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		// Massdriver adds md_metadata to a deployment's params. The instance's params never have it.
		if k != "md_metadata" {
			out[k] = v
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// parity lists the differences between an environment and its reference: a component on one side only, another
// bundle version, or a param leaf with another value.
func parity(now time.Time, reference string, comparisons []InstanceComparison) Parity {
	p := Parity{Tool: tool, ReferenceEnvironment: reference, CheckedAt: timestamp(now)}
	add := func(instance, field, message string) {
		p.Differences++
		if len(p.Items) < maxItems {
			p.Items = append(p.Items, ParityItem{Instance: clip(instance, 255), Field: clip(field, 255), Message: clip(message, 280)})
		}
	}
	for _, c := range comparisons {
		if c.Equal {
			continue
		}
		name := c.Component.ID
		if c.Source != nil {
			name = c.Source.ID
		}
		switch {
		case c.Source == nil:
			add(name, "instance", "Only in "+reference+".")
			continue
		case c.Target == nil:
			add(name, "instance", "Not in "+reference+".")
			continue
		}
		if !c.Version.Equal {
			add(name, "version", fmt.Sprintf("Version %s here, %s in %s.", deref(c.Version.Source), deref(c.Version.Target), reference))
		}
		for _, param := range c.Params {
			if !param.Equal {
				add(name, param.Path, "Differs from "+reference+".")
			}
		}
	}
	return p
}

func deref(s *string) string {
	if s == nil {
		return "none"
	}
	return *s
}

func clip(s string, n int) string {
	if len([]rune(s)) <= n {
		return s
	}
	return string([]rune(s)[:n-3]) + "..."
}

func median(vs []float64) float64 {
	s := append([]float64(nil), vs...)
	sort.Float64s(s)
	n := len(s)
	if n%2 == 1 {
		return s[n/2]
	}
	return (s[n/2-1] + s[n/2]) / 2
}

func round(v float64, places int) float64 {
	p := math.Pow(10, float64(places))
	return math.Round(v*p) / p
}

// compareVersions compares two semantic versions: -1, 0, or 1. A pre-release is lower than its release.
func compareVersions(a, b string) int {
	ac, ap, _ := strings.Cut(strings.TrimPrefix(a, "v"), "-")
	bc, bp, _ := strings.Cut(strings.TrimPrefix(b, "v"), "-")
	as, bs := strings.Split(ac, "."), strings.Split(bc, ".")
	for i := 0; i < max(len(as), len(bs)); i++ {
		x, y := part(as, i), part(bs, i)
		if x != y {
			if x < y {
				return -1
			}
			return 1
		}
	}
	switch {
	case ap == bp:
		return 0
	case ap == "":
		return 1
	case bp == "":
		return -1
	case ap < bp:
		return -1
	}
	return 1
}

func part(parts []string, i int) int {
	if i >= len(parts) {
		return 0
	}
	n, _ := strconv.Atoi(strings.Split(parts[i], "+")[0])
	return n
}
