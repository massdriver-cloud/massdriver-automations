package massdriver

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/massdriver-cloud/massdriver-automations/pkg/automation"
)

// DefaultResultPrefix is the start of the well-known result keys, such as md/v1/drift.
const DefaultResultPrefix = "md/v1"

// ResultsConfig is the `results` section of an automation's config.
type ResultsConfig struct {
	// Prefix replaces md/v1. The server rejects an md/ key that its widget catalog does not have, so until the
	// catalog has the result keys, a prefix outside md/ is the way to see them as metadata rows.
	Prefix string `yaml:"prefix"`
}

// Results writes deployment and instance results to the stash. A nil API writes nothing.
type Results struct {
	API    *Client
	Prefix string
}

// NewResults returns the writer for cfg.
func NewResults(api *Client, cfg ResultsConfig) Results {
	return Results{API: api, Prefix: cfg.Prefix}
}

// Key is the stash key of a result name, such as drift or step/rollback.
func (r Results) Key(name string) string {
	prefix := strings.TrimRight(r.Prefix, "/")
	if prefix == "" {
		prefix = DefaultResultPrefix
	}
	return prefix + "/" + name
}

// Write sets a JSON result with a label, so it shows as a metadata row while no widget draws it. A deployment's
// result is immutable, because it records what already happened. A failed write is only logged: the result adds to
// what the automation did, and must not make Massdriver send the event again.
func (r Results) Write(ctx context.Context, scope, id, name, label string, value any) {
	if r.API == nil || id == "" {
		return
	}
	log := automation.Logger(ctx).With("result", name, "scope", scope)
	b, err := json.Marshal(value)
	if err != nil {
		log.Warn("result write failed", "err", err)
		return
	}
	err = r.API.SetStashEntry(ctx, StashEntry{
		Scope: scope, ID: id, Key: r.Key(name), Format: "JSON", Label: label, Value: string(b),
		Immutable: scope == "deployment",
	})
	if errors.Is(err, ErrAlreadySet) {
		log.Debug("result already written")
		return
	}
	if err != nil {
		log.Warn("result write failed", "err", err)
		return
	}
	log.Info("result written")
}

// Timestamp formats t in RFC 3339, in UTC.
func Timestamp(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// Step is the value of step/<step>: how a step ran. An automation's step is the part it played in a deployment.
type Step struct {
	Tool string `json:"tool"`
	// Action is plan, apply, destroy, test, or scan. The server refuses a step without one.
	Action     string `json:"action"`
	Status     string `json:"status"`
	FinishedAt string `json:"finished_at,omitempty"`
	Message    string `json:"message,omitempty"`
}

// Changes is the value of changes/<step>: what a plan changes.
type Changes struct {
	Tool    string `json:"tool"`
	Add     int    `json:"add"`
	Change  int    `json:"change"`
	Destroy int    `json:"destroy"`
}

// Drift is the value of drift: whether real infrastructure matches the config. Status is none, detected, or
// unknown.
type Drift struct {
	Status    string `json:"status"`
	Resources int    `json:"resources"`
	CheckedAt string `json:"checked_at"`
}

// Source is the value of source: which code was deployed.
type Source struct {
	Repo   string `json:"repo"`
	Ref    string `json:"ref,omitempty"`
	Commit string `json:"commit,omitempty"`
	URL    string `json:"url,omitempty"`
}

// Release is the value of release: what is running now.
type Release struct {
	Version    string `json:"version"`
	DeployedAt string `json:"deployed_at"`
}
