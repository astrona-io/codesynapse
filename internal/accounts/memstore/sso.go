package memstore

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/astrona-io/codesynapse/internal/accounts"
)

// PutIdentityProvider implements accounts.Store.
func (s *Store) PutIdentityProvider(_ context.Context, p accounts.IdentityProvider) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.orgs[p.OrgID]; !ok {
		return fmt.Errorf("organisation: %w", accounts.ErrNotFound)
	}
	p.RoleGroups, p.TeamGroups, p.Scopes = maps.Clone(p.RoleGroups), maps.Clone(p.TeamGroups), slices.Clone(p.Scopes)
	s.idps[p.OrgID] = p
	return nil
}

// IdentityProvider implements accounts.Store.
func (s *Store) IdentityProvider(_ context.Context, orgID string) (accounts.IdentityProvider, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	p, ok := s.idps[orgID]
	if !ok {
		return accounts.IdentityProvider{}, fmt.Errorf("identity provider: %w", accounts.ErrNotFound)
	}
	p.RoleGroups, p.TeamGroups, p.Scopes = maps.Clone(p.RoleGroups), maps.Clone(p.TeamGroups), slices.Clone(p.Scopes)
	return p, nil
}

// DeleteIdentityProvider implements accounts.Store.
func (s *Store) DeleteIdentityProvider(_ context.Context, orgID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.idps[orgID]; !ok {
		return fmt.Errorf("identity provider: %w", accounts.ErrNotFound)
	}
	delete(s.idps, orgID)
	return nil
}

// IdentityBySubject implements accounts.Store.
func (s *Store) IdentityBySubject(_ context.Context, issuer, subject string) (accounts.ExternalIdentity, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, e := range s.identities {
		if e.Issuer == issuer && e.Subject == subject {
			return e, nil
		}
	}
	return accounts.ExternalIdentity{}, fmt.Errorf("identity: %w", accounts.ErrNotFound)
}

// PutIdentity implements accounts.Store.
func (s *Store) PutIdentity(_ context.Context, e accounts.ExternalIdentity) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, x := range s.identities {
		if id != e.ID && x.Issuer == e.Issuer && x.Subject == e.Subject {
			return fmt.Errorf("identity: %w", accounts.ErrExists)
		}
	}
	if _, ok := s.users[e.UserID]; !ok {
		return fmt.Errorf("user: %w", accounts.ErrNotFound)
	}
	s.identities[e.ID] = e
	return nil
}

// UserIdentities implements accounts.Store.
func (s *Store) UserIdentities(_ context.Context, userID string) ([]accounts.ExternalIdentity, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := []accounts.ExternalIdentity{}
	for _, e := range s.identities {
		if e.UserID == userID {
			out = append(out, e)
		}
	}
	slices.SortFunc(out, func(a, b accounts.ExternalIdentity) int { return strings.Compare(a.ID, b.ID) })
	return out, nil
}

// DeleteIdentity implements accounts.Store.
func (s *Store) DeleteIdentity(_ context.Context, userID, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e, ok := s.identities[id]; !ok || e.UserID != userID {
		return fmt.Errorf("identity: %w", accounts.ErrNotFound)
	}
	delete(s.identities, id)
	return nil
}

// PutSSOState implements accounts.Store.
func (s *Store) PutSSOState(_ context.Context, st accounts.SSOState) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ssoStates[st.StateHash] = st
	return nil
}

// TakeSSOState implements accounts.Store.
func (s *Store) TakeSSOState(_ context.Context, stateHash string) (accounts.SSOState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.ssoStates[stateHash]
	if !ok {
		return accounts.SSOState{}, fmt.Errorf("sso state: %w", accounts.ErrNotFound)
	}
	delete(s.ssoStates, stateHash)
	return st, nil
}

// PruneSSOStates implements accounts.Store.
func (s *Store) PruneSSOStates(_ context.Context, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for h, st := range s.ssoStates {
		if !now.Before(st.ExpiresAt) {
			delete(s.ssoStates, h)
		}
	}
	return nil
}

// ManagedByIDP implements accounts.Store.
func (s *Store) ManagedByIDP(_ context.Context, orgID, userID string) (bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.managed[orgID+"|"+userID], nil
}

// SetManagedByIDP implements accounts.Store.
func (s *Store) SetManagedByIDP(_ context.Context, orgID, userID string, managed bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.members[orgID][userID]; !ok && managed {
		return fmt.Errorf("membership: %w", accounts.ErrNotFound)
	}
	if managed {
		s.managed[orgID+"|"+userID] = true
	} else {
		delete(s.managed, orgID+"|"+userID)
	}
	return nil
}
