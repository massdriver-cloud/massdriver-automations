package stashsync

import (
	"context"
	_ "embed"
	"encoding/json"
	"strings"
	"time"

	"github.com/massdriver-cloud/massdriver-automations/pkg/massdriver"
)

var (
	//go:embed queries/set_stash_entry.graphql
	setStashEntryMutation string
	//go:embed queries/widget_catalog.graphql
	widgetCatalogQuery string
)

// Entry is one stash entry to write.
type Entry struct {
	Scope  Scope
	Key    string
	Format string
	Label  string
	// Data is a string for a TEXT entry, and a map for a JSON entry.
	Data any
}

func (e Entry) id() string { return e.Scope.Kind + "/" + e.Scope.ID + "/" + e.Key }

// Write is an entry as it is sent: its value encoded, with a TTL.
type Write struct {
	Scope  Scope
	Key    string
	Format string
	Label  string
	Value  string
	TTL    time.Duration
}

// Widget is the part of a widget definition that stash-sync checks a value against.
type Widget struct {
	Key    string
	Scope  string // lowercase, such as instance
	Format string
	Schema map[string]any
}

// Stash writes stash entries and reads the widget catalog. The GraphQL implementation calls the alpha stash API.
type Stash interface {
	Set(ctx context.Context, w Write) error
	WidgetCatalog(ctx context.Context) ([]Widget, error)
}

// GraphQLStash calls the Massdriver V2 GraphQL API.
type GraphQLStash struct {
	API *massdriver.Client
}

// Set writes one entry with alphaSetStashEntry.
func (s *GraphQLStash) Set(ctx context.Context, w Write) error {
	input := map[string]any{"value": w.Value, "format": w.Format}
	if w.TTL > 0 {
		input["ttl"] = int(w.TTL / time.Second)
	}
	if w.Label != "" {
		input["label"] = w.Label
	}
	var out struct {
		Set massdriver.Payload[struct {
			Key string `json:"key"`
		}] `json:"alphaSetStashEntry"`
	}
	err := s.API.Do(ctx, setStashEntryMutation, map[string]any{
		"org":   s.API.OrganizationID,
		"scope": map[string]any{w.Scope.Kind: w.Scope.ID},
		"key":   w.Key,
		"input": input,
	}, &out)
	if err != nil {
		return err
	}
	return out.Set.Err()
}

// WidgetCatalog reads alphaWidgetCatalog.
func (s *GraphQLStash) WidgetCatalog(ctx context.Context) ([]Widget, error) {
	var out struct {
		Catalog []struct {
			Key    string          `json:"key"`
			Scope  string          `json:"scope"`
			Format string          `json:"format"`
			Schema json.RawMessage `json:"schema"`
		} `json:"alphaWidgetCatalog"`
	}
	if err := s.API.Do(ctx, widgetCatalogQuery, nil, &out); err != nil {
		return nil, err
	}
	widgets := make([]Widget, 0, len(out.Catalog))
	for _, w := range out.Catalog {
		schema, err := decodeMap(w.Schema)
		if err != nil {
			return nil, err
		}
		widgets = append(widgets, Widget{Key: w.Key, Scope: strings.ToLower(w.Scope), Format: w.Format, Schema: schema})
	}
	return widgets, nil
}

// decodeMap reads a Map field, which arrives as an object or as a JSON-encoded string.
func decodeMap(raw json.RawMessage) (map[string]any, error) {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		raw = json.RawMessage(s)
	}
	var m map[string]any
	err := json.Unmarshal(raw, &m)
	return m, err
}
