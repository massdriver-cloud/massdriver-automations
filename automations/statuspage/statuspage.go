// Package statuspage serves a public status site built from the alarms, instances, and deployments of
// Massdriver environments.
package statuspage

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	// Timezones must resolve in a distroless image, which has no zoneinfo files.
	_ "time/tzdata"

	"github.com/massdriver-cloud/massdriver-automations/pkg/automation"
	"github.com/massdriver-cloud/massdriver-automations/pkg/massdriver"
)

const pageLimit = 100

var (
	//go:embed queries/environment_instances.graphql
	environmentInstancesQuery string
	//go:embed queries/instance.graphql
	instanceQuery string
	//go:embed queries/instance_alarms.graphql
	instanceAlarmsQuery string
	//go:embed template.html
	defaultTemplate string
)

var events = []string{
	"alarm.created",
	"alarm.updated",
	"alarm.deleted",
	"alarm_state.created",
	"deployment.started",
	"deployment.completed",
	"deployment.failed",
	"deployment.aborted",
	"instance.created",
	"instance.updated",
	"instance.deleted",
	"instance.orphaned",
	"instance.moved",
}

// Definition registers the automation.
var Definition = automation.Definition{
	Name:        "statuspage",
	Description: "Serves a public status page from alarms, instances, and deployments.",
	Events:      events,
	New: func() (automation.Automation, error) {
		api, err := massdriver.FromEnv()
		if err != nil {
			return nil, err
		}
		cfg, err := LoadConfig(automation.Env("STATUSPAGE_CONFIG", "./statuspage.yaml"))
		if err != nil {
			return nil, err
		}
		s, err := New(api, cfg, os.Getenv("STATE_DIR"), os.Getenv("OUTPUT_DIR"))
		if err != nil {
			return nil, err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if err := s.Bootstrap(ctx); err != nil {
			return nil, err
		}
		go func() {
			// Time alone changes the page: flap suppression ends, maintenance windows open and close, days roll over.
			for range time.Tick(30 * time.Second) {
				s.render()
			}
		}()
		return s, nil
	},
}

// StatusPage holds the current state of the configured environments and the rendered site.
type StatusPage struct {
	api       *massdriver.Client
	cfg       *Config
	tmpl      *template.Template
	stateDir  string
	outputDir string
	now       func() time.Time
	run       func(func())

	mu        sync.Mutex
	envs      map[string]bool
	instances map[string]*instance
	hist      history
	updatedAt time.Time
	files     map[string]file
}

type file struct {
	Type string
	Body []byte
}

// New returns the automation with history loaded from stateDir. It renders nothing until Bootstrap.
func New(api *massdriver.Client, cfg *Config, stateDir, outputDir string) (*StatusPage, error) {
	src := defaultTemplate
	if cfg.template != "" {
		src = cfg.template
	}
	tmpl, err := template.New("page").Parse(src)
	if err != nil {
		return nil, fmt.Errorf("template: %w", err)
	}
	h, err := loadHistory(stateDir)
	if err != nil {
		return nil, fmt.Errorf("state: %w", err)
	}
	envs := map[string]bool{}
	for _, id := range cfg.environmentIDs() {
		envs[id] = true
	}
	return &StatusPage{
		api:       api,
		cfg:       cfg,
		tmpl:      tmpl,
		stateDir:  stateDir,
		outputDir: outputDir,
		now:       time.Now,
		run:       func(f func()) { go f() },
		envs:      envs,
		instances: map[string]*instance{},
		hist:      h,
		files:     map[string]file{},
	}, nil
}

// --- API -------------------------------------------------------------------

type cursor struct {
	Next *string `json:"next"`
}

type apiDeployment struct {
	ID                 string     `json:"id"`
	Status             string     `json:"status"`
	Action             string     `json:"action"`
	CreatedAt          time.Time  `json:"createdAt"`
	LastTransitionedAt *time.Time `json:"lastTransitionedAt"`
}

type apiInstance struct {
	ID                  string         `json:"id"`
	Name                string         `json:"name"`
	Status              string         `json:"status"`
	EffectiveAttributes map[string]any `json:"effectiveAttributes"`
	Environment         *struct {
		ID string `json:"id"`
	} `json:"environment"`
	LatestDeployment *apiDeployment `json:"latestDeployment"`
}

type apiAlarm struct {
	ID           string `json:"id"`
	DisplayName  string `json:"displayName"`
	CurrentState *struct {
		Status     string    `json:"status"`
		Message    string    `json:"message"`
		OccurredAt time.Time `json:"occurredAt"`
	} `json:"currentState"`
}

// Bootstrap loads every configured environment's instances and alarms, then renders the site.
func (s *StatusPage) Bootstrap(ctx context.Context) error {
	for _, envID := range s.cfg.environmentIDs() {
		var out struct {
			Environment *struct {
				Instances []apiInstance `json:"instances"`
			} `json:"environment"`
		}
		vars := map[string]any{"organizationId": s.api.OrganizationID, "id": envID}
		if err := s.api.Do(ctx, environmentInstancesQuery, vars, &out); err != nil {
			return fmt.Errorf("environment %s: %w", envID, err)
		}
		if out.Environment == nil {
			return fmt.Errorf("environment %s: not found", envID)
		}
		for _, inst := range out.Environment.Instances {
			alarms, err := s.alarms(ctx, inst.ID)
			if err != nil {
				return fmt.Errorf("instance %s alarms: %w", inst.ID, err)
			}
			s.mu.Lock()
			s.load(envID, inst, alarms)
			s.mu.Unlock()
		}
	}
	s.render()
	return nil
}

func (s *StatusPage) alarms(ctx context.Context, instanceID string) ([]apiAlarm, error) {
	var all []apiAlarm
	var next *string
	for {
		var out struct {
			Instance struct {
				Alarms struct {
					Cursor cursor     `json:"cursor"`
					Items  []apiAlarm `json:"items"`
				} `json:"alarms"`
			} `json:"instance"`
		}
		vars := map[string]any{
			"organizationId": s.api.OrganizationID,
			"id":             instanceID,
			"cursor":         map[string]any{"limit": pageLimit, "next": next},
		}
		if err := s.api.Do(ctx, instanceAlarmsQuery, vars, &out); err != nil {
			return nil, err
		}
		all = append(all, out.Instance.Alarms.Items...)
		next = out.Instance.Alarms.Cursor.Next
		if next == nil {
			return all, nil
		}
	}
}

// Events do not carry an instance's name, and an instance the site has not seen needs its alarms too.
func (s *StatusPage) refresh(id string) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	log := slog.With("automation", "statuspage", "instance", id)

	var out struct {
		Instance *apiInstance `json:"instance"`
	}
	if err := s.api.Do(ctx, instanceQuery, map[string]any{"organizationId": s.api.OrganizationID, "id": id}, &out); err != nil {
		log.Error("refresh instance", "error", err)
		return
	}
	if out.Instance == nil || out.Instance.Environment == nil || !s.envs[out.Instance.Environment.ID] {
		s.mu.Lock()
		delete(s.instances, id)
		s.mu.Unlock()
		s.render()
		return
	}
	alarms, err := s.alarms(ctx, id)
	if err != nil {
		log.Error("refresh alarms", "error", err)
		return
	}
	s.mu.Lock()
	s.load(out.Instance.Environment.ID, *out.Instance, alarms)
	s.mu.Unlock()
	s.render()
}

