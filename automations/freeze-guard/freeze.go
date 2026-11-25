// Package freezeguard enforces change freeze windows with deny policies on groups.
package freezeguard

import (
	"context"
	"crypto/subtle"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/massdriver-cloud/massdriver-automations/pkg/automation"
	"github.com/massdriver-cloud/massdriver-automations/pkg/massdriver"
	"github.com/massdriver-cloud/massdriver-automations/pkg/slack"
)

const automationName = "freeze-guard"

var (
	//go:embed queries/groups.graphql
	groupsQuery string
	//go:embed queries/group_policies.graphql
	groupPoliciesQuery string
	//go:embed queries/create_group_policy.graphql
	createGroupPolicyMutation string
	//go:embed queries/delete_policy.graphql
	deletePolicyMutation string
)

var jsonString = massdriver.JSONString

// Definition registers the automation.
var Definition = automation.Definition{
	Name:        automationName,
	Description: "Adds deny policies for deployment actions to groups during change freeze windows.",
	Events:      []string{"group.created", "group.updated", "group.deleted"},
	New: func() (automation.Automation, error) {
		var cfg Config
		if err := automation.LoadConfig(&cfg); err != nil {
			return nil, err
		}
		api, err := massdriver.FromEnv()
		if err != nil {
			return nil, err
		}
		sl := slack.FromEnv()
		sl.WarnIgnoredChannels(slog.Default(), cfg.channels()...)
		g, err := New(api, sl, cfg.windows, Options{
			StateDir:   os.Getenv("STATE_DIR"),
			AdminToken: os.Getenv("FREEZE_ADMIN_TOKEN"),
		})
		if err != nil {
			return nil, err
		}
		go g.Run(context.Background(), time.Minute)
		return g, nil
	},
}

// Options are the settings that come from the environment.
type Options struct {
	StateDir   string
	AdminToken string
}

// Guard keeps one deny policy on each non-exempt group for each active window.
type Guard struct {
	api     *massdriver.Client
	slack   *slack.Client
	windows []Window
	opts    Options
	now     func() time.Time
	wake    chan struct{}

	mu sync.Mutex
	st state
	// adopt is set when no state file existed at start. The first reconcile then takes over policies that exactly
	// match a window, so a lost state file cannot leave a freeze in place.
	adopt bool
}

// New returns the automation with its state loaded from opts.StateDir.
func New(api *massdriver.Client, sl *slack.Client, windows []Window, opts Options) (*Guard, error) {
	st, found, err := loadState(opts.StateDir)
	if err != nil {
		return nil, fmt.Errorf("state: %w", err)
	}
	return &Guard{api: api, slack: sl, windows: windows, opts: opts, now: time.Now, wake: make(chan struct{}, 1), st: st, adopt: !found}, nil
}

// Run reconciles at start, every interval, and after each group event, until ctx ends.
func (g *Guard) Run(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		rctx, cancel := context.WithTimeout(ctx, interval)
		if err := g.Reconcile(rctx); err != nil {
			slog.Error("reconcile failed", "err", err)
		}
		cancel()
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-g.wake:
		}
	}
}

// Handle wakes the reconcile loop when a group appears, changes its name, or goes away.
func (g *Guard) Handle(_ context.Context, ev automation.Event) error {
	switch ev.Type {
	case "group.created", "group.updated", "group.deleted":
		select {
		case g.wake <- struct{}{}:
		default:
		}
	}
	return nil
}

// Routes serves POST /freeze/lift?window=<name> when an admin token is set.
func (g *Guard) Routes(mux *http.ServeMux) {
	if g.opts.AdminToken == "" {
		return
	}
	mux.HandleFunc("POST /freeze/lift", func(w http.ResponseWriter, r *http.Request) {
		want := []byte("Bearer " + g.opts.AdminToken)
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), want) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		name := r.URL.Query().Get("window")
		until, err := g.Lift(name)
		switch {
		case errors.Is(err, errUnknownWindow):
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		case errors.Is(err, errNotActive):
			http.Error(w, err.Error(), http.StatusConflict)
			return
		case err != nil:
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		body := map[string]string{"window": name, "until": until.UTC().Format(time.RFC3339)}
		status := http.StatusOK
		if err := g.Reconcile(r.Context()); err != nil {
			// The lift is saved. The loop removes the remaining policies on its next run.
			body["error"] = err.Error()
			status = http.StatusAccepted
		}
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(body)
	})
}

