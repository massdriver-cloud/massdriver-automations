package statuspage

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"
)

const (
	operational   = "operational"
	maintenance   = "maintenance"
	degraded      = "degraded"
	partialOutage = "partial_outage"
	majorOutage   = "major_outage"
	noData        = "none"
)

var rank = map[string]int{operational: 0, maintenance: 1, degraded: 2, partialOutage: 3, majorOutage: 4}

func worst(a, b string) string {
	if rank[b] > rank[a] {
		return b
	}
	return a
}

func isOutage(status string) bool { return rank[status] >= rank[degraded] }

type alarm struct {
	ID      string
	Name    string
	Status  string
	Message string
	Since   time.Time
}

type deploy struct {
	Status string
	Action string
	At     time.Time
}

func (d deploy) terminal() bool {
	return d.Status == "COMPLETED" || d.Status == "FAILED" || d.Status == "ABORTED" || d.Status == "REJECTED"
}

type instance struct {
	ID       string
	Env      string
	Name     string
	Status   string
	StatusAt time.Time
	Attrs    map[string]string
	AttrsAt  time.Time
	Alarms   map[string]*alarm
	Deploys  map[string]deploy
}

func newInstance(id, env string) *instance {
	return &instance{ID: id, Env: env, Attrs: map[string]string{}, Alarms: map[string]*alarm{}, Deploys: map[string]deploy{}}
}

func (i *instance) setStatus(status string, at time.Time) {
	if status != "" && !at.Before(i.StatusAt) {
		i.Status, i.StatusAt = status, at
	}
}

func (i *instance) setAlarmState(id, name, status, message string, since time.Time) {
	a := i.Alarms[id]
	if a == nil {
		a = &alarm{ID: id}
		i.Alarms[id] = a
	}
	if name != "" {
		a.Name = name
	}
	// Massdriver delivers in any order; an older state must not replace a newer one.
	if status != "" && (a.Status == "" || !since.Before(a.Since)) {
		a.Status, a.Message, a.Since = status, message, since
	}
}

func (i *instance) setDeploy(id string, d deploy) {
	if old, ok := i.Deploys[id]; ok && old.terminal() {
		return
	}
	i.Deploys[id] = d
	for k, v := range i.Deploys {
		if v.terminal() && d.At.Sub(v.At) > time.Hour {
			delete(i.Deploys, k)
		}
	}
}

func (i *instance) deploying() (time.Time, bool) {
	for _, d := range i.Deploys {
		if d.Status == "RUNNING" && (d.Action == "PROVISION" || d.Action == "DECOMMISSION") {
			return d.At, true
		}
	}
	return time.Time{}, false
}

// row is one component line on a page, as evaluated at one moment.
type row struct {
	Key         string
	Title       string
	Description string
	Status      string
	Since       time.Time
	Firing      []*alarm
	Deploying   bool
}

type envState struct {
	Cfg    EnvConfig
	Rows   []row
	Status string
}

type pageState struct {
	Cfg    PageConfig
	Envs   []envState
	Status string
}

type cause struct {
	status string
	at     time.Time
}

func (s *StatusPage) evaluate(now time.Time) []pageState {
	var pages []pageState
	for _, p := range s.cfg.Pages {
		ps := pageState{Cfg: p, Status: operational}
		for _, e := range p.Environments {
			es := envState{Cfg: e, Status: operational}
			for _, r := range s.rows(p, e, now) {
				es.Rows = append(es.Rows, r)
				es.Status = worst(es.Status, r.Status)
			}
			ps.Status = worst(ps.Status, es.Status)
			ps.Envs = append(ps.Envs, es)
		}
		pages = append(pages, ps)
	}
	return pages
}

func (s *StatusPage) visible(env string) []*instance {
	var out []*instance
	for _, i := range s.instances {
		if i.Env != env || (i.Status != "PROVISIONED" && i.Status != "FAILED") || i.Attrs[s.cfg.Attributes.Hidden] == "true" {
			continue
		}
		out = append(out, i)
	}
	sort.Slice(out, func(a, b int) bool {
		if out[a].Name != out[b].Name {
			return out[a].Name < out[b].Name
		}
		return out[a].ID < out[b].ID
	})
	return out
}

