// Package releasenotes posts each new bundle version, its changelog, and the instances that can upgrade to it to
// Slack and Microsoft Teams.
package releasenotes

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/massdriver-cloud/massdriver-automations/pkg/automation"
	"github.com/massdriver-cloud/massdriver-automations/pkg/massdriver"
)

const (
	pageLimit = 100
	// Slack rejects a section longer than 3000 characters.
	maxSection = 2900
)

var (
	//go:embed queries/bundle_versions.graphql
	bundleVersionsQuery string
	//go:embed queries/repo_instances.graphql
	repoInstancesQuery string
)

// Definition registers the automation.
var Definition = automation.Definition{
	Name:        "release-notes",
	Description: "Posts new bundle versions and the instances that can upgrade to Slack and Microsoft Teams.",
	Events:      []string{"repo.published"},
	New: func() (automation.Automation, error) {
		api, err := massdriver.FromEnv()
		if err != nil {
			return nil, err
		}
		cfg := Config{
			SlackWebhookURL:    automation.Env("SLACK_WEBHOOK_URL", ""),
			TeamsWebhookURL:    automation.Env("TEAMS_WEBHOOK_URL", ""),
			IncludeDevelopment: automation.Env("INCLUDE_DEVELOPMENT", "false") == "true",
		}
		if cfg.SlackWebhookURL == "" && cfg.TeamsWebhookURL == "" {
			return nil, errors.New("set SLACK_WEBHOOK_URL, TEAMS_WEBHOOK_URL, or both")
		}
		return New(api, cfg), nil
	},
}

// Config is where the notes go and which versions get them.
type Config struct {
	SlackWebhookURL    string
	TeamsWebhookURL    string
	IncludeDevelopment bool
}

// ReleaseNotes holds the API client and the versions it already announced.
type ReleaseNotes struct {
	api *massdriver.Client
	cfg Config
	run func(func())

	mu        sync.Mutex
	announced map[string]bool
}

// New returns the automation.
func New(api *massdriver.Client, cfg Config) *ReleaseNotes {
	return &ReleaseNotes{api: api, cfg: cfg, run: func(f func()) { go f() }, announced: map[string]bool{}}
}

// Handle starts the announcement of a published bundle version.
func (n *ReleaseNotes) Handle(_ context.Context, ev automation.Event) error {
	if ev.Type != "repo.published" {
		return nil
	}
	var repo struct {
		Name string `json:"name"`
	}
	if err := ev.DecodeEntity(&repo); err != nil {
		return err
	}
	if repo.Name == "" {
		return nil
	}
	// Listing every instance of a widely used bundle can take longer than the 10 seconds Massdriver waits.
	n.run(func() { n.announce(ev, repo.Name) })
	return nil
}

type version struct {
	Tag       string    `json:"tag"`
	CreatedAt time.Time `json:"createdAt"`
	Changelog *string   `json:"changelog"`
}

type instance struct {
	ID               string  `json:"id"`
	Status           string  `json:"status"`
	Version          string  `json:"version"`
	ResolvedVersion  string  `json:"resolvedVersion"`
	DeployedVersion  *string `json:"deployedVersion"`
	AvailableUpgrade *string `json:"availableUpgrade"`
	Environment      *struct {
		ID      string `json:"id"`
		Project *struct {
			ID string `json:"id"`
		} `json:"project"`
	} `json:"environment"`
}

// Note is one announcement.
type Note struct {
	Bundle    string
	Version   string
	Changelog string
	// Upgradable instances are pinned to an older version and need a version change to get this one.
	Upgradable []instance
	// OnChannel instances follow a release channel. Massdriver moves them to this version and deploys them.
	OnChannel []instance
}

