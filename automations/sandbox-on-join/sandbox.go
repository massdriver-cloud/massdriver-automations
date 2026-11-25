// Package sandboxonjoin forks a template environment for each person who joins a configured group, and tears the
// sandbox down when the person leaves.
package sandboxonjoin

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/massdriver-cloud/massdriver-automations/pkg/automation"
	"github.com/massdriver-cloud/massdriver-automations/pkg/massdriver"
)

const (
	pageLimit         = 100
	maxIdentifierLen  = 20
	maxDescriptionLen = 255
	maxSuffix         = 99
)

var (
	//go:embed queries/account_groups.graphql
	accountGroupsQuery string
	//go:embed queries/members_by_email.graphql
	membersByEmailQuery string
	//go:embed queries/sandboxes.graphql
	sandboxesQuery string
	//go:embed queries/environment_ids.graphql
	environmentIDsQuery string
	//go:embed queries/environment.graphql
	environmentQuery string
	//go:embed queries/fork_environment.graphql
	forkEnvironmentMutation string
	//go:embed queries/deploy_environment.graphql
	deployEnvironmentMutation string
	//go:embed queries/decommission_environment.graphql
	decommissionEnvironmentMutation string
	//go:embed queries/delete_environment.graphql
	deleteEnvironmentMutation string
)

// Definition registers the automation.
var Definition = automation.Definition{
	Name:        "sandbox-on-join",
	Description: "Forks a sandbox environment for each person who joins a group, and removes it when they leave.",
	Events:      []string{"group_membership.added", "group_membership.removed", "deployment.completed"},
	New: func() (automation.Automation, error) {
		api, err := massdriver.FromEnv()
		if err != nil {
			return nil, err
		}
		path := automation.Env("SANDBOX_CONFIG", "/config/sandboxes.yaml")
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("SANDBOX_CONFIG: %w", err)
		}
		rules, err := ParseConfig(raw)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		return New(api, rules), nil
	},
}

// Rule gives the members of Group a fork of the template environment Project-Environment.
type Rule struct {
	Group                   string `yaml:"group"`
	Project                 string `yaml:"project"`
	Environment             string `yaml:"environment"`
	Deploy                  bool   `yaml:"deploy"`
	DeleteAfterDecommission bool   `yaml:"deleteAfterDecommission"`
	CopySecrets             bool   `yaml:"copySecrets"`
	CopyEnvironmentDefaults bool   `yaml:"copyEnvironmentDefaults"`
	CopyRemoteReferences    bool   `yaml:"copyRemoteReferences"`
}

func (r Rule) template() string { return r.Project + "-" + r.Environment }

// ParseConfig reads the `sandboxes:` list of rules.
func ParseConfig(raw []byte) ([]Rule, error) {
	var doc struct {
		Sandboxes []Rule `yaml:"sandboxes"`
	}
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(&doc); err != nil {
		return nil, err
	}
	if len(doc.Sandboxes) == 0 {
		return nil, errors.New("no sandboxes configured")
	}
	for i, r := range doc.Sandboxes {
		if r.Group == "" || r.Project == "" || r.Environment == "" {
			return nil, fmt.Errorf("sandboxes[%d]: group, project, and environment are required", i)
		}
	}
	return doc.Sandboxes, nil
}

// Sandboxes holds the API client and the rules.
type Sandboxes struct {
	api   *massdriver.Client
	rules []Rule
	run   func(func())
}

// New returns the automation.
func New(api *massdriver.Client, rules []Rule) *Sandboxes {
	return &Sandboxes{api: api, rules: rules, run: func(f func()) { go f() }}
}

type membership struct {
	AccountID    string `json:"account_id"`
	AccountEmail string `json:"account_email"`
	AccountName  string `json:"account_name"`
	GroupName    string `json:"group_name"`
}

// Handle starts the work for a membership change or for a completed decommission in a sandbox.
func (s *Sandboxes) Handle(_ context.Context, ev automation.Event) error {
	var work func(context.Context)

	switch ev.Type {
	case "group_membership.added", "group_membership.removed":
		var m membership
		if err := ev.DecodeEntity(&m); err != nil {
			return err
		}
		// A service account's membership has no account_id.
		if m.AccountID == "" || len(s.rulesForGroup(m.GroupName)) == 0 {
			return nil
		}
		if ev.Type == "group_membership.added" {
			work = func(ctx context.Context) { s.createSandboxes(ctx, ev.ID, m) }
		} else {
			work = func(ctx context.Context) { s.removeSandboxes(ctx, ev.ID, m) }
		}
	case "deployment.completed":
		project, environment := ev.Attribute("md-project"), ev.Attribute("md-environment")
		if ev.Metadata("deployment_action") != "DECOMMISSION" || ev.Attribute("sandbox") != "true" ||
			project == "" || environment == "" {
			return nil
		}
		work = func(ctx context.Context) { s.deleteIfTornDown(ctx, ev.ID, project+"-"+environment) }
	default:
		return nil
	}

	// A fork followed by a deploy can take longer than the 10 seconds Massdriver waits for a response.
	s.run(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		work(ctx)
	})
	return nil
}

