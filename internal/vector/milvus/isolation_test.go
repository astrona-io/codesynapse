package milvus

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/parisnakitakejser/codesynapse/internal/testutil/assert"
	"github.com/parisnakitakejser/codesynapse/internal/testutil/require"
)

func TestSearchBindsScopeAndRejectsCorruptResults(t *testing.T) {
	const tenant = `tenant' || true || '`
	const namespace = tenant + "|repo"
	searched := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		var data any
		switch r.URL.Path {
		case "/v2/vectordb/collections/list":
			data = []string{}
		case "/v2/vectordb/collections/has":
			data = map[string]bool{"has": true}
		case "/v2/vectordb/collections/load":
			data = map[string]any{}
		case "/v2/vectordb/entities/search":
			searched = true
			assert.Equal(t, body["filter"], searchFilter)
			assert.Equal(t, body["exprParams"], any(map[string]any{"tenant": tenant, "ns": namespace}))
			assert.Equal(t, body["limit"], any(float64(10)))
			assert.Equal(t, body["consistencyLevel"], any("Strong"))
			data = []map[string]any{
				{"id": "good", "tenant_id": tenant, "ns": namespace, "distance": .8},
				{"id": "good", "tenant_id": tenant, "ns": namespace, "distance": .5},
				{"id": "foreign", "tenant_id": "other", "ns": namespace, "distance": 1},
				{"id": "other-repo", "tenant_id": tenant, "ns": tenant + "|other", "distance": 1},
				{"id": "invalid", "tenant_id": tenant, "ns": namespace, "distance": 9},
			}
		default:
			t.Errorf("unexpected operation %s", r.URL.Path)
		}
		require.NoError(t, json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": data}))
	}))
	defer srv.Close()
	x, err := New(t.Context(), Config{URL: srv.URL})
	require.NoError(t, err)
	defer x.Close(t.Context()) //nolint:errcheck // closes idle connections
	hits, err := x.Search(t.Context(), "model", tenant, namespace, []float32{1, 0}, 1000000)
	require.NoError(t, err)
	assert.True(t, searched)
	require.Len(t, hits, 1)
	assert.Equal(t, hits[0].ID, "good")
}
