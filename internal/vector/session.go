package vector

import (
	"context"
	"slices"
	"time"

	"github.com/parisnakitakejser/codesynapse/internal/execution"
	"github.com/parisnakitakejser/codesynapse/internal/memory"
	"github.com/parisnakitakejser/codesynapse/internal/sessionmemory"
)

// SessionIndex uses separate collections and explicit tenant/session/namespace filters.
type SessionIndex interface {
	UpsertSession(context.Context, sessionmemory.Scope, string, []Item) error
	SearchSession(context.Context, sessionmemory.Scope, string, []float32, int) ([]Hit, error)
	DeleteSession(context.Context, sessionmemory.Scope) error
	DeleteExecution(context.Context, string, string) error
}

// CheckSession rejects vector items outside the authenticated session scope.
func CheckSession(ctx context.Context, scope sessionmemory.Scope, items []Item) error {
	if err := scope.Check(ctx); err != nil {
		return err
	}
	for _, it := range items {
		if it.Tenant != scope.TenantID || it.SessionID != scope.SessionID || it.Namespace != scope.Namespace || it.ExpiresAt <= time.Now().UnixMilli() {
			return sessionmemory.ErrScope
		}
	}
	return Check(items)
}

// IsolateSession drops foreign-session hits before tenant and namespace filtering.
func IsolateSession(hits []Hit, scope sessionmemory.Scope, k int) []Hit {
	if scope.SessionID == "" {
		return nil
	}
	kept := []Hit{}
	for _, h := range hits {
		if h.SessionID == scope.SessionID && h.ExpiresAt > time.Now().UnixMilli() {
			h.SessionID = ""
			kept = append(kept, h)
		}
	}
	kept = Isolate(kept, scope.TenantID, scope.Namespace, k)
	for i := range kept {
		kept[i].SessionID = scope.SessionID
	}
	return kept
}

// UpsertSession writes vectors to the separate session-memory index.
func (f *Flat) UpsertSession(ctx context.Context, scope sessionmemory.Scope, model string, items []Item) error {
	if err := CheckSession(ctx, scope, items); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.sessionItems == nil {
		f.sessionItems = map[string]flatItem{}
	}
	for _, it := range items {
		it.Vector = slices.Clone(it.Vector)
		f.sessionItems[scope.Key()+"|"+it.ID] = flatItem{Item: it, model: model}
	}
	return nil
}

// SearchSession searches with tenant, session and namespace filters.
func (f *Flat) SearchSession(ctx context.Context, scope sessionmemory.Scope, model string, vec []float32, k int) ([]Hit, error) {
	if err := scope.Check(ctx); err != nil {
		return nil, err
	}
	if err := memory.CheckVectors(map[string][]float32{"query": vec}); err != nil {
		return nil, err
	}
	f.mu.RLock()
	defer f.mu.RUnlock()
	hits := []Hit{}
	for _, it := range f.sessionItems {
		if it.model == model && it.Tenant == scope.TenantID && it.SessionID == scope.SessionID && it.Namespace == scope.Namespace && len(it.Vector) == len(vec) {
			hits = append(hits, Hit{ID: it.ID, Tenant: it.Tenant, SessionID: it.SessionID, Namespace: it.Namespace, ExpiresAt: it.ExpiresAt, Score: memory.Similarity(vec, it.Vector)})
		}
	}
	return IsolateSession(hits, scope, ClampK(k)), nil
}

// DeleteSession deletes only one session namespace from the vector index.
func (f *Flat) DeleteSession(ctx context.Context, scope sessionmemory.Scope) error {
	if err := scope.Check(ctx); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for key, it := range f.sessionItems {
		if it.Tenant == scope.TenantID && it.SessionID == scope.SessionID && it.Namespace == scope.Namespace {
			delete(f.sessionItems, key)
		}
	}
	return nil
}

// DeleteExecution deletes only the selected execution across its namespaces.
func (f *Flat) DeleteExecution(ctx context.Context, tenant, session string) error {
	id, err := execution.Require(ctx)
	if err != nil {
		return err
	}
	if id.TenantID != tenant || id.SessionID != session {
		return sessionmemory.ErrScope
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for key, it := range f.sessionItems {
		if it.Tenant == tenant && it.SessionID == session {
			delete(f.sessionItems, key)
		}
	}
	return nil
}
