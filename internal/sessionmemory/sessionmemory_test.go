package sessionmemory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

func TestFullDocumentDropsOldestFacts(t *testing.T) {
	scope := Scope{TenantID: "org-a", SessionID: "chat-a", Namespace: "org-a|repo"}
	now := time.Now()
	d := Document{}
	var err error
	for batch := range 40 {
		triples := make([]memory.Triple, 0, 10)
		for i := range 10 {
			triples = append(triples, memory.Triple{
				Subject:   fmt.Sprintf("module number %d of batch %d", i, batch),
				Predicate: "DEPENDS_ON",
				Object:    fmt.Sprintf("component %d of batch %d with a longer name", i, batch),
			})
		}
		d, err = Apply(d, scope, triples, "test", now.Add(time.Hour), now.Add(time.Duration(batch)*time.Second))
		require.NoError(t, err, "batch %d", batch)
		b, err := json.Marshal(d)
		require.NoError(t, err)
		assert.True(t, len(b) <= MaxDocumentBytes, "document stays within its cap")
		require.NoError(t, CheckDocument(d, scope, now))
	}
	names := map[string]bool{}
	for _, e := range d.Graph.Entities {
		names[e.Name] = true
	}
	assert.True(t, names["module number 0 of batch 39"], "newest facts are kept")
	assert.False(t, names["module number 0 of batch 0"], "oldest facts were dropped")
	for _, f := range d.Graph.Facts {
		assert.True(t, f.Current())
	}
}

func TestOversizedWriteKeepsItsBestFacts(t *testing.T) {
	scope := Scope{TenantID: "org-a", SessionID: "chat-a", Namespace: "org-a|repo"}
	now := time.Now()
	triples := make([]memory.Triple, 0, memory.MaxTriplesPerCall)
	for i := range memory.MaxTriplesPerCall {
		triples = append(triples, memory.Triple{
			Subject:    fmt.Sprintf("subsystem number %d of the billing platform", i),
			Predicate:  "DEPENDS_ON",
			Object:     fmt.Sprintf("shared component number %d used by the invoicing module", i),
			Confidence: 0.5 + float64(i)/200,
		})
	}
	d, err := Apply(Document{}, scope, triples, "test", now.Add(time.Hour), now)
	require.NoError(t, err, "one extraction larger than the document is trimmed, not refused")
	require.NotEmpty(t, d.Graph.Facts)
	assert.True(t, len(d.Graph.Facts) < len(triples))
	best := 0.0
	for _, f := range d.Graph.Facts {
		best = max(best, f.Confidence)
	}
	assert.True(t, best >= 0.5+float64(len(triples)-1)/200-1e-9, "the most confident fact is kept")
	b, err := json.Marshal(d)
	require.NoError(t, err)
	assert.True(t, len(b) <= MaxDocumentBytes)
}
