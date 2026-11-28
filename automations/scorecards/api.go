package scorecards

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"time"

	"github.com/massdriver-cloud/massdriver-automations/pkg/massdriver"
)

var (
	//go:embed queries/instance.graphql
	instanceQuery string
	//go:embed queries/deployments.graphql
	deploymentsQuery string
	//go:embed queries/alarms.graphql
	alarmsQuery string
	//go:embed queries/audit_logs.graphql
	auditLogsQuery string
	//go:embed queries/tags.graphql
	tagsQuery string
	//go:embed queries/stash_entries.graphql
	stashEntriesQuery string
	//go:embed queries/instances.graphql
	instancesQuery string
	//go:embed queries/compare_environments.graphql
	compareEnvironmentsQuery string
	//go:embed queries/groups.graphql
	groupsQuery string
	//go:embed queries/group_members.graphql
	groupMembersQuery string
	//go:embed queries/environment_owners.graphql
	environmentActionsQuery string
	//go:embed queries/project_environments.graphql
	projectEnvironmentsQuery string
)

const (
	pageLimit = 100
	// maxPages bounds every list, so one large organization cannot hold a pass forever.
	maxPages = 50
	// maxStashScopes is the most scopes alphaStashEntriesForScopes takes in one call.
	maxStashScopes = 200
)

// ErrNotFound is an instance or environment that the API does not return, because it is gone or the service
// account cannot view it.
var ErrNotFound = errors.New("not found")

// Instance is the state of one instance that facts and checks read.
type Instance struct {
	ID                  string         `json:"id"`
	Status              string         `json:"status"`
	Params              map[string]any `json:"params"`
	DeployedVersion     string         `json:"deployedVersion"`
	ResolvedVersion     string         `json:"resolvedVersion"`
	FiringAlarmCount    int            `json:"firingAlarmCount"`
	EffectiveAttributes map[string]any `json:"effectiveAttributes"`
	Environment         struct {
		ID      string `json:"id"`
		Project struct {
			ID string `json:"id"`
		} `json:"project"`
	} `json:"environment"`
	Bundle *struct {
		Name string `json:"name"`
	} `json:"bundle"`
	LatestProvision *Deployment `json:"latestProvision"`
}

// Deployment is one deployment of an instance.
type Deployment struct {
	ID                 string         `json:"id"`
	Status             string         `json:"status"`
	Action             string         `json:"action"`
	Version            string         `json:"version"`
	Params             map[string]any `json:"params"`
	CreatedAt          time.Time      `json:"createdAt"`
	LastTransitionedAt *time.Time     `json:"lastTransitionedAt"`
	ElapsedTime        int            `json:"elapsedTime"`
}

// Alarm is one alarm of an instance, with its current state.
type Alarm struct {
	DisplayName  string      `json:"displayName"`
	CurrentState *AlarmState `json:"currentState"`
}

// AlarmState is a state an alarm entered.
type AlarmState struct {
	Status     string    `json:"status"`
	OccurredAt time.Time `json:"occurredAt"`
}

// AuditEvent is one audit log record.
type AuditEvent struct {
	OccurredAt time.Time      `json:"occurredAt"`
	Type       string         `json:"type"`
	Data       map[string]any `json:"data"`
}

// Tag is one published version of a bundle.
type Tag struct {
	Tag       string    `json:"tag"`
	CreatedAt time.Time `json:"createdAt"`
}

// StashValue is one stash entry as read.
type StashValue struct {
	Scope struct {
		Kind string `json:"kind"`
		ID   string `json:"id"`
	} `json:"scope"`
	Key       string     `json:"key"`
	Value     *string    `json:"value"`
	Format    string     `json:"format"`
	ExpiresAt *time.Time `json:"expiresAt"`
	UpdatedAt time.Time  `json:"updatedAt"`
}

