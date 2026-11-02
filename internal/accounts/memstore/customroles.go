package memstore

import (
	"context"
	"slices"
	"strings"

	"github.com/astrona-io/codesynapse/internal/accounts"
)

// CustomRoles implements accounts.Store.
func (s *Store) CustomRoles(_ context.Context, orgIDs []string) ([]accounts.CustomRole, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := []accounts.CustomRole{}
	for _, r := range s.roles {
		if slices.Contains(orgIDs, r.OrgID) {
			r.Permissions = slices.Clone(r.Permissions)
			out = append(out, r)
		}
	}
	slices.SortFunc(out, func(a, b accounts.CustomRole) int {
		if c := strings.Compare(a.OrgID, b.OrgID); c != 0 {
			return c
		}
		return strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name))
	})
	return out, nil
}

func (s *Store) roleNameTaken(r accounts.CustomRole) bool {
	for _, x := range s.roles {
		if x.ID != r.ID && x.OrgID == r.OrgID && strings.EqualFold(x.Name, r.Name) {
			return true
		}
	}
	return false
}

// CreateCustomRole implements accounts.Store.
func (s *Store) CreateCustomRole(_ context.Context, r accounts.CustomRole) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.orgs[r.OrgID]; !ok {
		return accounts.ErrNotFound
	}
	if _, ok := s.roles[r.ID]; ok || s.roleNameTaken(r) {
		return accounts.ErrExists
	}
	r.Permissions = slices.Clone(r.Permissions)
	s.roles[r.ID] = r
	return nil
}

// UpdateCustomRole implements accounts.Store.
func (s *Store) UpdateCustomRole(_ context.Context, r accounts.CustomRole) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, ok := s.roles[r.ID]
	if !ok || cur.OrgID != r.OrgID {
		return accounts.ErrNotFound
	}
	if s.roleNameTaken(r) {
		return accounts.ErrExists
	}
	cur.Name, cur.Description, cur.Permissions, cur.UpdatedAt = r.Name, r.Description, slices.Clone(r.Permissions), r.UpdatedAt
	s.roles[r.ID] = cur
	return nil
}

// DeleteCustomRole implements accounts.Store.
func (s *Store) DeleteCustomRole(_ context.Context, orgID, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r, ok := s.roles[id]; !ok || r.OrgID != orgID {
		return accounts.ErrNotFound
	}
	delete(s.roles, id)
	return nil
}
