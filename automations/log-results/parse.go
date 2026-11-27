package logresults

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"

	"github.com/massdriver-cloud/massdriver-automations/pkg/massdriver"
)

const (
	// A longer line is cut. Summary lines are short; a long line is a dumped manifest or JSON blob.
	maxLineBytes = 64 << 10
	// A longer capture is cut, so one error message cannot make an entry the server refuses.
	maxCaptureBytes = 1024
)

// Deployment is what a value expression knows about the deployment besides its logs.
type Deployment struct {
	ID string
	// Action is the Massdriver deployment action: PROVISION, PLAN, or DECOMMISSION.
	Action string
	// Failed is true for a deployment.failed event.
	Failed     bool
	Attributes map[string]any
}

// Entry is one stash entry that a rule produced.
type Entry struct {
	Rule   string
	Key    string
	Label  string
	Format string
	Value  string
}

// Skip is a rule that produced no entry, and why.
type Skip struct {
	Rule   string
	Reason string
	// Detail is the cause of a value error. It can quote the logs, so it never goes to the automation's logs.
	Detail string
}

// Skip reasons.
const (
	SkipNotApplicable = "rule_does_not_apply"
	SkipNotDetected   = "detect_did_not_match"
	SkipNoMatch       = "no_pattern_matched"
	SkipValueFailed   = "value_failed"
)

// state is what one rule has read so far.
type state struct {
	rule     *Rule
	detected bool
	matches  int
	captures map[string]any
}

// Parse reads the logs once, line by line, with every rule that applies to the deployment, and returns the entries
// in rule order. Memory stays bounded by the longest line and the captures, not by the size of the logs.
func (c *Config) Parse(ctx context.Context, logs io.Reader, d Deployment) ([]Entry, []Skip, error) {
	var states []*state
	var skips []Skip
	for _, r := range c.Rules {
		if !r.applies(d.Action, d.Attributes) {
			skips = append(skips, Skip{Rule: r.Name, Reason: SkipNotApplicable})
			continue
		}
		states = append(states, &state{rule: r, detected: r.detect == nil, captures: map[string]any{}})
	}
	if len(states) == 0 {
		_, err := io.Copy(io.Discard, logs)
		return nil, skips, err
	}

	br := bufio.NewReaderSize(logs, maxLineBytes)
	for {
		line, err := readLine(br)
		if len(line) > 0 {
			c.scan(states, line)
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, nil, err
		}
	}

	var entries []Entry
	for _, s := range states {
		r := s.rule
		switch {
		case !s.detected:
			skips = append(skips, Skip{Rule: r.Name, Reason: SkipNotDetected})
			continue
		case s.matches == 0:
			skips = append(skips, Skip{Rule: r.Name, Reason: SkipNoMatch})
			continue
		}
		value, err := r.render(ctx, s, d)
		if err != nil {
			skips = append(skips, Skip{Rule: r.Name, Reason: SkipValueFailed, Detail: err.Error()})
			continue
		}
		entries = append(entries, Entry{
			Rule: r.Name, Key: c.Results.Prefix + "/" + r.Key, Label: r.Label, Format: r.Format, Value: value,
		})
	}
	return entries, skips, nil
}

// readLine returns the next line without its line ending, cut at maxLineBytes, with ANSI colors removed.
func readLine(br *bufio.Reader) (string, error) {
	chunk, err := br.ReadSlice('\n')
	line := string(chunk)
	for errors.Is(err, bufio.ErrBufferFull) {
		_, err = br.ReadSlice('\n')
	}
	line = strings.TrimRight(line, "\r\n")
	if strings.Contains(line, "\x1b") {
		line = massdriver.StripANSI(line)
	}
	return line, err
}

func (c *Config) scan(states []*state, line string) {
	for _, s := range states {
		if !s.detected && s.rule.detect.MatchString(line) {
			s.detected = true
		}
		if s.matches >= c.MaxMatches {
			continue
		}
		for _, re := range s.rule.patterns {
			idx := re.FindStringSubmatchIndex(line)
			if idx == nil {
				continue
			}
			s.matches++
			s.merge(re, line, idx)
			if s.matches >= c.MaxMatches {
				break
			}
		}
	}
}

var integer = regexp.MustCompile(`^-?[0-9]{1,15}$`)

// merge adds the named groups of one match. A group that did not take part in the match leaves the earlier value.
func (s *state) merge(re *regexp.Regexp, line string, idx []int) {
	for i, name := range re.SubexpNames() {
		if name == "" || idx[2*i] < 0 {
			continue
		}
		v := capture(line[idx[2*i]:idx[2*i+1]])
		old, seen := s.captures[name]
		switch s.rule.Mode {
		case ModeFirst:
			if !seen {
				s.captures[name] = v
			}
		case ModeSum:
			a, aok := old.(int)
			b, bok := v.(int)
			if seen && aok && bok {
				s.captures[name] = a + b
			} else {
				s.captures[name] = v
			}
		default:
			s.captures[name] = v
		}
	}
}

// capture returns an integer as a number, so it is a number in JSON, and anything else as text. A decimal stays
// text: a version such as 1.10 must not become 1.1. `tonumber` in the value converts one.
func capture(s string) any {
	if integer.MatchString(s) {
		if n, err := strconv.Atoi(s); err == nil {
			return n
		}
	}
	if len(s) > maxCaptureBytes {
		s = s[:maxCaptureBytes]
	}
	return s
}

// render builds the entry's value from the captures.
func (r *Rule) render(ctx context.Context, s *state, d Deployment) (string, error) {
	var out any
	switch {
	case r.value != nil:
		input := make(map[string]any, len(s.captures))
		for k, v := range s.captures {
			input[k] = v
		}
		attrs := d.Attributes
		if attrs == nil {
			attrs = map[string]any{}
		}
		status := "succeeded"
		if d.Failed {
			status = "failed"
		}
		iter := r.value.RunWithContext(ctx, input, status, toolAction(d.Action), s.matches, d.ID, attrs)
		v, ok := iter.Next()
		if !ok {
			return "", errors.New("the value has no output")
		}
		if err, isErr := v.(error); isErr {
			return "", err
		}
		out = v
	case r.capture != "":
		v, ok := s.captures[r.capture]
		if !ok {
			return "", fmt.Errorf("group %s did not match", r.capture)
		}
		out = v
	default:
		out = s.captures
	}

	if r.Format == FormatText {
		switch v := out.(type) {
		case string:
			return v, nil
		case nil, map[string]any, []any:
			return "", errors.New("a text value must be a string, a number, or a boolean")
		}
	}
	b, err := json.Marshal(out)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// toolAction names a deployment action the way the step results do.
func toolAction(action string) string {
	switch action {
	case "PLAN":
		return "plan"
	case "DECOMMISSION":
		return "destroy"
	default:
		return "apply"
	}
}
