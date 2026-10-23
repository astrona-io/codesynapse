package telemetry

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace/noop"

	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

func TestTracingExports(t *testing.T) {
	var got atomic.Int32
	var path, auth atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		path.Store(r.URL.Path)
		auth.Store(r.Header.Get("Authorization"))
		if b, _ := io.ReadAll(r.Body); len(b) > 0 {
			got.Add(1)
		}
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() {
		otel.SetTracerProvider(noop.NewTracerProvider())
		otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator())
	})
	tp, err := NewTracing(context.Background(), TracingConfig{URL: srv.URL + "/insert/opentelemetry/v1/traces", User: "u", Password: "p", Service: "codesynapse-test", Sample: 1})
	require.NoError(t, err)
	ctx, span := Tracer().Start(context.Background(), "work")
	tp2 := TraceParent(ctx)
	span.End()
	require.NoError(t, tp.Shutdown(context.Background()))
	assert.True(t, got.Load() > 0, "spans were sent")
	assert.Equal(t, path.Load(), "/insert/opentelemetry/v1/traces")
	assert.Equal(t, auth.Load(), "Basic dTpw")
	assert.Equal(t, TraceID(WithTraceParent(context.Background(), tp2)), TraceID(ctx), "a stored traceparent continues the trace")

	for _, bad := range []string{"ftp://x/y", "http://u:p@x/v1/traces", "http://x/v1/traces?q=1", "x"} {
		_, err := NewTracing(context.Background(), TracingConfig{URL: bad, Service: "s"})
		assert.Error(t, err, bad)
	}
}
