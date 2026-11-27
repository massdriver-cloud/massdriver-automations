package stashsync

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/massdriver-cloud/massdriver-automations/pkg/automation"
)

// publisher writes entries that changed, and rewrites unchanged entries before they expire.
type publisher struct {
	stash    Stash
	interval time.Duration
	now      func() time.Time

	catalog catalog
	// catalogErr is the error of this cycle's attempt to read the catalog, so a cycle reads it at most once.
	catalogErr error
	last       map[string]sent
}

type sent struct {
	value, format, label string
	at                   time.Time
}

func newPublisher(stash Stash, interval time.Duration, now func() time.Time) *publisher {
	return &publisher{stash: stash, interval: interval, now: now, last: map[string]sent{}}
}

// ttl is three intervals: a sync that stops makes its entries expire after three missed cycles.
func (p *publisher) ttl() time.Duration { return 3 * p.interval }

// refreshAfter is when an unchanged entry is written again. Cycles are about one interval apart, and a cycle's
// write time moves with how long its queries take, so the entry is rewritten on the second cycle after its write
// even when that cycle's queries are faster.
func (p *publisher) refreshAfter() time.Duration { return p.interval * 3 / 2 }

type publishStats struct {
	written, unchanged, invalid, failed int
}

// publish writes each entry that changed. One entry that fails does not stop the others.
func (p *publisher) publish(ctx context.Context, entries []Entry) publishStats {
	log := automation.Logger(ctx)
	var st publishStats
	p.catalogErr = nil
	for _, e := range entries {
		attrs := []any{"scope", e.Scope.Kind, "scope_id", e.Scope.ID, "key", e.Key}
		value, err := p.encode(ctx, e)
		if err != nil {
			st.invalid++
			log.Warn("stash entry skipped", append(attrs, "reason", err.Error())...)
			continue
		}
		now := p.now()
		if last, ok := p.last[e.id()]; ok && last.value == value && last.format == e.Format &&
			last.label == e.Label && now.Sub(last.at) < p.refreshAfter() {
			st.unchanged++
			continue
		}
		w := Write{Scope: e.Scope, Key: e.Key, Format: e.Format, Label: e.Label, Value: value, TTL: p.ttl()}
		if err := p.stash.Set(ctx, w); err != nil {
			st.failed++
			log.Error("stash entry write failed", append(attrs, "err", err)...)
			continue
		}
		st.written++
		p.last[e.id()] = sent{value: value, format: e.Format, label: e.Label, at: now}
		log.Debug("stash entry set", attrs...)
	}
	return st
}

// encode checks an md/ entry against the widget catalog, and returns the value as a string.
func (p *publisher) encode(ctx context.Context, e Entry) (string, error) {
	data := e.Data
	if strings.HasPrefix(e.Key, widgetPrefix) {
		if err := p.loadCatalog(ctx); err != nil {
			return "", fmt.Errorf("the widget catalog is not loaded: %w", err)
		}
		checked, err := p.catalog.prepare(e)
		if err != nil {
			return "", err
		}
		data = checked
	}
	if e.Format == FormatText {
		s, ok := data.(string)
		if !ok {
			return "", fmt.Errorf("a TEXT value must be a string")
		}
		return s, nil
	}
	if _, ok := data.(map[string]any); !ok {
		return "", fmt.Errorf("a JSON value must be an object")
	}
	b, err := json.Marshal(data)
	return string(b), err
}

// loadCatalog reads the widget catalog once. After a failure, it tries again in the next cycle.
func (p *publisher) loadCatalog(ctx context.Context) error {
	if p.catalog != nil || p.catalogErr != nil {
		return p.catalogErr
	}
	widgets, err := p.stash.WidgetCatalog(ctx)
	if err != nil {
		p.catalogErr = err
		return err
	}
	p.catalog = newCatalog(widgets)
	automation.Logger(ctx).Info("widget catalog loaded", "widgets", len(widgets))
	return nil
}
