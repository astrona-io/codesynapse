package vectortest

import (
	"context"
	"testing"
	"time"

	"github.com/astrona-io/codesynapse/internal/execution"
	"github.com/astrona-io/codesynapse/internal/sessionmemory"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
	"github.com/astrona-io/codesynapse/internal/vector"
)

// RunSession checks session partitioning independently of graph hydration.
func RunSession(t *testing.T, index vector.Index) {
	s, ok := index.(vector.SessionIndex)
	require.True(t, ok)
	a := sessionmemory.Scope{TenantID: "org-vector-a", SessionID: "chat-a", Namespace: "org-vector-a|repo"}
	b := a
	b.SessionID = "chat-b"
	ctxOf := func(scope sessionmemory.Scope) context.Context {
		ctx, err := execution.With(context.Background(), execution.Identity{TenantID: scope.TenantID, SessionID: scope.SessionID, ParentSessionID: scope.SessionID, SubjectID: "user"})
		require.NoError(t, err)
		return ctx
	}
	for _, scope := range []sessionmemory.Scope{a, b} {
		items := []vector.Item{{ID: scope.Key(), Tenant: scope.TenantID, SessionID: scope.SessionID, Namespace: scope.Namespace, ExpiresAt: time.Now().Add(time.Hour).UnixMilli(), Vector: []float32{1, 0, 0}}}
		require.NoError(t, s.UpsertSession(ctxOf(scope), scope, "session-test", items))
	}
	hits, err := s.SearchSession(ctxOf(a), a, "session-test", []float32{1, 0, 0}, 10)
	require.NoError(t, err)
	require.Len(t, hits, 1)
	assert.Equal(t, hits[0].ID, a.Key())
	_, err = s.SearchSession(ctxOf(a), b, "session-test", []float32{1, 0, 0}, 10)
	require.Error(t, err)
	legacy, err := index.Search(context.Background(), "session-test", a.TenantID, a.Namespace, []float32{1, 0, 0}, 10)
	require.NoError(t, err)
	assert.Len(t, legacy, 0)
	require.NoError(t, index.Delete(context.Background(), []string{a.Key()}))
	hits, err = s.SearchSession(ctxOf(a), a, "session-test", []float32{1, 0, 0}, 10)
	require.NoError(t, err)
	require.Len(t, hits, 1, "legacy deletion must not touch session vectors")
	require.NoError(t, s.DeleteSession(ctxOf(a), a))
	hits, err = s.SearchSession(ctxOf(a), a, "session-test", []float32{1, 0, 0}, 10)
	require.NoError(t, err)
	assert.Len(t, hits, 0)
	hits, err = s.SearchSession(ctxOf(b), b, "session-test", []float32{1, 0, 0}, 10)
	require.NoError(t, err)
	require.Len(t, hits, 1)
	require.NoError(t, s.DeleteExecution(ctxOf(b), b.TenantID, b.SessionID))
}
