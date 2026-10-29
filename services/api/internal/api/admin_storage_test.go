package api

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/astrona-io/codesynapse/internal/accounts"
	"github.com/astrona-io/codesynapse/internal/telemetry"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

// TestStatsStorageKeepsRegisteredKinds: the storage section reads the
// sample back with constant queries and shows only kinds a store
// registered, whatever else the metrics store holds.
func TestStatsStorageKeepsRegisteredKinds(t *testing.T) {
	telemetry.AllowStoreKinds(telemetry.DBSystemNebula, "storage_test_kind")
	var queries []string
	vm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("query")
		queries = append(queries, q)
		switch q {
		case storeRowsQuery:
			_, _ = w.Write([]byte(`{"status":"success","data":{"result":[
				{"metric":{"db_system":"nebulagraph","kind":"storage_test_kind"},"values":[[1,"10"],[2,"25"]]},
				{"metric":{"db_system":"nebulagraph","kind":"acme/secret-repo"},"values":[[1,"1"]]}]}}`))
		case storeBytesQuery:
			_, _ = w.Write([]byte(`{"status":"success","data":{"result":[]}}`))
		case storeTimeQuery:
			_, _ = w.Write([]byte(`{"status":"success","data":{"result":[{"metric":{},"values":[[2,"1793232000"]]}]}}`))
		default:
			t.Errorf("unexpected query %q", q)
		}
	}))
	defer vm.Close()
	s := &Server{cfg: Config{Metrics: &telemetry.Querier{URL: vm.URL}}, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	out, err := s.statsStorage(t.Context(), "7d", accounts.StatsQuery{Now: now, Since: now.Add(-7 * 24 * time.Hour)})
	require.NoError(t, err)
	assert.True(t, out.Available)
	require.Equal(t, len(out.Stores), 1)
	row := out.Stores[0]
	assert.Equal(t, row.Kind, "storage_test_kind")
	assert.Equal(t, *row.Rows, 25.0)
	assert.Equal(t, *row.RowsChange, 15.0)
	assert.True(t, row.Bytes == nil, "NebulaGraph reports no bytes")
	require.True(t, out.SampledAt != nil)
	assert.Equal(t, out.SampledAt.Year(), 2026)
	for _, q := range queries {
		assert.False(t, strings.Contains(q, "tenant"), "platform figures: "+q)
	}

	s.cfg.Metrics = nil
	out, err = s.statsStorage(t.Context(), "7d", accounts.StatsQuery{Now: now})
	require.NoError(t, err)
	assert.False(t, out.Available)
	assert.Equal(t, out.Reason, "no metrics store")
}

func TestStatsAlertsWithoutVmalert(t *testing.T) {
	s := &Server{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	out := s.statsAlerts(t.Context())
	assert.False(t, out.Configured)
	assert.True(t, s.alertsFiring(t.Context()) == nil)

	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusBadGateway) }))
	defer down.Close()
	s.cfg.Alerts = &telemetry.AlertsClient{URL: down.URL}
	out = s.statsAlerts(t.Context())
	assert.True(t, out.Configured)
	assert.False(t, out.Reachable)
	assert.True(t, s.alertsFiring(t.Context()) == nil)
}
