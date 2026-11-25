package bundlescanner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

var severityRank = map[string]int{"UNKNOWN": 0, "LOW": 1, "MEDIUM": 2, "HIGH": 3, "CRITICAL": 4}

type finding struct {
	Tool     string
	ID       string
	Title    string
	Severity string
	File     string
	Line     int
}

func (f finding) location() string {
	if f.Line > 0 {
		return fmt.Sprintf("%s:%d", f.File, f.Line)
	}
	return f.File
}

// Tool is one IaC scanner command and the parser of its JSON output.
type Tool struct {
	Name  string
	Bin   string
	Args  []string
	Parse func([]byte) ([]finding, error)
}

var tools = map[string]Tool{
	"trivy": {
		Name: "trivy",
		Bin:  "trivy",
		// The checks built into the pinned Trivy version keep results stable between runs.
		Args:  []string{"config", "--format", "json", "--quiet", "--skip-check-update", "."},
		Parse: parseTrivy,
	},
	"checkov": {
		Name:  "checkov",
		Bin:   "checkov",
		Args:  []string{"--directory", ".", "--output", "json", "--quiet", "--compact", "--skip-download"},
		Parse: parseCheckov,
	},
}

func (t Tool) run(ctx context.Context, dir string) ([]finding, error) {
	cmd := exec.CommandContext(ctx, t.Bin, t.Args...)
	cmd.Dir = dir
	cmd.WaitDelay = 10 * time.Second
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	runErr := cmd.Run()

	// Checkov exits 1 when it finds a failed check. The JSON output decides success, not the exit code.
	var exitErr *exec.ExitError
	if runErr != nil && !errors.As(runErr, &exitErr) {
		return nil, runErr
	}
	findings, err := t.Parse(stdout.Bytes())
	if err != nil {
		return nil, fmt.Errorf("%v: %s", errors.Join(runErr, err), tail(stderr.String(), 500))
	}
	return findings, nil
}

func tail(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return "…" + s[len(s)-n:]
	}
	return s
}

func parseTrivy(out []byte) ([]finding, error) {
	var report struct {
		Results []struct {
			Target            string `json:"Target"`
			Misconfigurations []struct {
				ID            string `json:"ID"`
				Title         string `json:"Title"`
				Severity      string `json:"Severity"`
				Status        string `json:"Status"`
				CauseMetadata struct {
					StartLine int `json:"StartLine"`
				} `json:"CauseMetadata"`
			} `json:"Misconfigurations"`
		} `json:"Results"`
	}
	if err := json.Unmarshal(out, &report); err != nil {
		return nil, fmt.Errorf("trivy output: %w", err)
	}
	var fs []finding
	for _, r := range report.Results {
		for _, m := range r.Misconfigurations {
			if m.Status != "FAIL" {
				continue
			}
			fs = append(fs, finding{
				Tool: "trivy", ID: m.ID, Title: m.Title, Severity: normalizeSeverity(m.Severity),
				File: r.Target, Line: m.CauseMetadata.StartLine,
			})
		}
	}
	return fs, nil
}

type checkovReport struct {
	Results struct {
		FailedChecks []struct {
			CheckID       string  `json:"check_id"`
			CheckName     string  `json:"check_name"`
			FilePath      string  `json:"file_path"`
			Severity      *string `json:"severity"`
			FileLineRange []int   `json:"file_line_range"`
		} `json:"failed_checks"`
	} `json:"results"`
}

// Checkov prints one report object for a single framework and a list of them for several.
func parseCheckov(out []byte) ([]finding, error) {
	out = bytes.TrimSpace(out)
	var reports []checkovReport
	if len(out) > 0 && out[0] == '[' {
		if err := json.Unmarshal(out, &reports); err != nil {
			return nil, fmt.Errorf("checkov output: %w", err)
		}
	} else {
		var r checkovReport
		if err := json.Unmarshal(out, &r); err != nil {
			return nil, fmt.Errorf("checkov output: %w", err)
		}
		reports = []checkovReport{r}
	}

	var fs []finding
	for _, r := range reports {
		for _, c := range r.Results.FailedChecks {
			sev := "UNKNOWN"
			if c.Severity != nil {
				sev = normalizeSeverity(*c.Severity)
			}
			line := 0
			if len(c.FileLineRange) > 0 {
				line = c.FileLineRange[0]
			}
			fs = append(fs, finding{
				Tool: "checkov", ID: c.CheckID, Title: c.CheckName, Severity: sev,
				File: strings.TrimPrefix(c.FilePath, "/"), Line: line,
			})
		}
	}
	return fs, nil
}

func normalizeSeverity(s string) string {
	s = strings.ToUpper(s)
	if _, ok := severityRank[s]; ok {
		return s
	}
	return "UNKNOWN"
}
