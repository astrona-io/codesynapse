package pgstore

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// NotificationsSeen implements accounts.Store.
func (s *Store) NotificationsSeen(ctx context.Context, userID string) (time.Time, error) {
	var at time.Time
	err := s.db.QueryRow(ctx, "SELECT seen_at FROM notifications_seen WHERE user_id = $1", userID).Scan(&at)
	if errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, nil
	}
	return at.UTC(), err
}

// SetNotificationsSeen implements accounts.Store.
func (s *Store) SetNotificationsSeen(ctx context.Context, userID string, at time.Time) error {
	_, err := s.db.Exec(ctx, `INSERT INTO notifications_seen (user_id, seen_at) VALUES ($1, $2)
ON CONFLICT (user_id) DO UPDATE SET seen_at = GREATEST(notifications_seen.seen_at, EXCLUDED.seen_at)`, userID, at.UTC())
	return mapErr(err, "user")
}
