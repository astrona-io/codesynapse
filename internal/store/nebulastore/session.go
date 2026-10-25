package nebulastore

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/astrona-io/codesynapse/internal/memory"
	"github.com/astrona-io/codesynapse/internal/sessionmemory"
	"github.com/astrona-io/codesynapse/internal/store"
	"github.com/astrona-io/codesynapse/internal/vector"
)

var _ store.SessionMemoryStore = (*Store)(nil)

// Transient chat memory keeps one JSON document per tenant, session and
// namespace (the source of truth; the vectors live in the vector index).
// Commits are serialised per tenant by a short lease, because quotas span
// the tenant's scopes and NebulaGraph has no transaction to check them in.

const (
	sessionLockTTL   = 30 * time.Second
	sessionLockTries = 40
)

func scopeVID(scope sessionmemory.Scope) string { return vid("t", scope.Key()) }
func revokedVID(tenant, session string) string  { return vid("x", tenant+"|"+session) }

// inTenant runs f holding the tenant's session lease; a lease that stays
// taken answers a conflict, which callers retry as they do any lost revision race.
func (s *Store) inTenant(ctx context.Context, tenant string, f func() error) error {
	err := s.locked(ctx, vid("l", tenant), tenant, sessionLockTTL, f)
	if errors.Is(err, errBusy) {
		return sessionmemory.ErrConflict
	}
	return err
}

// locked runs f holding the lease vertex head for a short while, waiting a
// little for another holder. NebulaGraph has no transaction, so writers that
// must not interleave take a lease first.
func (s *Store) locked(ctx context.Context, head, owner string, ttl time.Duration, f func() error) error {
	run, err := newRunID()
	if err != nil {
		return err
	}
	for try := 1; ; try++ {
		err = s.lease(ctx, head, owner, run, ttl)
		if !errors.Is(err, errBusy) {
			break
		}
		if try == sessionLockTries {
			return errBusy
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(try) * 10 * time.Millisecond):
		}
	}
	if err != nil {
		return err
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		_ = s.release(ctx, head, run)
	}()
	return f()
}

func (s *Store) revoked(ctx context.Context, tenant, session string) (bool, error) {
	rows, err := s.read(ctx, "FETCH PROP ON `session_revoked` "+literal(revokedVID(tenant, session))+" YIELD properties(vertex).session_id AS id", nil)
	return len(rows) > 0, err
}

func (s *Store) sessionDoc(ctx context.Context, scope sessionmemory.Scope) (sessionmemory.Document, error) {
	var d sessionmemory.Document
	doc, ok, err := s.getDoc(ctx, "session_scope", scopeVID(scope))
	if err != nil || !ok || doc == "" {
		return d, err
	}
	err = json.Unmarshal([]byte(doc), &d)
	return d, err
}

// LoadSessionDocument loads one tenant/session/namespace snapshot, checking revocation first.
func (s *Store) LoadSessionDocument(ctx context.Context, scope sessionmemory.Scope) (sessionmemory.Document, error) {
	if err := scope.Check(ctx); err != nil {
		return sessionmemory.Document{}, err
	}
	gone, err := s.revoked(ctx, scope.TenantID, scope.SessionID)
	if err != nil {
		return sessionmemory.Document{}, err
	}
	if gone {
		return sessionmemory.Document{}, sessionmemory.ErrClosed
	}
	return s.sessionDoc(ctx, scope)
}

// CommitSessionDocument commits one revision under the same scope.
func (s *Store) CommitSessionDocument(ctx context.Context, d sessionmemory.Document, expected int64) error {
	if err := sessionmemory.CheckCommit(d, expected); err != nil {
		return err
	}
	if err := d.Scope.Check(ctx); err != nil {
		return err
	}
	raw, err := json.Marshal(d)
	if err != nil {
		return err
	}
	err = s.inTenant(ctx, d.Scope.TenantID, func() error {
		gone, err := s.revoked(ctx, d.Scope.TenantID, d.Scope.SessionID)
		if err != nil {
			return err
		}
		if gone {
			return sessionmemory.ErrClosed
		}
		old, err := s.sessionDoc(ctx, d.Scope)
		if err != nil {
			return err
		}
		if old.Revision != expected {
			return sessionmemory.ErrConflict
		}
		now := time.Now()
		if err := sessionmemory.CheckTransition(old, d, now); err != nil {
			return err
		}
		rows, err := s.quotaRows(ctx, d.Scope.TenantID)
		if err != nil {
			return err
		}
		if err := sessionmemory.CheckQuota(rows, d, now); err != nil {
			return err
		}
		_, err = s.write(ctx, "INSERT VERTEX `session_scope` (`tenant_id`,`session_id`,`ns`,`doc`,`revision`,`expires`,`facts`,`deleted`) VALUES "+literal(scopeVID(d.Scope))+":($tenant,$session,$ns,$doc,$revision,$expires,$facts,$deleted)",
			map[string]any{"tenant": d.Scope.TenantID, "session": d.Scope.SessionID, "ns": d.Scope.Namespace, "doc": string(raw), "revision": d.Revision,
				"expires": d.ExpiresAt.UnixMilli(), "facts": int64(len(d.Graph.Facts)), "deleted": d.Deleted})
		return err
	})
	if err != nil {
		return err
	}
	// A new revision invalidates the vectors made for the previous one.
	if index, ok := s.vec.(vector.SessionIndex); ok {
		return index.DeleteSession(ctx, d.Scope)
	}
	return nil
}