func (s *Sandboxes) rulesForGroup(group string) []Rule {
	var out []Rule
	for _, r := range s.rules {
		if r.Group == group {
			out = append(out, r)
		}
	}
	return out
}

// Leaving one of two groups that share a template keeps the sandbox.
func (s *Sandboxes) templatesForGroups(groups map[string]bool) map[string]bool {
	out := map[string]bool{}
	for _, r := range s.rules {
		if groups[r.Group] {
			out[r.template()] = true
		}
	}
	return out
}

func (s *Sandboxes) createSandboxes(ctx context.Context, eventID string, m membership) {
	log := slog.With("automation", Definition.Name, "event_id", eventID, "email", m.AccountEmail, "group", m.GroupName)

	// Events arrive in any order. A late "added" after a "removed" must not fork.
	groups, err := s.accountGroups(ctx, m.AccountID)
	if err != nil {
		log.Error("list groups", "error", err)
		return
	}
	if !groups[m.GroupName] {
		log.Info("skip: no longer in group")
		return
	}

	existing, err := s.sandboxes(ctx, m.AccountEmail)
	if err != nil {
		log.Error("list sandboxes", "error", err)
		return
	}

	for _, rule := range s.rulesForGroup(m.GroupName) {
		l := log.With("template", rule.template())
		if hasSandboxFrom(existing, rule.template()) {
			l.Info("skip: sandbox exists")
			continue
		}

		id, err := s.freeIdentifier(ctx, rule.Project, baseIdentifier(m.AccountEmail))
		if err != nil {
			l.Error("choose identifier", "error", err)
			continue
		}
		envID, err := s.fork(ctx, rule, id, m)
		if err != nil {
			l.Error("fork", "error", err)
			continue
		}
		existing = append(existing, sandbox{ID: envID, Parent: &ref{ID: rule.template()}})
		l.Info("forked", "environment", envID)

		if rule.Deploy {
			if err := s.mutate(ctx, deployEnvironmentMutation, "deployEnvironment", envID); err != nil {
				l.Error("deploy", "environment", envID, "error", err)
				continue
			}
			l.Info("deploying", "environment", envID)
		}
	}
}

func (s *Sandboxes) removeSandboxes(ctx context.Context, eventID string, m membership) {
	log := slog.With("automation", Definition.Name, "event_id", eventID, "email", m.AccountEmail, "group", m.GroupName)

	groups, err := s.accountGroups(ctx, m.AccountID)
	if err != nil {
		log.Error("list groups", "error", err)
		return
	}
	kept := s.templatesForGroups(groups)

	existing, err := s.sandboxes(ctx, m.AccountEmail)
	if err != nil {
		log.Error("list sandboxes", "error", err)
		return
	}

	for _, rule := range s.rulesForGroup(m.GroupName) {
		if kept[rule.template()] {
			log.Info("skip: another group keeps the sandbox", "template", rule.template())
			continue
		}
		for _, sb := range existing {
			if sb.Parent == nil || sb.Parent.ID != rule.template() {
				continue
			}
			l := log.With("environment", sb.ID)
			switch {
			case sb.active():
				if err := s.mutate(ctx, decommissionEnvironmentMutation, "decommissionEnvironment", sb.ID); err != nil {
					l.Error("decommission", "error", err)
					continue
				}
				l.Info("decommissioning")
			case rule.DeleteAfterDecommission:
				// Nothing is deployed, so no deployment.completed event will arrive to start the delete.
				if err := s.mutate(ctx, deleteEnvironmentMutation, "deleteEnvironment", sb.ID); err != nil {
					l.Error("delete", "error", err)
					continue
				}
				l.Info("deleted")
			}
		}
	}
}

