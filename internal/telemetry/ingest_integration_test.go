package telemetry

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

func TestVictoriaMetricsIngest(t *testing.T) {
	endpoint := os.Getenv("CODESYNAPSE_TEST_METRICS_URL")
	if endpoint == "" {
		t.Skip("CODESYNAPSE_TEST_METRICS_URL not set")
	}
	user, password := os.Getenv("CODESYNAPSE_TEST_METRICS_USER"), os.Getenv("CODESYNAPSE_TEST_METRICS_PASSWORD")
	q := &Querier{URL: endpoint, User: user, Password: password}
	for _, format := range []string{"otlp", "prometheus"} {
		t.Run(format, func(t *testing.T) {
			s := ingestFixture()
			s.StreamID = fmt.Sprintf("%024x", time.Now().UnixNano())
			s.Totals[0].RepositoryID = s.StreamID
			s.Totals[0].Scanned = 37809
			s.DroppedSeries = 6
			cfg := IngestExportConfig{URL: endpoint, User: user, Password: password, Format: format}
			for range 2 {
				e, err := NewIngestExporter(t.Context(), cfg)
				require.NoError(t, err)
				require.NoError(t, e.Export(t.Context(), s))
				require.NoError(t, e.Export(t.Context(), s))
				require.NoError(t, e.Shutdown(t.Context()))
			}
			selector := fmt.Sprintf(`%s{repo="%s"}`, FilesScannedMetric, s.StreamID)
			got := awaitOTLPValue(t.Context(), t, q, selector, []string{s.Totals[0].TenantID}, 37809)
			require.Len(t, got, 1, "restart and retries retain one cumulative series")
			assert.Equal(t, got[0].Labels["service.instance.id"], s.StreamID)
			assert.Equal(t, got[0].Labels["service.version"], ingestSchemaVersion)
			assert.Equal(t, got[0].Labels["mcp_tool"], "none")
			assert.Equal(t, got[0].Labels["llm_provider"], "none")
			assert.Equal(t, got[0].Labels["llm_model"], "none")
			awaitOTLPValue(t.Context(), t, q, "sum(increase("+selector+"[10m]))", []string{s.Totals[0].TenantID}, 37809)
			// A later owner publishes the new absolute total. Repeated historical baselines must not reset it.
			s.SampleAt = s.SampleAt.Add(time.Second)
			s.Totals[0].Scanned += 7
			e, err := NewIngestExporter(t.Context(), cfg)
			require.NoError(t, err)
			require.NoError(t, e.Export(t.Context(), s))
			require.NoError(t, e.Shutdown(context.Background()))
			awaitOTLPValue(t.Context(), t, q, "sum(increase("+selector+"[10m]))", []string{s.Totals[0].TenantID}, 37816)
			end := time.Now().Add(time.Minute)
			none, err := q.QueryRangeForRepo(t.Context(), selector, end.Add(-5*time.Minute), end, time.Second, []string{"dddddddddddddddddddddddd"}, s.StreamID)
			require.NoError(t, err)
			assert.Len(t, none, 0)
			none, err = q.QueryRangeForRepo(t.Context(), selector, end.Add(-5*time.Minute), end, time.Second, []string{s.Totals[0].TenantID}, "eeeeeeeeeeeeeeeeeeeeeeee")
			require.NoError(t, err)
			assert.Len(t, none, 0)
		})
	}
}
