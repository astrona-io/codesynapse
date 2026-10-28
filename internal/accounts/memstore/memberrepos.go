package memstore

import (
	"context"
	"fmt"

	"github.com/astrona-io/codesynapse/internal/accounts"
)

// MemberReposEnabled implements accounts.Store.
func (s *Store) MemberReposEnabled(_ context.Context, orgID string) (bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if on, chose := s.memberRepos[orgID]; chose {
		return on, nil
	}
	return true, nil // never chose: allowed by default
}

// SetMemberReposEnabled implements accounts.Store.
func (s *Store) SetMemberReposEnabled(_ context.Context, orgID string, enabled bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.orgs[orgID]; !ok {
		return fmt.Errorf("organisation: %w", accounts.ErrNotFound)
	}
	if s.memberRepos == nil {
		s.memberRepos = map[string]bool{}
	}
	s.memberRepos[orgID] = enabled
	return nil
}
