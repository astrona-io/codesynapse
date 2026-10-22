package telemetry

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

type metricTransport func(*http.Request) (*http.Response, error)

func (f metricTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func metricResponse(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}
}

func TestOTLPTransportBoundaries(t *testing.T) {
	for _, status := range []int{http.StatusNoContent, http.StatusTemporaryRedirect, http.StatusUnauthorized} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			r := NewRegistry()
			r.ScopedCounter("requests_total", "Requests.").IncContext(WithTenant(context.Background(), "aaaaaaaaaaaaaaaaaaaaaaaa"))
			var calls atomic.Int32
			client := &http.Client{Transport: metricTransport(func(req *http.Request) (*http.Response, error) {
				calls.Add(1)
				assert.Equal(t, req.URL.String(), "http://metrics.test/prefix/opentelemetry/v1/metrics")
				user, password, ok := req.BasicAuth()
				assert.True(t, ok)
				assert.Equal(t, user, "metrics")
				assert.Equal(t, password, "test-only")
				assert.Equal(t, req.Header.Get("Content-Type"), "application/x-protobuf")
				res := metricResponse(status, "receiver-secret")
				res.Header.Set("Location", "http://other.test/leak")
				return res, nil
			})}
			mp, err := NewOTLP(context.Background(), OTLPConfig{URL: "http://metrics.test/prefix", User: "metrics", Password: "test-only", Registry: r, Service: "api", HTTP: client, Interval: time.Hour})
			require.NoError(t, err)
			err = mp.ForceFlush(context.Background())
			if status == http.StatusNoContent {
				require.NoError(t, err)
				assert.Equal(t, calls.Load(), int32(2), "zero baseline and current value")
			} else {
				require.Error(t, err)
				assert.False(t, strings.Contains(err.Error(), "receiver-secret"))
				assert.Equal(t, calls.Load(), int32(1), "no redirects or permanent-error retries")
			}
			_ = mp.Shutdown(context.Background())
		})
	}
}

func TestOTLPOutageDoesNotBlockRecording(t *testing.T) {
	r := NewRegistry()
	c := r.ScopedCounter("requests_total", "Requests.")
	ctx := WithTenant(context.Background(), "aaaaaaaaaaaaaaaaaaaaaaaa")
	c.IncContext(ctx)
	entered := make(chan struct{}, 1)
	client := &http.Client{Transport: metricTransport(func(req *http.Request) (*http.Response, error) {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-req.Context().Done()
		return nil, req.Context().Err()
	})}
	mp, err := NewOTLP(context.Background(), OTLPConfig{URL: "http://metrics.test", Registry: r, Service: "api", HTTP: client, Interval: time.Hour, Timeout: time.Second})
	require.NoError(t, err)
	done := make(chan error, 1)
	flushCtx, cancel := context.WithCancel(context.Background())
	go func() { done <- mp.ForceFlush(flushCtx) }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("export did not start")
	}
	for range 1000 {
		c.IncContext(ctx)
	}
	snapshot, err := r.Produce(ctx)
	require.NoError(t, err)
	assert.Equal(t, snapshot[0].Metrics[0].Data.(metricdata.Sum[float64]).DataPoints[0].Value, float64(1001))
	cancel()
	require.Error(t, <-done)
	stopCtx, stop := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer stop()
	start := time.Now()
	require.Error(t, mp.Shutdown(stopCtx))
	assert.True(t, time.Since(start) < time.Second, "shutdown obeys its deadline")
}

type testMetricExporter struct {
	sdkmetric.Exporter
	fail   bool
	values []float64
}

func (e *testMetricExporter) Export(_ context.Context, data *metricdata.ResourceMetrics) error {
	for _, m := range data.ScopeMetrics[0].Metrics {
		v := m.Data.(metricdata.Sum[float64])
		e.values = append(e.values, v.DataPoints[0].Value)
	}
	if e.fail {
		return errors.New("receiver secret")
	}
	return nil
}

func TestOTLPBaselineRetry(t *testing.T) {
	r := NewRegistry()
	r.Counter("tokens_total", "Tokens.").Add(37809)
	scopes, err := r.Produce(context.Background())
	require.NoError(t, err)
	inner := &testMetricExporter{fail: true}
	e := &firstValueExporter{Exporter: inner, seen: map[seriesIdentity]bool{}}
	data := &metricdata.ResourceMetrics{ScopeMetrics: scopes}
	require.Error(t, e.Export(context.Background(), data))
	assert.Equal(t, len(e.seen), 0)
	inner.fail = false
	require.NoError(t, e.Export(context.Background(), data))
	require.NoError(t, e.Export(context.Background(), data))
	assert.Equal(t, fmt.Sprint(inner.values), "[0 0 37809 37809]")
}

func TestMetricsURLRejectsCredentialsAndSuffixes(t *testing.T) {
	for _, raw := range []string{"http://user:secret@metrics.test", "http://metrics.test?token=secret", "http://metrics.test#secret", "http://metrics.test?"} {
		require.Error(t, CheckURL(raw))
	}
}

func BenchmarkExport(b *testing.B) {
	for _, format := range []string{"prometheus", "otlp"} {
		b.Run(format, func(b *testing.B) {
			r := NewRegistry()
			c := r.ScopedCounter("tokens_total", "Tokens.")
			h := r.ScopedHistogram("query_seconds", "Latency.", latency)
			for i := range 100 {
				ctx := WithTenant(context.Background(), fmt.Sprintf("%024x", i))
				c.AddContext(ctx, 1000)
				h.ObserveContext(ctx, 0.02)
			}
			client := &http.Client{Transport: metricTransport(func(req *http.Request) (*http.Response, error) {
				_, _ = io.Copy(io.Discard, req.Body)
				return metricResponse(http.StatusNoContent, ""), nil
			})}
			p := &Pusher{URL: "http://metrics.test", Registry: r, HTTP: client}
			export := func() error { return p.Push(context.Background()) }
			if format == "otlp" {
				mp, err := NewOTLP(context.Background(), OTLPConfig{URL: "http://metrics.test", Service: "api", Registry: r, HTTP: client, Interval: time.Hour})
				if err != nil {
					b.Fatal(err)
				}
				b.Cleanup(func() { _ = mp.Shutdown(context.Background()) })
				export = func() error { return mp.ForceFlush(context.Background()) }
			}
			if err := export(); err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				if err := export(); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
