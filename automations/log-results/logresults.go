// Package logresults reads the logs of each finished deployment with rules from its config, and writes what the
// rules find, such as a plan's change counts or a scanner's findings, to the deployment's stash.
package logresults

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/massdriver-cloud/massdriver-automations/pkg/automation"
	"github.com/massdriver-cloud/massdriver-automations/pkg/massdriver"
)

const (
	// Massdriver waits 10 seconds for a response. The logs must arrive before that, with room for the writes.
	fetchTimeout = 7 * time.Second
	// Logs that are still missing this long after the deployment finished will not arrive, and a deployment that
	// failed before its provisioner started has none.
	giveUpAfter = 15 * time.Minute
)

// Definition registers the automation.
var Definition = automation.Definition{
	Name:        "log-results",
	Description: "Reads each finished deployment's logs with configured rules, and writes the results to the deployment's stash.",
	Events:      []string{"deployment.completed", "deployment.failed"},
	New: func() (automation.Automation, error) {
		var cfg Config
		if err := automation.LoadConfig(&cfg); err != nil {
			return nil, err
		}
		api, err := massdriver.FromEnv()
		if err != nil {
			return nil, err
		}
		return New(api, &cfg), nil
	},
	Test: Test,
}

// LogResults holds the API client and the rules.
type LogResults struct {
	api *massdriver.Client
	cfg *Config
	now func() time.Time
}

// New returns the automation. cfg must be validated.
func New(api *massdriver.Client, cfg *Config) *LogResults {
	return &LogResults{api: api, cfg: cfg, now: time.Now}
}

// Handle reads the logs of a finished deployment and writes the entries that the rules produce.
func (l *LogResults) Handle(ctx context.Context, ev automation.Event) error {
	if ev.Type != "deployment.completed" && ev.Type != "deployment.failed" {
		return nil
	}
	action := ev.Metadata("deployment_action")
	if !slices.Contains(l.cfg.Actions, action) {
		return nil
	}
	var dep struct {
		ID string `json:"id"`
	}
	if err := ev.DecodeEntity(&dep); err != nil || dep.ID == "" {
		automation.Logger(ctx).Warn("event skipped", "reason", "no_deployment_id")
		return nil
	}
	d := Deployment{ID: dep.ID, Action: action, Failed: ev.Type == "deployment.failed", Attributes: ev.Data.Attributes}
	log := automation.Logger(ctx).With("deployment_id", d.ID, "instance", ev.Attribute("md-instance"))

	if !slices.ContainsFunc(l.cfg.Rules, func(r *Rule) bool { return r.applies(action, d.Attributes) }) {
		log.Debug("logs skipped", "reason", "no_rule_applies")
		return nil
	}

	entries, skips, err := l.read(ctx, d)
	if err != nil {
		// The logs of a deployment that just finished may not be stored yet, and the API returns none while
		// storage fails. Either way, the next delivery of the event can read them, until the event is too old.
		if ev.Time.IsZero() || l.now().Sub(ev.Time) > giveUpAfter {
			log.Warn("logs unavailable", "err", err)
			return nil
		}
		return err
	}
	for _, s := range skips {
		if s.Reason == SkipValueFailed {
			log.Warn("rule value failed", "rule", s.Rule)
		} else {
			log.Debug("rule skipped", "rule", s.Rule, "reason", s.Reason)
		}
	}

	// A failed write is logged and not returned: a redelivery would read the same logs and fail the same way.
	for _, e := range entries {
		err := l.api.SetStashEntry(ctx, massdriver.StashEntry{
			Scope: "deployment", ID: d.ID, Key: e.Key, Format: strings.ToUpper(e.Format), Label: e.Label, Value: e.Value,
			Immutable: true,
		})
		if errors.Is(err, massdriver.ErrAlreadySet) {
			log.Debug("stash entry already written", "rule", e.Rule, "key", e.Key)
			continue
		}
		if err != nil {
			log.Warn("stash write failed", "rule", e.Rule, "key", e.Key, "err", err)
			continue
		}
		log.Info("stash entry written", "rule", e.Rule, "key", e.Key)
	}
	return nil
}

var errNoLogs = errors.New("the deployment has no logs yet")

// read streams the logs from the API into the parser, so the two run together and neither holds all of the logs.
func (l *LogResults) read(ctx context.Context, d Deployment) ([]Entry, []Skip, error) {
	fctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()

	pr, pw := io.Pipe()
	fetched := make(chan int64, 1)
	go func() {
		n, truncated, err := l.api.DeploymentLogs(fctx, d.ID, pw, l.cfg.maxLogBytes())
		if truncated {
			automation.Logger(ctx).Warn("logs truncated", "deployment_id", d.ID, "max_log_mb", l.cfg.MaxLogMB)
		}
		fetched <- n
		_ = pw.CloseWithError(err)
	}()
	entries, skips, err := l.cfg.Parse(fctx, pr, d)
	_ = pr.Close()
	n := <-fetched
	if err != nil {
		return nil, nil, fmt.Errorf("deployment logs: %w", err)
	}
	if n == 0 {
		return nil, nil, errNoLogs
	}
	return entries, skips, nil
}
