package drift

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

const stateFile = "drift.json"

// plan is a PLAN deployment that a check started and has not seen finish.
type plan struct {
	Check       string    `json:"check"`
	Instance    string    `json:"instance"`
	Source      string    `json:"source"`
	Project     string    `json:"project"`
	Environment string    `json:"environment"`
	Component   string    `json:"component"`
	Steps       []string  `json:"steps"`
	Undeployed  []string  `json:"undeployed,omitempty"`
	StartedAt   time.Time `json:"started_at"`
	// Resolution is the deployment that a resolve created, so a redelivered event does not create a second one.
	Resolution string `json:"resolution,omitempty"`
	Outcome    string `json:"outcome,omitempty"`
	// Alerted and Replied record the Slack posts that went out, so a retry after a failed post does not repeat
	// them. AlertChannel and AlertTS place the outcome reply in the alert's thread.
	Alerted      bool   `json:"alerted,omitempty"`
	AlertChannel string `json:"alert_channel,omitempty"`
	AlertTS      string `json:"alert_ts,omitempty"`
	Replied      bool   `json:"replied,omitempty"`
}

// run counts the outcomes of one run of a check.
type run struct {
	StartedAt time.Time `json:"started_at"`
	Planned   int       `json:"planned"`
	Clean     int       `json:"clean"`
	Drifted   int       `json:"drifted"`
	Unknown   int       `json:"unknown"`
	Failed    int       `json:"failed"`
	Skipped   int       `json:"skipped"`
}

type state struct {
	Plans map[string]*plan `json:"plans"`
	// Queues hold the instances a check has yet to plan, so max_concurrent survives a restart.
	Queues map[string][]string `json:"queues"`
	Runs   map[string]*run     `json:"runs"`
}

func newState() state {
	return state{Plans: map[string]*plan{}, Queues: map[string][]string{}, Runs: map[string]*run{}}
}

func loadState(dir string) (state, error) {
	s := newState()
	if dir == "" {
		return s, nil
	}
	b, err := os.ReadFile(filepath.Join(dir, stateFile))
	if os.IsNotExist(err) {
		return s, nil
	}
	if err != nil {
		return s, err
	}
	if err := json.Unmarshal(b, &s); err != nil {
		return s, err
	}
	if s.Plans == nil {
		s.Plans = map[string]*plan{}
	}
	if s.Queues == nil {
		s.Queues = map[string][]string{}
	}
	if s.Runs == nil {
		s.Runs = map[string]*run{}
	}
	return s, nil
}

func saveState(dir string, s state) error {
	if dir == "" {
		return nil
	}
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	path := filepath.Join(dir, stateFile)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
