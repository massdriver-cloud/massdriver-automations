package stashsync

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch/types"
)

// The numbers that an alarm query returns for each alarm state.
const (
	AlarmOK               = 0
	AlarmInsufficientData = 1
	AlarmAlarm            = 2
)

// CloudWatchQuery reads the latest data point of one metric, or the state of one alarm.
type CloudWatchQuery struct {
	Namespace  string            `yaml:"namespace"`
	Metric     string            `yaml:"metric"`
	Dimensions map[string]string `yaml:"dimensions"`
	Stat       string            `yaml:"stat"`
	Period     Duration          `yaml:"period"`
	Alarm      string            `yaml:"alarm"`
}

func (*CloudWatchQuery) source() string { return "cloudwatch" }

func (q *CloudWatchQuery) validate() error {
	if q.Alarm != "" {
		if q.Namespace != "" || q.Metric != "" || len(q.Dimensions) > 0 || q.Stat != "" || q.Period != 0 {
			return errors.New("an alarm query takes only alarm")
		}
		return nil
	}
	if q.Namespace == "" || q.Metric == "" {
		return errors.New("namespace and metric are required, or set alarm")
	}
	if q.Stat == "" {
		q.Stat = "Average"
	}
	if q.Period == 0 {
		q.Period = Duration(time.Minute)
	}
	if p := time.Duration(q.Period); p < time.Minute || p > 24*time.Hour || p%time.Minute != 0 {
		return errors.New("period must be a whole number of minutes, up to 24h")
	}
	return nil
}

// cloudWatchAPI is the part of the CloudWatch client that the source calls.
type cloudWatchAPI interface {
	GetMetricData(ctx context.Context, in *cloudwatch.GetMetricDataInput, opts ...func(*cloudwatch.Options)) (*cloudwatch.GetMetricDataOutput, error)
	DescribeAlarms(ctx context.Context, in *cloudwatch.DescribeAlarmsInput, opts ...func(*cloudwatch.Options)) (*cloudwatch.DescribeAlarmsOutput, error)
}

// CloudWatch reads metrics and alarm states with GetMetricData and DescribeAlarms.
type CloudWatch struct {
	api cloudWatchAPI
	now func() time.Time
}

// NewCloudWatch returns a CloudWatch source for an AWS config, such as one from the default credential chain.
func NewCloudWatch(cfg aws.Config) *CloudWatch {
	return &CloudWatch{api: cloudwatch.NewFromConfig(cfg), now: time.Now}
}

// Query returns the latest data point of a metric, or the state of an alarm as AlarmOK, AlarmInsufficientData,
// or AlarmAlarm.
func (c *CloudWatch) Query(ctx context.Context, query Query) (float64, error) {
	q, ok := query.(*CloudWatchQuery)
	if !ok {
		return 0, fmt.Errorf("cloudwatch cannot run a %s query", query.source())
	}
	if q.Alarm != "" {
		return c.alarm(ctx, q.Alarm)
	}
	return c.metric(ctx, q)
}

func (c *CloudWatch) metric(ctx context.Context, q *CloudWatchQuery) (float64, error) {
	period := time.Duration(q.Period)
	end := c.now()
	names := make([]string, 0, len(q.Dimensions))
	for k := range q.Dimensions {
		names = append(names, k)
	}
	sort.Strings(names)
	dims := make([]types.Dimension, 0, len(names))
	for _, k := range names {
		dims = append(dims, types.Dimension{Name: aws.String(k), Value: aws.String(q.Dimensions[k])})
	}
	out, err := c.api.GetMetricData(ctx, &cloudwatch.GetMetricDataInput{
		// Three periods cover the delay before CloudWatch has the latest one.
		StartTime: aws.Time(end.Add(-3 * period)),
		EndTime:   aws.Time(end),
		ScanBy:    types.ScanByTimestampDescending,
		MetricDataQueries: []types.MetricDataQuery{{
			Id: aws.String("m0"),
			MetricStat: &types.MetricStat{
				Metric: &types.Metric{Namespace: aws.String(q.Namespace), MetricName: aws.String(q.Metric), Dimensions: dims},
				// validate keeps the period at or under 24h, which fits.
				Period: aws.Int32(int32(period / time.Second)), //nolint:gosec
				Stat:   aws.String(q.Stat),
			},
		}},
	})
	if err != nil {
		return 0, err
	}
	for _, r := range out.MetricDataResults {
		if r.StatusCode == types.StatusCodeForbidden || r.StatusCode == types.StatusCodeInternalError {
			return 0, fmt.Errorf("cloudwatch status %s", r.StatusCode)
		}
		if len(r.Values) > 0 {
			return r.Values[0], nil
		}
	}
	return 0, ErrNoData
}

func (c *CloudWatch) alarm(ctx context.Context, name string) (float64, error) {
	out, err := c.api.DescribeAlarms(ctx, &cloudwatch.DescribeAlarmsInput{
		AlarmNames: []string{name},
		AlarmTypes: []types.AlarmType{types.AlarmTypeMetricAlarm, types.AlarmTypeCompositeAlarm},
	})
	if err != nil {
		return 0, err
	}
	var state types.StateValue
	switch {
	case len(out.MetricAlarms) > 0:
		state = out.MetricAlarms[0].StateValue
	case len(out.CompositeAlarms) > 0:
		state = out.CompositeAlarms[0].StateValue
	default:
		return 0, errors.New("alarm not found")
	}
	switch state {
	case types.StateValueOk:
		return AlarmOK, nil
	case types.StateValueAlarm:
		return AlarmAlarm, nil
	}
	return AlarmInsufficientData, nil
}
