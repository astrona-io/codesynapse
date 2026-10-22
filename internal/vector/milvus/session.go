package milvus

import (
	"context"
	"strings"
	"time"

	"github.com/astrona-io/codesynapse/internal/execution"
	"github.com/astrona-io/codesynapse/internal/memory"
	"github.com/astrona-io/codesynapse/internal/sessionmemory"
	"github.com/astrona-io/codesynapse/internal/vector"
)

const sessionFilter = "tenant_id == {tenant} && session_id == {session} && ns == {ns}"

// UpsertSession writes vectors to the separate session-memory index.
func (x *Index) UpsertSession(ctx context.Context, scope sessionmemory.Scope, model string, items []vector.Item) error {
	if err := vector.CheckSession(ctx, scope, items); err != nil {
		return err
	}
	return x.sessions.Upsert(ctx, model, items)
}

// SearchSession searches with tenant, session and namespace filters.
func (x *Index) SearchSession(ctx context.Context, scope sessionmemory.Scope, model string, vec []float32, k int) ([]vector.Hit, error) {
	if err := scope.Check(ctx); err != nil {
		return nil, err
	}
	if err := memory.CheckVectors(map[string][]float32{"query": vec}); err != nil {
		return nil, err
	}
	y := x.sessions
	name := y.collection(model, len(vec))
	has, err := y.exists(ctx, name)
	if err != nil || !has {
		return nil, err
	}
	if err = y.ensure(ctx, name, len(vec)); err != nil {
		return nil, err
	}
	var rows []struct {
		ID        string  `json:"id"`
		EntityID  string  `json:"entity_id"`
		ExpiresAt int64   `json:"expires_at"`
		Tenant    string  `json:"tenant_id"`
		Session   string  `json:"session_id"`
		NS        string  `json:"ns"`
		Distance  float64 `json:"distance"`
	}
	err = y.call(ctx, "entities/search", map[string]any{"collectionName": name, "data": [][]float32{vec}, "annsField": "vec", "limit": vector.ClampK(k), "filter": sessionFilter + " && expires_at > {now}",
		"exprParams": map[string]any{"tenant": scope.TenantID, "session": scope.SessionID, "ns": scope.Namespace, "now": time.Now().UnixMilli()}, "outputFields": []string{"entity_id", "tenant_id", "session_id", "ns", "expires_at"}, "consistencyLevel": "Strong"}, &rows)
	if err != nil {
		return nil, err
	}
	hits := []vector.Hit{}
	for _, r := range rows {
		hits = append(hits, vector.Hit{ID: r.EntityID, Tenant: r.Tenant, SessionID: r.Session, Namespace: r.NS, ExpiresAt: r.ExpiresAt, Score: (1 + r.Distance) / 2})
	}
	return vector.IsolateSession(hits, scope, vector.ClampK(k)), nil
}

// DeleteSession deletes only one session namespace from the vector index.
func (x *Index) DeleteSession(ctx context.Context, scope sessionmemory.Scope) error {
	if err := scope.Check(ctx); err != nil {
		return err
	}
	return x.sessions.deleteSessionFilter(ctx, sessionFilter, map[string]any{"tenant": scope.TenantID, "session": scope.SessionID, "ns": scope.Namespace})
}

// DeleteExecution deletes only the selected execution across its namespaces.
func (x *Index) DeleteExecution(ctx context.Context, tenant, session string) error {
	id, err := execution.Require(ctx)
	if err != nil {
		return err
	}
	if id.TenantID != tenant || id.SessionID != session {
		return sessionmemory.ErrScope
	}
	return x.sessions.deleteSessionFilter(ctx, "tenant_id == {tenant} && session_id == {session}", map[string]any{"tenant": tenant, "session": session})
}

func (x *Index) deleteSessionFilter(ctx context.Context, filter string, params map[string]any) error {
	names, err := x.names(ctx)
	if err != nil {
		return err
	}
	for _, name := range names {
		if strings.HasPrefix(name, x.prefix) {
			if err = x.call(ctx, "entities/delete", map[string]any{"collectionName": name, "filter": filter, "exprParams": params}, nil); err != nil {
				return err
			}
		}
	}
	return nil
}
