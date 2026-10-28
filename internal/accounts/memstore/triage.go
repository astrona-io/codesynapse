package memstore

import (
	"cmp"
	"context"
	"slices"

	"github.com/astrona-io/codesynapse/internal/accounts"
)

// PutFindingTriage implements accounts.Store.
func (s *Store) PutFindingTriage(_ context.Context, t accounts.FindingTriage) error {
	p, err := accounts.PrepareFindingTriage(t)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key := p.OrgID + "|" + p.Graph
	if _, ok := s.repos[key]; !ok {
		return accounts.ErrNotFound
	}
	if _, ok := s.users[p.ActorID]; !ok {
		return accounts.ErrNotFound
	}
	rows := s.triage[key]
	if rows == nil {
		rows = map[string]accounts.FindingTriage{}
		s.triage[key] = rows
	}
	if _, ok := rows[p.Fingerprint]; !ok && len(rows) >= accounts.MaxFindingTriage {
		return accounts.ErrAccessLimit
	}
	rows[p.Fingerprint] = p
	return nil
}

// DeleteFindingTriage implements accounts.Store.
func (s *Store) DeleteFindingTriage(_ context.Context, orgID, graph, fingerprint string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows := s.triage[orgID+"|"+graph]
	if _, ok := rows[fingerprint]; !ok {
		return accounts.ErrNotFound
	}
	delete(rows, fingerprint)
	return nil
}

// ListFindingTriage implements accounts.Store.
func (s *Store) ListFindingTriage(_ context.Context, orgID, graph string, limit int) ([]accounts.FindingTriage, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := []accounts.FindingTriage{}
	for _, t := range s.triage[orgID+"|"+graph] {
		if t.ExpiresAt != nil {
			at := *t.ExpiresAt
			t.ExpiresAt = &at
		}
		t.ActorName = s.users[t.ActorID].Name
		out = append(out, t)
	}
	slices.SortFunc(out, func(a, b accounts.FindingTriage) int {
		return cmp.Or(b.CreatedAt.Compare(a.CreatedAt), cmp.Compare(a.Fingerprint, b.Fingerprint))
	})
	return out[:min(len(out), min(max(limit, 1), accounts.MaxFindingTriage))], nil
}
