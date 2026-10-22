package sessionmemory

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/astrona-io/codesynapse/internal/execution"
	"github.com/astrona-io/codesynapse/internal/memory"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

func TestScopeAndLifetime(t *testing.T) {
	scope := Scope{TenantID: "org-a", SessionID: "chat-a", Namespace: "org-a|repo"}
	ctx, err := execution.With(context.Background(), execution.Identity{TenantID: "org-a", SessionID: "chat-a", ParentSessionID: "chat-a", SubjectID: "user"})
	require.NoError(t, err)
	require.NoError(t, scope.Check(ctx))
	require.Error(t, scope.Check(context.Background()))
	other := scope
	other.SessionID = "chat-b"
	assert.True(t, errors.Is(other.Check(ctx), ErrScope))
	now := time.Now()
	triples := []memory.Triple{{Subject: "User", Predicate: "PREFERS", Object: "Tests"}}
	d, err := Apply(Document{}, scope, triples, "test", now.Add(time.Hour), now)
	require.NoError(t, err)
	d2, err := Apply(d, scope, triples, "test", now.Add(Retention), now.Add(time.Minute))
	require.NoError(t, err)
	assert.Equal(t, d2.ExpiresAt, d.ExpiresAt, "writes cannot extend retention")
	require.Error(t, CheckDocument(d2, scope, now.Add(2*time.Hour)))
	d2.Deleted = true
	_, err = Apply(d2, scope, triples, "test", time.Time{}, now)
	assert.True(t, errors.Is(err, ErrClosed))
	d.Graph.Entities[0].Repo = "org-b|repo"
	assert.True(t, errors.Is(CheckDocument(d, scope, now), ErrScope))
}

func TestSessionCapsAndProvenance(t *testing.T) {
	scope := Scope{TenantID: "org-a", SessionID: "chat-a", Namespace: "org-a|repo"}
	now := time.Now()
	d, err := Apply(Document{}, scope, []memory.Triple{{Subject: "Card 4111111111111111", Predicate: "USES", Object: "secret token=abcdefghijklmnopqrstuvwxyz123456"}}, "agent", now.Add(time.Hour), now)
	require.NoError(t, err)
	require.Len(t, d.Graph.Facts, 1)
	assert.Equal(t, d.Graph.Facts[0].OriginSessionID, scope.SessionID)
	for _, e := range d.Graph.Entities {
		assert.False(t, strings.Contains(e.Name, "4111111111111111"))
		assert.False(t, strings.Contains(e.Name, "abcdefghijklmnopqrstuvwxyz123456"))
	}
	rows := []Quota{}
	for range MaxNamespaces {
		other := scope
		other.Namespace += "x"
		rows = append(rows, Quota{Scope: other, ExpiresAt: now.Add(time.Hour), Facts: 1})
	}
	assert.True(t, errors.Is(CheckQuota(rows, d, now), ErrLimit))
}

func TestRetainedQuota(t *testing.T) {
	now := time.Now()
	scope := Scope{TenantID: "tenant", SessionID: "session", Namespace: "tenant|new"}
	next := Document{Scope: scope, Deleted: true}
	for _, state := range []string{"deleted", "expired"} {
		t.Run(state, func(t *testing.T) {
			rows := make([]Quota, MaxTenantScopes)
			for i := range rows {
				rows[i] = Quota{Scope: Scope{TenantID: "tenant", SessionID: "other", Namespace: "tenant|other"}, Deleted: state == "deleted", ExpiresAt: now.Add(-time.Hour)}
			}
			assert.True(t, errors.Is(CheckQuota(rows, next, now), ErrLimit))
		})
	}
}
