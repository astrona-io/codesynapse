package memstore

import (
	"context"
	"fmt"
	"time"

	"github.com/astrona-io/codesynapse/internal/accounts"
)

// NotificationsSeen implements accounts.Store.
func (s *Store) NotificationsSeen(_ context.Context, userID string) (time.Time, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.seen[userID], nil
}

// SetNotificationsSeen implements accounts.Store.
func (s *Store) SetNotificationsSeen(_ context.Context, userID string, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.users[userID]; !ok {
		return fmt.Errorf("user: %w", accounts.ErrNotFound)
	}
	if s.seen == nil {
		s.seen = map[string]time.Time{}
	}
	if at.UTC().After(s.seen[userID]) {
		s.seen[userID] = at.UTC()
	}
	return nil
}
