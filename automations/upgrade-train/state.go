package upgradetrain

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

const stateFile = "upgrade-train.json"

// Run phases.
const (
	PhaseDeploying = "deploying"
	PhaseSoaking   = "soaking"
	PhaseHalted    = "halted"
)

// run is one version of one bundle on its way through a train.
type run struct {
	Train   string `json:"train"`
	Bundle  string `json:"bundle"`
	Version string `json:"version"`
	Stage   int    `json:"stage"`
	Phase   string `json:"phase"`
	// Deployments maps each deployment the stage created to its instance.
	Deployments map[string]string `json:"deployments"`
	// Done are the stage's instances on the version: deployed by the stage, or already there.
	Done []string `json:"done"`
	// Waiting counts the ticks each busy instance has waited for its deployment in progress to end.
	Waiting   map[string]int `json:"waiting"`
	SoakStart time.Time      `json:"soak_start"`
	StartedAt time.Time      `json:"started_at"`
}

func (r *run) key() string { return runKey(r.Train, r.Bundle) }

func runKey(train, bundle string) string { return train + "/" + bundle }

type state struct {
	Runs map[string]*run `json:"runs"`
}

func loadState(dir string) (state, error) {
	s := state{Runs: map[string]*run{}}
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
