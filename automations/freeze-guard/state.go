package freezeguard

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

const stateFile = "freeze-guard.json"

type state struct {
	// Policies are the deny policies that freeze-guard created: window name, then group id, then policy id.
	Policies map[string]map[string]string `json:"policies"`
	// Active holds the windows that were active at the last reconcile, so a restart does not announce them again.
	Active map[string]bool `json:"active"`
	// Lifts hold each lifted window until its next start.
	Lifts map[string]time.Time `json:"lifts"`
}

func newState() state {
	return state{Policies: map[string]map[string]string{}, Active: map[string]bool{}, Lifts: map[string]time.Time{}}
}

// loadState reports whether a state file existed.
func loadState(dir string) (state, bool, error) {
	s := newState()
	if dir == "" {
		return s, false, nil
	}
	b, err := os.ReadFile(filepath.Join(dir, stateFile))
	if os.IsNotExist(err) {
		return s, false, nil
	}
	if err != nil {
		return s, false, err
	}
	if err := json.Unmarshal(b, &s); err != nil {
		return s, false, err
	}
	if s.Policies == nil {
		s.Policies = map[string]map[string]string{}
	}
	if s.Active == nil {
		s.Active = map[string]bool{}
	}
	if s.Lifts == nil {
		s.Lifts = map[string]time.Time{}
	}
	return s, true, nil
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
