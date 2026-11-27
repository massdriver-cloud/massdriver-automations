package stashsync

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Preset is a named set of rules for a common metric. It fills a widget key on one instance.
type Preset struct {
	Preset   string            `yaml:"preset"`
	Instance string            `yaml:"instance"`
	With     map[string]string `yaml:"with"`
}

// presetDef builds a preset's rules. need are the `with` keys it requires, and may are the ones it also takes.
type presetDef struct {
	need, may []string
	build     func(instance string, with map[string]string) ([]Rule, error)
}

// Presets are the names that `preset` takes.
var presets = map[string]presetDef{
	"aws-rds-connections": {
		need: []string{"db_instance", "max"},
		may:  []string{"label"},
		build: func(instance string, with map[string]string) ([]Rule, error) {
			limit, err := strconv.ParseFloat(with["max"], 64)
			if err != nil || limit <= 0 {
				return nil, errors.New("with.max must be a number more than 0")
			}
			label := with["label"]
			if label == "" {
				label = "connections"
			}
			t := Target{Instance: instance, Key: "md/v1/usage"}
			return []Rule{
				{Target: field(t, "value"), CloudWatch: &CloudWatchQuery{
					Namespace: "AWS/RDS", Metric: "DatabaseConnections", Stat: "Average", Period: Duration(time.Minute),
					Dimensions: map[string]string{"DBInstanceIdentifier": with["db_instance"]},
				}},
				{Target: field(t, "max"), Value: limit},
				{Target: field(t, "label"), Value: label},
			}, nil
		},
	},
	"aws-alb-traffic": {
		need: []string{"load_balancer"},
		may:  []string{"target_group"},
		build: func(instance string, with map[string]string) ([]Rule, error) {
			dims := map[string]string{"LoadBalancer": with["load_balancer"]}
			if tg := with["target_group"]; tg != "" {
				dims["TargetGroup"] = tg
			}
			metric := func(name, stat string, period time.Duration) *CloudWatchQuery {
				return &CloudWatchQuery{Namespace: "AWS/ApplicationELB", Metric: name, Stat: stat,
					Period: Duration(period), Dimensions: dims}
			}
			t := Target{Instance: instance, Key: "md/v1/traffic"}
			return []Rule{
				// RequestCount is a sum for each period. A period of 60s divided by 60 is requests per second.
				{Target: field(t, "rps"), CloudWatch: metric("RequestCount", "Sum", time.Minute),
					Scale: ptr(1.0 / 60), Default: ptr(0.0)},
				// CloudWatch has no data point for a period without errors.
				{Target: field(t, "errors_5m"), CloudWatch: metric("HTTPCode_Target_5XX_Count", "Sum", 5*time.Minute),
					Default: ptr(0.0)},
				{Target: field(t, "p95_ms"), CloudWatch: metric("TargetResponseTime", "p95", time.Minute),
					Scale: ptr(1000.0)},
			}, nil
		},
	},
	"aws-alarm-health": {
		need: []string{"alarm"},
		build: func(instance string, with map[string]string) ([]Rule, error) {
			return []Rule{{
				Target:     Target{Instance: instance, Key: "md/v1/health"},
				CloudWatch: &CloudWatchQuery{Alarm: with["alarm"]},
				Text: []TextRule{
					{Below: ptr(float64(AlarmInsufficientData)), Text: "healthy"},
					{Below: ptr(float64(AlarmAlarm)), Text: "degraded"},
					{Text: "down"},
				},
			}}, nil
		},
	},
	"prometheus-traffic": {
		may: []string{"job", "selector", "requests_metric", "duration_metric", "code_label"},
		build: func(instance string, with map[string]string) ([]Rule, error) {
			sel := with["selector"]
			if job := with["job"]; job != "" {
				if sel != "" {
					return nil, errors.New("set with.job or with.selector, not both")
				}
				sel = "job=" + strconv.Quote(job)
			}
			if sel == "" {
				return nil, errors.New("with.job or with.selector is required")
			}
			requests := or(with["requests_metric"], "http_requests_total")
			duration := or(with["duration_metric"], "http_request_duration_seconds")
			code := or(with["code_label"], "code")
			t := Target{Instance: instance, Key: "md/v1/traffic"}
			q := func(s string) *PrometheusQuery { return &PrometheusQuery{Query: s} }
			return []Rule{
				{Target: field(t, "rps"), Default: ptr(0.0),
					Prometheus: q(fmt.Sprintf(`sum(rate(%s{%s}[5m]))`, requests, sel))},
				{Target: field(t, "errors_5m"), Default: ptr(0.0),
					Prometheus: q(fmt.Sprintf(`sum(increase(%s{%s,%s=~"5.."}[5m]))`, requests, sel, code))},
				{Target: field(t, "p95_ms"), Prometheus: q(fmt.Sprintf(
					`histogram_quantile(0.95, sum by (le) (rate(%s_bucket{%s}[5m]))) * 1000`, duration, sel))},
			}, nil
		},
	},
}

// PresetNames returns every preset name, sorted.
func PresetNames() []string {
	out := make([]string, 0, len(presets))
	for name := range presets {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

func (p Preset) rules() ([]Rule, error) {
	def, ok := presets[p.Preset]
	if !ok {
		return nil, fmt.Errorf("preset must be one of %s", strings.Join(PresetNames(), ", "))
	}
	if p.Instance == "" {
		return nil, errors.New("instance is required")
	}
	allowed := map[string]bool{}
	for _, k := range append(append([]string{}, def.need...), def.may...) {
		allowed[k] = true
	}
	for k := range p.With {
		if !allowed[k] {
			return nil, fmt.Errorf("with.%s is not a setting of %s", k, p.Preset)
		}
	}
	for _, k := range def.need {
		if p.With[k] == "" {
			return nil, fmt.Errorf("with.%s is required", k)
		}
	}
	return def.build(p.Instance, p.With)
}

func field(t Target, name string) Target {
	t.Field = name
	return t
}

func ptr[T any](v T) *T { return &v }

func or(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}
