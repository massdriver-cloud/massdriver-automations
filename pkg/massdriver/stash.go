package massdriver

import (
	"context"
	_ "embed"
	"errors"
	"strings"
	"time"
)

//go:embed queries/set_stash_entry.graphql
var setStashEntryMutation string

// StashEntry is one stash entry to set.
type StashEntry struct {
	// Scope is the kind of entity that owns the stash: project, environment, component, instance, or deployment.
	Scope string
	// ID is the entity's ID. A deployment ID is a UUID.
	ID     string
	Key    string
	Format string // TEXT or JSON
	Label  string
	Value  string
	// TTL of 0 keeps the entry until it is set again or deleted.
	TTL time.Duration
	// Immutable entries can't be changed or deleted until they expire. A deployment's results describe what
	// already happened, so nothing should rewrite them afterwards.
	Immutable bool
}

// ErrAlreadySet is returned when the key already has an immutable value, or a live value that an immutable write
// can't replace. For a write that is repeated because Massdriver sent an event again, the first write already won.
var ErrAlreadySet = errors.New("the key already has a value that cannot be changed")

// SetStashEntry sets one entry with alphaSetStashEntry. An entry the server rejects returns its messages.
func (c *Client) SetStashEntry(ctx context.Context, e StashEntry) error {
	input := map[string]any{"value": e.Value, "format": e.Format}
	if e.TTL > 0 {
		input["ttl"] = int(e.TTL / time.Second)
	}
	if e.Label != "" {
		input["label"] = e.Label
	}
	if e.Immutable {
		input["immutable"] = true
	}
	var out struct {
		Set Payload[struct {
			Key string `json:"key"`
		}] `json:"alphaSetStashEntry"`
	}
	err := c.Do(ctx, setStashEntryMutation, map[string]any{
		"org":   c.OrganizationID,
		"scope": map[string]any{e.Scope: e.ID},
		"key":   e.Key,
		"input": input,
	}, &out)
	if err != nil {
		return err
	}
	if err := out.Set.Err(); err != nil {
		if strings.Contains(err.Error(), "cannot be changed") {
			return ErrAlreadySet
		}
		return err
	}
	return nil
}
