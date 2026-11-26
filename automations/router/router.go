// Package router runs configured HTTP requests, commands, and Massdriver API calls when Massdriver events match a
// route.
package router

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/massdriver-cloud/massdriver-automations/pkg/automation"
	"github.com/massdriver-cloud/massdriver-automations/pkg/massdriver"
)

const (
	automationName = "router"
	queueSize      = 100
	maxOutputBytes = 4096
	// errorOutputBytes is how much of a failed step's output an error record keeps.
	errorOutputBytes = 512
	doneTTL          = time.Hour
)

// Definition registers the automation.
var Definition = automation.Definition{
	Name:        automationName,
	Description: "Runs HTTP requests, commands, and Massdriver API calls for the events that match configured routes.",
	New: func() (automation.Automation, error) {
		var cfg Config
		if err := automation.LoadConfig(&cfg); err != nil {
			return nil, err
		}
		var md *massdriver.Client
		var err error
		if cfg.usesMassdriver() {
			if md, err = massdriver.FromEnv(); err != nil {
				return nil, err
			}
		}
		r := New(&cfg, md)
		go r.Work()
		return r, nil
	},
}

// Router runs the routes that an event selects: sync routes before it responds, async routes after.
type Router struct {
	routes []*Route
	http   *http.Client
	md     *massdriver.Client
	queue  chan job
	done   *doneSet
}

type job struct {
	log    *slog.Logger
	in     input
	routes []*Route
}

// New returns the router. md may be nil when no route has a massdriver step. Call Work to run async routes.
func New(cfg *Config, md *massdriver.Client) *Router {
	return &Router{
		routes: cfg.Routes,
		http:   automation.HTTPClient(0),
		md:     md,
		queue:  make(chan job, queueSize),
		done:   &doneSet{seen: map[string]time.Time{}},
	}
}

// Handle runs the sync routes that the event selects, and queues the async ones. A failed sync route responds 500,
// so Massdriver sends the event again; routes that already succeeded for the event do not run again.
func (r *Router) Handle(ctx context.Context, ev automation.Event) error {
	var data any
	if err := json.Unmarshal(ev.Raw, &data); err != nil {
		return fmt.Errorf("decode event: %w", err)
	}
	in := input{ev: ev, data: data}

	var syncRoutes, asyncRoutes []*Route
	var errs []error
	for _, rt := range r.routes {
		if r.done.has(ev.ID, rt.Name) {
			continue
		}
		ok, err := rt.selects(ctx, in)
		if err != nil {
			automation.Logger(ctx).Debug("route match failed", "route", rt.Name, "detail", exprDetail(err))
			automation.Logger(ctx).Error("route match failed", "route", rt.Name, "err", err)
			errs = append(errs, fmt.Errorf("route %q: %w", rt.Name, err))
			continue
		}
		if !ok {
			continue
		}
		if rt.Async {
			asyncRoutes = append(asyncRoutes, rt)
		} else {
			syncRoutes = append(syncRoutes, rt)
		}
	}

	// Sync routes run at the same time, so the slowest one, not their sum, must fit in Massdriver's wait.
	var wg sync.WaitGroup
	var mu sync.Mutex
	for _, rt := range syncRoutes {
		wg.Go(func() {
			if err := r.runRoute(ctx, rt, in); err != nil {
				mu.Lock()
				errs = append(errs, fmt.Errorf("route %q: %w", rt.Name, err))
				mu.Unlock()
				return
			}
			r.done.add(ev.ID, rt.Name)
		})
	}
	wg.Wait()

	if len(asyncRoutes) > 0 {
		select {
		case r.queue <- job{log: automation.Logger(ctx), in: in, routes: asyncRoutes}:
			for _, rt := range asyncRoutes {
				r.done.add(ev.ID, rt.Name)
			}
		default:
			errs = append(errs, errors.New("async queue is full"))
		}
	}
	return errors.Join(errs...)
}

// Work runs queued async routes until the process exits.
func (r *Router) Work() {
	for j := range r.queue {
		r.process(j)
	}
}

func (r *Router) process(j job) {
	ctx := automation.WithLogger(context.Background(), j.log)
	for _, rt := range j.routes {
		_ = r.runRoute(ctx, rt, j.in)
	}
}

func (rt *Route) selects(ctx context.Context, in input) (bool, error) {
	ev := in.ev
	matched := slices.ContainsFunc(rt.Events, func(e string) bool {
		if prefix, ok := strings.CutSuffix(e, "*"); ok {
			return strings.HasPrefix(ev.Type, prefix)
		}
		return e == ev.Type
	})
	if !matched {
		return false, nil
	}
	for k, v := range rt.Match {
		if _, ok := ev.Data.Attributes[k]; !ok || ev.Attribute(k) != v {
			return false, nil
		}
	}
	if rt.when == nil {
		return true, nil
	}
	return rt.when.truth(ctx, in)
}

