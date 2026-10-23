package memstore

import (
	"context"
	"errors"
	"fmt"

	"github.com/astrona-io/codesynapse/internal/accounts"
)

// MarkBootstrapRoot implements accounts.Store.
func (s *Store) MarkBootstrapRoot(_ context.Context, userID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.users[userID]
	if !ok {
		return fmt.Errorf("user: %w", accounts.ErrNotFound)
	}
	for id, x := range s.users {
		if x.BootstrapRoot && id != userID {
			return fmt.Errorf("bootstrap root: %w", accounts.ErrExists)
		}
	}
	u.BootstrapRoot = true
	s.users[userID] = u
	return nil
}

// Setup implements accounts.Store.
func (s *Store) Setup(context.Context) (accounts.Setup, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.setup, nil
}

// SaveSetup implements accounts.Store; like PostgreSQL it refuses to change
// a completed setup.
func (s *Store) SaveSetup(_ context.Context, st accounts.Setup) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.setup.State == accounts.SetupComplete {
		return errors.New("a completed setup cannot be changed")
	}
	s.setup = st
	return nil
}
