package scorecards

import (
	"reflect"
	"testing"
	"time"
)

func TestDeliveryWithNoDeploymentsLeavesOutRates(t *testing.T) {
	d := delivery(30*day, nil, nil, nil)
	want := Delivery{Tool: "scorecards", WindowDays: 30}
	if !reflect.DeepEqual(d, want) {
		t.Errorf("delivery = %+v", d)
	}
}

func TestDeliveryCountsRollbacksAndLastStatus(t *testing.T) {
	deps := []Deployment{
		{ID: "d5", Status: "REJECTED", Action: "PROVISION", Version: "2.0.0", CreatedAt: ago(time.Hour)},
		{ID: "d4", Status: "COMPLETED", Action: "PROVISION", Version: "1.1.0", CreatedAt: ago(day), ElapsedTime: 40,
			LastTransitionedAt: tp(ago(day))},
		{ID: "d3", Status: "ABORTED", Action: "PROVISION", Version: "1.2.0", CreatedAt: ago(2 * day), ElapsedTime: 10},
		{ID: "d2", Status: "COMPLETED", Action: "PROVISION", Version: "1.2.0", CreatedAt: ago(3 * day), ElapsedTime: 50,
			LastTransitionedAt: tp(ago(3 * day))},
		{ID: "d1", Status: "COMPLETED", Action: "DECOMMISSION", Version: "1.2.0", CreatedAt: ago(4 * day), ElapsedTime: 20},
	}
	d := delivery(7*day, deps, map[string]bool{"d4": true, "d5": true}, nil)
	rate, med := 0.0, 30.0
	want := Delivery{Tool: "scorecards", WindowDays: 7, Deployments: 4, Failed: 0, RolledBack: 1, Automated: 1,
		FailureRate: &rate, MedianDurationSeconds: &med, LastStatus: "rejected", LastCompletedAt: "2026-11-26T12:00:00Z"}
	if !reflect.DeepEqual(d, want) {
		t.Errorf("delivery = %+v\nwant %+v", d, want)
	}
}

func TestReliabilityFromAlarmChanges(t *testing.T) {
	change := func(name, status string, at time.Time) AuditEvent {
		return AuditEvent{Type: "alarm_state.created", OccurredAt: at,
			Data: map[string]any{"entity": map[string]any{"alarm_id": name, "status": status}}}
	}
	alarms := []Alarm{
		{ID: "cpu", CurrentState: &AlarmState{Status: "ALARM", OccurredAt: ago(time.Hour)}},
		{ID: "disk", CurrentState: &AlarmState{Status: "OK", OccurredAt: ago(5 * day)}},
		// latency fired before the window and has not changed since.
		{ID: "latency", CurrentState: &AlarmState{Status: "ALARM", OccurredAt: ago(40 * day)}},
		{ID: "new"},
	}
	changes := []AuditEvent{
		// disk was firing when the window started, and recovered.
		change("disk", "OK", ago(5*day)),
		change("cpu", "ALARM", ago(3*day)),
		change("cpu", "OK", ago(3*day-10*time.Minute)),
		change("cpu", "ALARM", ago(time.Hour)),
	}
	r := reliability(now, 30*day, alarms, changes)
	med := 600.0
	want := Reliability{Tool: "scorecards", WindowDays: 30, Alarms: 4, Firing: 2, Incidents: 2,
		SecondsInAlarm: (25*day + 10*time.Minute + time.Hour + 30*day).Seconds(), MedianRecoverySeconds: &med,
		LastFiredAt: "2026-11-27T11:00:00Z"}
	if !reflect.DeepEqual(r, want) {
		t.Errorf("reliability = %+v\nwant %+v", r, want)
	}
}

func TestCurrency(t *testing.T) {
	tags := []Tag{{Tag: "2.0.0", CreatedAt: ago(2 * day)}, {Tag: "1.10.0", CreatedAt: ago(9 * day)},
		{Tag: "1.9.0", CreatedAt: ago(20 * day)}}
	c, ok := currency(now, "1.9.0", tags)
	days := 9
	if !ok || !reflect.DeepEqual(c, Currency{Tool: "scorecards", Version: "1.9.0", Latest: "2.0.0", Behind: 2,
		LatestPublishedAt: "2026-11-25T12:00:00Z", DaysBehind: &days}) {
		t.Errorf("currency = %+v", c)
	}
	if c, _ := currency(now, "2.0.0", tags); c.Behind != 0 || *c.DaysBehind != 0 {
		t.Errorf("current = %+v", c)
	}
	if c, _ := currency(now, "2.0.0-dev.20261101T000000Z", tags); c.Behind != 1 {
		t.Errorf("pre-release = %+v", c)
	}
	dev := []Tag{{Tag: "1.0.0", CreatedAt: ago(10 * day)}, {Tag: "1.0.0-dev.20261120T000000Z", CreatedAt: ago(7 * day)},
		{Tag: "1.0.0-dev.20261124T000000Z", CreatedAt: ago(3 * day)}, {Tag: "1.0.0-dev.20261126T000000Z", CreatedAt: ago(day)}}
	days = 3
	if c, _ := currency(now, "1.0.0-dev.20261120T000000Z", dev); !reflect.DeepEqual(c, Currency{Tool: "scorecards",
		Version: "1.0.0-dev.20261120T000000Z", Latest: "1.0.0-dev.20261126T000000Z", Behind: 2,
		LatestPublishedAt: "2026-11-26T12:00:00Z", DaysBehind: &days}) {
		t.Errorf("development build = %+v", c)
	}
	if _, ok := currency(now, "", tags); ok {
		t.Error("never deployed has currency")
	}
}

func TestPending(t *testing.T) {
	inst := &Instance{Params: map[string]any{"size": "large"}, DeployedVersion: "1.0.0", ResolvedVersion: "1.0.0",
		LatestProvision: &Deployment{Status: "COMPLETED", Params: map[string]any{"size": "small"},
			LastTransitionedAt: tp(ago(day))}}
	if p := pending(inst); !p.Changed || p.DeployedAt != "2026-11-26T12:00:00Z" {
		t.Errorf("params changed = %+v", p)
	}
	inst.Params["size"] = "small"
	if p := pending(inst); p.Changed {
		t.Errorf("same params = %+v", p)
	}
	inst.ResolvedVersion = "1.1.0"
	if p := pending(inst); !p.Changed {
		t.Errorf("new version = %+v", p)
	}
}

func TestCompareVersions(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want int
	}{
		{"1.10.0", "1.9.0", 1}, {"1.2.0", "1.2.0", 0}, {"1.2.0-dev.1", "1.2.0", -1}, {"2.0.0", "10.0.0", -1},
		{"1.2.0-dev.2", "1.2.0-dev.1", 1},
	} {
		if got := compareVersions(c.a, c.b); got != c.want {
			t.Errorf("compareVersions(%s, %s) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestParityListsAtMost50Items(t *testing.T) {
	var cs []InstanceComparison
	for i := 0; i < 60; i++ {
		c := InstanceComparison{}
		c.Component.ID = "c"
		cs = append(cs, c)
	}
	p := parity(now, "ref", cs)
	if p.Differences != 60 || len(p.Items) != 50 {
		t.Errorf("differences %d, items %d", p.Differences, len(p.Items))
	}
}
