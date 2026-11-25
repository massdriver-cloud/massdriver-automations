// Package bundlescanner scans the IaC of each new bundle version with Trivy and Checkov, and reports the findings.
package bundlescanner

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/massdriver-cloud/massdriver-automations/pkg/automation"
	"github.com/massdriver-cloud/massdriver-automations/pkg/massdriver"
	"github.com/massdriver-cloud/massdriver-automations/pkg/slack"
)

const (
	automationName     = "bundle-scanner"
	bundleArtifactType = "application/vnd.massdriver.bundle.v1+json"
	queueSize          = 100
	slackFindingLines  = 10
)

var (
	//go:embed queries/recent_versions.graphql
	recentVersionsQuery string
	//go:embed queries/update_repo_attributes.graphql
	updateRepoAttributesMutation string
)

// Definition registers the automation.
var Definition = automation.Definition{
	Name:        automationName,
	Description: "Scans each new bundle version with Trivy and Checkov and posts the findings to Slack.",
	Events:      []string{"repo.published"},
	New: func() (automation.Automation, error) {
		cfg := DefaultConfig()
		if err := automation.LoadConfig(&cfg); err != nil {
			return nil, err
		}
		api, err := massdriver.FromEnv()
		if err != nil {
			return nil, err
		}
		sl := slack.FromEnv()
		sl.WarnIgnoredChannels(slog.Default(), cfg.Slack.Channel)
		s := New(api, sl, cfg)
		go s.Work()
		return s, nil
	},
}

// Config is the config file of bundle-scanner.
type Config struct {
	Scanners        []string      `yaml:"scanners"`
	FailSeverity    string        `yaml:"fail_severity"`
	ResultAttribute string        `yaml:"result_attribute"`
	Timeout         time.Duration `yaml:"timeout"`
	Slack           slack.Config  `yaml:"slack"`

	tools []Tool
}

// DefaultConfig runs both scanners, fails at HIGH, and sets no attribute.
func DefaultConfig() Config {
	return Config{Scanners: []string{"trivy", "checkov"}, FailSeverity: "HIGH", Timeout: 10 * time.Minute}
}

// Validate checks the scanners and the severity.
func (c *Config) Validate() error {
	c.FailSeverity = strings.ToUpper(c.FailSeverity)
	if _, ok := severityRank[c.FailSeverity]; !ok {
		return fmt.Errorf("fail_severity %q is not one of UNKNOWN, LOW, MEDIUM, HIGH, CRITICAL", c.FailSeverity)
	}
	if c.Timeout <= 0 {
		return errors.New("timeout must be more than 0, such as 10m")
	}
	if len(c.Scanners) == 0 {
		return errors.New("scanners: set trivy, checkov, or both")
	}
	c.tools = nil
	for _, name := range c.Scanners {
		tool, ok := tools[name]
		if !ok {
			return fmt.Errorf("scanners: unknown scanner %q; use trivy, checkov, or both", name)
		}
		c.tools = append(c.tools, tool)
	}
	return nil
}

// Scanner queues bundle versions and scans them one at a time.
type Scanner struct {
	api      *massdriver.Client
	slack    *slack.Client
	registry string
	cfg      Config
	http     *http.Client
	queue    chan job

	mu      sync.Mutex
	scanned map[string]bool
}

type job struct {
	log  *slog.Logger
	repo string
	time time.Time
}

// New returns the automation. Call Work to start scanning.
func New(api *massdriver.Client, sl *slack.Client, cfg Config) *Scanner {
	return &Scanner{
		api:      api,
		slack:    sl,
		registry: strings.TrimSuffix(api.Endpoint, "/api/v2"),
		cfg:      cfg,
		http:     &http.Client{Timeout: 5 * time.Minute},
		queue:    make(chan job, queueSize),
		scanned:  map[string]bool{},
	}
}

// Handle queues a scan of the bundle version that the event announces.
func (s *Scanner) Handle(ctx context.Context, ev automation.Event) error {
	if ev.Type != "repo.published" {
		return nil
	}
	var repo struct {
		Name         string `json:"name"`
		ArtifactType string `json:"artifact_type"`
	}
	if err := ev.DecodeEntity(&repo); err != nil {
		return err
	}
	if repo.ArtifactType != bundleArtifactType || repo.Name == "" {
		return nil
	}
	select {
	case s.queue <- job{log: automation.Logger(ctx), repo: repo.Name, time: ev.Time}:
		return nil
	default:
		// A 500 makes Massdriver retry the event later, when the queue has room.
		return errors.New("scan queue is full")
	}
}

// Work scans queued bundle versions until the process exits.
func (s *Scanner) Work() {
	for j := range s.queue {
		s.process(j)
	}
}

type report struct {
	bundle   string
	findings []finding
	err      error
}

