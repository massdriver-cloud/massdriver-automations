// Package freezeguard aborts deployments that start inside a change freeze window.
package freezeguard

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
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/massdriver-cloud/massdriver-automations/pkg/automation"
	"github.com/massdriver-cloud/massdriver-automations/pkg/massdriver"
)

//go:embed queries/abort_deployment.graphql
var abortDeploymentMutation string

// Definition registers the automation.
var Definition = automation.Definition{
	Name:        "freeze-guard",
	Description: "Aborts deployments that start inside a change freeze window.",
	Events:      []string{"deployment.created", "deployment.enqueued"},
	New: func() (automation.Automation, error) {
		api, err := massdriver.FromEnv()
		if err != nil {
			return nil, err
		}
		v, err := automation.RequireEnv("FREEZE_WINDOWS_FILE")
		if err != nil {
			return nil, err
		}
		b, err := os.ReadFile(v["FREEZE_WINDOWS_FILE"])
		if err != nil {
			return nil, err
		}
		windows, err := ParseWindows(b)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", v["FREEZE_WINDOWS_FILE"], err)
		}
		return New(api, windows, automation.Env("FREEZE_OVERRIDE_TOKEN", "[freeze-override]"),
			automation.Env("SLACK_WEBHOOK_URL", "")), nil
	},
}

// --- windows ---------------------------------------------------------------

// Window is one freeze. It is either weekly (recurs every week) or a fixed range between two times.
type Window struct {
	Name  string
	Match map[string]string
	// Weekly ranges are minutes since Sunday 00:00 in loc. A range whose end is before its start wraps past Saturday.
	weekly   [][2]int
	from, to time.Time
	loc      *time.Location
}

type fileWindow struct {
	Name     string            `yaml:"name"`
	Match    map[string]string `yaml:"match"`
	Timezone string            `yaml:"timezone"`
	Weekly   []struct {
		From string `yaml:"from"`
		To   string `yaml:"to"`
	} `yaml:"weekly"`
	From string `yaml:"from"`
	To   string `yaml:"to"`
}

// ParseWindows reads the windows file. YAML and JSON both parse.
func ParseWindows(b []byte) ([]Window, error) {
	var file struct {
		Timezone string       `yaml:"timezone"`
		Windows  []fileWindow `yaml:"windows"`
	}
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&file); err != nil {
		return nil, err
	}

	var out []Window
	for i, fw := range file.Windows {
		name := fw.Name
		if name == "" {
			name = fmt.Sprintf("windows[%d]", i)
		}
		tz := fw.Timezone
		if tz == "" {
			tz = file.Timezone
		}
		if tz == "" {
			tz = "UTC"
		}
		loc, err := time.LoadLocation(tz)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		w := Window{Name: name, Match: fw.Match, loc: loc}

		fixed := fw.From != "" || fw.To != ""
		if fixed == (len(fw.Weekly) > 0) {
			return nil, fmt.Errorf("%s: set either weekly or from and to", name)
		}
		if fixed {
			if w.from, err = parseTime(fw.From, loc); err != nil {
				return nil, fmt.Errorf("%s: from: %w", name, err)
			}
			if w.to, err = parseTime(fw.To, loc); err != nil {
				return nil, fmt.Errorf("%s: to: %w", name, err)
			}
			if !w.to.After(w.from) {
				return nil, fmt.Errorf("%s: to must be after from", name)
			}
		}
		for _, r := range fw.Weekly {
			from, err := parseWeekTime(r.From)
			if err != nil {
				return nil, fmt.Errorf("%s: weekly from: %w", name, err)
			}
			to, err := parseWeekTime(r.To)
			if err != nil {
				return nil, fmt.Errorf("%s: weekly to: %w", name, err)
			}
			if from == to {
				return nil, fmt.Errorf("%s: weekly from and to are the same", name)
			}
			w.weekly = append(w.weekly, [2]int{from, to})
		}
		out = append(out, w)
	}
	return out, nil
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
	return time.Time{}, fmt.Errorf("%q is not RFC 3339, YYYY-MM-DD HH:MM, or YYYY-MM-DD", s)
}

var weekdays = map[string]int{"sun": 0, "mon": 1, "tue": 2, "wed": 3, "thu": 4, "fri": 5, "sat": 6}

