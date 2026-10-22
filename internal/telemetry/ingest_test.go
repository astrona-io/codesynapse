package telemetry

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func ingestFixture() IngestSnapshot {
	start := time.Now().Add(-time.Minute).Truncate(time.Millisecond)
	return IngestSnapshot{StreamID: "cccccccccccccccccccccccc", StartedAt: start, SampleAt: start.Add(time.Second), Totals: []IngestTotal{{TenantID: "aaaaaaaaaaaaaaaaaaaaaaaa", RepositoryID: "bbbbbbbbbbbbbbbbbbbbbbbb", StartedAt: start, Scanned: 100, Written: 20, Skipped: map[string]int64{"ignored_path": 70, "binary": 10}}}}
}

func TestIngestMetricShape(t *testing.T) {
	s := ingestFixture()
	s.DroppedSeries = 6
	data := ingestMetricData(s)
	instance, ok := data.Resource.Set().Value(attribute.Key("service.instance.id"))
	assert.True(t, ok)
	assert.Equal(t, instance, attribute.StringValue(s.StreamID))
	metrics := data.ScopeMetrics[0].Metrics
	require.Len(t, metrics, 4)
	for i, metric := range metrics {
		sum := metric.Data.(metricdata.Sum[int64])
		assert.Equal(t, sum.Temporality, metricdata.CumulativeTemporality)
		assert.True(t, sum.IsMonotonic)
		for j, point := range sum.DataPoints {
			if i == 3 {
				assert.Equal(t, point.Attributes.Len(), 0)
			} else {
				for key, want := range map[string]string{"tenant_id": s.Totals[0].TenantID, "repo": s.Totals[0].RepositoryID, "mcp_tool": "none", "llm_provider": "none", "llm_model": "none"} {
					value, ok := point.Attributes.Value(attribute.Key(key))
					assert.True(t, ok)
					assert.Equal(t, value.AsString(), want)
				}
			}
			if j%2 == 0 {
				assert.Equal(t, point.Value, int64(0))
				assert.True(t, point.Time.Equal(s.StartedAt.Add(-time.Millisecond)))
			} else {
				assert.True(t, point.Time.Equal(s.SampleAt))
			}
		}
	}
	assert.Len(t, metrics[2].Data.(metricdata.Sum[int64]).DataPoints, 8)
	produced, err := Default.Produce(t.Context())
	require.NoError(t, err)
	for _, scope := range produced {
		for _, metric := range scope.Metrics {
			assert.False(t, metric.Name == FilesScannedMetric || metric.Name == FilesIndexedMetric || metric.Name == FilesSkippedMetric, "durable counters must not also enter the process registry")
		}
	}
}

func TestIngestSnapshotValidation(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*IngestSnapshot)
	}{
		{"repository name", func(s *IngestSnapshot) { s.Totals[0].RepositoryID = "owner/private-repo" }},
		{"tenant injection", func(s *IngestSnapshot) { s.Totals[0].TenantID = `a"}` }},
		{"duplicate repository", func(s *IngestSnapshot) { s.Totals = append(s.Totals, s.Totals[0]) }},
		{"unknown reason", func(s *IngestSnapshot) { s.Totals[0].Skipped["secret/path"] = 0 }},
		{"negative counter", func(s *IngestSnapshot) { s.Totals[0].Written = -1 }},
		{"impossible partition", func(s *IngestSnapshot) { s.Totals[0].Written = 21 }},
		{"precision overflow", func(s *IngestSnapshot) { s.Totals[0].Scanned = maxIngestCounter + 1 }},
		{"future sample", func(s *IngestSnapshot) { s.SampleAt = time.Now().Add(time.Hour) }},
		{"missing start", func(s *IngestSnapshot) { s.Totals[0].StartedAt = time.Time{} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := ingestFixture()
			tc.edit(&s)
			e := &IngestExporter{admitted: map[string]ingestAdmission{}}
			assert.True(t, e.validate(t.Context(), s) != "")
			assert.Len(t, e.admitted, 0)
		})
	}
	s := ingestFixture()
	e := &IngestExporter{admitted: map[string]ingestAdmission{}}
	assert.Equal(t, e.validate(WithTenant(t.Context(), "dddddddddddddddddddddddd"), s), "scope")
	assert.Equal(t, e.validate(t.Context(), s), "")
	e.admit(s)
	assert.Equal(t, e.validate(t.Context(), s), "", "identical retry is safe")
	s.Totals[0].Scanned++
	assert.Equal(t, e.validate(t.Context(), s), "stale", "one timestamp cannot carry conflicting values")
	s.SampleAt = s.SampleAt.Add(time.Second)
	assert.Equal(t, e.validate(t.Context(), s), "")
	e.admit(s)
	s.Totals[0].Scanned--
	assert.Equal(t, e.validate(t.Context(), s), "stale")
	s.Totals[0].TenantID = "dddddddddddddddddddddddddd"
	assert.Equal(t, e.validate(t.Context(), s), "scope", "repository IDs cannot move to another tenant")
}