// quotaRows are the tenant's retained scopes, tombstones included.
func (s *Store) quotaRows(ctx context.Context, tenant string) ([]sessionmemory.Quota, error) {
	rows, err := s.read(ctx, "MATCH (v:`session_scope`) WHERE v.`session_scope`.`tenant_id` == $tenant RETURN properties(v) AS p LIMIT $limit",
		map[string]any{"tenant": tenant, "limit": int64(sessionmemory.MaxTenantScopes + 2)})
	if err != nil {
		return nil, err
	}
	out := make([]sessionmemory.Quota, 0, len(rows))
	for _, r := range rows {
		p, _ := r["p"].(map[string]any)
		facts, _ := p["facts"].(int64)
		expires, _ := p["expires"].(int64)
		deleted, _ := p["deleted"].(bool)
		out = append(out, sessionmemory.Quota{Scope: sessionmemory.Scope{TenantID: str(p, "tenant_id"), SessionID: str(p, "session_id"), Namespace: str(p, "ns")},
			Facts: int(facts), ExpiresAt: time.UnixMilli(expires), Deleted: deleted})
	}
	return out, nil
}

// RevokeSessionDocuments tombstones the execution and removes all of its scoped chat data.
func (s *Store) RevokeSessionDocuments(ctx context.Context, tenant, session string) error {
	if err := sessionmemory.CheckExecution(ctx, tenant, session); err != nil {
		return err
	}
	err := s.inTenant(ctx, tenant, func() error {
		if _, err := s.write(ctx, "INSERT VERTEX `session_revoked` (`tenant_id`,`session_id`) VALUES "+literal(revokedVID(tenant, session))+":($tenant,$session)",
			map[string]any{"tenant": tenant, "session": session}); err != nil {
			return err
		}
		rows, err := s.read(ctx, "MATCH (v:`session_scope`) WHERE v.`session_scope`.`tenant_id` == $tenant AND v.`session_scope`.`session_id` == $session RETURN id(v) AS id LIMIT $limit",
			map[string]any{"tenant": tenant, "session": session, "limit": int64(sessionmemory.MaxTenantScopes + 2)})
		if err != nil {
			return err
		}
		for _, r := range rows {
			id, _ := r["id"].(string)
			if _, err := s.write(ctx, "UPSERT VERTEX ON `session_scope` "+literal(id)+" SET `deleted` = true, `doc` = \"\", `facts` = 0", nil); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	if index, ok := s.vec.(vector.SessionIndex); ok {
		return index.DeleteExecution(ctx, tenant, session)
	}
	return nil
}

// WriteSessionVectors writes vectors only for the selected session namespace.
func (s *Store) WriteSessionVectors(ctx context.Context, scope sessionmemory.Scope, model string, vecs map[string][]float32) error {
	if err := scope.Check(ctx); err != nil {
		return err
	}
	index, ok := s.vec.(vector.SessionIndex)
	if !ok {
		return nil
	}
	d, err := s.LoadSessionDocument(ctx, scope)
	if err != nil {
		return err
	}
	if err = sessionmemory.CheckDocument(d, scope, time.Now()); err != nil {
		return err
	}
	items := []vector.Item{}
	for id, v := range vecs {
		items = append(items, vector.Item{ID: id, Tenant: scope.TenantID, SessionID: scope.SessionID, Namespace: scope.Namespace, ExpiresAt: d.ExpiresAt.UnixMilli(), Vector: v})
	}
	if err = index.UpsertSession(ctx, scope, model, items); err != nil {
		return err
	}
	// The document may have moved on while the vectors were written.
	latest, err := s.LoadSessionDocument(ctx, scope)
	if err == nil {
		err = sessionmemory.CheckDocument(latest, scope, time.Now())
	}
	if err == nil && latest.Revision != d.Revision {
		err = sessionmemory.ErrConflict
	}
	if err != nil {
		if clean := index.DeleteSession(ctx, scope); clean != nil {
			return clean
		}
		return err
	}
	return nil
}

// SearchSessionVectors searches only the selected session namespace.
func (s *Store) SearchSessionVectors(ctx context.Context, scope sessionmemory.Scope, model string, vec []float32, k int) ([]memory.Scored, error) {
	if err := scope.Check(ctx); err != nil {
		return nil, err
	}
	index, ok := s.vec.(vector.SessionIndex)
	if !ok {
		return []memory.Scored{}, nil
	}
	hits, err := index.SearchSession(ctx, scope, model, vec, k)
	if err != nil {
		return nil, err
	}
	out := []memory.Scored{}
	for _, h := range vector.IsolateSession(hits, scope, k) {
		out = append(out, memory.Scored{Entity: memory.Entity{ID: h.ID, Repo: scope.Key()}, Score: h.Score})
	}
	return out, nil
}