func (s *StatusPage) load(envID string, in apiInstance, alarms []apiAlarm) {
	now := s.now()
	inst := s.instances[in.ID]
	if inst == nil {
		inst = newInstance(in.ID, envID)
		s.instances[in.ID] = inst
	}
	inst.Env = envID
	inst.Name = in.Name
	inst.setStatus(in.Status, now)
	if !now.Before(inst.AttrsAt) {
		inst.Attrs, inst.AttrsAt = stringify(in.EffectiveAttributes), now
	}
	if d := in.LatestDeployment; d != nil {
		at := d.CreatedAt
		if d.LastTransitionedAt != nil {
			at = *d.LastTransitionedAt
		}
		inst.setDeploy(d.ID, deploy{Status: d.Status, Action: d.Action, At: at})
	}
	for _, a := range alarms {
		if a.CurrentState == nil {
			inst.setAlarmState(a.ID, a.DisplayName, "", "", time.Time{})
			continue
		}
		inst.setAlarmState(a.ID, a.DisplayName, a.CurrentState.Status, a.CurrentState.Message, a.CurrentState.OccurredAt)
	}
}

func stringify(m map[string]any) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		if s, ok := v.(string); ok {
			out[k] = s
		} else if v != nil {
			out[k] = fmt.Sprint(v)
		}
	}
	return out
}

// --- events ----------------------------------------------------------------

// entity holds the `data.entity` fields of every handled event type. Each type fills a subset.
type entity struct {
	ID                 string    `json:"id"`
	Identifier         string    `json:"identifier"`
	PreviousIdentifier string    `json:"previous_identifier"`
	Status             string    `json:"status"`
	Action             string    `json:"action"`
	DisplayName        string    `json:"display_name"`
	AlarmName          string    `json:"alarm_name"`
	Message            string    `json:"message"`
	OccurredAt         time.Time `json:"occurred_at"`
	PackageAlarmID     string    `json:"package_alarm_id"`
}

var deploymentStatus = map[string]string{
	"deployment.started":   "RUNNING",
	"deployment.completed": "COMPLETED",
	"deployment.failed":    "FAILED",
	"deployment.aborted":   "ABORTED",
}

