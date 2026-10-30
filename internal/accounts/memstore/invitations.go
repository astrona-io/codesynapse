package memstore

import (
	"cmp"
	"context"
	"fmt"
	"slices"

	"github.com/astrona-io/codesynapse/internal/accounts"
)

// PutInvitation implements accounts.Store.
func (s *Store) PutInvitation(_ context.Context, inv accounts.Invitation) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.orgs[inv.OrgID]; !ok {
		return fmt.Errorf("organisation: %w", accounts.ErrNotFound)
	}
	if _, ok := s.users[inv.InvitedBy]; !ok {
		return fmt.Errorf("user: %w", accounts.ErrNotFound)
	}
	for _, x := range s.invites {
		if x.ID == inv.ID || x.TokenHash == inv.TokenHash {
			return fmt.Errorf("invitation: %w", accounts.ErrExists)
		}
	}
	s.invites[inv.ID] = inv
	return nil
}

// InvitationByTokenHash implements accounts.Store.
func (s *Store) InvitationByTokenHash(_ context.Context, tokenHash string) (accounts.Invitation, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, inv := range s.invites {
		if inv.TokenHash == tokenHash {
			return inv, nil
		}
	}
	return accounts.Invitation{}, fmt.Errorf("invitation: %w", accounts.ErrNotFound)
}

// OrgInvitations implements accounts.Store.
func (s *Store) OrgInvitations(_ context.Context, orgID string) ([]accounts.Invitation, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := []accounts.Invitation{}
	for _, inv := range s.invites {
		if inv.OrgID == orgID {
			out = append(out, inv)
		}
	}
	slices.SortFunc(out, func(a, b accounts.Invitation) int {
		return cmp.Or(a.CreatedAt.Compare(b.CreatedAt), cmp.Compare(a.ID, b.ID))
	})
	return out, nil
}

// DeleteInvitation implements accounts.Store.
func (s *Store) DeleteInvitation(_ context.Context, orgID, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if inv, ok := s.invites[id]; !ok || inv.OrgID != orgID {
		return fmt.Errorf("invitation: %w", accounts.ErrNotFound)
	}
	delete(s.invites, id)
	return nil
}