// InstanceRef is an instance in a list: where it is and its attributes.
type InstanceRef struct {
	ID                  string         `json:"id"`
	EffectiveAttributes map[string]any `json:"effectiveAttributes"`
	Environment         struct {
		ID      string `json:"id"`
		Project struct {
			ID string `json:"id"`
		} `json:"project"`
	} `json:"environment"`
}

// InstanceComparison is one component compared across two environments.
type InstanceComparison struct {
	Component struct {
		ID string `json:"id"`
	} `json:"component"`
	Source *struct {
		ID string `json:"id"`
	} `json:"source"`
	Target *struct {
		ID string `json:"id"`
	} `json:"target"`
	Version struct {
		Source *string `json:"source"`
		Target *string `json:"target"`
		Equal  bool    `json:"equal"`
	} `json:"version"`
	Params []struct {
		Path  string `json:"path"`
		Equal bool   `json:"equal"`
	} `json:"params"`
	Equal bool `json:"equal"`
}

// Group is a group of the organization.
type Group struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Role string `json:"role"`
}

// Scope is the entity that owns a stash.
type Scope struct {
	Kind string // project, environment, instance, or deployment
	ID   string
}

// API is what scorecards reads from Massdriver.
type API interface {
	Instance(ctx context.Context, id string) (*Instance, error)
	// Deployments returns the instance's deployments created since `since`, newest first.
	Deployments(ctx context.Context, instance string, since time.Time) ([]Deployment, error)
	Alarms(ctx context.Context, instance string) ([]Alarm, error)
	// AuditEvents returns the events of the types on the instance since `since`, oldest first.
	AuditEvents(ctx context.Context, instance string, types []string, actorType string, since time.Time) ([]AuditEvent, error)
	// Tags returns the stable versions of a bundle, newest first.
	Tags(ctx context.Context, repo string) ([]Tag, error)
	StashEntries(ctx context.Context, scopes []Scope, keys []string) ([]StashValue, error)
	// Instances lists instances: every one, or those of an environment, a project, or a bundle.
	Instances(ctx context.Context, f InstanceFilter) ([]InstanceRef, error)
	ProjectEnvironments(ctx context.Context, project string) ([]string, error)
	CompareEnvironments(ctx context.Context, source, target string) ([]InstanceComparison, error)
	Groups(ctx context.Context) ([]Group, error)
	GroupMembers(ctx context.Context, group string) ([]string, error)
	EnvironmentActions(ctx context.Context, environment, group string) ([]string, error)
}

// InstanceFilter selects instances. Empty selects every instance.
type InstanceFilter struct {
	Environment, Project, Repo string
}

// GraphQLAPI calls the Massdriver V2 GraphQL API.
type GraphQLAPI struct {
	Client *massdriver.Client
}

func (a *GraphQLAPI) vars(v map[string]any) map[string]any {
	v["org"] = a.Client.OrganizationID
	return v
}

type page struct {
	Cursor struct {
		Next *string `json:"next"`
	} `json:"cursor"`
	Items json.RawMessage `json:"items"`
}

