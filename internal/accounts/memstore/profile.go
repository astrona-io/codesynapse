package memstore

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/astrona-io/codesynapse/internal/accounts"
)

// UserSessions implements accounts.Store.
func (s *Store) UserSessions(_ context.Context, userID string) ([]accounts.Session, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	now := time.Now()
	var out []accounts.Session
	for _, sess := range s.sessions {
		if sess.UserID == userID && sess.ExpiresAt.After(now) {
			out = append(out, sess)
		}
	}
	slices.SortFunc(out, func(a, b accounts.Session) int {
		return cmp.Or(b.CreatedAt.Compare(a.CreatedAt), cmp.Compare(a.TokenHash, b.TokenHash))
	})
	return out[:min(len(out), 100)], nil
}

// DeleteOtherSessions implements accounts.Store.
func (s *Store) DeleteOtherSessions(_ context.Context, userID, keepHash string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for h, sess := range s.sessions {
		if sess.UserID == userID && h != keepHash {
			delete(s.sessions, h)
		}
	}
	return nil
}

// SetUserName implements accounts.Store.
func (s *Store) SetUserName(_ context.Context, userID, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.users[userID]
	if !ok {
		return fmt.Errorf("user: %w", accounts.ErrNotFound)
	}
	u.Name = name
	s.users[userID] = u
	return nil
}

// SetUserEmail implements accounts.Store.
func (s *Store) SetUserEmail(_ context.Context, userID, email string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.users[userID]
	if !ok {
		return fmt.Errorf("user: %w", accounts.ErrNotFound)
	}
	for id, other := range s.users {
		if id != userID && other.Email == email {
			return fmt.Errorf("user: %w", accounts.ErrExists)
		}
	}
	u.Email = email
	s.users[userID] = u
	return nil
}
