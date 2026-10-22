package milvus

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/parisnakitakejser/codesynapse/internal/execution"
	"github.com/parisnakitakejser/codesynapse/internal/sessionmemory"
	"github.com/parisnakitakejser/codesynapse/internal/testutil/assert"
	"github.com/parisnakitakejser/codesynapse/internal/testutil/require"
)

func TestSessionSearchDefendsAgainstForeignAndExpiredHits(t *testing.T) {
	scope := sessionmemory.Scope{TenantID: "org-a", SessionID: "chat-a", Namespace: "org-a|repo' || true"}
	ctx, err := execution.With(context.Background(), execution.Identity{TenantID: scope.TenantID, SessionID: scope.SessionID, ParentSessionID: scope.SessionID, SubjectID: "user"})
	require.NoError(t, err)
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
			assert.Equal(t, body["filter"], any(sessionFilter+" && expires_at > {now}"))
			params, ok := body["exprParams"].(map[string]any)
			require.True(t, ok)
			assert.Equal(t, params["tenant"], any(scope.TenantID))
			assert.Equal(t, params["session"], any(scope.SessionID))
			assert.Equal(t, params["ns"], any(scope.Namespace))
			data = []map[string]any{
				{"entity_id": "good", "tenant_id": scope.TenantID, "session_id": scope.SessionID, "ns": scope.Namespace, "distance": 1, "expires_at": time.Now().Add(time.Hour).UnixMilli()},
				{"entity_id": "other-session", "tenant_id": scope.TenantID, "session_id": "chat-b", "ns": scope.Namespace, "distance": 1, "expires_at": time.Now().Add(time.Hour).UnixMilli()},
				{"entity_id": "expired", "tenant_id": scope.TenantID, "session_id": scope.SessionID, "ns": scope.Namespace, "distance": 1, "expires_at": time.Now().Add(-time.Hour).UnixMilli()},
				{"entity_id": "other-tenant", "tenant_id": "org-b", "session_id": scope.SessionID, "ns": scope.Namespace, "distance": 1, "expires_at": time.Now().Add(time.Hour).UnixMilli()},
			}
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		require.NoError(t, json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": data}))
	}))
	defer srv.Close()
	x, err := New(ctx, Config{URL: srv.URL})
	require.NoError(t, err)
	hits, err := x.SearchSession(ctx, scope, "model", []float32{1, 0}, 10)
	require.NoError(t, err)
	require.Len(t, hits, 1)
	assert.Equal(t, hits[0].ID, "good")
}
