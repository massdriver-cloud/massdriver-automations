package massdriver

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"

	"github.com/massdriver-cloud/massdriver-automations/pkg/automation"
)

func TestAPICallFromBackgroundWorkIsAChildSpanOfTheWebhook(t *testing.T) {
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	t.Cleanup(func() {
		otel.SetTracerProvider(noop.NewTracerProvider())
		otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator())
	})

	traceparents := make(chan string, 1)
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		traceparents <- r.Header.Get("traceparent")
		w.Write([]byte(`{"data":{"ok":true}}`))
	}))
	t.Cleanup(api.Close)
	c := &Client{Endpoint: api.URL + "/api/v2", APIKey: "md_test"}

	secret := []byte("test-signing-secret")
	done := make(chan error, 1)
	h := automation.Handler(handlerFunc(func(ctx context.Context, _ automation.Event) error {
		bg := context.WithoutCancel(ctx)
		go func() { done <- c.Do(bg, "query Groups($id: ID!) { ok }", map[string]any{"id": "ops"}, nil) }()
		return nil
	}), secret)
	body := `{"id":"evt-1","type":"group.created","data":{}}`
	req := httptest.NewRequest(http.MethodPost, automation.HookPath, strings.NewReader(body))
	automation.SignRequest(req, secret, "evt-1", []byte(body), time.Now())
	h.ServeHTTP(httptest.NewRecorder(), req)
	if err := <-done; err != nil {
		t.Fatal(err)
	}

	var server, client tracetest.SpanStub
	for _, s := range exp.GetSpans() {
		switch s.SpanKind {
		case trace.SpanKindServer:
			server = s
		case trace.SpanKindClient:
			client = s
		}
	}
	if client.Name != "graphql Groups" {
		t.Fatalf("client span = %q, want graphql Groups", client.Name)
	}
	if client.Parent.SpanID() != server.SpanContext.SpanID() {
		t.Errorf("client span parent = %s, want the webhook span %s", client.Parent.SpanID(), server.SpanContext.SpanID())
	}
	want := "00-" + client.SpanContext.TraceID().String() + "-" + client.SpanContext.SpanID().String() + "-01"
	if got := <-traceparents; got != want {
		t.Errorf("traceparent = %q, want %q", got, want)
	}
	a := map[attribute.Key]string{}
	for _, kv := range client.Attributes {
		a[kv.Key] = kv.Value.Emit()
	}
	for k, v := range map[attribute.Key]string{
		"url.full": api.URL, "graphql.operation.type": "query", "graphql.operation.name": "Groups",
	} {
		if a[k] != v {
			t.Errorf("%s = %q, want %q", k, a[k], v)
		}
	}
	for k, v := range a {
		if strings.Contains(v, "ops") {
			t.Errorf("%s holds a variable: %q", k, v)
		}
	}
}

type handlerFunc func(ctx context.Context, ev automation.Event) error

func (f handlerFunc) Handle(ctx context.Context, ev automation.Event) error { return f(ctx, ev) }

func TestOperation(t *testing.T) {
	for doc, want := range map[string][2]string{
		"query Groups { ok }":                    {"query", "Groups"},
		"# comment\nmutation Deploy($id: ID!) {": {"mutation", "Deploy"},
		"query($id: ID!) { ok }":                 {"query", ""},
		"{ ok }":                                 {"query", ""},
	} {
		typ, name := operation(doc)
		if typ != want[0] || name != want[1] {
			t.Errorf("operation(%q) = %q %q, want %q %q", doc, typ, name, want[0], want[1])
		}
	}
}