// The instance status that Massdriver sets when a deployment ends.
var instanceStatusAfter = map[[2]string]string{
	{"PROVISION", "COMPLETED"}:    "PROVISIONED",
	{"PROVISION", "FAILED"}:       "FAILED",
	{"DECOMMISSION", "COMPLETED"}: "DECOMMISSIONED",
	{"DECOMMISSION", "FAILED"}:    "FAILED",
}

// Handle applies one event to the state and renders the site again.
func (s *StatusPage) Handle(_ context.Context, ev automation.Event) error {
	known := false
	for _, t := range events {
		known = known || t == ev.Type
	}
	if !known {
		return nil
	}
	var ent entity
	if err := ev.DecodeEntity(&ent); err != nil {
		return err
	}

	s.mu.Lock()
	refresh := s.apply(ev, ent)
	s.mu.Unlock()

	if refresh != "" {
		s.run(func() { s.refresh(refresh) })
	}
	s.render()
	return nil
}

// apply returns the id of an instance that the API must fill in.
func (s *StatusPage) apply(ev automation.Event, ent entity) string {
	id := ev.Attribute("md-instance")
	if strings.HasPrefix(ev.Type, "instance.") && ent.Identifier != "" {
		id = ent.Identifier
	}
	env := ev.Attribute("md-project") + "-" + ev.Attribute("md-environment")

	switch ev.Type {
	case "instance.deleted":
		delete(s.instances, id)
		return ""
	case "instance.moved":
		delete(s.instances, ent.PreviousIdentifier)
	}
	if id == "" {
		return ""
	}

	refresh := ""
	inst := s.instances[id]
	if inst == nil {
		if !s.envs[env] {
			return ""
		}
		inst = newInstance(id, env)
		s.instances[id] = inst
		refresh = id
	}
	if !ev.Time.Before(inst.AttrsAt) && len(ev.Data.Attributes) > 0 {
		inst.Attrs, inst.AttrsAt = stringify(ev.Data.Attributes), ev.Time
	}

	switch ev.Type {
	case "alarm.created", "alarm.updated":
		inst.setAlarmState(ent.ID, ent.DisplayName, "", "", time.Time{})
	case "alarm.deleted":
		delete(inst.Alarms, ent.ID)
	case "alarm_state.created":
		inst.setAlarmState(ent.PackageAlarmID, ent.AlarmName, ent.Status, ent.Message, ent.OccurredAt)
	case "deployment.started", "deployment.completed", "deployment.failed", "deployment.aborted":
		action := ev.Metadata("deployment_action")
		if action == "" {
			action = ent.Action
		}
		status := deploymentStatus[ev.Type]
		inst.setDeploy(ent.ID, deploy{Status: status, Action: action, At: ev.Time})
		inst.setStatus(instanceStatusAfter[[2]string{action, status}], ev.Time)
	case "instance.created", "instance.updated", "instance.orphaned", "instance.moved":
		inst.setStatus(ent.Status, ev.Time)
	}
	return refresh
}

// --- site ------------------------------------------------------------------

func (s *StatusPage) render() {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now()
	pages := s.evaluate(now)
	if s.record(pages, now) {
		s.updatedAt = now
		if s.stateDir != "" {
			if err := saveHistory(s.stateDir, s.hist); err != nil {
				slog.Error("save state", "automation", "statuspage", "error", err)
			}
		}
	}

	files, err := s.build(pages, now)
	if err != nil {
		slog.Error("render", "automation", "statuspage", "error", err)
		return
	}
	if s.outputDir != "" {
		for path, f := range files {
			if old, ok := s.files[path]; ok && bytes.Equal(old.Body, f.Body) {
				continue
			}
			if err := writeAtomic(filepath.Join(s.outputDir, filepath.FromSlash(path)), f.Body); err != nil {
				slog.Error("write output", "automation", "statuspage", "path", path, "error", err)
			}
		}
	}
	s.files = files
}

// Routes serves the rendered site.
func (s *StatusPage) Routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /", s.serve)
}

func (s *StatusPage) serve(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/")
	if path == "" || strings.HasSuffix(path, "/") {
		path += "index.html"
	}
	s.mu.Lock()
	f, ok := s.files[path]
	_, isDir := s.files[path+"/index.html"]
	s.mu.Unlock()

	switch {
	case ok:
		w.Header().Set("content-type", f.Type)
		w.Header().Set("cache-control", "no-cache")
		_, _ = w.Write(f.Body)
	case isDir:
		http.Redirect(w, r, "/"+path+"/", http.StatusMovedPermanently)
	default:
		http.NotFound(w, r)
	}
}

func marshalJSON(v any) ([]byte, error) {
	b, err := json.MarshalIndent(v, "", "  ")
	return append(b, '\n'), err
}
