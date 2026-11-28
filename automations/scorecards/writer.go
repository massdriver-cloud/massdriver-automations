package scorecards

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/massdriver-cloud/massdriver-automations/pkg/massdriver"
)

// Write is one JSON entry to set. Entries have no TTL: each event and each interval computes them again.
type Write struct {
	Scope Scope
	Key   string
	// Label names the entry in the UI while no widget draws its key.
	Label string
	Value string
}

// Stash writes stash entries.
type Stash interface {
	Set(ctx context.Context, w Write) error
	Delete(ctx context.Context, scope Scope, key string) error
}

// GraphQLStash calls the alpha stash API of Massdriver.
type GraphQLStash struct {
	API *massdriver.Client
}

// Set writes one entry with alphaSetStashEntry.
func (s *GraphQLStash) Set(ctx context.Context, w Write) error {
	return s.API.SetStashEntry(ctx, massdriver.StashEntry{
		Scope: w.Scope.Kind, ID: w.Scope.ID, Key: w.Key, Format: "JSON", Label: w.Label, Value: w.Value,
	})
}

// Delete deletes one entry with alphaDeleteStashEntry.
func (s *GraphQLStash) Delete(ctx context.Context, scope Scope, key string) error {
	return s.API.DeleteStashEntry(ctx, scope.Kind, scope.ID, key)
}

// volatile are the fields that say when a value was computed. A value that differs only in them has not changed.
var volatile = []string{"evaluated_at", "checked_at"}

// writer sets an entry when its value changed, or when it was last set more than refresh ago, so its evaluated_at
// is never much older than the last computation. It remembers what it wrote, and before its first write of an
// entry it reads the entry, so a restart does not rewrite every entry.
type writer struct {
	stash   Stash
	api     API
	now     func() time.Time
	refresh time.Duration

	mu   sync.Mutex
	last map[string]written
}

// written is what the writer knows of an entry. An entry it deleted, or found missing, is absent.
type written struct {
	stable string
	at     time.Time
	absent bool
}

func newWriter(stash Stash, api API, now func() time.Time, refresh time.Duration) *writer {
	return &writer{stash: stash, api: api, now: now, refresh: refresh, last: map[string]written{}}
}

// write sets the entry when it changed or is due, and reports whether it wrote.
func (w *writer) write(ctx context.Context, scope Scope, key, label string, v any) (bool, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return false, err
	}
	value := string(b)
	id := scope.Kind + "/" + scope.ID + "/" + key
	stable := stableForm(value)

	w.mu.Lock()
	last, known := w.last[id]
	w.mu.Unlock()
	if !known {
		if current, err := w.api.StashEntries(ctx, []Scope{scope}, []string{key}); err == nil {
			for _, e := range current {
				if e.Key == key && e.Value != nil {
					last, known = written{stable: stableForm(*e.Value), at: e.UpdatedAt}, true
				}
			}
		}
	}
	now := w.now()
	if known && !last.absent && last.stable == stable && now.Sub(last.at) < w.refresh {
		w.remember(id, last)
		return false, nil
	}
	if err := w.stash.Set(ctx, Write{Scope: scope, Key: key, Label: label, Value: value}); err != nil {
		return false, err
	}
	w.remember(id, written{stable: stable, at: now})
	return true, nil
}

// forget drops what the writer remembers, so the next write of each entry reads it first. A pass calls it, so an
// entry that someone deleted or changed is written again.
func (w *writer) forget() {
	w.mu.Lock()
	w.last = map[string]written{}
	w.mu.Unlock()
}

func (w *writer) remember(id string, v written) {
	w.mu.Lock()
	w.last[id] = v
	w.mu.Unlock()
}

// stableForm is a JSON value without its volatile fields, with sorted keys.
func stableForm(value string) string {
	var m map[string]any
	if err := json.Unmarshal([]byte(value), &m); err != nil {
		return value
	}
	for _, k := range volatile {
		delete(m, k)
	}
	b, err := json.Marshal(m)
	if err != nil {
		return value
	}
	return string(b)
}

// clear deletes the entries of keys that scope has, and returns how many it deleted. An entry the writer knows is
// absent is not read again.
func (w *writer) clear(ctx context.Context, scope Scope, keys []string) (int, error) {
	var unknown []string
	w.mu.Lock()
	for _, key := range keys {
		if last, ok := w.last[scope.Kind+"/"+scope.ID+"/"+key]; !ok || !last.absent {
			unknown = append(unknown, key)
		}
	}
	w.mu.Unlock()
	if len(unknown) == 0 {
		return 0, nil
	}
	current, err := w.api.StashEntries(ctx, []Scope{scope}, unknown)
	if err != nil {
		return 0, err
	}
	present := map[string]bool{}
	for _, e := range current {
		present[e.Key] = e.Value != nil
	}
	deleted := 0
	for _, key := range unknown {
		if present[key] {
			if err := w.stash.Delete(ctx, scope, key); err != nil {
				return deleted, err
			}
			deleted++
		}
		w.remember(scope.Kind+"/"+scope.ID+"/"+key, written{absent: true})
	}
	return deleted, nil
}
