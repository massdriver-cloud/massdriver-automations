package statuspage

import (
	"context"
	_ "embed"
	"encoding/json"
	"sort"
	"strings"
	"time"

	"github.com/massdriver-cloud/massdriver-automations/pkg/automation"
)

const (
	healthKey = "md/v1/health"
	bannerKey = "md/v1/banner"
	// The API takes at most this many scopes in one read.
	stashScopeLimit = 200
)

//go:embed queries/stash_entries.graphql
var stashEntriesQuery string

var healthStatus = map[string]string{
	"healthy": operational, "ok": operational, "up": operational,
	"degraded": degraded, "warning": degraded, "recovering": degraded,
	"down": majorOutage, "failed": majorOutage, "error": majorOutage,
}

// The page styles notices by the levels of config announcements.
var bannerNotice = map[string]string{"info": "info", "warning": "warning", "error": "critical"}

// stashEntry is the last known value of one key in one stash. A deleted entry stays as a marker, so an older
// event that arrives late cannot bring the value back.
type stashEntry struct {
	Value   string
	Level   string
	Since   time.Time
	SetAt   time.Time
	Expires time.Time
	Deleted bool
	// seen is when this service stored the entry, by its own clock, so it compares with the start of a read
	// without trusting that the two clocks agree.
	seen time.Time
}

func (e *stashEntry) live(now time.Time) bool {
	return e != nil && !e.Deleted && (e.Expires.IsZero() || now.Before(e.Expires))
}

// setStash keeps the newer of the stored entry and next. Since stays put while the value does not change,
// because stash-sync writes the same value again on every interval.
func setStash(m map[string]*stashEntry, id string, next stashEntry, now time.Time) {
	old := m[id]
	if old != nil && next.SetAt.Before(old.SetAt) {
		return
	}
	next.seen = now
	next.Since = next.SetAt
	if old != nil && !old.Deleted && !next.Deleted && old.Value == next.Value && old.Level == next.Level {
		next.Since = old.Since
	}
	m[id] = &next
}

// parseBanner reads the JSON value of md/v1/banner. A banner without a message or with an unknown level shows
// nothing.
func parseBanner(value string) (message, level string, ok bool) {
	var b struct {
		Message string `json:"message"`
		Level   string `json:"level"`
	}
	if json.Unmarshal([]byte(value), &b) != nil || strings.TrimSpace(b.Message) == "" {
		return "", "", false
	}
	if b.Level == "" {
		b.Level = "info"
	}
	if _, known := bannerLevels[b.Level]; !known {
		return "", "", false
	}
	return b.Message, b.Level, true
}

func newStashEntry(key, value string, setAt time.Time, expires *time.Time) stashEntry {
	e := stashEntry{Value: strings.ToLower(strings.TrimSpace(value)), SetAt: setAt}
	if key == bannerKey {
		msg, level, ok := parseBanner(value)
		e = stashEntry{Value: msg, Level: level, SetAt: setAt, Deleted: !ok}
	}
	if expires != nil {
		e.Expires = *expires
	}
	return e
}

// --- reads -----------------------------------------------------------------

type apiStashEntry struct {
	Scope struct {
		Kind string `json:"kind"`
		ID   string `json:"id"`
	} `json:"scope"`
	Key       string     `json:"key"`
	Value     *string    `json:"value"`
	ExpiresAt *time.Time `json:"expiresAt"`
	UpdatedAt time.Time  `json:"updatedAt"`
}

func (s *StatusPage) stashKeys() []string {
	var keys []string
	if s.cfg.Stash.Health {
		keys = append(keys, healthKey)
	}
	if s.cfg.Stash.Banners {
		keys = append(keys, bannerKey)
	}
	return keys
}

// refreshStash reads the stash keys of every configured environment and its instances. Events keep the entries
// current between reads; the reads catch what an event missed, such as an entry set before the service started.
func (s *StatusPage) refreshStash(ctx context.Context) {
	if !s.cfg.Stash.enabled() {
		return
	}
	for _, envID := range s.cfg.environmentIDs() {
		// Every scope of one read must be in one project, and an environment's instances are in its project.
		var scopes []map[string]any
		var instances []string
		if s.cfg.Stash.Banners {
			scopes = append(scopes, map[string]any{"environment": envID})
		}
		s.mu.Lock()
		if s.cfg.Stash.Health {
			for id, inst := range s.instances {
				if inst.Env == envID {
					instances = append(instances, id)
				}
			}
		}
		s.mu.Unlock()
		sort.Strings(instances)
		for _, id := range instances {
			scopes = append(scopes, map[string]any{"instance": id})
		}
		if len(scopes) == 0 {
			continue
		}

		start := s.now()
		entries, err := s.stashEntries(ctx, scopes)
		if err != nil {
			automation.Logger(ctx).Error("stash read failed", "environment", envID, "err", err)
			continue
		}
		s.mu.Lock()
		s.applyStash(envID, instances, entries, start)
		s.mu.Unlock()
	}
}

