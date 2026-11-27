package stashsync

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch/types"
)

type fakeCloudWatch struct {
	metricIn *cloudwatch.GetMetricDataInput
	values   []float64
	status   types.StatusCode
	alarms   *cloudwatch.DescribeAlarmsOutput
	alarmIn  *cloudwatch.DescribeAlarmsInput
}

func (f *fakeCloudWatch) GetMetricData(_ context.Context, in *cloudwatch.GetMetricDataInput, _ ...func(*cloudwatch.Options)) (*cloudwatch.GetMetricDataOutput, error) {
	f.metricIn = in
	status := f.status
	if status == "" {
		status = types.StatusCodeComplete
	}
	return &cloudwatch.GetMetricDataOutput{MetricDataResults: []types.MetricDataResult{
		{Id: aws.String("m0"), Values: f.values, StatusCode: status},
	}}, nil
}

func (f *fakeCloudWatch) DescribeAlarms(_ context.Context, in *cloudwatch.DescribeAlarmsInput, _ ...func(*cloudwatch.Options)) (*cloudwatch.DescribeAlarmsOutput, error) {
	f.alarmIn = in
	return f.alarms, nil
}

func TestCloudWatchMetric(t *testing.T) {
	fake := &fakeCloudWatch{values: []float64{42, 40}}
	cw := &CloudWatch{api: fake, now: func() time.Time { return start }}
	q := &CloudWatchQuery{Namespace: "AWS/RDS", Metric: "DatabaseConnections", Stat: "Average",
		Period: Duration(time.Minute), Dimensions: map[string]string{"DBInstanceIdentifier": "ledger-pg"}}
	v, err := cw.Query(context.Background(), q)
	if err != nil || v != 42 {
		t.Fatalf("v, err = %v, %v", v, err)
	}
	in := fake.metricIn
	ms := in.MetricDataQueries[0].MetricStat
	if *ms.Metric.Namespace != "AWS/RDS" || *ms.Metric.MetricName != "DatabaseConnections" || *ms.Stat != "Average" ||
		*ms.Period != 60 || len(ms.Metric.Dimensions) != 1 || *ms.Metric.Dimensions[0].Name != "DBInstanceIdentifier" ||
		*ms.Metric.Dimensions[0].Value != "ledger-pg" {
		t.Errorf("metric stat = %+v", ms)
	}
	if !in.EndTime.Equal(start) || !in.StartTime.Equal(start.Add(-3*time.Minute)) || in.ScanBy != types.ScanByTimestampDescending {
		t.Errorf("window = %v to %v, %v", in.StartTime, in.EndTime, in.ScanBy)
	}

	fake.values = nil
	if _, err := cw.Query(context.Background(), q); !errors.Is(err, ErrNoData) {
		t.Errorf("no values: err = %v", err)
	}
	fake.status = types.StatusCodeForbidden
	if _, err := cw.Query(context.Background(), q); err == nil || errors.Is(err, ErrNoData) {
		t.Errorf("forbidden: err = %v", err)
	}
}

func TestCloudWatchAlarm(t *testing.T) {
	fake := &fakeCloudWatch{}
	cw := &CloudWatch{api: fake, now: time.Now}
	q := &CloudWatchQuery{Alarm: "ledger-cpu"}
	for state, want := range map[types.StateValue]float64{
		types.StateValueOk: AlarmOK, types.StateValueInsufficientData: AlarmInsufficientData, types.StateValueAlarm: AlarmAlarm,
	} {
		fake.alarms = &cloudwatch.DescribeAlarmsOutput{MetricAlarms: []types.MetricAlarm{{StateValue: state}}}
		if v, err := cw.Query(context.Background(), q); err != nil || v != want {
			t.Errorf("%s: v, err = %v, %v", state, v, err)
		}
	}
	if fake.alarmIn.AlarmNames[0] != "ledger-cpu" {
		t.Errorf("alarm names = %v", fake.alarmIn.AlarmNames)
	}

	fake.alarms = &cloudwatch.DescribeAlarmsOutput{CompositeAlarms: []types.CompositeAlarm{{StateValue: types.StateValueAlarm}}}
	if v, err := cw.Query(context.Background(), q); err != nil || v != AlarmAlarm {
		t.Errorf("composite: v, err = %v, %v", v, err)
	}
	fake.alarms = &cloudwatch.DescribeAlarmsOutput{}
	if _, err := cw.Query(context.Background(), q); err == nil {
		t.Error("missing alarm: no error")
	}
}

func TestPrometheus(t *testing.T) {
	var gotAuth, gotQuery, gotPath string
	body := ""
	status := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotPath = r.Header.Get("Authorization"), r.URL.Path
		_ = r.ParseForm()
		gotQuery = r.PostForm.Get("query")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()
	q := &PrometheusQuery{Query: `sum(rate(http_requests_total{job="app"}[5m]))`}

	p := NewPrometheus(srv.URL+"/", "secret-token", "", "", srv.Client())
	body = `{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[1764234000,"12.5"]}]}}`
	v, err := p.Query(context.Background(), q)
	if err != nil || v != 12.5 {
		t.Fatalf("vector: v, err = %v, %v", v, err)
	}
	if gotAuth != "Bearer secret-token" || gotPath != "/api/v1/query" || gotQuery != q.Query {
		t.Errorf("request = %q %q %q", gotAuth, gotPath, gotQuery)
	}

	body = `{"status":"success","data":{"resultType":"scalar","result":[1764234000,"3"]}}`
	if v, err := p.Query(context.Background(), q); err != nil || v != 3 {
		t.Errorf("scalar: v, err = %v, %v", v, err)
	}

	for name, b := range map[string]string{
		"empty": `{"status":"success","data":{"resultType":"vector","result":[]}}`,
		"NaN":   `{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[1764234000,"NaN"]}]}}`,
	} {
		body = b
		if _, err := p.Query(context.Background(), q); !errors.Is(err, ErrNoData) {
			t.Errorf("%s: err = %v", name, err)
		}
	}

	body = `{"status":"success","data":{"resultType":"vector","result":[{"metric":{"a":"1"},"value":[1,"1"]},{"metric":{"a":"2"},"value":[1,"2"]}]}}`
	if _, err := p.Query(context.Background(), q); err == nil || !strings.Contains(err.Error(), "2 series") {
		t.Errorf("two series: err = %v", err)
	}
	body = `{"status":"success","data":{"resultType":"matrix","result":[]}}`
	if _, err := p.Query(context.Background(), q); err == nil {
		t.Error("matrix: no error")
	}
	status = http.StatusBadRequest
	body = `{"status":"error","errorType":"bad_data","error":"parse error"}`
	if _, err := p.Query(context.Background(), q); err == nil || !strings.Contains(err.Error(), "bad_data") {
		t.Errorf("error: err = %v", err)
	}

	status = http.StatusOK
	body = `{"status":"success","data":{"resultType":"scalar","result":[1764234000,"1"]}}`
	basic := NewPrometheus(srv.URL, "", "grafana", "pw", srv.Client())
	if _, err := basic.Query(context.Background(), q); err != nil || gotAuth != "Basic Z3JhZmFuYTpwdw==" {
		t.Errorf("basic auth = %q, err = %v", gotAuth, err)
	}
}
