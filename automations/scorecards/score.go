package scorecards

import (
	"context"
	"encoding/json"
	"time"
)

// NoLevel is the level of a score that reaches no threshold.
const NoLevel = "none"

// Check statuses.
const (
	StatusPassed  = "passed"
	StatusFailed  = "failed"
	StatusUnknown = "unknown"
)

// checkTimeout bounds one jq expression.
const checkTimeout = 2 * time.Second

// InstanceScore is the value of md/v1/scorecard/{scorecard} on an instance.
type InstanceScore struct {
	Tool        string        `json:"tool"`
	Level       string        `json:"level"`
	Score       float64       `json:"score"`
	Passed      int           `json:"passed"`
	Failed      int           `json:"failed"`
	Unknown     int           `json:"unknown"`
	Checks      []CheckResult `json:"checks"`
	EvaluatedAt string        `json:"evaluated_at"`
}

// CheckResult is the outcome of one check.
type CheckResult struct {
	ID      string `json:"id"`
	Status  string `json:"status"`
	Message string `json:"message,omitempty"`
}

// Rollup is the value of md/v1/scorecard/{scorecard} on an environment or a project.
type Rollup struct {
	Tool        string         `json:"tool"`
	Level       string         `json:"level"`
	Score       float64        `json:"score"`
	Instances   int            `json:"instances"`
	Levels      map[string]int `json:"levels"`
	LowestLevel string         `json:"lowest_level"`
	EvaluatedAt string         `json:"evaluated_at"`
}

// inputs are what an instance's checks read: stash values by scope and key, and the instance itself.
type inputs struct {
	now      time.Time
	stash    map[string]StashValue // by scope kind, then "/", then key
	instance any
}

func stashID(kind, key string) string { return kind + "/" + key }

// score runs a scorecard's checks. A check whose input is missing, expired, or older than max_age is unknown,
// and so is a check whose expression fails. The score counts passed and failed checks only.
func score(ctx context.Context, sc *ScorecardConfig, in inputs) InstanceScore {
	s := InstanceScore{Tool: tool, EvaluatedAt: timestamp(in.now), Checks: make([]CheckResult, 0, len(sc.Checks))}
	for i := range sc.Checks {
		r := runCheck(ctx, &sc.Checks[i], in)
		switch r.Status {
		case StatusPassed:
			s.Passed++
		case StatusFailed:
			s.Failed++
		default:
			s.Unknown++
		}
		s.Checks = append(s.Checks, r)
	}
	if s.Passed+s.Failed > 0 {
		s.Score = round(float64(s.Passed)/float64(s.Passed+s.Failed)*100, 2)
		s.Level = sc.levelOf(s.Score)
	} else {
		s.Level = NoLevel
	}
	return s
}

func runCheck(ctx context.Context, ch *CheckConfig, in inputs) CheckResult {
	r := CheckResult{ID: ch.ID}
	var input any
	if ch.Stash != nil {
		v, ok := in.stash[stashID(ch.Stash.Scope, ch.Stash.Key)]
		switch {
		case !ok || v.Value == nil:
			r.Status, r.Message = StatusUnknown, "No value for "+ch.Stash.Key+"."
			return r
		case v.ExpiresAt != nil && !v.ExpiresAt.After(in.now):
			r.Status, r.Message = StatusUnknown, ch.Stash.Key+" expired."
			return r
		case ch.MaxAge > 0 && in.now.Sub(v.UpdatedAt) > time.Duration(ch.MaxAge):
			r.Status, r.Message = StatusUnknown, ch.Stash.Key+" is older than max_age."
			return r
		}
		input = *v.Value
		if v.Format == "JSON" {
			if err := json.Unmarshal([]byte(*v.Value), &input); err != nil {
				r.Status, r.Message = StatusUnknown, ch.Stash.Key+" is not valid JSON."
				return r
			}
		}
	} else {
		input = in.instance
	}

	ctx, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()
	out, ok := ch.pass.RunWithContext(ctx, input).Next()
	switch {
	case !ok:
		r.Status, r.Message = StatusUnknown, "The pass expression returned nothing."
	default:
		if err, isErr := out.(error); isErr {
			r.Status, r.Message = StatusUnknown, clip("The pass expression failed: "+err.Error(), 280)
			return r
		}
		if truthy(out) {
			r.Status = StatusPassed
		} else {
			r.Status = StatusFailed
		}
	}
	return r
}

// truthy is jq's truth: everything but false and null.
func truthy(v any) bool { return v != nil && v != false }

// levelOf is the highest level whose threshold the score reaches.
func (sc *ScorecardConfig) levelOf(score float64) string {
	for _, l := range sc.levels {
		if score >= float64(l.threshold) {
			return l.name
		}
	}
	return NoLevel
}

// rank orders levels: 0 is the highest. NoLevel and unknown names rank last.
func (sc *ScorecardConfig) rank(name string) int {
	for i, l := range sc.levels {
		if l.name == name {
			return i
		}
	}
	return len(sc.levels)
}

// rollup summarizes the scores of instances. Its score is their mean, and its level is the level of that mean.
func rollup(now time.Time, sc *ScorecardConfig, scores []InstanceScore) (Rollup, bool) {
	if len(scores) == 0 {
		return Rollup{}, false
	}
	r := Rollup{Tool: tool, EvaluatedAt: timestamp(now), Instances: len(scores), Levels: map[string]int{}}
	var sum float64
	lowest := -1
	for _, s := range scores {
		r.Levels[s.Level]++
		sum += s.Score
		if rank := sc.rank(s.Level); rank > lowest {
			lowest, r.LowestLevel = rank, s.Level
		}
	}
	r.Score = round(sum/float64(len(scores)), 2)
	r.Level = sc.levelOf(r.Score)
	return r, true
}
