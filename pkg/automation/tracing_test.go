package automation

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

const (
	eventTraceparent  = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"
	headerTraceparent = "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01"
)

func recordSpans(t *testing.T) *tracetest.InMemoryExporter {
	t.Helper()
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	t.Cleanup(func() {
		otel.SetTracerProvider(noop.NewTracerProvider())
		otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator())
		_ = tp.Shutdown(context.Background())
	})
	return exp
}

func onlySpan(t *testing.T, exp *tracetest.InMemoryExporter) tracetest.SpanStub {
	t.Helper()
	spans := exp.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("got %d spans, want 1: %v", len(spans), spans)
	}
	return spans[0]
}

func attrs(s tracetest.SpanStub) map[attribute.Key]attribute.Value {
	m := map[attribute.Key]attribute.Value{}
	for _, kv := range s.Attributes {
		m[kv.Key] = kv.Value
	}
	return m
}

const tracedEventBody = `{"specversion":"1.0","id":"evt-7","type":"deployment.failed",` +
	`"subject":"mri:acme:api:prod:database","traceparent":"` + eventTraceparent + `","data":{}}`

func TestWebhookSpanContinuesTheEventTrace(t *testing.T) {
	exp := recordSpans(t)
	h := Handler(automationFunc(func(context.Context, Event) error { return nil }), testSecret)

	req := signedRequest("evt-7", tracedEventBody)
	req.Header.Set("traceparent", headerTraceparent)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d", rec.Code)
	}

	span := onlySpan(t, exp)
	if span.Name != "POST /hooks/massdriver" || span.SpanKind != trace.SpanKindServer {
		t.Errorf("span = %q %v, want POST /hooks/massdriver server", span.Name, span.SpanKind)
	}
	if got := span.Parent.TraceID().String(); got != "4bf92f3577b34da6a3ce929d0e0e4736" {
		t.Errorf("parent trace = %s, want the event's", got)
	}
	if got := span.Parent.SpanID().String(); got != "00f067aa0ba902b7" || !span.Parent.IsRemote() {
		t.Errorf("parent span = %s, want the event's remote span", got)
	}
	a := attrs(span)
	for k, want := range map[attribute.Key]attribute.Value{
		"cloudevents.event_id":      attribute.StringValue("evt-7"),
		"cloudevents.event_type":    attribute.StringValue("deployment.failed"),
		"cloudevents.event_subject": attribute.StringValue("mri:acme:api:prod:database"),
		"http.request.method":       attribute.StringValue("POST"),
		"http.route":                attribute.StringValue("/hooks/massdriver"),
		"http.response.status_code": attribute.IntValue(204),
	} {
		if a[k] != want {
			t.Errorf("%s = %v, want %v", k, a[k].String(), want.String())
		}
	}
}

func TestWebhookSpanContinuesTheHeaderTrace(t *testing.T) {
	exp := recordSpans(t)
	h := Handler(automationFunc(func(context.Context, Event) error { return nil }), testSecret)

	req := signedRequest("evt-1", eventBody)
	req.Header.Set("traceparent", headerTraceparent)
	h.ServeHTTP(httptest.NewRecorder(), req)

	span := onlySpan(t, exp)
	if got := span.Parent.SpanID().String(); got != "b7ad6b7169203331" {
		t.Errorf("parent span = %s, want the header's", got)
	}
}

func TestWebhookSpanIsARootWithoutATraceparent(t *testing.T) {
	exp := recordSpans(t)
	h := Handler(automationFunc(func(context.Context, Event) error { return nil }), testSecret)

	h.ServeHTTP(httptest.NewRecorder(), signedRequest("evt-1", eventBody))

	span := onlySpan(t, exp)
	if span.Parent.IsValid() {
		t.Errorf("parent = %v, want none", span.Parent)
	}
	if !span.SpanContext.IsValid() {
		t.Error("span has no trace id")
	}
}

func TestWebhookSpanRecordsTheHandlerError(t *testing.T) {
	exp := recordSpans(t)
	h := Handler(automationFunc(func(context.Context, Event) error { return errors.New("deploy refused") }), testSecret)

	h.ServeHTTP(httptest.NewRecorder(), signedRequest("evt-1", eventBody))

	span := onlySpan(t, exp)
	if span.Status.Code != codes.Error {
		t.Errorf("status = %v, want error", span.Status)
	}
	if len(span.Events) != 1 || span.Events[0].Name != "exception" {
		t.Fatalf("events = %v, want one exception", span.Events)
	}
	if got := attrs(span)["http.response.status_code"]; got != attribute.IntValue(500) {
		t.Errorf("status code = %v", got.String())
	}
}

func TestOtherRoutesGetSpansAndHealthzDoesNot(t *testing.T) {
	exp := recordSpans(t)
	h := Handler(routedAutomation{}, testSecret)

	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/healthz", nil))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/status", nil))

	span := onlySpan(t, exp)
	if span.Name != "GET /status" {
		t.Errorf("span = %q, want GET /status", span.Name)
	}
}

type routedAutomation struct{}

func (routedAutomation) Handle(context.Context, Event) error { return nil }

func (routedAutomation) Routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /status", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })
}

func TestLogsCarryTheTraceOfTheRequest(t *testing.T) {
	exp := recordSpans(t)
	log, buf := newTestLogger(t, "info")
	h := newHandler(automationFunc(func(ctx context.Context, ev Event) error {
		Logger(ctx).Info("deployment created")
		return nil
	}), testSecret, log)

	h.ServeHTTP(httptest.NewRecorder(), signedRequest("evt-7", tracedEventBody))

	span := onlySpan(t, exp)
	lines := buf.lines(t)
	for _, msg := range []string{"deployment created", "request"} {
		line := findMsg(t, lines, msg)
		if line["trace_id"] != "4bf92f3577b34da6a3ce929d0e0e4736" || line["span_id"] != span.SpanContext.SpanID().String() {
			t.Errorf("%s line trace_id=%v span_id=%v, want the request span", msg, line["trace_id"], line["span_id"])
		}
		if line["event_id"] == nil && msg == "deployment created" {
			t.Errorf("%s line lost event_id", msg)
		}
	}
}

func TestLogsHaveNoTraceWithoutTracing(t *testing.T) {
	log, buf := newTestLogger(t, "info")
	h := newHandler(automationFunc(func(ctx context.Context, ev Event) error {
		Logger(ctx).Info("deployment created")
		return nil
	}), testSecret, log)

	h.ServeHTTP(httptest.NewRecorder(), signedRequest("evt-7", tracedEventBody))

	for _, line := range buf.lines(t) {
		if _, ok := line["trace_id"]; ok {
			t.Errorf("%s line has trace_id without tracing", line["msg"])
		}
	}
}

func TestStartTracingWithoutAnEndpointIsANoop(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "")
	otel.SetTracerProvider(noop.NewTracerProvider())

	shutdown, err := StartTracing(context.Background(), "router", "1.2.3")
	if err != nil {
		t.Fatal(err)
	}
	if err := shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := otel.GetTracerProvider().(noop.TracerProvider); !ok {
		t.Errorf("tracer provider = %T, want it unchanged", otel.GetTracerProvider())
	}
	if _, span := tracer().Start(context.Background(), "x"); span.IsRecording() {
		t.Error("span records without an endpoint")
	}
}

func TestStartTracingRejectsAnUnknownProtocol(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://127.0.0.1:4318")
	t.Setenv("OTEL_EXPORTER_OTLP_PROTOCOL", "http/json")
	if _, err := StartTracing(context.Background(), "router", "1.2.3"); err == nil {
		t.Fatal("want an error")
	}
}