var (
	errUnknownWindow = errors.New("unknown window")
	errNotActive     = errors.New("window is not active")
)

// Lift ends the named window until its next start and saves the lift.
func (g *Guard) Lift(name string) (time.Time, error) {
	w, ok := g.window(name)
	if !ok {
		return time.Time{}, errUnknownWindow
	}
	now := g.now()
	if !w.Contains(now) {
		return time.Time{}, errNotActive
	}
	until := w.NextStart(now)
	g.mu.Lock()
	defer g.mu.Unlock()
	g.st.Lifts[name] = until
	slog.Info("lift requested", "window", name, "until", until.UTC().Format(time.RFC3339))
	return until, saveState(g.opts.StateDir, g.st)
}

func (g *Guard) window(name string) (Window, bool) {
	for _, w := range g.windows {
		if w.Name == name {
			return w, true
		}
	}
	return Window{}, false
}

// --- reconcile -------------------------------------------------------------

type policy struct {
	ID         string          `json:"id"`
	Actions    []string        `json:"actions"`
	Conditions json.RawMessage `json:"conditions"`
}

type policiesPage struct {
	Cursor struct {
		Next string `json:"next"`
	} `json:"cursor"`
	Items []policy `json:"items"`
}

type group struct {
	ID       string        `json:"id"`
	Name     string        `json:"name"`
	Role     string        `json:"role"`
	Policies *policiesPage `json:"policies"`
}

