// Package autodeploydownstream deploys the opted-in dependents of an instance after the instance deploys.
package autodeploydownstream

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/massdriver-cloud/massdriver-automations/pkg/automation"
	"github.com/massdriver-cloud/massdriver-automations/pkg/massdriver"
)

const pageLimit = 100

var (
	//go:embed queries/upstream.graphql
	upstreamQuery string
	//go:embed queries/resource_consumers.graphql
	resourceConsumersQuery string
	//go:embed queries/environment_instances.graphql
	environmentInstancesQuery string
	//go:embed queries/create_deployment.graphql
	createDeploymentMutation string
	//go:embed queries/propose_deployment.graphql
	proposeDeploymentMutation string
)

// Definition registers the automation.
var Definition = automation.Definition{
	Name:        "autodeploy-downstream",
	Description: "Deploys opted-in dependents when an instance deploys.",
	Events:      []string{"deployment.completed"},
	New: func() (automation.Automation, error) {
		var cfg Config
		if err := automation.LoadConfig(&cfg); err != nil {
			return nil, err
		}
		api, err := massdriver.FromEnv()
		if err != nil {
			return nil, err
		}
		return New(api, cfg), nil
	},
}

// Autodeploy holds the API client and the rules that opt instances in.
type Autodeploy struct {
	api     *massdriver.Client
	results massdriver.Results
	rules   []Rule
	run     func(func())
}

// New returns the automation.
func New(api *massdriver.Client, cfg Config) *Autodeploy {
	return &Autodeploy{api: api, results: massdriver.NewResults(api, cfg.Results), rules: cfg.Rules, run: func(f func()) { go f() }}
}

// Handle starts the downstream deploys after a completed PROVISION.
func (a *Autodeploy) Handle(ctx context.Context, ev automation.Event) error {
	upstream := ev.Attribute("md-instance")
	if ev.Type != "deployment.completed" || ev.Metadata("deployment_action") != "PROVISION" || upstream == "" {
		return nil
	}
	var dep struct {
		ID string `json:"id"`
	}
	if err := ev.DecodeEntity(&dep); err != nil {
		return err
	}
	var rules []Rule
	for _, r := range a.rules {
		if matches(r.Upstream, ev.Data.Attributes) {
			rules = append(rules, r)
		}
	}
	if len(rules) == 0 {
		automation.Logger(ctx).Debug("deploy skipped", "upstream_instance", upstream, "reason", "no_upstream_rule")
		return nil
	}
	// A cascade takes longer than the 10 seconds Massdriver waits for a response.
	bg := context.WithoutCancel(ctx)
	a.run(func() { a.deployDownstream(bg, rules, upstream, dep.ID) })
	return nil
}

type instance struct {
	ID                  string          `json:"id"`
	Status              string          `json:"status"`
	Params              json.RawMessage `json:"params"`
	EffectiveAttributes map[string]any  `json:"effectiveAttributes"`
	Dependencies        []struct {
		Resource struct {
			ID string `json:"id"`
		} `json:"resource"`
	} `json:"dependencies"`
}

func (a *Autodeploy) deployDownstream(ctx context.Context, rules []Rule, upstream, deploymentID string) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	log := automation.Logger(ctx).With("upstream_instance", upstream, "upstream_deployment_id", deploymentID)

	downstream, err := a.findDownstream(ctx, upstream)
	if err != nil {
		log.Error("downstream lookup failed", "err", err)
		return
	}

	for _, inst := range downstream {
		l := log.With("instance", inst.ID)
		i := slices.IndexFunc(rules, func(r Rule) bool { return matches(r.Downstream, inst.EffectiveAttributes) })
		if i < 0 {
			l.Debug("deploy skipped", "reason", "not_opted_in")
			continue
		}
		rule := rules[i]
		l = l.With("rule", rule.Name)
		if inst.Status != "PROVISIONED" {
			l.Debug("deploy skipped", "reason", "not_provisioned", "status", inst.Status)
			continue
		}
		msg := fmt.Sprintf("Auto-deploy (rule %s): upstream %s completed deployment %s", rule.Name, upstream, deploymentID)
		if rule.Action == ActionPropose {
			id, err := a.deploy(ctx, proposeDeploymentMutation, "proposeDeployment", inst, msg)
			if err != nil {
				l.Error("deployment propose failed", "err", err)
				continue
			}
			l.Info("deployment proposed", "deployment_id", id)
			a.writeStep(ctx, id, fmt.Sprintf("Proposed by rule %s after upstream %s completed deployment %s", rule.Name, upstream, deploymentID))
			continue
		}
		id, err := a.deploy(ctx, createDeploymentMutation, "createDeployment", inst, msg)
		if err != nil {
			l.Error("deployment create failed", "err", err)
			continue
		}
		l.Info("deployment created", "deployment_id", id)
		a.writeStep(ctx, id, fmt.Sprintf("Deployed by rule %s after upstream %s completed deployment %s", rule.Name, upstream, deploymentID))
	}
}

