package milvus

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/astrona-io/codesynapse/internal/telemetry"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

// TestStoreSizesSumsOwnCollections: only this index's collections are
// counted, per family, and no collection name leaves as a kind.
func TestStoreSizesSumsOwnCollections(t *testing.T) {
	counts := map[string]int64{"cs_memory_aa_8": 5, "cs_memory_bb_16": 7, "session_cs_memory_aa_8": 2, "someone_else": 100}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		var data any
		switch r.URL.Path {
		case "/v2/vectordb/collections/list":
			data = []string{"cs_memory_aa_8", "cs_memory_bb_16", "session_cs_memory_aa_8", "someone_else"}
		case "/v2/vectordb/collections/get_stats":
			name, _ := body["collectionName"].(string)
			assert.NotEqual(t, name, "someone_else")
			data = map[string]int64{"rowCount": counts[name]}
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		require.NoError(t, json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": data}))
	}))
	defer srv.Close()
	x, err := New(t.Context(), Config{URL: srv.URL})
	require.NoError(t, err)
	got, err := x.StoreSizes(t.Context())
	require.NoError(t, err)
	assert.Equal(t, got, []telemetry.StoreSize{
		{System: telemetry.DBSystemMilvus, Kind: KindVectors, Rows: 12, Bytes: -1},
		{System: telemetry.DBSystemMilvus, Kind: KindSessionVectors, Rows: 2, Bytes: -1},
	})
	for _, s := range got {
		assert.True(t, telemetry.StoreKindAllowed(s.System, s.Kind))
	}
}