// pages reads every page of a list. get runs one query and returns the list's page; done stops early.
func pages[T any](ctx context.Context, get func(cursor map[string]any) (*page, error), done func(T) bool) ([]T, error) {
	var out []T
	var next *string
	for i := 0; i < maxPages; i++ {
		p, err := get(map[string]any{"limit": pageLimit, "next": next})
		if err != nil {
			return nil, err
		}
		if p == nil {
			return out, nil
		}
		var items []T
		if err := json.Unmarshal(p.Items, &items); err != nil {
			return nil, err
		}
		for _, it := range items {
			if done != nil && done(it) {
				return out, nil
			}
			out = append(out, it)
		}
		if next = p.Cursor.Next; next == nil {
			return out, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
	}
	return out, nil
}

// Instance reads one instance.
func (a *GraphQLAPI) Instance(ctx context.Context, id string) (*Instance, error) {
	var out struct {
		Instance *Instance `json:"instance"`
	}
	if err := a.Client.Do(ctx, instanceQuery, a.vars(map[string]any{"id": id}), &out); err != nil {
		return nil, err
	}
	if out.Instance == nil {
		return nil, ErrNotFound
	}
	return out.Instance, nil
}

// Deployments reads the instance's deployments, newest first, back to `since`.
func (a *GraphQLAPI) Deployments(ctx context.Context, instance string, since time.Time) ([]Deployment, error) {
	return pages(ctx, func(cursor map[string]any) (*page, error) {
		var out struct {
			Deployments *page `json:"deployments"`
		}
		err := a.Client.Do(ctx, deploymentsQuery, a.vars(map[string]any{"instance": instance, "cursor": cursor}), &out)
		return out.Deployments, err
	}, func(d Deployment) bool { return d.CreatedAt.Before(since) })
}

// Alarms reads the instance's alarms.
func (a *GraphQLAPI) Alarms(ctx context.Context, instance string) ([]Alarm, error) {
	return pages[Alarm](ctx, func(cursor map[string]any) (*page, error) {
		var out struct {
			Alarms *page `json:"instanceAlarms"`
		}
		err := a.Client.Do(ctx, alarmsQuery, a.vars(map[string]any{"instance": instance, "cursor": cursor}), &out)
		return out.Alarms, err
	}, nil)
}

// AuditEvents reads audit log records of the instance.
func (a *GraphQLAPI) AuditEvents(ctx context.Context, instance string, types []string, actorType string, since time.Time) ([]AuditEvent, error) {
	attrs, err := massdriver.JSONString(map[string]any{"md-instance": instance})
	if err != nil {
		return nil, err
	}
	filter := map[string]any{
		"type":       map[string]any{"in": types},
		"occurredAt": map[string]any{"gte": since.UTC().Format(time.RFC3339)},
		"attributes": attrs,
	}
	if actorType != "" {
		filter["actorType"] = map[string]any{"eq": actorType}
	}
	return pages[AuditEvent](ctx, func(cursor map[string]any) (*page, error) {
		var out struct {
			Logs *page `json:"auditLogs"`
		}
		err := a.Client.Do(ctx, auditLogsQuery, a.vars(map[string]any{"filter": filter, "cursor": cursor}), &out)
		return out.Logs, err
	}, nil)
}

// Tags reads a bundle's stable versions, newest first.
func (a *GraphQLAPI) Tags(ctx context.Context, repo string) ([]Tag, error) {
	return pages[Tag](ctx, func(cursor map[string]any) (*page, error) {
		var out struct {
			Repo *struct {
				Tags *page `json:"tags"`
			} `json:"ociRepo"`
		}
		err := a.Client.Do(ctx, tagsQuery, a.vars(map[string]any{"repo": repo, "cursor": cursor}), &out)
		if err != nil || out.Repo == nil {
			return nil, err
		}
		return out.Repo.Tags, nil
	}, nil)
}

// StashEntries reads keys from several stashes, up to 200 scopes a call.
func (a *GraphQLAPI) StashEntries(ctx context.Context, scopes []Scope, keys []string) ([]StashValue, error) {
	var out []StashValue
	for start := 0; start < len(scopes); start += maxStashScopes {
		chunk := scopes[start:min(start+maxStashScopes, len(scopes))]
		refs := make([]map[string]any, len(chunk))
		for i, s := range chunk {
			refs[i] = map[string]any{s.Kind: s.ID}
		}
		vars := map[string]any{"scopes": refs}
		if len(keys) > 0 {
			vars["filter"] = map[string]any{"keys": keys}
		}
		entries, err := pages[StashValue](ctx, func(cursor map[string]any) (*page, error) {
			var res struct {
				Entries *page `json:"alphaStashEntriesForScopes"`
			}
			vars["cursor"] = cursor
			err := a.Client.Do(ctx, stashEntriesQuery, a.vars(vars), &res)
			return res.Entries, err
		}, nil)
		if err != nil {
			return nil, err
		}
		out = append(out, entries...)
	}
	return out, nil
}

// Instances lists the instances that f selects.
func (a *GraphQLAPI) Instances(ctx context.Context, f InstanceFilter) ([]InstanceRef, error) {
	filter := map[string]any{}
	if f.Environment != "" {
		filter["environmentId"] = map[string]any{"eq": f.Environment}
	}
	if f.Project != "" {
		filter["projectId"] = map[string]any{"eq": f.Project}
	}
	if f.Repo != "" {
		filter["ociRepoName"] = map[string]any{"eq": f.Repo}
	}
	return pages[InstanceRef](ctx, func(cursor map[string]any) (*page, error) {
		var out struct {
			Instances *page `json:"instances"`
		}
		err := a.Client.Do(ctx, instancesQuery, a.vars(map[string]any{"filter": filter, "cursor": cursor}), &out)
		return out.Instances, err
	}, nil)
}

// ProjectEnvironments lists the IDs of a project's environments.
func (a *GraphQLAPI) ProjectEnvironments(ctx context.Context, project string) ([]string, error) {
	envs, err := pages[struct {
		ID string `json:"id"`
	}](ctx, func(cursor map[string]any) (*page, error) {
		var out struct {
			Project *struct {
				Environments *page `json:"environments"`
			} `json:"project"`
		}
		err := a.Client.Do(ctx, projectEnvironmentsQuery, a.vars(map[string]any{"id": project, "cursor": cursor}), &out)
		if err != nil || out.Project == nil {
			return nil, err
		}
		return out.Project.Environments, nil
	}, nil)
	ids := make([]string, len(envs))
	for i, e := range envs {
		ids[i] = e.ID
	}
	return ids, err
}

// CompareEnvironments compares the instances of two environments.
func (a *GraphQLAPI) CompareEnvironments(ctx context.Context, source, target string) ([]InstanceComparison, error) {
	var out struct {
		Comparison *struct {
			Instances []InstanceComparison `json:"instances"`
		} `json:"compareEnvironments"`
	}
	err := a.Client.Do(ctx, compareEnvironmentsQuery, a.vars(map[string]any{"source": source, "target": target}), &out)
	if err != nil {
		return nil, err
	}
	if out.Comparison == nil {
		return nil, ErrNotFound
	}
	return out.Comparison.Instances, nil
}

// Groups lists the organization's groups.
func (a *GraphQLAPI) Groups(ctx context.Context) ([]Group, error) {
	return pages[Group](ctx, func(cursor map[string]any) (*page, error) {
		var out struct {
			Groups *page `json:"groups"`
		}
		err := a.Client.Do(ctx, groupsQuery, a.vars(map[string]any{"cursor": cursor}), &out)
		return out.Groups, err
	}, nil)
}

// GroupMembers lists the account IDs of a group's members.
func (a *GraphQLAPI) GroupMembers(ctx context.Context, group string) ([]string, error) {
	members, err := pages[struct {
		ID string `json:"id"`
	}](ctx, func(cursor map[string]any) (*page, error) {
		var out struct {
			Group *struct {
				Members *page `json:"members"`
			} `json:"group"`
		}
		err := a.Client.Do(ctx, groupMembersQuery, a.vars(map[string]any{"id": group, "cursor": cursor}), &out)
		if err != nil || out.Group == nil {
			return nil, err
		}
		return out.Group.Members, nil
	}, nil)
	ids := make([]string, len(members))
	for i, m := range members {
		ids[i] = m.ID
	}
	return ids, err
}

// EnvironmentActions returns the project and environment actions that a group's policies allow on an
// environment.
func (a *GraphQLAPI) EnvironmentActions(ctx context.Context, environment, group string) ([]string, error) {
	var out struct {
		Environment *struct {
			Actions []string `json:"groupAllowedActions"`
		} `json:"environment"`
	}
	err := a.Client.Do(ctx, environmentActionsQuery, a.vars(map[string]any{"id": environment, "group": group}), &out)
	if err != nil {
		return nil, err
	}
	if out.Environment == nil {
		return nil, ErrNotFound
	}
	return out.Environment.Actions, nil
}
