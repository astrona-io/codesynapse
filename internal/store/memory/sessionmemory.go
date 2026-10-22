package memory

import (
	"context"
	"encoding/json"
	"time"

	"github.com/astrona-io/codesynapse/internal/memory"
	"github.com/astrona-io/codesynapse/internal/sessionmemory"
	"github.com/astrona-io/codesynapse/internal/vector"
)

// LoadSessionDocument loads one tenant/session/namespace snapshot, checking revocation first.
func (s *Store) LoadSessionDocument(ctx context.Context, scope sessionmemory.Scope) (sessionmemory.Document, error) {
	if err := scope.Check(ctx); err != nil {
		return sessionmemory.Document{}, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.sessionRevoked[scope.TenantID+"|"+scope.SessionID] {
		return sessionmemory.Document{}, sessionmemory.ErrClosed
	}
	d := s.sessions[scope.Key()]
	data, err := json.Marshal(d)
	if err != nil {
		return d, err
	}
	var cloned sessionmemory.Document
	err = json.Unmarshal(data, &cloned)
	return cloned, err
}

// CommitSessionDocument commits one revision and its materialized graph under the same scope.
func (s *Store) CommitSessionDocument(ctx context.Context, d sessionmemory.Document, expected int64) error {
	if err := sessionmemory.CheckCommit(d, expected); err != nil {
		return err
	}
	if err := d.Scope.Check(ctx); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sessionRevoked[d.Scope.TenantID+"|"+d.Scope.SessionID] {
		return sessionmemory.ErrClosed
	}
	if s.sessions[d.Scope.Key()].Revision != expected {
		return sessionmemory.ErrConflict
	}
	if err := sessionmemory.CheckTransition(s.sessions[d.Scope.Key()], d, time.Now()); err != nil {
		return err
	}
	rows := []sessionmemory.Quota{}
	for _, old := range s.sessions {
		rows = append(rows, sessionmemory.Quota{Scope: old.Scope, Facts: len(old.Graph.Facts), ExpiresAt: old.ExpiresAt, Deleted: old.Deleted})
	}
	if err := sessionmemory.CheckQuota(rows, d, time.Now()); err != nil {
		return err
	}
	if err := s.sessionVectors.DeleteSession(ctx, d.Scope); err != nil {
		return err
	}
	data, err := json.Marshal(d)
	if err != nil {
		return err
	}
	var cloned sessionmemory.Document
	if err := json.Unmarshal(data, &cloned); err != nil {
		return err
	}
	s.sessions[d.Scope.Key()] = cloned
	return nil
}

// RevokeSessionDocuments tombstones the execution and removes all of its scoped chat data.
func (s *Store) RevokeSessionDocuments(ctx context.Context, tenant, session string) error {
	if err := sessionmemory.CheckExecution(ctx, tenant, session); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessionRevoked[tenant+"|"+session] = true
	for key, d := range s.sessions {
		if d.Scope.TenantID == tenant && d.Scope.SessionID == session {
			d.Deleted = true
			d.Graph = sessionmemory.EmptyGraph()
			d.Revision++
			s.sessions[key] = d
		}
	}
	return s.sessionVectors.DeleteExecution(ctx, tenant, session)
}

// WriteSessionVectors writes vectors only for the selected session namespace.
func (s *Store) WriteSessionVectors(ctx context.Context, scope sessionmemory.Scope, model string, vecs map[string][]float32) error {
	if err := scope.Check(ctx); err != nil {
		return err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	d := s.sessions[scope.Key()]
	if err := sessionmemory.CheckDocument(d, scope, time.Now()); err != nil {
		return err
	}
	if s.sessionRevoked[scope.TenantID+"|"+scope.SessionID] {
		return sessionmemory.ErrClosed
	}
	items := []vector.Item{}
	for id, v := range vecs {
		items = append(items, vector.Item{ID: id, Tenant: scope.TenantID, SessionID: scope.SessionID, Namespace: scope.Namespace, ExpiresAt: d.ExpiresAt.UnixMilli(), Vector: v})
	}
	return s.sessionVectors.UpsertSession(ctx, scope, model, items)
}

// SearchSessionVectors searches only the selected session namespace.
func (s *Store) SearchSessionVectors(ctx context.Context, scope sessionmemory.Scope, model string, vec []float32, k int) ([]memory.Scored, error) {
	if err := scope.Check(ctx); err != nil {
		return nil, err
	}
	hits, err := s.sessionVectors.SearchSession(ctx, scope, model, vec, k)
	out := []memory.Scored{}
	for _, h := range hits {
		out = append(out, memory.Scored{Entity: memory.Entity{ID: h.ID, Repo: scope.Key()}, Score: h.Score})
	}
	return out, err
}
