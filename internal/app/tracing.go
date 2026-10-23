package app

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"sync"
	"time"

	"go.opentelemetry.io/otel/trace"

	"github.com/astrona-io/codesynapse/internal/telemetry"
)

// Trace export (docs/SAAS.md, 5e). Unset URL: no export; trace IDs still
// cross services when a trusted caller sends a traceparent.
const (
	EnvTracesURL      = "CODESYNAPSE_TRACES_URL"
	EnvTracesUser     = "CODESYNAPSE_TRACES_USER"
	EnvTracesPassword = "CODESYNAPSE_TRACES_PASSWORD" //nolint:gosec // env var name, not a credential
	EnvTracesSample   = "CODESYNAPSE_TRACES_SAMPLE"
)

// StartTracing exports spans of service when CODESYNAPSE_TRACES_URL is set.
// The returned stop flushes them.
func StartTracing(ctx context.Context, logger *slog.Logger, service string, versions ...string) (stop func(), err error) {
	url := os.Getenv(EnvTracesURL)
	if url == "" {
		return func() {}, nil
	}
	sample := 1.0
	if v := os.Getenv(EnvTracesSample); v != "" {
		if sample, err = strconv.ParseFloat(v, 64); err != nil || sample < 0 || sample > 1 {
			return nil, fmt.Errorf("%s must be a number from 0 to 1", EnvTracesSample)
		}
	}
	version := "unknown"
	if len(versions) > 0 && versions[0] != "" {
		version = versions[0]
	}
	tp, err := telemetry.NewTracing(ctx, telemetry.TracingConfig{URL: url, User: os.Getenv(EnvTracesUser), Password: os.Getenv(EnvTracesPassword),
		Service: "codesynapse-" + service, Instance: telemetry.InstanceID(), Version: version, Environment: Getenv(EnvEnvironment, "unknown"), Sample: sample})
	if err != nil {
		return nil, fmt.Errorf("%s: %w", EnvTracesURL, err)
	}
	logger.Info("trace export enabled", "service", service, "sample", sample)
	return sync.OnceFunc(func() {
		final, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if err := tp.Shutdown(final); err != nil {
			logger.Warn("flushing traces failed", "error", err)
		}
	}), nil
}

// traceHandler adds trace_id and span_id to records logged with a context
// that carries a span, so a log line leads to its trace.
type traceHandler struct{ slog.Handler }

func (h traceHandler) Handle(ctx context.Context, r slog.Record) error {
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		r.AddAttrs(slog.String("trace_id", sc.TraceID().String()), slog.String("span_id", sc.SpanID().String()))
	}
	return h.Handler.Handle(ctx, r)
}

func (h traceHandler) WithAttrs(as []slog.Attr) slog.Handler {
	return traceHandler{h.Handler.WithAttrs(as)}
}

func (h traceHandler) WithGroup(name string) slog.Handler {
	return traceHandler{h.Handler.WithGroup(name)}
}