// Reconcile creates the deny policies that the active windows need and deletes the ones that it created and no
// window needs. It changes no other policy.
func (g *Guard) Reconcile(ctx context.Context) error {
	g.mu.Lock()
	defer g.mu.Unlock()

	groups, err := g.listGroups(ctx)
	if err != nil {
		return err
	}
	now := g.now()
	for name, until := range g.st.Lifts {
		if !now.Before(until) {
			delete(g.st.Lifts, name)
		}
	}

	byID := map[string]group{}
	exists := map[string]bool{}
	for _, gr := range groups {
		byID[gr.ID] = gr
		for _, p := range gr.Policies.Items {
			exists[p.ID] = true
		}
	}

	if g.adopt {
		g.adoptMatching(groups)
		g.adopt = false
	}

	// A policy that someone deleted, or that went with its group, is gone. Forget it, so an active window
	// creates it again.
	for wname, m := range g.st.Policies {
		for gid, pid := range m {
			if !exists[pid] {
				delete(m, gid)
			}
		}
		if len(m) == 0 {
			delete(g.st.Policies, wname)
		}
	}

	var errs []error
	// A window removed from the windows file still owns the policies it created. No window needs them.
	for wname, m := range g.st.Policies {
		if _, ok := g.window(wname); ok {
			continue
		}
		for _, gid := range sortedKeys(m) {
			if err := g.deletePolicy(ctx, m[gid]); err != nil {
				errs = append(errs, fmt.Errorf("%s: delete policy %s of group %s: %w", wname, m[gid], gid, err))
				continue
			}
			delete(m, gid)
		}
		if len(m) == 0 {
			delete(g.st.Policies, wname)
		}
	}
	for wname := range g.st.Active {
		if _, ok := g.window(wname); !ok {
			delete(g.st.Active, wname)
		}
	}
	for wname := range g.st.Lifts {
		if _, ok := g.window(wname); !ok {
			delete(g.st.Lifts, wname)
		}
	}

	for _, w := range g.windows {
		active := g.active(w, now)
		frozen := map[string]bool{}
		if active {
			for _, gr := range groups {
				if gr.Role == "CUSTOM" && !w.Exempt(gr.Name) {
					frozen[gr.ID] = true
				}
			}
		}

		for _, gid := range sortedKeys(g.st.Policies[w.Name]) {
			pid := g.st.Policies[w.Name][gid]
			if frozen[gid] {
				continue
			}
			if err := g.deletePolicy(ctx, pid); err != nil {
				errs = append(errs, fmt.Errorf("%s: delete policy %s of group %s: %w", w.Name, pid, gid, err))
				continue
			}
			delete(g.st.Policies[w.Name], gid)
			errs = appendErr(errs, saveState(g.opts.StateDir, g.st))
		}

		for _, gr := range groups {
			if !frozen[gr.ID] || g.st.Policies[w.Name][gr.ID] != "" {
				continue
			}
			pid, err := g.createPolicy(ctx, gr.ID, w)
			if err != nil {
				errs = append(errs, fmt.Errorf("%s: create policy on group %s: %w", w.Name, gr.Name, err))
				continue
			}
			if g.st.Policies[w.Name] == nil {
				g.st.Policies[w.Name] = map[string]string{}
			}
			g.st.Policies[w.Name][gr.ID] = pid
			errs = appendErr(errs, saveState(g.opts.StateDir, g.st))
		}

		if active != g.st.Active[w.Name] {
			g.announce(ctx, w, active, byID)
			if active {
				g.st.Active[w.Name] = true
			} else {
				delete(g.st.Active, w.Name)
			}
		}
	}
	errs = appendErr(errs, saveState(g.opts.StateDir, g.st))
	return errors.Join(errs...)
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func appendErr(errs []error, err error) []error {
	if err != nil {
		return append(errs, err)
	}
	return errs
}

func (g *Guard) active(w Window, now time.Time) bool {
	if until, lifted := g.st.Lifts[w.Name]; lifted && now.Before(until) {
		return false
	}
	return w.Contains(now)
}

// adoptMatching takes over the deny policies whose actions and conditions exactly match a window.
func (g *Guard) adoptMatching(groups []group) {
	owned := map[string]bool{}
	for _, m := range g.st.Policies {
		for _, pid := range m {
			owned[pid] = true
		}
	}
	for _, gr := range groups {
		for _, p := range gr.Policies.Items {
			if owned[p.ID] {
				continue
			}
			for _, w := range g.windows {
				if g.st.Policies[w.Name][gr.ID] != "" || !matches(p, w) {
					continue
				}
				if g.st.Policies[w.Name] == nil {
					g.st.Policies[w.Name] = map[string]string{}
				}
				g.st.Policies[w.Name][gr.ID] = p.ID
				owned[p.ID] = true
				slog.Info("policy adopted", "window", w.Name, "group", gr.Name, "group_id", gr.ID, "policy_id", p.ID)
				break
			}
		}
	}
}

// matches reports whether a deny policy has exactly the window's actions and conditions.
func matches(p policy, w Window) bool {
	if !slices.Equal(sortedUnique(p.Actions), w.Actions) || len(p.Actions) != len(w.Actions) {
		return false
	}
	var raw any
	if err := json.Unmarshal(p.Conditions, &raw); err != nil {
		return false
	}
	if s, ok := raw.(string); ok {
		return s == "*" && len(w.Conditions) == 0
	}
	obj, ok := raw.(map[string]any)
	if !ok || len(obj) != len(w.Conditions) {
		return false
	}
	for k, v := range obj {
		want, ok := w.Conditions[k]
		if !ok {
			return false
		}
		var got []string
		switch v := v.(type) {
		case string:
			got = []string{v}
		case []any:
			for _, e := range v {
				s, ok := e.(string)
				if !ok {
					return false
				}
				got = append(got, s)
			}
		default:
			return false
		}
		if !slices.Equal(sortedUnique(got), want) || len(got) != len(want) {
			return false
		}
	}
	return true
}

func (g *Guard) listGroups(ctx context.Context) ([]group, error) {
	var out []group
	next := ""
	for {
		cursor := map[string]any{"limit": 20}
		if next != "" {
			cursor["next"] = next
		}
		var resp struct {
			Groups struct {
				Cursor struct {
					Next string `json:"next"`
				} `json:"cursor"`
				Items []group `json:"items"`
			} `json:"groups"`
		}
		if err := g.api.Do(ctx, groupsQuery, map[string]any{"org": g.api.OrganizationID, "cursor": cursor}, &resp); err != nil {
			return nil, err
		}
		for _, gr := range resp.Groups.Items {
			if gr.Policies == nil {
				gr.Policies = &policiesPage{}
			}
			for gr.Policies.Cursor.Next != "" {
				page, err := g.groupPolicies(ctx, gr.ID, gr.Policies.Cursor.Next)
				if err != nil {
					return nil, err
				}
				gr.Policies.Items = append(gr.Policies.Items, page.Items...)
				gr.Policies.Cursor = page.Cursor
			}
			out = append(out, gr)
		}
		next = resp.Groups.Cursor.Next
		if next == "" {
			return out, nil
		}
	}
}

func (g *Guard) groupPolicies(ctx context.Context, groupID, next string) (policiesPage, error) {
	var resp struct {
		Group *struct {
			Policies *policiesPage `json:"policies"`
		} `json:"group"`
	}
	vars := map[string]any{"org": g.api.OrganizationID, "id": groupID, "cursor": map[string]any{"limit": 100, "next": next}}
	if err := g.api.Do(ctx, groupPoliciesQuery, vars, &resp); err != nil {
		return policiesPage{}, err
	}
	if resp.Group == nil || resp.Group.Policies == nil {
		return policiesPage{}, nil
	}
	return *resp.Group.Policies, nil
}

func (g *Guard) createPolicy(ctx context.Context, groupID string, w Window) (string, error) {
	var resp struct {
		CreateGroupPolicy massdriver.Payload[struct {
			ID string `json:"id"`
		}] `json:"createGroupPolicy"`
	}
	vars := map[string]any{
		"org":   g.api.OrganizationID,
		"group": groupID,
		"input": map[string]any{"effect": "DENY", "actions": w.Actions, "conditions": w.conditionsInput()},
	}
	if err := g.api.Do(ctx, createGroupPolicyMutation, vars, &resp); err != nil {
		return "", err
	}
	if err := resp.CreateGroupPolicy.Err(); err != nil {
		return "", err
	}
	automation.Logger(ctx).Info("policy created", "window", w.Name, "group_id", groupID,
		"policy_id", resp.CreateGroupPolicy.Result.ID)
	return resp.CreateGroupPolicy.Result.ID, nil
}

func (g *Guard) deletePolicy(ctx context.Context, id string) error {
	var resp struct {
		DeletePolicy massdriver.Payload[struct {
			ID string `json:"id"`
		}] `json:"deletePolicy"`
	}
	if err := g.api.Do(ctx, deletePolicyMutation, map[string]any{"org": g.api.OrganizationID, "id": id}, &resp); err != nil {
		return err
	}
	if err := resp.DeletePolicy.Err(); err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "not found") {
			return nil
		}
		return err
	}
	automation.Logger(ctx).Info("policy deleted", "policy_id", id)
	return nil
}

