package prsync

import (
	"context"
	"time"

	"github.com/massdriver-cloud/massdriver-automations/pkg/massdriver"
)

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
	return s.API.SetStashEntry(ctx, massdriver.StashEntry{
		Scope: "project", ID: w.Project, Key: w.Key, Format: "JSON", Label: w.Label, Value: w.Value, TTL: w.TTL,
	})
}
