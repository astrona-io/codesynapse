package api

import (
	"net/http"
	"net/netip"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	"github.com/astrona-io/codesynapse/internal/telemetry"
)

// traceRequest starts the request's span. A traceparent is continued only
// from a trusted proxy (the dashboard): anyone else could force sampling or
// splice their requests into another trace.
func (s *Server) traceRequest(r *http.Request) (*http.Request, trace.Span) {
	ctx := r.Context()
	if s.fromTrustedProxy(r) {
		ctx = otel.GetTextMapPropagator().Extract(ctx, propagation.HeaderCarrier(r.Header))
	}
	ctx, span := telemetry.Tracer().Start(ctx, "HTTP "+r.Method, trace.WithSpanKind(trace.SpanKindServer))
	return r.WithContext(ctx), span
}

// endRequestSpan names the span by its route pattern, never the path: paths
// carry repository names and IDs.
func endRequestSpan(span trace.Span, method, pattern string, status int) {
	if pattern != "" {
		span.SetName(pattern)
	}
	span.SetAttributes(attribute.String("http.request.method", method), attribute.String("http.route", pattern), attribute.Int("http.response.status_code", status))
	if status >= http.StatusInternalServerError {
		span.SetStatus(codes.Error, http.StatusText(status))
	}
	span.End()
}

func (s *Server) fromTrustedProxy(r *http.Request) bool {
	addr, err := netip.ParseAddrPort(r.RemoteAddr)
	if err != nil {
		return false
	}
	ip := addr.Addr().Unmap()
	for _, p := range s.cfg.TrustedProxies {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}
