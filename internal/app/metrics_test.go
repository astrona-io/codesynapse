package app

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	collector "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	"google.golang.org/protobuf/proto"

	"github.com/parisnakitakejser/codesynapse/internal/telemetry"
	"github.com/parisnakitakejser/codesynapse/internal/testutil/assert"
	"github.com/parisnakitakejser/codesynapse/internal/testutil/require"
)

func metricsLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestMetricsDisabledWithoutURL(t *testing.T) {
	t.Setenv(EnvMetricsURL, "")
	t.Setenv(EnvMetricsExporter, "not-an-exporter")
	stop, err := StartMetrics(t.Context(), metricsLogger(), "api", "test")
	require.NoError(t, err)
	stop()
	stop()
}

func TestMetricsRejectInvalidConfiguration(t *testing.T) {
	for _, tc := range []struct{ name, url, exporter string }{
		{"selector", "http://localhost:8428", "invalid"},
		{"scheme", "file:///metrics", "otlp"},
		{"credentials", "https://user:secret-value@example.com", "otlp"},
		{"query", "https://example.com?token=secret-value", "prometheus"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(EnvMetricsURL, tc.url)
			t.Setenv(EnvMetricsExporter, tc.exporter)
			stop, err := StartMetrics(t.Context(), metricsLogger(), "api", "test")
			require.Error(t, err)
			assert.True(t, stop == nil)
			assert.False(t, strings.Contains(err.Error(), "secret-value"))
		})
	}
}

func TestMetricsExportsOnlySelectedProtocolAndFlushesAfterCancel(t *testing.T) {
	for _, exporter := range []string{"", "prometheus"} {
		t.Run(exporter, func(t *testing.T) {
			type observation struct {
				path, instance string
				body           []byte
			}
			seen := make(chan observation, 20)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				user, password, ok := r.BasicAuth()
				assert.True(t, ok)
				assert.Equal(t, user, "metrics-user")
				assert.Equal(t, password, "test-password")
				body, err := io.ReadAll(io.LimitReader(r.Body, 2<<20))
				require.NoError(t, err)
				instance := ""
				for _, label := range r.URL.Query()["extra_label"] {
					if strings.HasPrefix(label, "instance=") {
						instance = strings.TrimPrefix(label, "instance=")
					}
				}
				seen <- observation{r.URL.Path, instance, body}
				w.WriteHeader(http.StatusOK)
			}))
			t.Cleanup(srv.Close)
			t.Setenv(EnvMetricsURL, srv.URL)
			t.Setenv(EnvMetricsExporter, exporter)
			t.Setenv(EnvMetricsUser, "metrics-user")
			t.Setenv(EnvMetricsPassword, "test-password")
			t.Setenv(EnvEnvironment, "test-deployment")
			instances := map[string]bool{}
			for range 2 {
				ctx, cancel := context.WithCancel(t.Context())
				stop, err := StartMetrics(ctx, metricsLogger(), "api", "test-version")
				require.NoError(t, err)
				telemetry.MCPSessions.IncContext(ctx, telemetry.NoTenant)
				cancel()
				stop()
				stop()
				require.True(t, len(seen) > 0, "shutdown must export after parent cancellation")
				for len(seen) > 0 {
					got := <-seen
					if exporter == "prometheus" {
						assert.Equal(t, got.path, "/api/v1/import/prometheus")
						assert.True(t, strings.Contains(string(got.body), "mcp_sessions_total"))
						require.True(t, got.instance != "")
						instances[got.instance] = true
						continue
					}
					assert.Equal(t, got.path, "/opentelemetry/v1/metrics")
					var payload collector.ExportMetricsServiceRequest
					require.NoError(t, proto.Unmarshal(got.body, &payload))
					require.True(t, len(payload.ResourceMetrics) > 0)
					attrs := map[string]string{}
					for _, attr := range payload.ResourceMetrics[0].Resource.Attributes {
						attrs[attr.Key] = attr.Value.GetStringValue()
					}
					assert.Equal(t, attrs["service.name"], "codesynapse-api")
					assert.Equal(t, attrs["service.version"], "test-version")
					assert.Equal(t, attrs["deployment.environment.name"], "test-deployment")
					require.True(t, attrs["service.instance.id"] != "")
					instances[attrs["service.instance.id"]] = true
				}
			}
			assert.Equal(t, len(instances), 2, "new exporters must not share cumulative stream identity")
		})
	}
}

func TestMetricsShutdownDeadline(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) })
	t.Setenv(EnvMetricsURL, srv.URL)
	t.Setenv(EnvMetricsExporter, "otlp")
	t.Setenv(EnvMetricsUser, "")
	t.Setenv(EnvMetricsPassword, "")
	stop, err := StartMetrics(t.Context(), metricsLogger(), "api", "test")
	require.NoError(t, err)
	telemetry.MCPSessions.IncContext(t.Context(), telemetry.NoTenant)
	start := time.Now()
	stop()
	assert.True(t, time.Since(start) < 7*time.Second, "shutdown must not wait for the ten-second export timeout")
}