func (n *ReleaseNotes) announce(ev automation.Event, repo string) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	log := slog.With("automation", Definition.Name, "event_id", ev.ID, "repo", repo)

	v, err := n.publishedVersion(ctx, repo, ev.Time)
	if err != nil {
		log.Error("find version", "error", err)
		return
	}
	log = log.With("version", v.Tag)
	if isDevelopment(v.Tag) && !n.cfg.IncludeDevelopment {
		log.Info("skip: development version")
		return
	}
	if !n.claim(repo + "@" + v.Tag) {
		log.Info("skip: already announced")
		return
	}

	insts, err := n.instances(ctx, repo)
	if err != nil {
		log.Error("list instances", "error", err)
		return
	}

	note := Note{Bundle: repo, Version: v.Tag}
	if v.Changelog != nil {
		note.Changelog = strings.TrimSpace(*v.Changelog)
	}
	for _, inst := range insts {
		if inst.Status != "PROVISIONED" && inst.Status != "FAILED" {
			continue
		}
		switch {
		case inst.AvailableUpgrade != nil && *inst.AvailableUpgrade == v.Tag:
			note.Upgradable = append(note.Upgradable, inst)
		case inst.ResolvedVersion == v.Tag && isChannel(inst.Version):
			note.OnChannel = append(note.OnChannel, inst)
		}
	}

	if n.cfg.SlackWebhookURL != "" {
		if err := postJSON(ctx, n.cfg.SlackWebhookURL, SlackMessage(note)); err != nil {
			log.Error("slack", "error", err)
		}
	}
	if n.cfg.TeamsWebhookURL != "" {
		if err := postJSON(ctx, n.cfg.TeamsWebhookURL, TeamsMessage(note)); err != nil {
			log.Error("teams", "error", err)
		}
	}
	log.Info("announced", "upgradable", len(note.Upgradable), "on_channel", len(note.OnChannel))
}

// The repo.published event carries the repo, not the version. The published version is the newest one that
// existed when the event occurred, so a later publish to the same repo is not announced twice.
func (n *ReleaseNotes) publishedVersion(ctx context.Context, repo string, at time.Time) (version, error) {
	var out struct {
		OciRepo *struct {
			Tags struct {
				Items []version `json:"items"`
			} `json:"tags"`
		} `json:"ociRepo"`
	}
	if err := n.api.Do(ctx, bundleVersionsQuery, map[string]any{"org": n.api.OrganizationID, "repo": repo}, &out); err != nil {
		return version{}, err
	}
	if out.OciRepo == nil {
		return version{}, fmt.Errorf("repo %s not found", repo)
	}
	for _, v := range out.OciRepo.Tags.Items {
		if at.IsZero() || !v.CreatedAt.After(at) {
			return v, nil
		}
	}
	return version{}, fmt.Errorf("repo %s has no version published before %s", repo, at.Format(time.RFC3339))
}

func (n *ReleaseNotes) claim(key string) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.announced[key] {
		return false
	}
	n.announced[key] = true
	return true
}

func (n *ReleaseNotes) instances(ctx context.Context, repo string) ([]instance, error) {
	var all []instance
	var next *string
	for {
		var out struct {
			Instances struct {
				Cursor struct {
					Next *string `json:"next"`
				} `json:"cursor"`
				Items []instance `json:"items"`
			} `json:"instances"`
		}
		err := n.api.Do(ctx, repoInstancesQuery, map[string]any{
			"org":    n.api.OrganizationID,
			"repo":   repo,
			"cursor": map[string]any{"limit": pageLimit, "next": next},
		}, &out)
		if err != nil {
			return nil, err
		}
		all = append(all, out.Instances.Items...)
		if out.Instances.Cursor.Next == nil {
			return all, nil
		}
		next = out.Instances.Cursor.Next
	}
}

func isDevelopment(tag string) bool { return strings.Contains(tag, "-") }

var exactVersion = regexp.MustCompile(`^\d+\.\d+\.\d+(-\S+)?$`)

func isChannel(constraint string) bool { return !exactVersion.MatchString(constraint) }

// --- messages --------------------------------------------------------------

type group struct {
	project     string
	environment string
	lines       []string
}

// Instances by project, then by environment, each as "id (from version)".
func grouped(insts []instance, from func(instance) string) []group {
	byKey := map[string]*group{}
	for _, inst := range insts {
		project, env := "", ""
		if inst.Environment != nil {
			env = inst.Environment.ID
			if inst.Environment.Project != nil {
				project = inst.Environment.Project.ID
			}
		}
		key := project + "\x00" + env
		g, ok := byKey[key]
		if !ok {
			g = &group{project: project, environment: env}
			byKey[key] = g
		}
		g.lines = append(g.lines, fmt.Sprintf("`%s` (%s)", inst.ID, from(inst)))
	}
	out := make([]group, 0, len(byKey))
	for _, g := range byKey {
		sort.Strings(g.lines)
		out = append(out, *g)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].project != out[j].project {
			return out[i].project < out[j].project
		}
		return out[i].environment < out[j].environment
	})
	return out
}

func currentVersion(inst instance) string { return inst.ResolvedVersion }

func deployedVersion(inst instance) string {
	if inst.DeployedVersion == nil {
		return "not deployed"
	}
	return "deployed " + *inst.DeployedVersion
}

