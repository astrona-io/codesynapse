package memstore

import (
	"context"
	"fmt"

	"github.com/astrona-io/codesynapse/internal/accounts"
)

// GatewayEnabled implements accounts.Store.
func (s *Store) GatewayEnabled(_ context.Context, orgID string) (bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if on, chose := s.gateway[orgID]; chose {
		return on, nil
	}
	return true, nil // never chose: on by default
}

// SetGatewayEnabled implements accounts.Store.
func (s *Store) SetGatewayEnabled(_ context.Context, orgID string, enabled bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.orgs[orgID]; !ok {
		return fmt.Errorf("organisation: %w", accounts.ErrNotFound)
	}
	if s.gateway == nil {
		s.gateway = map[string]bool{}
	}
	s.gateway[orgID] = enabled
	return nil
}

// GatewayLocalFirst implements accounts.Store.
func (s *Store) GatewayLocalFirst(_ context.Context, orgID string) (bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if on, chose := s.localFirst[orgID]; chose {
		return on, nil
	}
	return true, nil // never chose: local first by default
}

// SetGatewayLocalFirst implements accounts.Store.
func (s *Store) SetGatewayLocalFirst(_ context.Context, orgID string, on bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.orgs[orgID]; !ok {
		return fmt.Errorf("organisation: %w", accounts.ErrNotFound)
	}
	if s.localFirst == nil {
		s.localFirst = map[string]bool{}
	}
	s.localFirst[orgID] = on
	return nil
}
