package pgstore

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/astrona-io/codesynapse/internal/accounts"
)

// UserSessions implements accounts.Store.
func (s *Store) UserSessions(ctx context.Context, userID string) ([]accounts.Session, error) {
	rows, err := s.db.Query(ctx, `SELECT token_hash, user_id, created_at, expires_at, user_agent FROM sessions
WHERE user_id = $1 AND expires_at > now() ORDER BY created_at DESC LIMIT 100`, userID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (accounts.Session, error) {
		var sess accounts.Session
		err := r.Scan(&sess.TokenHash, &sess.UserID, &sess.CreatedAt, &sess.ExpiresAt, &sess.UserAgent)
		sess.CreatedAt, sess.ExpiresAt = sess.CreatedAt.UTC(), sess.ExpiresAt.UTC()
		return sess, err
	})
}

// DeleteOtherSessions implements accounts.Store.
func (s *Store) DeleteOtherSessions(ctx context.Context, userID, keepHash string) error {
	_, err := s.db.Exec(ctx, "DELETE FROM sessions WHERE user_id = $1 AND token_hash <> $2", userID, keepHash)
	return err
}

// SetUserName implements accounts.Store.
func (s *Store) SetUserName(ctx context.Context, userID, name string) error {
	tag, err := s.db.Exec(ctx, "UPDATE users SET name = $2 WHERE id = $1", userID, name)
	return affected(tag, err, "user")
}

// SetUserEmail implements accounts.Store.
func (s *Store) SetUserEmail(ctx context.Context, userID, email string) error {
	tag, err := s.db.Exec(ctx, "UPDATE users SET email = $2 WHERE id = $1", userID, email)
	return affected(tag, err, "user")
}