// parseWeekTime reads "Fri 17:00" as minutes since Sunday 00:00.
func parseWeekTime(s string) (int, error) {
	day, clock, ok := strings.Cut(strings.TrimSpace(s), " ")
	d, known := weekdays[strings.ToLower(day)[:min(3, len(day))]]
	if !ok || !known {
		return 0, fmt.Errorf("%q is not like \"Fri 17:00\"", s)
	}
	t, err := time.Parse("15:04", strings.TrimSpace(clock))
	if err != nil {
		return 0, fmt.Errorf("%q is not like \"Fri 17:00\"", s)
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

// Matches reports whether the event's entity has every attribute of the window's match.
func (w Window) Matches(ev automation.Event) bool {
	for k, v := range w.Match {
		if ev.Attribute(k) != v {
			return false
		}
	}
	return true
}

// --- automation ------------------------------------------------------------

// Guard holds the windows and the deployments it aborted.
type Guard struct {
	api           *massdriver.Client
	windows       []Window
	overrideToken string
	slackWebhook  string
	now           func() time.Time

	mu      sync.Mutex
	aborted map[string]time.Time
}

// New returns the automation.
func New(api *massdriver.Client, windows []Window, overrideToken, slackWebhook string) *Guard {
	return &Guard{
		api: api, windows: windows, overrideToken: overrideToken, slackWebhook: slackWebhook,
		now: time.Now, aborted: map[string]time.Time{},
	}
}

type deployment struct {
	ID      string `json:"id"`
	Status  string `json:"status"`
	Action  string `json:"action"`
	Version string `json:"version"`
	Message string `json:"message"`
}

// Handle aborts the deployment when the event occurred inside a matching window.
func (g *Guard) Handle(ctx context.Context, ev automation.Event) error {
	if ev.Type != "deployment.created" && ev.Type != "deployment.enqueued" {
		return nil
	}
	var dep deployment
	if err := ev.DecodeEntity(&dep); err != nil {
		return err
	}
	// A plan changes no infrastructure. A proposal cannot be aborted; its approval sends deployment.enqueued.
	if dep.Action == "PLAN" || (dep.Status != "PENDING" && dep.Status != "APPROVED") {
		return nil
	}
	log := slog.With("automation", Definition.Name, "event_id", ev.ID, "deployment_id", dep.ID, "instance", ev.Attribute("md-instance"))

	// Delivery can lag. The freeze applies to when the deployment started, not when the event arrived.
	at := ev.Time
	if at.IsZero() {
		at = g.now()
	}
	var window *Window
	for i := range g.windows {
		if g.windows[i].Matches(ev) && g.windows[i].Contains(at) {
			window = &g.windows[i]
			break
		}
	}
	if window == nil {
		return nil
	}
	if g.overrideToken != "" && strings.Contains(dep.Message, g.overrideToken) {
		log.Info("override", "window", window.Name)
		return nil
	}
	if g.wasAborted(dep.ID) {
		return nil
	}

	var out struct {
		AbortDeployment massdriver.Payload[struct {
			Status string `json:"status"`
		}] `json:"abortDeployment"`
	}
	err := g.api.Do(ctx, abortDeploymentMutation, map[string]any{"org": g.api.OrganizationID, "id": dep.ID}, &out)
	if err != nil {
		return err
	}
	g.markAborted(dep.ID)
	// A refusal means the deployment already reached a status that abort does not accept. A retry cannot change that.
	if err := out.AbortDeployment.Err(); err != nil {
		log.Warn("abort refused", "window", window.Name, "error", err)
		return nil
	}

	reason := fmt.Sprintf("Aborted %s deployment %s of %s (version %s) inside freeze window %q. "+
		"Put %s in the deployment message to deploy during a freeze.",
		dep.Action, dep.ID, ev.Attribute("md-instance"), dep.Version, window.Name, g.overrideToken)
	log.Info("aborted", "window", window.Name, "reason", reason)

	if g.slackWebhook != "" {
		if err := g.notify(ctx, reason); err != nil {
			log.Error("slack webhook", "error", err)
		}
	}
	return nil
}

func (g *Guard) wasAborted(id string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	_, ok := g.aborted[id]
	return ok
}

func (g *Guard) markAborted(id string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := time.Now()
	for k, t := range g.aborted {
		if now.Sub(t) > 24*time.Hour {
			delete(g.aborted, k)
		}
	}
	g.aborted[id] = now
}

func (g *Guard) notify(ctx context.Context, text string) error {
	body, err := json.Marshal(map[string]string{"text": text})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.slackWebhook, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("content-type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return errors.New(resp.Status + ": " + string(b))
	}
	return nil
}
