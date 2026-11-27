package prsync

import (
	"context"
	_ "embed"
	"time"

	"github.com/massdriver-cloud/massdriver-automations/pkg/massdriver"
)

//go:embed queries/set_stash_entry.graphql
var setStashEntryMutation string

// Write is one JSON entry in a project's stash.
type Write struct {
	Project string
	Key     string
	Label   string
	Value   string
	TTL     time.Duration
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
	input := map[string]any{"value": w.Value, "format": "JSON", "ttl": int(w.TTL / time.Second)}
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
		"scope": map[string]any{"project": w.Project},
		"key":   w.Key,
		"input": input,
	}, &out)
	if err != nil {
		return err
	}
	return out.Set.Err()
}