// Each instance's decommission sends its own deployment.completed event. The last one finds nothing active.
func (s *Sandboxes) deleteIfTornDown(ctx context.Context, eventID, envID string) {
	log := slog.With("automation", Definition.Name, "event_id", eventID, "environment", envID)

	var out struct {
		Environment struct {
			Attributes map[string]any `json:"attributes"`
			Parent     *ref           `json:"parent"`
			Instances  []struct {
				Status string `json:"status"`
			} `json:"instances"`
			Deletable struct {
				Result      bool `json:"result"`
				Constraints []struct {
					Type    string `json:"type"`
					ID      string `json:"id"`
					Message string `json:"message"`
				} `json:"constraints"`
			} `json:"deletable"`
		} `json:"environment"`
	}
	if err := s.api.Do(ctx, environmentQuery, map[string]any{"org": s.api.OrganizationID, "id": envID}, &out); err != nil {
		log.Error("get environment", "error", err)
		return
	}
	e := out.Environment
	owner, _ := e.Attributes["owner"].(string)
	if e.Parent == nil || owner == "" || e.Attributes["sandbox"] != "true" {
		return
	}

	deleteOn := false
	for _, r := range s.rules {
		if r.template() == e.Parent.ID && r.DeleteAfterDecommission {
			deleteOn = true
		}
	}
	if !deleteOn {
		return
	}
	for _, inst := range e.Instances {
		if isActive(inst.Status) {
			return
		}
	}

	// The owner can decommission their own sandbox. Delete it only when no group gives them the template.
	groups := map[string]bool{}
	accountID, err := s.accountIDByEmail(ctx, owner)
	if err != nil {
		log.Error("find owner", "error", err)
		return
	}
	if accountID != "" {
		if groups, err = s.accountGroups(ctx, accountID); err != nil {
			log.Error("list groups", "error", err)
			return
		}
	}
	if s.templatesForGroups(groups)[e.Parent.ID] {
		log.Info("skip: owner still in group", "owner", owner)
		return
	}

	if !e.Deletable.Result {
		b, _ := json.Marshal(e.Deletable.Constraints)
		log.Warn("skip: not deletable", "constraints", string(b))
		return
	}
	if err := s.mutate(ctx, deleteEnvironmentMutation, "deleteEnvironment", envID); err != nil {
		log.Error("delete", "error", err)
		return
	}
	log.Info("deleted", "owner", owner)
}

type ref struct {
	ID string `json:"id"`
}

type sandbox struct {
	ID        string `json:"id"`
	Parent    *ref   `json:"parent"`
	Instances []struct {
		Status string `json:"status"`
	} `json:"instances"`
}

// FAILED counts as active: a failed provision or decommission can leave cloud resources behind.
func isActive(status string) bool { return status == "PROVISIONED" || status == "FAILED" }

func (sb sandbox) active() bool {
	for _, inst := range sb.Instances {
		if isActive(inst.Status) {
			return true
		}
	}
	return false
}

func hasSandboxFrom(sandboxes []sandbox, template string) bool {
	for _, sb := range sandboxes {
		if sb.Parent != nil && sb.Parent.ID == template {
			return true
		}
	}
	return false
}

type cursor struct {
	Next *string `json:"next"`
}

func (s *Sandboxes) sandboxes(ctx context.Context, owner string) ([]sandbox, error) {
	var all []sandbox
	var next *string
	for {
		var out struct {
			Environments struct {
				Cursor cursor    `json:"cursor"`
				Items  []sandbox `json:"items"`
			} `json:"environments"`
		}
		err := s.api.Do(ctx, sandboxesQuery, map[string]any{
			"org": s.api.OrganizationID, "owner": owner, "cursor": map[string]any{"limit": pageLimit, "next": next},
		}, &out)
		if err != nil {
			return nil, err
		}
		all = append(all, out.Environments.Items...)
		if next = out.Environments.Cursor.Next; next == nil {
			return all, nil
		}
	}
}

func (s *Sandboxes) accountGroups(ctx context.Context, accountID string) (map[string]bool, error) {
	groups := map[string]bool{}
	var next *string
	for {
		var out struct {
			Groups struct {
				Cursor cursor `json:"cursor"`
				Items  []struct {
					Name string `json:"name"`
				} `json:"items"`
			} `json:"groups"`
		}
		err := s.api.Do(ctx, accountGroupsQuery, map[string]any{
			"org": s.api.OrganizationID, "accountId": accountID, "cursor": map[string]any{"limit": pageLimit, "next": next},
		}, &out)
		if err != nil {
			return nil, err
		}
		for _, g := range out.Groups.Items {
			groups[g.Name] = true
		}
		if next = out.Groups.Cursor.Next; next == nil {
			return groups, nil
		}
	}
}

