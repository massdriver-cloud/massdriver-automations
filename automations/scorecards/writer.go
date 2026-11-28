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

type written struct {
	stable string
	at     time.Time
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
	if known && last.stable == stable && now.Sub(last.at) < w.refresh {
		w.remember(id, last)
		return false, nil
	}
	if err := w.stash.Set(ctx, Write{Scope: scope, Key: key, Label: label, Value: value}); err != nil {
		return false, err
	}
	w.remember(id, written{stable: stable, at: now})
	return true, nil
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
