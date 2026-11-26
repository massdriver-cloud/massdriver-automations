package automation

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.40.0"
	"go.opentelemetry.io/otel/trace"
)

// DefaultServiceName is the service.name of traces when OTEL_SERVICE_NAME is not set.
const DefaultServiceName = "massdriver-automations"

const tracerName = "github.com/massdriver-cloud/massdriver-automations/pkg/automation"

// TracingConfigured reports whether the environment names an OTLP endpoint for traces.
func TracingConfigured() bool {
	if strings.EqualFold(os.Getenv("OTEL_SDK_DISABLED"), "true") || os.Getenv("OTEL_TRACES_EXPORTER") == "none" {
		return false
	}
	return os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") != "" || os.Getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT") != ""
}

// StartTracing exports traces over OTLP when TracingConfigured, and sets the global tracer provider and W3C trace
// context propagation. Otherwise it changes nothing, so every span is a no-op. The exporter reads the standard
// OTEL_* environment variables. Call shutdown before exit to flush spans that are not yet exported.
func StartTracing(ctx context.Context, automationName, version string) (shutdown func(context.Context) error, err error) {
	if !TracingConfigured() {
		return func(context.Context) error { return nil }, nil
	}
	exp, err := newExporter(ctx)
	if err != nil {
		return nil, err
	}
	res, err := resource.New(ctx,
		resource.WithAttributes(
			semconv.ServiceName(DefaultServiceName),
			semconv.ServiceVersion(version),
			attribute.String("automation", automationName),
		),
		resource.WithFromEnv(),
		resource.WithTelemetrySDK(),
	)
	if err != nil && !errors.Is(err, resource.ErrPartialResource) {
		return nil, fmt.Errorf("tracing resource: %w", err)
	}
	tp := sdktrace.NewTracerProvider(sdktrace.WithBatcher(exp), sdktrace.WithResource(res))
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}))
	// The default handler prints unstructured lines; export errors belong in the structured log.
	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) {
		Logger(ctx).Warn("tracing error", "err", RedactURL(err))
	}))
	return tp.Shutdown, nil
}

func newExporter(ctx context.Context) (sdktrace.SpanExporter, error) {
	protocol := Env("OTEL_EXPORTER_OTLP_TRACES_PROTOCOL", Env("OTEL_EXPORTER_OTLP_PROTOCOL", "http/protobuf"))
	switch protocol {
	case "http/protobuf":
		return otlptracehttp.New(ctx)
	case "grpc":
		return otlptracegrpc.New(ctx)
	default:
		return nil, fmt.Errorf("OTEL_EXPORTER_OTLP_PROTOCOL %q: want http/protobuf or grpc", protocol)
	}
}

func tracer() trace.Tracer { return otel.Tracer(tracerName) }

// withTracing starts a server span for each request except health checks. A webhook's span continues the trace in
// the CloudEvent's traceparent attribute, then the traceparent header, and otherwise starts a trace.
func withTracing(next http.Handler, mux *http.ServeMux) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			next.ServeHTTP(w, r)
			return
		}
		_, pattern := mux.Handler(r)
		route := pattern
		if _, after, ok := strings.Cut(pattern, " "); ok {
			route = after
		}
		name := r.Method
		attrs := []attribute.KeyValue{semconv.HTTPRequestMethodKey.String(r.Method)}
		if route != "" {
			name += " " + route
			attrs = append(attrs, semconv.HTTPRoute(route))
		}

		prop := otel.GetTextMapPropagator()
		ctx := prop.Extract(r.Context(), propagation.HeaderCarrier(r.Header))
		if pattern == http.MethodPost+" "+HookPath {
			if carrier := eventTraceContext(r); carrier != nil {
				ctx = prop.Extract(ctx, carrier)
			}
		}

		ctx, span := tracer().Start(ctx, name, trace.WithSpanKind(trace.SpanKindServer), trace.WithAttributes(attrs...))
		defer span.End()
		rec := &responseRecorder{ResponseWriter: w}
		next.ServeHTTP(rec, r.WithContext(ctx))

		status := rec.statusCode()
		span.SetAttributes(semconv.HTTPResponseStatusCode(status))
		if status >= 500 {
			span.SetStatus(codes.Error, http.StatusText(status))
		}
	})
}

// eventTraceContext returns the traceparent and tracestate attributes of the CloudEvent in r's body (the CloudEvents
// distributed tracing extension), or nil. It leaves r.Body readable from the start.
func eventTraceContext(r *http.Request) propagation.MapCarrier {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes+1))
	r.Body = struct {
		io.Reader
		io.Closer
	}{io.MultiReader(bytes.NewReader(body), r.Body), r.Body}
	if err != nil || len(body) > maxBodyBytes || !bytes.Contains(body, []byte(`"traceparent"`)) {
		return nil
	}
	var ext struct {
		Traceparent string `json:"traceparent"`
		Tracestate  string `json:"tracestate"`
	}
	if json.Unmarshal(body, &ext) != nil || ext.Traceparent == "" {
		return nil
	}
	return propagation.MapCarrier{"traceparent": ext.Traceparent, "tracestate": ext.Tracestate}
}

// recordEvent adds the event's CloudEvents attributes to the request span.
func recordEvent(ctx context.Context, ev Event) {
	trace.SpanFromContext(ctx).SetAttributes(
		semconv.CloudEventsEventID(ev.ID),
		semconv.CloudEventsEventType(ev.Type),
		semconv.CloudEventsEventSubject(ev.Subject),
	)
}

// recordError marks the span in ctx as failed with err.
func recordError(ctx context.Context, err error) {
	span := trace.SpanFromContext(ctx)
	span.RecordError(err)
	span.SetStatus(codes.Error, err.Error())
}

// HTTPClient returns a client whose requests are traced: each request gets a client span and carries the
// traceparent header. A timeout of 0 means none.
func HTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{Timeout: timeout, Transport: Transport(http.DefaultTransport)}
}

// Transport wraps base so each request gets a client span and carries the traceparent header. The span records the
// URL as scheme and host only: webhook URLs hold their credential in the path, and query strings can hold tokens.
// The span is named "<method> <host>" unless the request's context comes from WithClientSpan.
func Transport(base http.RoundTripper) http.RoundTripper {
	return otelhttp.NewTransport(hostOnly{base}, otelhttp.WithSpanNameFormatter(clientSpanName))
}

type clientSpanKey struct{}

type clientSpan struct {
	name  string
	attrs []attribute.KeyValue
}

// WithClientSpan names the client span, and adds attrs to it, for requests made with the returned context through
// a Transport. Neither may hold a credential.
func WithClientSpan(ctx context.Context, name string, attrs ...attribute.KeyValue) context.Context {
	return context.WithValue(ctx, clientSpanKey{}, clientSpan{name: name, attrs: attrs})
}

func clientSpanName(_ string, r *http.Request) string {
	if cs, ok := r.Context().Value(clientSpanKey{}).(clientSpan); ok {
		return cs.name
	}
	return r.Method + " " + r.URL.Hostname()
}

type hostOnly struct{ base http.RoundTripper }

// RoundTrip runs inside the otelhttp span, after otelhttp sets url.full, so these attributes replace its values.
func (h hostOnly) RoundTrip(r *http.Request) (*http.Response, error) {
	span := trace.SpanFromContext(r.Context())
	if span.IsRecording() {
		span.SetAttributes(semconv.URLFull(r.URL.Scheme + "://" + r.URL.Host))
		if cs, ok := r.Context().Value(clientSpanKey{}).(clientSpan); ok {
			span.SetAttributes(cs.attrs...)
		}
	}
	return h.base.RoundTrip(r)
}