func (s *Scanner) process(j job) {
	ctx, cancel := context.WithTimeout(context.Background(), s.cfg.Timeout)
	defer cancel()
	log := j.log.With("repo", j.repo)

	version, attrs, err := s.resolveVersion(ctx, j)
	if err != nil {
		log.Error("version lookup failed", "err", err)
		return
	}
	bundle := j.repo + "@" + version
	log = log.With("bundle", bundle)

	s.mu.Lock()
	done := s.scanned[bundle]
	s.scanned[bundle] = true
	s.mu.Unlock()
	if done {
		log.Debug("scan skipped", "reason", "already_scanned")
		return
	}

	r := report{bundle: bundle}
	r.findings, r.err = s.scan(ctx, j.repo, version)
	passed := r.err == nil && !s.fails(r.findings)
	if r.err != nil {
		log.Error("scan failed", "err", r.err)
	} else {
		log.Info("bundle scanned", "findings", len(r.findings), "passed", passed)
	}

	if err := s.postSlack(ctx, s.slackText(r)); err != nil {
		log.Error("slack post failed", "err", err)
	}
	if s.cfg.ResultAttribute != "" && r.err == nil {
		if err := s.setAttribute(ctx, j.repo, attrs, passed); err != nil {
			log.Error("repo attribute update failed", "err", err)
		}
	}
}

// The event names the repo but not the version. The version is the newest tag that existed when the event happened,
// so a later publish to the same repo does not take its place.
func (s *Scanner) resolveVersion(ctx context.Context, j job) (string, map[string]any, error) {
	var out struct {
		OciRepo struct {
			ArtifactType string         `json:"artifactType"`
			Attributes   map[string]any `json:"attributes"`
			Tags         struct {
				Items []struct {
					Tag       string    `json:"tag"`
					CreatedAt time.Time `json:"createdAt"`
				} `json:"items"`
			} `json:"tags"`
		} `json:"ociRepo"`
	}
	if err := s.api.Do(ctx, recentVersionsQuery, map[string]any{"org": s.api.OrganizationID, "id": j.repo}, &out); err != nil {
		return "", nil, err
	}
	for _, t := range out.OciRepo.Tags.Items {
		if j.time.IsZero() || !t.CreatedAt.After(j.time) {
			return t.Tag, out.OciRepo.Attributes, nil
		}
	}
	return "", nil, fmt.Errorf("no version of %s published at or before %s", j.repo, j.time.Format(time.RFC3339))
}

func (s *Scanner) scan(ctx context.Context, repo, version string) ([]finding, error) {
	dir, err := os.MkdirTemp("", "bundle-scan-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)

	if err := s.pull(ctx, repo, version, dir); err != nil {
		return nil, fmt.Errorf("pull: %w", err)
	}
	var all []finding
	for _, tool := range s.cfg.tools {
		fs, err := tool.run(ctx, dir)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", tool.Name, err)
		}
		all = append(all, fs...)
	}
	sort.SliceStable(all, func(i, k int) bool { return severityRank[all[i].Severity] > severityRank[all[k].Severity] })
	return all, nil
}

func (s *Scanner) fails(findings []finding) bool {
	for _, f := range findings {
		if severityRank[f.Severity] >= severityRank[s.cfg.FailSeverity] {
			return true
		}
	}
	return false
}

func (s *Scanner) slackText(r report) string {
	if r.err != nil {
		return fmt.Sprintf("Error: IaC scan of *%s* did not finish: %s", r.bundle, r.err)
	}

	counts := map[string]int{}
	for _, f := range r.findings {
		counts[f.Severity]++
	}
	var b strings.Builder
	if s.fails(r.findings) {
		fmt.Fprintf(&b, "Failed: IaC scan of *%s*", r.bundle)
	} else {
		fmt.Fprintf(&b, "Passed: IaC scan of *%s*", r.bundle)
	}
	fmt.Fprintf(&b, " (fails at %s or above): %d critical, %d high, %d medium, %d low, %d unrated.",
		s.cfg.FailSeverity, counts["CRITICAL"], counts["HIGH"], counts["MEDIUM"], counts["LOW"], counts["UNKNOWN"])

	shown := 0
	for _, f := range r.findings {
		if severityRank[f.Severity] < severityRank[s.cfg.FailSeverity] {
			break
		}
		if shown == slackFindingLines {
			fmt.Fprintf(&b, "\n…and more.")
			break
		}
		fmt.Fprintf(&b, "\n• %s `%s` %s — %s (%s)", f.Severity, f.ID, f.location(), f.Title, f.Tool)
		shown++
	}
	return b.String()
}

func (s *Scanner) postSlack(ctx context.Context, text string) error {
	if !s.slack.Sends(s.cfg.Slack.Channel) {
		return nil
	}
	_, err := s.slack.Post(ctx, slack.Message{Channel: s.cfg.Slack.Channel, Text: text})
	return err
}

// updateOciRepo replaces the whole attribute map, so the other attributes of the repo are sent back unchanged.
func (s *Scanner) setAttribute(ctx context.Context, repo string, current map[string]any, passed bool) error {
	attrs := map[string]any{}
	for k, v := range current {
		attrs[k] = v
	}
	attrs[s.cfg.ResultAttribute] = "failed"
	if passed {
		attrs[s.cfg.ResultAttribute] = "passed"
	}
	encoded, err := massdriver.JSONString(attrs)
	if err != nil {
		return err
	}
	var out struct {
		UpdateOciRepo massdriver.Payload[any] `json:"updateOciRepo"`
	}
	err = s.api.Do(ctx, updateRepoAttributesMutation, map[string]any{
		"org":   s.api.OrganizationID,
		"id":    repo,
		"input": map[string]any{"attributes": encoded},
	}, &out)
	if err != nil {
		return err
	}
	return out.UpdateOciRepo.Err()
}