func (s *StatusPage) rows(p PageConfig, e EnvConfig, now time.Time) []row {
	insts := s.visible(e.ID)
	prefix := p.Slug + "/" + e.ID + "/"
	var rows []row

	if len(e.Components) == 0 {
		for _, i := range insts {
			title := i.Attrs[s.cfg.Attributes.Title]
			if title == "" {
				title = i.Name
			}
			rows = append(rows, s.row(prefix+i.ID, title, "", "", e, []*instance{i}, now))
		}
		return rows
	}

	claimed := map[string]bool{}
	for _, c := range e.Components {
		var members []*instance
		for _, i := range insts {
			if !claimed[i.ID] && matches(i.Attrs, c.Match) {
				claimed[i.ID] = true
				members = append(members, i)
			}
		}
		if len(members) > 0 {
			rows = append(rows, s.row(prefix+slugify(c.Title), c.Title, c.Description, c.Impact, e, members, now))
		}
	}
	return rows
}

func matches(attrs, match map[string]string) bool {
	for k, v := range match {
		if attrs[k] != v {
			return false
		}
	}
	return true
}

func (s *StatusPage) row(key, title, desc, impact string, e EnvConfig, members []*instance, now time.Time) row {
	r := row{Key: key, Title: title, Description: desc, Status: operational}
	var causes []cause

	for _, i := range members {
		if i.Status == "FAILED" {
			causes = append(causes, cause{partialOutage, i.StatusAt})
		}
		configured := impact
		if v := i.Attrs[s.cfg.Attributes.Impact]; impacts[v] {
			configured = v
		}
		imp := configured
		if imp == "" {
			imp = degraded
		}
		if c, ok := s.healthCause(i, configured, now); ok {
			causes = append(causes, c)
		}
		ids := make([]string, 0, len(i.Alarms))
		for id := range i.Alarms {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			a := i.Alarms[id]
			if a.Status == "ALARM" && now.Sub(a.Since) >= time.Duration(s.cfg.MinDuration) {
				r.Firing = append(r.Firing, a)
				causes = append(causes, cause{imp, a.Since})
			}
		}
		if at, ok := i.deploying(); ok && s.cfg.deploysAsMaintenance(e) {
			r.Deploying = true
			causes = append(causes, cause{maintenance, at})
		}
	}
	for _, m := range s.cfg.Maintenance {
		if !now.Before(m.StartsAt) && now.Before(m.EndsAt) && slices.Contains(m.Environments, e.ID) &&
			(len(m.Components) == 0 || slices.Contains(m.Components, title)) {
			causes = append(causes, cause{maintenance, m.StartsAt})
		}
	}

	for _, c := range causes {
		r.Status = worst(r.Status, c.status)
	}
	for _, c := range causes {
		if c.status == r.Status && (r.Since.IsZero() || c.at.Before(r.Since)) {
			r.Since = c.at
		}
	}
	return r
}

// --- history ---------------------------------------------------------------

type transition struct {
	At     time.Time `json:"at"`
	Status string    `json:"status"`
}

type incidentUpdate struct {
	At      time.Time `json:"at"`
	Status  string    `json:"status"`
	Message string    `json:"message"`
}

type incident struct {
	ID         string           `json:"id"`
	Page       string           `json:"page"`
	Row        string           `json:"row"`
	Title      string           `json:"title"`
	EnvTitle   string           `json:"environment_title"`
	Impact     string           `json:"impact"`
	Current    string           `json:"current"`
	StartedAt  time.Time        `json:"started_at"`
	ResolvedAt *time.Time       `json:"resolved_at,omitempty"`
	Updates    []incidentUpdate `json:"updates"`
}

type history struct {
	Rows      map[string][]transition `json:"rows"`
	Incidents []*incident             `json:"incidents"`
}

func (s *StatusPage) record(pages []pageState, now time.Time) bool {
	changed := false
	current := map[string]bool{}

	for _, p := range pages {
		for _, e := range p.Envs {
			for _, r := range e.Rows {
				current[r.Key] = true
				ts := s.hist.Rows[r.Key]
				if len(ts) == 0 || ts[len(ts)-1].Status != r.Status {
					at := r.Since
					if at.IsZero() || at.After(now) {
						at = now
					}
					if len(ts) > 0 && at.Before(ts[len(ts)-1].At) {
						at = ts[len(ts)-1].At
					}
					s.hist.Rows[r.Key] = append(ts, transition{At: at, Status: r.Status})
					changed = true
				}
				if s.trackIncident(p.Cfg.Slug, e.Cfg.Title, r, now) {
					changed = true
				}
			}
		}
	}

	for _, inc := range s.hist.Incidents {
		if inc.ResolvedAt == nil && !current[inc.Row] {
			s.resolve(inc, now)
			changed = true
		}
	}
	if changed {
		s.prune(now)
	}
	return changed
}