func instanceList(groups []group, bold func(string) string) string {
	var b strings.Builder
	for _, g := range groups {
		fmt.Fprintf(&b, "%s · `%s`: %s\n", bold(g.project), g.environment, strings.Join(g.lines, ", "))
	}
	return strings.TrimRight(b.String(), "\n")
}

func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

var (
	mdHeading = regexp.MustCompile(`(?m)^#{1,6}\s+(.+)$`)
	mdBold    = regexp.MustCompile(`\*\*(.+?)\*\*`)
	mdLink    = regexp.MustCompile(`\[([^\]]+)\]\(([^)\s]+)\)`)
	mdBullet  = regexp.MustCompile(`(?m)^(\s*)[-*]\s+`)
)

// Slack mrkdwn has no headings and uses single asterisks for bold and <url|text> for links.
func slackMarkdown(md string) string {
	s := mdBullet.ReplaceAllString(md, "$1• ")
	s = mdHeading.ReplaceAllString(s, "**$1**")
	s = mdBold.ReplaceAllString(s, "*$1*")
	return mdLink.ReplaceAllString(s, "<$2|$1>")
}

// Adaptive Card text blocks have no headings.
func teamsMarkdown(md string) string { return mdHeading.ReplaceAllString(md, "**$1**") }

func slackBold(s string) string { return "*" + s + "*" }
func teamsBold(s string) string { return "**" + s + "**" }

// SlackMessage is the incoming-webhook payload for a note.
func SlackMessage(n Note) map[string]any {
	title := fmt.Sprintf("%s %s published", n.Bundle, n.Version)
	section := func(text string) map[string]any {
		return map[string]any{"type": "section", "text": map[string]any{"type": "mrkdwn", "text": clip(text, maxSection)}}
	}

	blocks := []any{map[string]any{"type": "header", "text": map[string]any{"type": "plain_text", "text": clip(title, 150)}}}
	if n.Changelog != "" {
		blocks = append(blocks, section(slackMarkdown(n.Changelog)))
	}
	if len(n.Upgradable) > 0 {
		blocks = append(blocks, section("*Instances that can upgrade*\n"+instanceList(grouped(n.Upgradable, currentVersion), slackBold)))
	} else {
		blocks = append(blocks, section(fmt.Sprintf("No instances can upgrade to %s.", n.Version)))
	}
	if len(n.OnChannel) > 0 {
		blocks = append(blocks, section(fmt.Sprintf("*Instances on a release channel that move to %s*\n", n.Version)+
			instanceList(grouped(n.OnChannel, deployedVersion), slackBold)))
	}
	return map[string]any{"text": title, "blocks": blocks}
}

// TeamsMessage is the Workflows webhook payload for a note: one Adaptive Card.
func TeamsMessage(n Note) map[string]any {
	text := func(s string, extra map[string]any) map[string]any {
		b := map[string]any{"type": "TextBlock", "text": s, "wrap": true}
		for k, v := range extra {
			b[k] = v
		}
		return b
	}

	body := []any{text(fmt.Sprintf("%s %s published", n.Bundle, n.Version), map[string]any{"size": "Large", "weight": "Bolder"})}
	if n.Changelog != "" {
		body = append(body, text(teamsMarkdown(n.Changelog), nil))
	}
	if len(n.Upgradable) > 0 {
		body = append(body,
			text("Instances that can upgrade", map[string]any{"weight": "Bolder", "separator": true}),
			text(instanceList(grouped(n.Upgradable, currentVersion), teamsBold), nil))
	} else {
		body = append(body, text(fmt.Sprintf("No instances can upgrade to %s.", n.Version), map[string]any{"separator": true}))
	}
	if len(n.OnChannel) > 0 {
		body = append(body,
			text(fmt.Sprintf("Instances on a release channel that move to %s", n.Version), map[string]any{"weight": "Bolder", "separator": true}),
			text(instanceList(grouped(n.OnChannel, deployedVersion), teamsBold), nil))
	}

	return map[string]any{
		"type": "message",
		"attachments": []any{map[string]any{
			"contentType": "application/vnd.microsoft.card.adaptive",
			"content": map[string]any{
				"$schema": "http://adaptivecards.io/schemas/adaptive-card.json",
				"type":    "AdaptiveCard",
				"version": "1.4",
				"body":    body,
			},
		}},
	}
}

func postJSON(ctx context.Context, url string, payload any) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("content-type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, body)
	}
	return nil
}
