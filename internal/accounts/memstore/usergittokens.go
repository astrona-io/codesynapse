package memstore

import (
	"cmp"
	"context"
	"fmt"
	"slices"

	"github.com/astrona-io/codesynapse/internal/accounts"
)

// PutUserGitToken implements accounts.Store.
func (s *Store) PutUserGitToken(_ context.Context, t accounts.UserGitToken) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.users[t.UserID]; !ok {
		return fmt.Errorf("user: %w", accounts.ErrNotFound)
	}
	k := t.UserID + "|" + t.Host
	if old, ok := s.userTokens[k]; ok {
		t.CreatedAt = old.CreatedAt
	}
	t.Sealed = slices.Clone(t.Sealed)
	s.userTokens[k] = t
	return nil
}

// UserGitToken implements accounts.Store.
func (s *Store) UserGitToken(_ context.Context, userID, host string) (accounts.UserGitToken, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	t, ok := s.userTokens[userID+"|"+host]
	if !ok {
		return accounts.UserGitToken{}, fmt.Errorf("git token: %w", accounts.ErrNotFound)
	}
	t.Sealed = slices.Clone(t.Sealed)
	return t, nil
}

// UserGitTokens implements accounts.Store.
func (s *Store) UserGitTokens(_ context.Context, userID string) ([]accounts.UserGitToken, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := []accounts.UserGitToken{}
	for _, t := range s.userTokens {
		if t.UserID == userID {
			t.Sealed = slices.Clone(t.Sealed)
			out = append(out, t)
		}
	}
	slices.SortFunc(out, func(a, b accounts.UserGitToken) int { return cmp.Compare(a.Host, b.Host) })
	return out, nil
}

// DeleteUserGitToken implements accounts.Store.
func (s *Store) DeleteUserGitToken(_ context.Context, userID, host string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := userID + "|" + host
	if _, ok := s.userTokens[k]; !ok {
		return fmt.Errorf("git token: %w", accounts.ErrNotFound)
	}
	delete(s.userTokens, k)
	return nil
}