func (s *StatusPage) trackIncident(page, envTitle string, r row, now time.Time) bool {
	var open *incident
	for _, inc := range s.hist.Incidents {
		if inc.Row == r.Key && inc.ResolvedAt == nil {
			open = inc
		}
	}
	switch {
	case open == nil && isOutage(r.Status):
		start := r.Since
		if start.IsZero() || start.After(now) {
			start = now
		}
		sum := sha256.Sum256([]byte(r.Key + start.UTC().Format(time.RFC3339Nano)))
		s.hist.Incidents = append(s.hist.Incidents, &incident{
			ID:        hex.EncodeToString(sum[:6]),
			Page:      page,
			Row:       r.Key,
			Title:     r.Title + ": " + strings.ToLower(statusLabel(r.Status)),
			EnvTitle:  envTitle,
			Impact:    r.Status,
			Current:   r.Status,
			StartedAt: start,
			Updates:   []incidentUpdate{{At: start, Status: "ongoing", Message: s.describe(r, statusLabel(r.Status)+".")}},
		})
		return true
	case open != nil && isOutage(r.Status) && open.Current != r.Status:
		open.Current = r.Status
		open.Impact = worst(open.Impact, r.Status)
		open.Updates = append(open.Updates, incidentUpdate{At: now, Status: "ongoing",
			Message: s.describe(r, "Impact changed to "+strings.ToLower(statusLabel(r.Status))+".")})
		return true
	case open != nil && !isOutage(r.Status):
		s.resolve(open, now)
		return true
	}
	return false
}

func (s *StatusPage) resolve(inc *incident, now time.Time) {
	inc.ResolvedAt = &now
	inc.Updates = append(inc.Updates, incidentUpdate{At: now, Status: "resolved", Message: "Resolved."})
}

// Alarm names and provider messages can name internal resources, so they appear only when the config allows.
func (s *StatusPage) describe(r row, lead string) string {
	if !s.cfg.Site.ShowAlarmDetails || len(r.Firing) == 0 {
		return lead
	}
	parts := []string{lead}
	for _, a := range r.Firing {
		if a.Message != "" {
			parts = append(parts, a.Name+": "+a.Message+".")
		} else {
			parts = append(parts, a.Name+".")
		}
	}
	return strings.Join(parts, " ")
}

func (s *StatusPage) prune(now time.Time) {
	cutoff := now.AddDate(0, 0, -s.cfg.HistoryDays-1)
	for k, ts := range s.hist.Rows {
		keep := 0
		for i, t := range ts {
			if !t.At.After(cutoff) {
				keep = i
			}
		}
		s.hist.Rows[k] = ts[keep:]
	}
	s.hist.Incidents = slices.DeleteFunc(s.hist.Incidents, func(inc *incident) bool {
		return inc.ResolvedAt != nil && inc.ResolvedAt.Before(cutoff)
	})
}

const stateFile = "statuspage.json"

func loadHistory(dir string) (history, error) {
	h := history{Rows: map[string][]transition{}}
	if dir == "" {
		return h, nil
	}
	b, err := os.ReadFile(filepath.Join(dir, stateFile))
	if os.IsNotExist(err) {
		return h, nil
	}
	if err != nil {
		return h, err
	}
	if err := json.Unmarshal(b, &h); err != nil {
		return h, err
	}
	if h.Rows == nil {
		h.Rows = map[string][]transition{}
	}
	return h, nil
}

func saveHistory(dir string, h history) error {
	b, err := json.Marshal(h)
	if err != nil {
		return err
	}
	return writeAtomic(filepath.Join(dir, stateFile), b)
}

// writeAtomic writes world-readable files: a web server that runs as another user serves output_dir.
func writeAtomic(path string, b []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil { //nolint:gosec
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil { //nolint:gosec
		return err
	}
	return os.Rename(tmp, path)
}
