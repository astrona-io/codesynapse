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
	return s.gateway[orgID], nil
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
