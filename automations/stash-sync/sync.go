package stashsync

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"sync"
	"time"

	"github.com/massdriver-cloud/massdriver-automations/pkg/automation"
)

// maxParallelQueries limits the queries that run at once in a cycle.
const maxParallelQueries = 8

// queryTimeout bounds one query, so a slow source does not hold up a cycle.
const queryTimeout = 30 * time.Second

// readings runs every rule's query and returns each group's entry. A group whose queries all fail, or whose
// single query fails, has no entry. A JSON group leaves out the fields whose queries fail.
func readings(ctx context.Context, groups []*group, sources map[string]Source) []Entry {
	log := automation.Logger(ctx)

	type result struct {
		value any
		err   error
	}
	results := make([][]result, len(groups))
	var wg sync.WaitGroup
	sem := make(chan struct{}, maxParallelQueries)
	for gi, g := range groups {
		results[gi] = make([]result, len(g.rules))
		for ri, r := range g.rules {
			wg.Add(1)
			go func() {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				v, err := evaluate(ctx, r, sources)
				results[gi][ri] = result{v, err}
			}()
		}
	}
	wg.Wait()

	entries := make([]Entry, 0, len(groups))
	for gi, g := range groups {
		attrs := []any{"scope", g.scope.Kind, "scope_id", g.scope.ID, "key", g.key}
		e := Entry{Scope: g.scope, Key: g.key, Format: g.format, Label: g.label}
		if g.whole {
			res := results[gi][0]
			if res.err != nil {
				log.Error("query failed", append(attrs, "err", res.err)...)
				continue
			}
			e.Data = res.value
			entries = append(entries, e)
			continue
		}
		fields := map[string]any{}
		for ri, r := range g.rules {
			res := results[gi][ri]
			if res.err != nil {
				log.Error("query failed", append(attrs, "field", r.Target.Field, "err", res.err)...)
				continue
			}
			fields[r.Target.Field] = res.value
		}
		if len(fields) == 0 {
			continue
		}
		e.Data = fields
		entries = append(entries, e)
	}
	return entries
}

// evaluate returns a rule's value: its fixed value, or its query's number after scale, default, and text.
func evaluate(ctx context.Context, r Rule, sources map[string]Source) (any, error) {
	if r.Value != nil {
		return fixedValue(r)
	}
	q := r.query()
	src, ok := sources[q.source()]
	if !ok {
		return nil, fmt.Errorf("%s is not set up", q.source())
	}
	qctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()
	v, err := src.Query(qctx, q)
	if errors.Is(err, ErrNoData) && r.Default != nil {
		v, err = *r.Default, nil
	}
	if err != nil {
		return nil, err
	}
	if r.Scale != nil {
		v *= *r.Scale
	}
	if r.Target.Field != "" {
		return round(v), nil
	}
	return text(r.Text, v), nil
}

// fixedValue is a rule's `value`. A TEXT entry takes a scalar as text.
func fixedValue(r Rule) (any, error) {
	if r.Target.Field != "" {
		return normalize(r.Value), nil
	}
	switch v := r.Value.(type) {
	case map[string]any:
		return normalize(v), nil
	case string:
		return v, nil
	case int:
		return strconv.Itoa(v), nil
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64), nil
	case bool:
		return strconv.FormatBool(v), nil
	}
	return nil, errors.New("a fixed TEXT value must be a string or a number")
}

// text is the text of the first rule that applies to v, or v itself.
func text(rules []TextRule, v float64) string {
	for _, t := range rules {
		if t.Below == nil || v < *t.Below {
			return t.Text
		}
	}
	return strconv.FormatFloat(round(v), 'f', -1, 64)
}

// round keeps two decimal places, so noise below that does not count as a change.
func round(v float64) float64 { return math.Round(v*100) / 100 }