func TestIngestAdmissionLimits(t *testing.T) {
	for _, perTenant := range []bool{false, true} {
		t.Run(fmt.Sprint(perTenant), func(t *testing.T) {
			s := ingestFixture()
			row := s.Totals[0]
			s.Totals = nil
			limit := MaxIngestRepositories
			if perTenant {
				limit = MaxTenantIngestRepositories
			}
			for i := range limit {
				r := row
				r.RepositoryID = fmt.Sprintf("%024x", i+1)
				if !perTenant {
					r.TenantID = fmt.Sprintf("%024x", i/MaxTenantIngestRepositories+1)
				}
				s.Totals = append(s.Totals, r)
			}
			e := &IngestExporter{admitted: map[string]ingestAdmission{}}
			assert.Equal(t, e.validate(t.Context(), s), "")
			e.admit(s)
			r := row
			r.RepositoryID = "eeeeeeeeeeeeeeeeeeeeeeee"
			s.Totals = []IngestTotal{r}
			s.SampleAt = s.SampleAt.Add(time.Second)
			assert.Equal(t, e.validate(t.Context(), s), "capacity", "rotating snapshots cannot evade lifetime caps")
		})
	}
}

func TestIngestExporterRestartAndTransport(t *testing.T) {
	for _, format := range []string{"otlp", "prometheus"} {
		t.Run(format, func(t *testing.T) {
			var bodies []string
			var instances []string
			client := &http.Client{Transport: metricTransport(func(req *http.Request) (*http.Response, error) {
				user, pass, ok := req.BasicAuth()
				assert.True(t, ok)
				assert.Equal(t, user, "metrics")
				assert.Equal(t, pass, "test-only")
				raw, err := io.ReadAll(req.Body)
				require.NoError(t, err)
				bodies = append(bodies, string(raw))
				instances = append(instances, req.URL.RawQuery)
				return metricResponse(http.StatusNoContent, ""), nil
			})}
			cfg := IngestExportConfig{URL: "http://metrics.test", Format: format, User: "metrics", Password: "test-only", HTTP: client}
			s := ingestFixture()
			for range 2 {
				e, err := NewIngestExporter(t.Context(), cfg)
				require.NoError(t, err)
				require.NoError(t, e.Export(t.Context(), s))
				require.NoError(t, e.Shutdown(t.Context()))
				require.NoError(t, e.Shutdown(t.Context()))
				require.Error(t, e.Export(t.Context(), s))
			}
			require.Len(t, bodies, 2, "shutdown must not export without the database fence")
			assert.Equal(t, bodies[0], bodies[1], "restart sends identical timestamps and totals")
			assert.Equal(t, instances[0], instances[1])
		})
	}
}

func TestIngestExportRefusesInvalidBeforeNetwork(t *testing.T) {
	for _, format := range []string{"otlp", "prometheus"} {
		t.Run(format, func(t *testing.T) {
			calls := 0
			client := &http.Client{Transport: metricTransport(func(*http.Request) (*http.Response, error) {
				calls++
				res := metricResponse(http.StatusTemporaryRedirect, "receiver-secret")
				res.Header.Set("Location", "http://other.test")
				return res, nil
			})}
			e, err := NewIngestExporter(t.Context(), IngestExportConfig{URL: "http://metrics.test", Format: format, HTTP: client})
			require.NoError(t, err)
			t.Cleanup(func() { _ = e.Shutdown(context.Background()) })
			s := ingestFixture()
			require.Error(t, e.Export(WithTenant(t.Context(), "dddddddddddddddddddddddd"), s))
			assert.Equal(t, calls, 0)
			err = e.Export(t.Context(), s)
			require.Error(t, err)
			assert.False(t, strings.Contains(err.Error(), "receiver-secret"))
			assert.Equal(t, calls, 1, "redirects must not forward credentials")
		})
	}
}

func TestQueryRangeRepositoryScope(t *testing.T) {
	calls := 0
	q := &Querier{URL: "http://metrics.test", HTTP: &http.Client{Transport: metricTransport(func(req *http.Request) (*http.Response, error) {
		calls++
		assert.Equal(t, req.URL.Query()["extra_filters[]"], []string{`{tenant_id=~"aaaaaaaaaaaaaaaaaaaaaaaa|bbbbbbbbbbbbbbbbbbbbbbbb",repo="cccccccccccccccccccccccc"}`})
		return metricResponse(http.StatusOK, `{"status":"success","data":{"result":[]}}`), nil
	})}}
	start, end := time.Now().Add(-time.Hour), time.Now()
	_, err := q.QueryRangeForRepo(t.Context(), "sum(metric)", start, end, time.Minute, []string{"aaaaaaaaaaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbbbbbbbbbb"}, "cccccccccccccccccccccccc")
	require.NoError(t, err)
	for _, tenants := range [][]string{nil, {}, {`unsafe"`}} {
		_, err = q.QueryRangeForRepo(t.Context(), "sum(metric)", start, end, time.Minute, tenants, "cccccccccccccccccccccccc")
		if tenants != nil && len(tenants) == 0 {
			require.NoError(t, err)
		} else {
			require.Error(t, err)
		}
	}
	_, err = q.QueryRangeForRepo(t.Context(), "metric", start, end, time.Minute, []string{"aaaaaaaaaaaaaaaaaaaaaaaa"}, "owner/repo")
	require.Error(t, err)
	assert.Equal(t, calls, 1)
}
