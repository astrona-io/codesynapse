package memstore

import (
	"cmp"
	"context"
	"slices"

	"github.com/astrona-io/codesynapse/internal/accounts"
)

// GetRepoPolicy returns one tenant's approval, including expired decisions.
func (s *Store) GetRepoPolicy(_ context.Context, orgID, graph string) (accounts.RepoPolicyApproval, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	p, ok := s.repoPolicies[orgID+"|"+graph]
	if !ok {
		return p, accounts.ErrNotFound
	}
	if p.ExpiresAt != nil {
		at := *p.ExpiresAt
		p.ExpiresAt = &at
	}
	return p, nil
}

// PutRepoPolicy replaces an approval for exactly one repository entry.
func (s *Store) PutRepoPolicy(_ context.Context, approval accounts.RepoPolicyApproval) error {
	p, err := accounts.PrepareRepoPolicy(approval)
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
	s.repoPolicies[key] = p
	return nil
}

// DeleteRepoPolicy removes the approval without removing annotation history.
func (s *Store) DeleteRepoPolicy(_ context.Context, orgID, graph string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := orgID + "|" + graph
	if _, ok := s.repoPolicies[key]; !ok {
		return accounts.ErrNotFound
	}
	delete(s.repoPolicies, key)
	return nil
}

// UpsertAnnotations commits a bounded batch after checking policy and identity.
func (s *Store) UpsertAnnotations(ctx context.Context, records []accounts.RepositoryAnnotation) error {
	prepared, err := accounts.PrepareAnnotations(ctx, records)
	if err != nil {
		return err
	}
	first := prepared[0]
	return s.PolicyTransaction(ctx, first.OrgID, func(st accounts.Store) error {
		if err := accounts.CheckAnnotationAuthority(ctx, st, first); err != nil {
			return err
		}
		tx := st.(*Store)
		tx.mu.Lock()
		defer tx.mu.Unlock()
		key := first.OrgID + "|" + first.Graph
		rows := tx.annotations[key]
		if rows == nil {
			rows = make(map[string]accounts.RepositoryAnnotation)
		}
		total := len(rows)
		for _, r := range prepared {
			if _, ok := rows[r.SymbolID]; !ok {
				total++
			}
		}
		if total > accounts.MaxRepositoryAnnotations {
			return accounts.ErrAccessLimit
		}
		for _, r := range prepared {
			if old, ok := rows[r.SymbolID]; ok && (old.JobID == r.JobID || old.UpdatedAt.After(r.UpdatedAt)) {
				continue
			}
			rows[r.SymbolID] = r
		}
		tx.annotations[key] = rows
		return nil
	})
}

// ListAnnotations returns independent copies in symbol order.
func (s *Store) ListAnnotations(_ context.Context, orgID, graph string, limit int) ([]accounts.RepositoryAnnotation, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := []accounts.RepositoryAnnotation{}
	for _, r := range s.annotations[orgID+"|"+graph] {
		r.Taxonomy = slices.Clone(r.Taxonomy)
		if r.Confidence != nil {
			value := *r.Confidence
			r.Confidence = &value
		}
		out = append(out, r)
	}
	slices.SortFunc(out, func(a, b accounts.RepositoryAnnotation) int { return cmp.Compare(a.SymbolID, b.SymbolID) })
	return out[:min(len(out), min(max(limit, 1), accounts.MaxRepositoryAnnotations))], nil
}
