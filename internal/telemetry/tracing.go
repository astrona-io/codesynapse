package telemetry

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

// Traces (docs/SAAS.md, 5e) follow one request from the API through its job
// and the worker to the graph store and model calls. Spans carry route
// patterns, kinds, opaque IDs and timings, never content: no prompts, code,
// arguments, paths, repository names or people. The trace ID doubles as the
// request ID, so audit entries, logs and traces join.

// TracingConfig configures span export.
type TracingConfig struct {
	// URL is the OTLP/HTTP traces endpoint, for VictoriaTraces
	// http://victoriatraces:10428/insert/opentelemetry/v1/traces.
	URL, User, Password                     string
	Service, Instance, Version, Environment string
	// Sample is the share of new traces kept (0..1); a sampled parent
	// keeps its children.
	Sample  float64
	Timeout time.Duration
	HTTP    *http.Client
}

// CheckTracesURL accepts http(s) endpoints with a path but no credentials,
// query or fragment.
func CheckTracesURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.ForceQuery {
		return errors.New("traces url must be an http(s) endpoint without credentials, query or fragment")
	}
	return nil
}

// NewTracing installs a batching span exporter as the global tracer
// provider and W3C trace context as the propagator. Call Shutdown with a
// deadline to flush.
func NewTracing(ctx context.Context, cfg TracingConfig) (*sdktrace.TracerProvider, error) {
	if err := CheckTracesURL(cfg.URL); err != nil {
		return nil, err
	}
	if cfg.Service == "" {
		return nil, errors.New("tracing service name is required")
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 10 * time.Second
	}
	cfg.Sample = min(max(cfg.Sample, 0), 1)
	client := http.Client{Timeout: cfg.Timeout}
	if cfg.HTTP != nil {
		client = *cfg.HTTP
		client.Timeout = cfg.Timeout
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	headers := map[string]string{}
	if cfg.User != "" {
		headers["Authorization"] = "Basic " + base64.StdEncoding.EncodeToString([]byte(cfg.User+":"+cfg.Password))
	}
	exporter, err := otlptracehttp.New(ctx, otlptracehttp.WithEndpointURL(cfg.URL), otlptracehttp.WithHTTPClient(&client),
		otlptracehttp.WithHeaders(headers), otlptracehttp.WithTimeout(cfg.Timeout),
		otlptracehttp.WithRetry(otlptracehttp.RetryConfig{Enabled: true, InitialInterval: 200 * time.Millisecond, MaxInterval: time.Second, MaxElapsedTime: cfg.Timeout}))
	if err != nil {
		return nil, fmt.Errorf("initializing OTLP traces: %w", err)
	}
	res := resource.NewSchemaless(
		attribute.String("service.namespace", "codesynapse"), attribute.String("service.name", cfg.Service),
		attribute.String("service.instance.id", cfg.Instance), attribute.String("service.version", cfg.Version),
		attribute.String("deployment.environment.name", cfg.Environment),
	)
	// Bounded: a slow or absent trace store drops spans, never blocks work.
	tp := sdktrace.NewTracerProvider(sdktrace.WithResource(res),
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(cfg.Sample))),
		sdktrace.WithBatcher(exporter, sdktrace.WithMaxQueueSize(4096), sdktrace.WithMaxExportBatchSize(512), sdktrace.WithExportTimeout(cfg.Timeout)))
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	return tp, nil
}

// Tracer is CodeSynapse's tracer; without NewTracing its spans record nothing.
func Tracer() trace.Tracer { return otel.Tracer("github.com/astrona-io/codesynapse") }

// TraceParent is ctx's W3C traceparent, or "" without a valid span.
func TraceParent(ctx context.Context) string {
	carrier := propagation.MapCarrier{}
	propagation.TraceContext{}.Inject(ctx, carrier)
	return carrier.Get("traceparent")
}

// WithTraceParent continues the trace in traceparent (from a job record).
func WithTraceParent(ctx context.Context, traceparent string) context.Context {
	if traceparent == "" {
		return ctx
	}
	return propagation.TraceContext{}.Extract(ctx, propagation.MapCarrier{"traceparent": traceparent})
}

// TraceID is ctx's trace ID, or "" without a valid span.
func TraceID(ctx context.Context) string {
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		return sc.TraceID().String()
	}
	return ""
}