func (s *StatusPage) stashEntries(ctx context.Context, scopes []map[string]any) ([]apiStashEntry, error) {
	var all []apiStashEntry
	for len(scopes) > 0 {
		n := min(len(scopes), stashScopeLimit)
		var next *string
		for {
			var out struct {
				Page struct {
					Cursor cursor          `json:"cursor"`
					Items  []apiStashEntry `json:"items"`
				} `json:"alphaStashEntriesForScopes"`
			}
			vars := map[string]any{
				"organizationId": s.api.OrganizationID,
				"scopes":         scopes[:n],
				"filter":         map[string]any{"keys": s.stashKeys()},
				"cursor":         map[string]any{"limit": pageLimit, "next": next},
			}
			if err := s.api.Do(ctx, stashEntriesQuery, vars, &out); err != nil {
				return nil, err
			}
			all = append(all, out.Page.Items...)
			if next = out.Page.Cursor.Next; next == nil {
				break
			}
		}
		scopes = scopes[n:]
	}
	return all, nil
}

// applyStash replaces the entries of one environment with a read that started at start. An entry the read did
// not return was deleted or has expired.
func (s *StatusPage) applyStash(envID string, instances []string, entries []apiStashEntry, start time.Time) {
	now := s.now()
	found := map[string]bool{}
	for _, e := range entries {
		if e.Value == nil {
			continue
		}
		kind := strings.ToLower(e.Scope.Kind)
		switch {
		case e.Key == healthKey && kind == "instance" && s.cfg.Stash.Health:
			setStash(s.health, e.Scope.ID, newStashEntry(e.Key, *e.Value, e.UpdatedAt, e.ExpiresAt), now)
		case e.Key == bannerKey && kind == "environment" && e.Scope.ID == envID && s.cfg.Stash.Banners:
			setStash(s.banners, envID, newStashEntry(e.Key, *e.Value, e.UpdatedAt, e.ExpiresAt), now)
		default:
			continue
		}
		found[kind+"/"+e.Scope.ID] = true
	}
	if s.cfg.Stash.Health {
		for _, id := range instances {
			if !found["instance/"+id] {
				clearStash(s.health, id, start)
			}
		}
	}
	if s.cfg.Stash.Banners && !found["environment/"+envID] {
		clearStash(s.banners, envID, start)
	}
}

// clearStash marks an entry that a read did not return as deleted, unless an event stored it after the read
// started. The marker keeps the entry's write time: the read has no time of its own on Massdriver's clock, and
// a later one would hide a newer set.
func clearStash(m map[string]*stashEntry, id string, start time.Time) {
	old := m[id]
	if old == nil || old.Deleted || !old.seen.Before(start) {
		return
	}
	m[id] = &stashEntry{SetAt: old.SetAt, Deleted: true, seen: start}
}

// applyStashEvent applies a stash_entry.set or stash_entry.deleted event.
func (s *StatusPage) applyStashEvent(ev automation.Event, ent entity) {
	kind := strings.ToLower(ent.Scope.Kind)
	var m map[string]*stashEntry
	switch {
	case ent.Key == healthKey && kind == "instance" && s.cfg.Stash.Health:
		if !s.envs[ev.Attribute("md-project")+"-"+ev.Attribute("md-environment")] {
			return
		}
		m = s.health
	case ent.Key == bannerKey && kind == "environment" && s.cfg.Stash.Banners:
		if !s.envs[ent.Scope.ID] {
			return
		}
		m = s.banners
	default:
		return
	}
	if ev.Type == "stash_entry.deleted" {
		setStash(m, ent.Scope.ID, stashEntry{SetAt: ev.Time, Deleted: true}, s.now())
		return
	}
	// A sensitive entry has no value in the event, and the page cannot show what it cannot read.
	if ent.Value == nil {
		return
	}
	setStash(m, ent.Scope.ID, newStashEntry(ent.Key, *ent.Value, ev.Time, ent.ExpiresAt), s.now())
}

// --- page ------------------------------------------------------------------

// healthCause returns the status that an instance's md/v1/health asks for. Missing, expired, and unknown
// values give none, so they never change a row.
func (s *StatusPage) healthCause(i *instance, impact string, now time.Time) (cause, bool) {
	if !s.cfg.Stash.Health {
		return cause{}, false
	}
	e := s.health[i.ID]
	if !e.live(now) {
		return cause{}, false
	}
	st, ok := healthStatus[e.Value]
	if !ok || st == operational {
		return cause{}, false
	}
	if st == majorOutage && impact != "" {
		st = impact
	}
	return cause{st, e.Since}, true
}

func (s *StatusPage) stashBanners(p pageState, now time.Time) []announcementView {
	if !s.cfg.Stash.Banners {
		return nil
	}
	var out []announcementView
	for _, e := range p.Envs {
		b := s.banners[e.Cfg.ID]
		if !b.live(now) || bannerLevels[b.Level] < bannerLevels[s.cfg.Stash.MinLevel] {
			continue
		}
		out = append(out, announcementView{Message: b.Value, Level: bannerNotice[b.Level], Environment: e.Cfg.Title})
	}
	return out
}
