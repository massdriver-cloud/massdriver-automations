package scorecards

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"
)

// fakeMassdriver is an organization in memory: instances, deployments, alarms, audit records, bundle versions,
// groups, and stashes. It is both the API and the Stash.
type fakeMassdriver struct {
	mu          sync.Mutex
	instances   map[string]*Instance
	deployments map[string][]Deployment // by instance, newest first
	alarms      map[string][]Alarm
	audit       []fakeAudit
	tags        map[string][]Tag
	stash       map[string]StashValue // by kind/id/key
	writes      []Write
	deletes     []string                        // kind/id/key
	compare     map[string][]InstanceComparison // by source/target
	groups      []Group
	members     map[string][]string
	actions     map[string][]string // by environment/group
	failing     map[string]error    // by method name
}

type fakeAudit struct {
	instance, actorType string
	event               AuditEvent
}

func newFake() *fakeMassdriver {
	return &fakeMassdriver{instances: map[string]*Instance{}, deployments: map[string][]Deployment{},
		alarms: map[string][]Alarm{}, tags: map[string][]Tag{}, stash: map[string]StashValue{},
		compare: map[string][]InstanceComparison{}, members: map[string][]string{}, actions: map[string][]string{},
		failing: map[string]error{}}
}

// addInstance adds an instance of project/environment/component, such as billing, dev, app.
func (f *fakeMassdriver) addInstance(project, env, component string, attrs map[string]any) *Instance {
	inst := &Instance{ID: project + "-" + env + "-" + component, Status: "PROVISIONED",
		EffectiveAttributes: map[string]any{"md-project": project, "md-environment": env,
			"md-instance": project + "-" + env + "-" + component}}
	for k, v := range attrs {
		inst.EffectiveAttributes[k] = v
	}
	inst.Environment.ID = project + "-" + env
	inst.Environment.Project.ID = project
	f.instances[inst.ID] = inst
	return inst
}

func (f *fakeMassdriver) err(method string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.failing[method]
}

func (f *fakeMassdriver) Instance(_ context.Context, id string) (*Instance, error) {
	if err := f.err("Instance"); err != nil {
		return nil, err
	}
	inst, ok := f.instances[id]
	if !ok {
		return nil, ErrNotFound
	}
	c := *inst
	return &c, nil
}

func (f *fakeMassdriver) Deployments(_ context.Context, instance string, since time.Time) ([]Deployment, error) {
	if err := f.err("Deployments"); err != nil {
		return nil, err
	}
	var out []Deployment
	for _, d := range f.deployments[instance] {
		if d.CreatedAt.Before(since) {
			break
		}
		out = append(out, d)
	}
	return out, nil
}

func (f *fakeMassdriver) Alarms(_ context.Context, instance string) ([]Alarm, error) {
	return f.alarms[instance], f.err("Alarms")
}

func (f *fakeMassdriver) AuditEvents(_ context.Context, instance string, types []string, actorType string, since time.Time) ([]AuditEvent, error) {
	if err := f.err("AuditEvents"); err != nil {
		return nil, err
	}
	var out []AuditEvent
	for _, a := range f.audit {
		if a.instance == instance && contains(types, a.event.Type) && !a.event.OccurredAt.Before(since) &&
			(actorType == "" || actorType == a.actorType) {
			out = append(out, a.event)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].OccurredAt.Before(out[j].OccurredAt) })
	return out, nil
}

func (f *fakeMassdriver) Tags(_ context.Context, repo, channel string) ([]Tag, error) {
	var out []Tag
	for _, t := range f.tags[repo] {
		if strings.Contains(t.Tag, "-") == (channel == "DEV") {
			out = append(out, t)
		}
	}
	return out, f.err("Tags")
}

func (f *fakeMassdriver) StashEntries(_ context.Context, scopes []Scope, keys []string) ([]StashValue, error) {
	if err := f.err("StashEntries"); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []StashValue
	for _, s := range scopes {
		for _, k := range keys {
			if v, ok := f.stash[s.Kind+"/"+s.ID+"/"+k]; ok {
				out = append(out, v)
			}
		}
	}
	return out, nil
}

func (f *fakeMassdriver) Instances(_ context.Context, filter InstanceFilter) ([]InstanceRef, error) {
	var out []InstanceRef
	for _, id := range sortedKeys(f.instances) {
		inst := f.instances[id]
		if filter.Environment != "" && inst.Environment.ID != filter.Environment ||
			filter.Project != "" && inst.Environment.Project.ID != filter.Project ||
			filter.Repo != "" && (inst.Bundle == nil || inst.Bundle.Name != filter.Repo) {
			continue
		}
		r := InstanceRef{ID: inst.ID, Status: inst.Status, EffectiveAttributes: inst.EffectiveAttributes}
		r.Environment.ID = inst.Environment.ID
		r.Environment.Project.ID = inst.Environment.Project.ID
		out = append(out, r)
	}
	return out, nil
}

func (f *fakeMassdriver) ProjectEnvironments(context.Context, string) ([]string, error) {
	return nil, nil
}

func (f *fakeMassdriver) CompareEnvironments(_ context.Context, source, target string) ([]InstanceComparison, error) {
	c, ok := f.compare[source+"/"+target]
	if !ok {
		return nil, ErrNotFound
	}
	return c, nil
}

func (f *fakeMassdriver) Groups(context.Context) ([]Group, error) { return f.groups, nil }

func (f *fakeMassdriver) GroupMembers(_ context.Context, group string) ([]string, error) {
	if err := f.err("GroupMembers"); err != nil {
		return nil, err
	}
	return f.members[group], nil
}

func (f *fakeMassdriver) EnvironmentActions(_ context.Context, env, group string) ([]string, error) {
	if err := f.err("EnvironmentActions:" + env); err != nil {
		return nil, err
	}
	return f.actions[env+"/"+group], nil
}

// Delete removes an entry and records it.
func (f *fakeMassdriver) Delete(_ context.Context, scope Scope, key string) error {
	if err := f.err("Delete"); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deletes = append(f.deletes, scope.Kind+"/"+scope.ID+"/"+key)
	delete(f.stash, scope.Kind+"/"+scope.ID+"/"+key)
	return nil
}

// Set stores a write.
func (f *fakeMassdriver) Set(_ context.Context, w Write) error {
	if err := f.err("Set"); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.writes = append(f.writes, w)
	v := w.Value
	f.stash[w.Scope.Kind+"/"+w.Scope.ID+"/"+w.Key] = stashValue(w.Scope, w.Key, v, "JSON", now)
	return nil
}

// put sets a stash value the way another automation would.
func (f *fakeMassdriver) put(kind, id, key, value string, updated time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stash[kind+"/"+id+"/"+key] = stashValue(Scope{Kind: kind, ID: id}, key, value, "JSON", updated)
}

func stashValue(scope Scope, key, value, format string, updated time.Time) StashValue {
	v := StashValue{Key: key, Value: &value, Format: format, UpdatedAt: updated}
	v.Scope.Kind = strings.ToUpper(scope.Kind)
	v.Scope.ID = scope.ID
	return v
}

func (f *fakeMassdriver) written() []Write {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Write(nil), f.writes...)
}

func (f *fakeMassdriver) value(kind, id, key string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.stash[kind+"/"+id+"/"+key]
	if !ok || v.Value == nil {
		return "", false
	}
	return *v.Value, true
}