// Returns "" when the email is not a member of the organization.
func (s *Sandboxes) accountIDByEmail(ctx context.Context, email string) (string, error) {
	var next *string
	for {
		var out struct {
			Organization struct {
				Members struct {
					Cursor cursor `json:"cursor"`
					Items  []struct {
						ID    string `json:"id"`
						Email string `json:"email"`
					} `json:"items"`
				} `json:"members"`
			} `json:"organization"`
		}
		err := s.api.Do(ctx, membersByEmailQuery, map[string]any{
			"org": s.api.OrganizationID, "search": email, "cursor": map[string]any{"limit": pageLimit, "next": next},
		}, &out)
		if err != nil {
			return "", err
		}
		for _, m := range out.Organization.Members.Items {
			if strings.EqualFold(m.Email, email) {
				return m.ID, nil
			}
		}
		if next = out.Organization.Members.Cursor.Next; next == nil {
			return "", nil
		}
	}
}

// Environment identifiers are 1-20 characters of a-z and 0-9.
func baseIdentifier(email string) string {
	local, _, _ := strings.Cut(strings.ToLower(email), "@")
	var b strings.Builder
	for _, c := range local {
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') {
			b.WriteRune(c)
		}
	}
	id := b.String()
	if id == "" {
		id = "sandbox"
	}
	if len(id) > maxIdentifierLen {
		id = id[:maxIdentifierLen]
	}
	return id
}

func candidate(base string, n int) string {
	if n == 1 {
		return base
	}
	suffix := strconv.Itoa(n)
	if len(base)+len(suffix) > maxIdentifierLen {
		base = base[:maxIdentifierLen-len(suffix)]
	}
	return base + suffix
}

// Two people can share an email local part, and the project can already hold an environment with that identifier.
// forkEnvironment returns an existing fork of the same parent instead of failing, so a taken identifier must be
// skipped before the fork.
func (s *Sandboxes) freeIdentifier(ctx context.Context, project, base string) (string, error) {
	ids := make([]string, 0, maxSuffix)
	for n := 1; n <= maxSuffix; n++ {
		ids = append(ids, project+"-"+candidate(base, n))
	}
	var out struct {
		Environments struct {
			Items []ref `json:"items"`
		} `json:"environments"`
	}
	if err := s.api.Do(ctx, environmentIDsQuery, map[string]any{"org": s.api.OrganizationID, "ids": ids}, &out); err != nil {
		return "", err
	}
	taken := map[string]bool{}
	for _, e := range out.Environments.Items {
		taken[e.ID] = true
	}
	for n := 1; n <= maxSuffix; n++ {
		if c := candidate(base, n); !taken[project+"-"+c] {
			return c, nil
		}
	}
	return "", fmt.Errorf("every identifier from %s to %s is taken", candidate(base, 1), candidate(base, maxSuffix))
}

func (s *Sandboxes) fork(ctx context.Context, rule Rule, id string, m membership) (string, error) {
	name := m.AccountName
	if name == "" {
		name = m.AccountEmail
	}
	attrs, err := massdriver.JSONString(map[string]string{"owner": m.AccountEmail, "sandbox": "true"})
	if err != nil {
		return "", err
	}
	desc := fmt.Sprintf("Sandbox of %s for %s", rule.template(), m.AccountEmail)
	if len(desc) > maxDescriptionLen {
		desc = desc[:maxDescriptionLen]
	}

	var out struct {
		ForkEnvironment massdriver.Payload[ref] `json:"forkEnvironment"`
	}
	err = s.api.Do(ctx, forkEnvironmentMutation, map[string]any{
		"org":      s.api.OrganizationID,
		"parentId": rule.template(),
		"input": map[string]any{
			"id":                      id,
			"name":                    fmt.Sprintf("%s (%s)", name, id),
			"description":             desc,
			"attributes":              attrs,
			"copySecrets":             rule.CopySecrets,
			"copyEnvironmentDefaults": rule.CopyEnvironmentDefaults,
			"copyRemoteReferences":    rule.CopyRemoteReferences,
		},
	}, &out)
	if err != nil {
		return "", err
	}
	if err := out.ForkEnvironment.Err(); err != nil {
		return "", fmt.Errorf("forkEnvironment: %w", err)
	}
	return out.ForkEnvironment.Result.ID, nil
}

func (s *Sandboxes) mutate(ctx context.Context, mutation, field, envID string) error {
	var out map[string]massdriver.Payload[ref]
	if err := s.api.Do(ctx, mutation, map[string]any{"org": s.api.OrganizationID, "id": envID}, &out); err != nil {
		return err
	}
	if err := out[field].Err(); err != nil {
		return fmt.Errorf("%s: %w", field, err)
	}
	return nil
}