func (r *Router) runRoute(ctx context.Context, rt *Route, in input) error {
	ctx, cancel := context.WithTimeout(ctx, rt.Timeout)
	defer cancel()
	log := automation.Logger(ctx).With("route", rt.Name, "async", rt.Async)
	routeStart := time.Now()
	for i, s := range rt.Do {
		start := time.Now()
		out, err := r.runStep(ctx, s, in)
		if err != nil && ctx.Err() != nil {
			err = fmt.Errorf("route timed out after %s: %w", rt.Timeout, err)
		}
		if err != nil {
			// Output can hold whatever a response or a script printed, so the full text stays at debug. The tail
			// usually explains the failure.
			log.Debug("step failed", "step", i+1, "action", s.kind(), "output", out, "detail", exprDetail(err))
			log.Error("route failed", "step", i+1, "action", s.kind(), "duration_ms", millis(time.Since(routeStart)),
				"output_tail", tail(out, errorOutputBytes), "err", err)
			return fmt.Errorf("step %d (%s): %w", i+1, s.kind(), err)
		}
		log.Debug("step completed", "step", i+1, "action", s.kind(), "duration_ms", millis(time.Since(start)),
			"output", out)
	}
	log.Info("route completed", "steps", len(rt.Do), "duration_ms", millis(time.Since(routeStart)))
	return nil
}

func (r *Router) runStep(ctx context.Context, s *Step, in input) (string, error) {
	switch {
	case s.HTTP != nil:
		return r.runHTTP(ctx, s.HTTP, in)
	case s.Exec != nil:
		return runExec(ctx, s, in)
	default:
		return r.runMassdriver(ctx, s.Massdriver, in)
	}
}

func (r *Router) runHTTP(ctx context.Context, h *HTTPStep, in input) (string, error) {
	url, err := h.url.text(ctx, in)
	if err != nil {
		return "", err
	}
	var body io.Reader
	if h.body != nil {
		b, err := h.body.body(ctx, in)
		if err != nil {
			return "", err
		}
		body = strings.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, h.Method, url, body)
	if err != nil {
		return "", err
	}
	if h.body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, p := range h.headers {
		v, err := p.text(ctx, in)
		if err != nil {
			return "", err
		}
		req.Header.Set(k, v)
	}
	resp, err := r.http.Do(req)
	if err != nil {
		return "", automation.RedactURL(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(io.LimitReader(resp.Body, maxOutputBytes))
	result := fmt.Sprintf("HTTP %d: %s", resp.StatusCode, bytes.TrimSpace(out))
	if resp.StatusCode/100 != 2 {
		// The path of a webhook URL is often its credential.
		return result, fmt.Errorf("%s %s://%s: HTTP %d", h.Method, req.URL.Scheme, req.URL.Host, resp.StatusCode)
	}
	return result, nil
}

func runExec(ctx context.Context, s *Step, in input) (string, error) {
	args := make([]string, len(s.args))
	for i, p := range s.args {
		a, err := p.text(ctx, in)
		if err != nil {
			return "", err
		}
		args[i] = a
	}
	// Running the command that the route configures is what an exec step does.
	cmd := exec.CommandContext(ctx, args[0], args[1:]...) //nolint:gosec
	cmd.Env = commandEnv(in.ev)
	cmd.Stdin = bytes.NewReader(in.ev.Raw)
	cmd.WaitDelay = 5 * time.Second
	var out limitedBuffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	return strings.TrimSpace(out.String()), err
}

var orgVariable = regexp.MustCompile(`\$org\b`)

func (r *Router) runMassdriver(ctx context.Context, m *MassdriverStep, in input) (string, error) {
	vars := map[string]any{}
	if m.variables != nil {
		v, err := m.variables.object(ctx, in)
		if err != nil {
			return "", err
		}
		vars = v
	}
	if _, set := vars["org"]; !set && orgVariable.MatchString(m.Query) {
		vars["org"] = r.md.OrganizationID
	}
	var data map[string]json.RawMessage
	if err := r.md.Do(ctx, m.Query, vars, &data); err != nil {
		return "", err
	}
	b, _ := json.Marshal(data)
	out := truncate(string(b))
	for _, field := range slices.Sorted(maps.Keys(data)) {
		var p massdriver.Payload[json.RawMessage]
		var probe struct {
			Successful *bool `json:"successful"`
		}
		if json.Unmarshal(data[field], &probe) != nil || probe.Successful == nil || *probe.Successful {
			continue
		}
		_ = json.Unmarshal(data[field], &p)
		return out, fmt.Errorf("%s: %w", field, p.Err())
	}
	return out, nil
}

var nonEnvChars = regexp.MustCompile(`[^A-Z0-9]+`)

func envKey(prefix, key string) string {
	return prefix + nonEnvChars.ReplaceAllString(strings.ToUpper(key), "_")
}

func commandEnv(ev automation.Event) []string {
	env := append(routeEnviron(),
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

func truncate(s string) string {
	if len(s) <= maxOutputBytes {
		return s
	}
	return s[:maxOutputBytes] + "..."
}

type limitedBuffer struct{ bytes.Buffer }

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if room := maxOutputBytes - b.Len(); room > 0 {
		b.Buffer.Write(p[:min(len(p), room)])
	}
	return len(p), nil
}

// doneSet remembers which routes succeeded for an event, so a retry runs only the routes that failed.
type doneSet struct {
	mu   sync.Mutex
	seen map[string]time.Time
}

func (d *doneSet) has(id, route string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	t, ok := d.seen[id+"\x00"+route]
	return ok && time.Since(t) < doneTTL
}

func (d *doneSet) add(id, route string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	now := time.Now()
	for k, t := range d.seen {
		if now.Sub(t) >= doneTTL {
			delete(d.seen, k)
		}
	}
	d.seen[id+"\x00"+route] = now
}

func millis(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }

// tail returns the last n bytes of s.
func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "..." + s[len(s)-n:]
}
