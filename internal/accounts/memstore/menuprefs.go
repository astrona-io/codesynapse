package memstore

import (
	"context"
	"fmt"
	"slices"

	"github.com/astrona-io/codesynapse/internal/accounts"
)

// MenuPrefs implements accounts.Store.
func (s *Store) MenuPrefs(_ context.Context, userID string) (accounts.MenuPrefs, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	m := s.menuPrefs[userID]
	return accounts.MenuPrefs{Pinned: append([]string{}, m.Pinned...), Hidden: append([]string{}, m.Hidden...)}, nil
}

// SetMenuPrefs implements accounts.Store.
func (s *Store) SetMenuPrefs(_ context.Context, userID string, m accounts.MenuPrefs) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.users[userID]; !ok {
		return fmt.Errorf("user: %w", accounts.ErrNotFound)
	}
	if s.menuPrefs == nil {
		s.menuPrefs = map[string]accounts.MenuPrefs{}
	}
	s.menuPrefs[userID] = accounts.MenuPrefs{Pinned: slices.Clone(m.Pinned), Hidden: slices.Clone(m.Hidden)}
	return nil
}
