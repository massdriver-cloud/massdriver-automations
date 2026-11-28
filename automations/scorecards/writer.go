package scorecards

import (
	"context"
	"encoding/json"
	"sync"

	"github.com/massdriver-cloud/massdriver-automations/pkg/massdriver"
)

// Write is one JSON entry to set. Entries have no TTL: each event and each interval computes them again.
type Write struct {
	Scope Scope
	Key   string
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
		Scope: w.Scope.Kind, ID: w.Scope.ID, Key: w.Key, Format: "JSON", Value: w.Value,
	})
}

// volatile are the fields that say when a value was computed. A value that differs only in them has not changed.
var volatile = []string{"evaluated_at", "checked_at"}

// writer sets an entry only when its value changed. It remembers what it wrote, and before its first write of an
// entry it reads the entry, so a restart does not rewrite every entry.
type writer struct {
	stash Stash
	api   API

	mu   sync.Mutex
	last map[string]string
}

func newWriter(stash Stash, api API) *writer {
	return &writer{stash: stash, api: api, last: map[string]string{}}
}

// write sets the entry when it changed, and reports whether it wrote.
func (w *writer) write(ctx context.Context, scope Scope, key string, v any) (bool, error) {
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
					last, known = stableForm(*e.Value), true
				}
			}
		}
	}
	if known && last == stable {
		w.remember(id, stable)
		return false, nil
	}
	if err := w.stash.Set(ctx, Write{Scope: scope, Key: key, Value: value}); err != nil {
		return false, err
	}
	w.remember(id, stable)
	return true, nil
}

// forget drops what the writer remembers, so the next write of each entry reads it first. A pass calls it, so an
// entry that someone deleted or changed is written again.
func (w *writer) forget() {
	w.mu.Lock()
	w.last = map[string]string{}
	w.mu.Unlock()
}

func (w *writer) remember(id, stable string) {
	w.mu.Lock()
	w.last[id] = stable
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
