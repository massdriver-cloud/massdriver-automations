package drift

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Results of a plan.
const (
	Clean   = "clean"
	Drifted = "drifted"
	Unknown = "unknown"
)

const (
	maxResourceLines = 20
	maxExcerptBytes  = 2000
)

var (
	ansi       = regexp.MustCompile(`\x1b\[[0-9;]*[A-Za-z]`)
	countEntry = regexp.MustCompile(`(\d+) to (\w+)`)
)

// Count is one "<n> to <verb>" entry of a plan summary line.
type Count struct {
	N    int
	Verb string
}

// StepResult is the outcome of one step of a plan.
type StepResult struct {
	Name   string
	Counts []Count
}

// Drifted reports whether the step plans any change.
func (s StepResult) Drifted() bool {
	for _, c := range s.Counts {
		if c.N > 0 {
			return true
		}
	}
	return false
}

func (s StepResult) String() string {
	if len(s.Counts) == 0 {
		return s.Name + ": no changes"
	}
	parts := make([]string, len(s.Counts))
	for i, c := range s.Counts {
		parts[i] = fmt.Sprintf("%d to %s", c.N, c.Verb)
	}
	return s.Name + ": " + strings.Join(parts, ", ")
}

// Result is what the logs of a plan say about drift.
type Result struct {
	Status    string
	Steps     []StepResult
	Resources []string
	// Expected is the number of steps the instance's bundle has.
	Expected int
}

// Analyze reads plan logs with the check's patterns. Steps are matched to step names in log order, because the
// logs do not say which step wrote them.
func Analyze(logs string, steps []string, ch *Check) Result {
	logs = ansi.ReplaceAllString(logs, "")

	type match struct {
		start int
		text  string
		clean bool
	}
	var matches []match
	for _, m := range ch.clean.FindAllStringIndex(logs, -1) {
		matches = append(matches, match{start: m[0], text: logs[m[0]:m[1]], clean: true})
	}
	for _, m := range ch.drift.FindAllStringIndex(logs, -1) {
		matches = append(matches, match{start: m[0], text: logs[m[0]:m[1]]})
	}
	sort.Slice(matches, func(i, j int) bool { return matches[i].start < matches[j].start })

	r := Result{Expected: max(len(steps), 1)}
	drifted := false
	for i, m := range matches {
		name := fmt.Sprintf("step %d", i+1)
		if i < len(steps) {
			name = steps[i]
		}
		s := StepResult{Name: name}
		if !m.clean {
			for _, c := range countEntry.FindAllStringSubmatch(m.text, -1) {
				n, _ := strconv.Atoi(c[1])
				s.Counts = append(s.Counts, Count{N: n, Verb: c[2]})
			}
		}
		drifted = drifted || s.Drifted()
		r.Steps = append(r.Steps, s)
	}

	switch {
	case drifted:
		r.Status = Drifted
	case len(r.Steps) >= r.Expected:
		r.Status = Clean
	default:
		r.Status = Unknown
	}

	if drifted {
		seen := map[string]bool{}
		size := 0
		for _, line := range ch.resource.FindAllString(logs, -1) {
			line = strings.TrimSpace(line)
			if seen[line] {
				continue
			}
			if len(r.Resources) == maxResourceLines || size+len(line) > maxExcerptBytes {
				r.Resources = append(r.Resources, "...")
				break
			}
			seen[line] = true
			size += len(line) + 1
			r.Resources = append(r.Resources, line)
		}
	}
	return r
}
