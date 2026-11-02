package memstore

import (
	"cmp"
	"context"
	"slices"
	"sync"
	"time"

	"github.com/astrona-io/codesynapse/internal/accounts"
)

var _ accounts.OpenFGASyncStore = (*Store)(nil)

// fgaTable holds OpenFGA sync records. Like jobs it is shared by policy
// transactions rather than copied: the sync writes it outside them.
type fgaTable struct {
	mu   sync.Mutex
	rows map[string]fgaRow
}

type fgaRow struct {
	sync    accounts.OpenFGASync
	synced  bool
	request *time.Time
}

func (s *Store) fgaTable() *fgaTable {
	s.fgaOnce.Do(func() {
		if s.fga == nil {
			s.fga = &fgaTable{rows: map[string]fgaRow{}}
		}
	})
	return s.fga
}

// OpenFGAPending implements accounts.OpenFGASyncStore.
func (s *Store) OpenFGAPending(_ context.Context, storeID, modelID string, limit int) ([]accounts.Org, error) {
	s.mu.RLock()
	orgs := make([]accounts.Org, 0, len(s.orgs))
	for _, o := range s.orgs {
		orgs = append(orgs, o)
	}
	s.mu.RUnlock()
	t := s.fgaTable()
	t.mu.Lock()
	defer t.mu.Unlock()
	out := []accounts.Org{}
	for _, o := range orgs {
		r, ok := t.rows[o.ID]
		if !ok || r.sync.StoreID != storeID || r.sync.ModelID != modelID || r.sync.Revision < o.PolicyRevision || r.request != nil {
			out = append(out, o)
		}
	}
	slices.SortFunc(out, func(a, b accounts.Org) int { return cmp.Compare(a.ID, b.ID) })
	return out[:min(len(out), min(max(limit, 1), accounts.MaxTuples))], nil
}

// OpenFGASynced implements accounts.OpenFGASyncStore.
func (s *Store) OpenFGASynced(_ context.Context, orgID string) (accounts.OpenFGASync, error) {
	t := s.fgaTable()
	t.mu.Lock()
	defer t.mu.Unlock()
	r, ok := t.rows[orgID]
	if !ok || !r.synced {
		return accounts.OpenFGASync{OrgID: orgID}, accounts.ErrNotFound
	}
	out := r.sync
	if r.request != nil {
		at := *r.request
		out.RebuildRequested = &at
	}
	return out, nil
}

// RecordOpenFGASync implements accounts.OpenFGASyncStore.
func (s *Store) RecordOpenFGASync(_ context.Context, rec accounts.OpenFGASync, started time.Time) error {
	s.mu.RLock()
	_, ok := s.orgs[rec.OrgID]
	s.mu.RUnlock()
	if !ok {
		return accounts.ErrNotFound
	}
	t := s.fgaTable()
	t.mu.Lock()
	defer t.mu.Unlock()
	r, had := t.rows[rec.OrgID]
	if had && r.sync.StoreID == rec.StoreID && r.sync.ModelID == rec.ModelID {
		rec.Revision = max(rec.Revision, r.sync.Revision)
	}
	rec.SyncedAt = rec.SyncedAt.UTC()
	rec.RebuildRequested = nil
	r.sync, r.synced = rec, true
	if r.request != nil && !r.request.After(started) {
		r.request = nil
	}
	t.rows[rec.OrgID] = r
	return nil
}

// RequestOpenFGARebuild implements accounts.OpenFGASyncStore.
func (s *Store) RequestOpenFGARebuild(_ context.Context, orgID string, at time.Time) (int, error) {
	s.mu.RLock()
	ids := []string{}
	for id := range s.orgs {
		if orgID == "" || id == orgID {
			ids = append(ids, id)
		}
	}
	s.mu.RUnlock()
	if orgID != "" && len(ids) == 0 {
		return 0, accounts.ErrNotFound
	}
	t := s.fgaTable()
	t.mu.Lock()
	defer t.mu.Unlock()
	at = at.UTC()
	for _, id := range ids {
		r := t.rows[id]
		when := at
		r.request = &when
		t.rows[id] = r
	}
	return len(ids), nil
}
