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

// ReplaceRoot implements accounts.Store.
func (s *Store) ReplaceRoot(_ context.Context, oldID string, u accounts.User) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	old, ok := s.users[oldID]
	if !ok || !old.BootstrapRoot {
		return fmt.Errorf("bootstrap root: %w", accounts.ErrNotFound)
	}
	for id, x := range s.users {
		if id == oldID {
			continue
		}
		if x.Email == u.Email {
			return fmt.Errorf("email: %w", accounts.ErrExists)
		}
		if u.Username != "" && x.Username == u.Username {
			return fmt.Errorf("username: %w", accounts.ErrExists)
		}
	}
	delete(s.users, oldID)
	for h, sess := range s.sessions {
		if sess.UserID == oldID {
			delete(s.sessions, h)
		}
	}
	for id, k := range s.keys {
		if k.UserID == oldID {
			delete(s.keys, id)
		}
	}
	for k, t := range s.userTokens {
		if t.UserID == oldID {
			delete(s.userTokens, k)
		}
	}
	u.BootstrapRoot = true
	s.users[u.ID] = u
	if s.setup.RootUserID == oldID {
		s.setup.RootUserID = u.ID
	}
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