// --- slack -----------------------------------------------------------------

func (g *Guard) announce(ctx context.Context, w Window, active bool, groups map[string]group) {
	var text, msg string
	switch {
	case active:
		msg = "freeze started"
		var frozen []string
		for gid := range g.st.Policies[w.Name] {
			frozen = append(frozen, groups[gid].Name)
		}
		sort.Strings(frozen)
		text = fmt.Sprintf("Freeze window %q started. Denied %s %s.\nFrozen groups: %s.\nExempt groups: %s.",
			w.Name, strings.Join(w.Actions, ", "), w.describeConditions(), list(frozen), list(w.ExemptGroups))
	case !g.st.Lifts[w.Name].IsZero():
		msg = "freeze lifted"
		text = fmt.Sprintf("Freeze window %q lifted until %s. Removed its deny policies.",
			w.Name, g.st.Lifts[w.Name].In(w.loc).Format("2006-01-02 15:04 MST"))
	default:
		msg = "freeze ended"
		text = fmt.Sprintf("Freeze window %q ended. Removed its deny policies.", w.Name)
	}
	automation.Logger(ctx).Info(msg, "window", w.Name)
	if !g.slack.Sends(w.Slack.Channel) {
		return
	}
	if _, err := g.slack.Post(ctx, slack.Message{Channel: w.Slack.Channel, Text: text}); err != nil {
		automation.Logger(ctx).Error("slack post failed", "window", w.Name, "err", err)
	}
}

func list(names []string) string {
	if len(names) == 0 {
		return "none"
	}
	return strings.Join(names, ", ")
}
