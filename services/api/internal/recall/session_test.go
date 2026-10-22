package recall

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/astrona-io/codesynapse/internal/execution"
	"github.com/astrona-io/codesynapse/internal/memory"
	"github.com/astrona-io/codesynapse/internal/sessionmemory"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

func TestSessionRecallHybridIsolation(t *testing.T) {
	s, st := setup(t, true)
	ctx := recallContext(t)
	scope := sessionmemory.Scope{TenantID: "tenant", SessionID: "thread", Namespace: "tenant|r"}
	require.NoError(t, st.UpsertSessionMemory(ctx, scope, []memory.Triple{{Subject: "Payments service", Predicate: "OWNED_BY", Object: "Session Team"}}, "runtime", time.Now().Add(time.Hour)))
	otherID := execution.Identity{TenantID: "tenant", SubjectID: "user", SessionID: "other-thread", ParentSessionID: "other-thread"}
	otherCtx, err := execution.With(t.Context(), otherID)
	require.NoError(t, err)
	otherScope := scope
	otherScope.SessionID = otherID.SessionID
	require.NoError(t, st.UpsertSessionMemory(otherCtx, otherScope, []memory.Triple{{Subject: "Payments service", Predicate: "OWNED_BY", Object: "Private other team"}}, "runtime", time.Now().Add(time.Hour)))
	require.NoError(t, s.IndexSession(ctx, scope, allowRecall))
	require.NoError(t, s.IndexSession(otherCtx, otherScope, allowRecall))
	got, err := s.RecallSession(ctx, scope, Query{Text: "where does the money flow", Depth: 2})
	require.NoError(t, err)
	assert.Equal(t, got.Mode, "hybrid")
	require.Len(t, got.Facts, 1)
	assert.Equal(t, got.Notice, memory.Notice)
	for _, e := range got.Entities {
		assert.Equal(t, e.Repo, scope.Key())
		assert.False(t, e.Name == "Private other team" || e.Name == "Team Blue")
	}
	assert.Equal(t, got.Facts[0].OriginSessionID, "thread")
	_, err = s.RecallSession(otherCtx, scope, Query{Text: "payments"})
	require.Error(t, err)
	_, err = s.RecallSession(context.Background(), scope, Query{Text: "thanks!"})
	require.Error(t, err, "even gated queries need a parent identity")
	require.NoError(t, st.DeleteSessionMemory(ctx, scope))
	_, err = s.RecallSession(ctx, scope, Query{Text: "payments"})
	assert.True(t, errors.Is(err, sessionmemory.ErrClosed))
}

func TestIndexingRequiresScopeAndCurrentPermission(t *testing.T) {
	s, _ := setup(t, true)
	_, err := s.IndexRepository(context.Background(), "tenant|r", allowRecall)
	assert.True(t, errors.Is(err, execution.ErrMissing))
	_, err = s.IndexRepository(recallContext(t, "foreign"), "tenant|r", allowRecall)
	require.Error(t, err)
	checks := 0
	_, err = s.IndexRepository(recallContext(t), "tenant|r", func(context.Context) error {
		checks++
		if checks > 1 {
			return errors.New("revoked before storage")
		}
		return nil
	})
	require.Error(t, err)
	assert.Equal(t, checks, 2)
}
