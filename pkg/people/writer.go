package people

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/massdriver-cloud/massdriver-automations/pkg/massdriver"
)

// Stash writes one stash entry. *massdriver.Client is one.
type Stash interface {
	SetStashEntry(ctx context.Context, e massdriver.StashEntry) error
}

// Writer writes lists to one widget key on environments, on an interval. Each write has a TTL of three intervals,
// so the entries expire when the automation stops. An unchanged list is written again only before it expires.
type Writer struct {
	stash    Stash
	key      string
	label    string
	interval time.Duration
	last     map[string]sent
}

type sent struct {
	value string
	at    time.Time
}

// NewWriter returns a Writer for the widget key, such as md/v1/leaderboard, and its label in the Massdriver UI.
func NewWriter(stash Stash, key, label string, interval time.Duration) *Writer {
	return &Writer{stash: stash, key: key, label: label, interval: interval, last: map[string]sent{}}
}

// ErrEmpty is returned for a list without people. The widget needs at least one.
var ErrEmpty = errors.New("the list has no people")

// Write sets the environment's entry to list when it changed or is about to expire. It reports whether it wrote.
func (w *Writer) Write(ctx context.Context, now time.Time, environment string, list List) (bool, error) {
	if len(list.Entries) == 0 {
		return false, ErrEmpty
	}
	b, err := json.Marshal(list)
	if err != nil {
		return false, err
	}
	value := string(b)
	if last, ok := w.last[environment]; ok && last.value == value && now.Sub(last.at) < w.interval*3/2 {
		return false, nil
	}
	err = w.stash.SetStashEntry(ctx, massdriver.StashEntry{
		Scope: "environment", ID: environment, Key: w.key, Format: "JSON", Label: w.label, Value: value,
		TTL: 3 * w.interval,
	})
	if err != nil {
		return false, err
	}
	w.last[environment] = sent{value: value, at: now}
	return true, nil
}