func (a *Autodeploy) writeStep(ctx context.Context, deploymentID, message string) {
	a.results.Write(ctx, "deployment", deploymentID, "step/autodeploy", "Autodeploy", massdriver.Step{
		Tool: "autodeploy-downstream", Status: "succeeded", FinishedAt: massdriver.Timestamp(time.Now()), Message: message,
	})
}

// An instance is downstream when one of its dependencies resolves to a resource the upstream produces.
// Candidates come from all three ways a dependency is filled: links and environment defaults in the
// upstream's environment, environment defaults in other environments, and remote references.
func (a *Autodeploy) findDownstream(ctx context.Context, upstream string) ([]instance, error) {
	var up struct {
		Instance struct {
			Environment struct {
				ID string `json:"id"`
			} `json:"environment"`
			Resources []struct {
				Resource struct {
					ID string `json:"id"`
				} `json:"resource"`
			} `json:"resources"`
		} `json:"instance"`
	}
	if err := a.api.Do(ctx, upstreamQuery, map[string]any{"org": a.api.OrganizationID, "id": upstream}, &up); err != nil {
		return nil, err
	}

	produced := map[string]bool{}
	environments := map[string]bool{up.Instance.Environment.ID: true}
	candidates := map[string]instance{}

	for _, res := range up.Instance.Resources {
		produced[res.Resource.ID] = true
		refs, envs, err := a.resourceConsumers(ctx, res.Resource.ID)
		if err != nil {
			return nil, err
		}
		for _, inst := range refs {
			candidates[inst.ID] = inst
		}
		for _, env := range envs {
			environments[env] = true
		}
	}

	for env := range environments {
		var out struct {
			Environment struct {
				Instances []instance `json:"instances"`
			} `json:"environment"`
		}
		if err := a.api.Do(ctx, environmentInstancesQuery, map[string]any{"org": a.api.OrganizationID, "id": env}, &out); err != nil {
			return nil, err
		}
		for _, inst := range out.Environment.Instances {
			candidates[inst.ID] = inst
		}
	}

	var downstream []instance
	for _, inst := range candidates {
		if inst.ID == upstream {
			continue
		}
		for _, dep := range inst.Dependencies {
			if produced[dep.Resource.ID] {
				downstream = append(downstream, inst)
				break
			}
		}
	}
	return downstream, nil
}

type cursor struct {
	Next *string `json:"next"`
}

func (a *Autodeploy) resourceConsumers(ctx context.Context, resourceID string) ([]instance, []string, error) {
	var insts []instance
	var envs []string
	var refsNext, defaultsNext *string
	refsDone, defaultsDone := false, false

	for !refsDone || !defaultsDone {
		var out struct {
			Resource struct {
				RemoteReferences struct {
					Cursor cursor `json:"cursor"`
					Items  []struct {
						Instance instance `json:"instance"`
					} `json:"items"`
				} `json:"remoteReferences"`
				EnvironmentDefaults struct {
					Cursor cursor `json:"cursor"`
					Items  []struct {
						Environment struct {
							ID string `json:"id"`
						} `json:"environment"`
					} `json:"items"`
				} `json:"environmentDefaults"`
			} `json:"resource"`
		}
		err := a.api.Do(ctx, resourceConsumersQuery, map[string]any{
			"org":      a.api.OrganizationID,
			"id":       resourceID,
			"refs":     map[string]any{"limit": pageLimit, "next": refsNext},
			"defaults": map[string]any{"limit": pageLimit, "next": defaultsNext},
		}, &out)
		if err != nil {
			return nil, nil, err
		}

		if !refsDone {
			for _, item := range out.Resource.RemoteReferences.Items {
				insts = append(insts, item.Instance)
			}
			refsNext = out.Resource.RemoteReferences.Cursor.Next
			refsDone = refsNext == nil
		}
		if !defaultsDone {
			for _, item := range out.Resource.EnvironmentDefaults.Items {
				envs = append(envs, item.Environment.ID)
			}
			defaultsNext = out.Resource.EnvironmentDefaults.Cursor.Next
			defaultsDone = defaultsNext == nil
		}
	}
	return insts, envs, nil
}

// deploy runs createDeployment or proposeDeployment, which take the same arguments and return the same payload.
func (a *Autodeploy) deploy(ctx context.Context, mutation, field string, inst instance, message string) (string, error) {
	params := string(inst.Params)
	if params == "" || params == "null" {
		params = "{}"
	}

	var out map[string]massdriver.Payload[struct {
		ID string `json:"id"`
	}]
	err := a.api.Do(ctx, mutation, map[string]any{
		"org":   a.api.OrganizationID,
		"id":    inst.ID,
		"input": map[string]any{"action": "PROVISION", "params": params, "message": message},
	}, &out)
	if err != nil {
		return "", err
	}
	payload := out[field]
	if err := payload.Err(); err != nil {
		return "", err
	}
	return payload.Result.ID, nil
}
