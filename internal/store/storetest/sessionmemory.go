package storetest

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/astrona-io/codesynapse/internal/execution"
	"github.com/astrona-io/codesynapse/internal/memory"
	"github.com/astrona-io/codesynapse/internal/sessionmemory"
	"github.com/astrona-io/codesynapse/internal/store"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

func sessionMemoryContract(t *testing.T, s store.Store) {
	st, ok := s.(store.SessionMemoryStore)
	require.True(t, ok)
	sid := fmt.Sprintf("chat-%d", time.Now().UnixNano())
	a := sessionmemory.Scope{TenantID: "org-session-a", SessionID: sid, Namespace: "org-session-a|fixture"}
	b := a
	b.SessionID += "-b"
	c := a
	c.TenantID = "org-session-b"
	c.Namespace = "org-session-b|fixture"
	ctxOf := func(scope sessionmemory.Scope) context.Context {
		ctx, err := execution.With(context.Background(), execution.Identity{TenantID: scope.TenantID, SessionID: scope.SessionID, ParentSessionID: scope.SessionID, SubjectID: "user-session"})
		require.NoError(t, err)
		return ctx
	}
	ctx := ctxOf(a)
	triple := []memory.Triple{{Subject: "User", Predicate: "PREFERS", Object: "Small tests"}}
	err := st.UpsertSessionMemory(context.Background(), a, triple, "test", time.Time{})
	assert.True(t, errors.Is(err, execution.ErrMissing))
	for _, scope := range []sessionmemory.Scope{a, b, c} {
		require.NoError(t, st.UpsertSessionMemory(ctxOf(scope), scope, triple, "test", time.Time{}))
	}
	_, err = st.ReadSessionMemory(ctx, b, "", 20)
	assert.True(t, errors.Is(err, sessionmemory.ErrScope))
	g, err := st.ReadSessionMemory(ctx, a, "", 512)
	require.NoError(t, err)
	require.Len(t, g.Facts, 1)
	require.Len(t, g.Entities, 2)
	assert.Equal(t, g.Facts[0].OriginSessionID, a.SessionID)
	gb, err := st.ReadSessionMemory(ctxOf(b), b, "", 512)
	require.NoError(t, err)
	assert.False(t, g.Entities[0].ID == gb.Entities[0].ID, "session participates in stable entity identity")
	repo, err := s.SearchMemory(ctx, a.Namespace, "", 20, false)
	require.NoError(t, err)
	assert.Len(t, repo.Entities, 0, "session facts never enter durable repository memory")
	vecs := map[string][]float32{}
	for _, e := range g.Entities {
		vecs[e.ID] = []float32{1, 0, 0}
	}
	require.NoError(t, st.SetSessionMemoryEmbeddings(ctx, a, "session-contract", vecs))
	hits, err := st.SessionMemoryNearest(ctx, a, "session-contract", []float32{1, 0, 0}, 8)
	require.NoError(t, err)
	require.Len(t, hits, 2)
	for _, hit := range hits {
		assert.Equal(t, hit.Repo, a.Key())
	}
	err = st.SetSessionMemoryEmbeddings(ctxOf(b), b, "session-contract", vecs)
	assert.True(t, errors.Is(err, sessionmemory.ErrScope))
	require.NoError(t, st.DeleteSessionMemory(ctx, a))
	backend, ok := s.(sessionmemory.Backend)
	require.True(t, ok)
	closed, err := backend.LoadSessionDocument(ctx, a)
	require.NoError(t, err)
	revived := closed
	revived.Deleted = false
	revived.Revision++
	err = backend.CommitSessionDocument(ctx, revived, closed.Revision)
	assert.True(t, errors.Is(err, sessionmemory.ErrClosed), "direct adapter writes cannot revive a namespace tombstone")
	_, err = st.ReadSessionMemory(ctx, a, "", 20)
	assert.True(t, errors.Is(err, sessionmemory.ErrClosed))
	err = st.UpsertSessionMemory(ctx, a, triple, "test", time.Time{})
	assert.True(t, errors.Is(err, sessionmemory.ErrClosed), "deleted memory cannot be recreated by replay")
	gb, err = st.ReadSessionMemory(ctxOf(b), b, "", 20)
	require.NoError(t, err)
	require.Len(t, gb.Facts, 1)
	live, err := backend.LoadSessionDocument(ctxOf(b), b)
	require.NoError(t, err)
	extended := live
	extended.ExpiresAt = extended.ExpiresAt.Add(time.Hour)
	extended.Revision++
	err = backend.CommitSessionDocument(ctxOf(b), extended, live.Revision)
	assert.True(t, errors.Is(err, sessionmemory.ErrClosed), "retention is immutable at the adapter boundary")
	other := b
	other.Namespace = "org-session-a|another"
	require.NoError(t, st.UpsertSessionMemory(ctxOf(b), other, triple, "test", time.Time{}))
	require.NoError(t, st.DeleteExecutionMemory(ctxOf(b), b.TenantID, b.SessionID))
	_, err = st.ReadSessionMemory(ctxOf(b), other, "", 20)
	assert.True(t, errors.Is(err, sessionmemory.ErrClosed))
	newNS := b
	newNS.Namespace = "org-session-a|never-created"
	err = st.UpsertSessionMemory(ctxOf(b), newNS, triple, "test", time.Time{})
	assert.True(t, errors.Is(err, sessionmemory.ErrClosed), "whole-session tombstone covers future namespaces")
	gc, err := st.ReadSessionMemory(ctxOf(c), c, "", 20)
	require.NoError(t, err)
	require.Len(t, gc.Facts, 1)
	require.NoError(t, st.DeleteExecutionMemory(ctxOf(c), c.TenantID, c.SessionID))
	capped := c
	capped.SessionID += "-capped"
	for i := range sessionmemory.MaxNamespaces {
		capped.Namespace = fmt.Sprintf("%s|deleted-%d", capped.TenantID, i)
		require.NoError(t, st.DeleteSessionMemory(ctxOf(capped), capped))
	}
	capped.Namespace = capped.TenantID + "|over-cap"
	err = st.DeleteSessionMemory(ctxOf(capped), capped)
	assert.True(t, errors.Is(err, sessionmemory.ErrLimit), "tombstones count against retained namespace capacity")
	err = st.UpsertSessionMemory(ctxOf(capped), capped, triple, "test", time.Time{})
	assert.True(t, errors.Is(err, sessionmemory.ErrLimit), "deleting namespaces cannot bypass retained capacity")

}
