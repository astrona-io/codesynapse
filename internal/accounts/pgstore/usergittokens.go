package pgstore

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/astrona-io/codesynapse/internal/accounts"
)

const userTokenCols = "user_id, host, preview, key_id, sealed, created_at, updated_at" //nolint:gosec // column names, not a credential

// PutUserGitToken implements accounts.Store.
func (s *Store) PutUserGitToken(ctx context.Context, t accounts.UserGitToken) error {
	_, err := s.db.Exec(ctx, "INSERT INTO user_git_tokens ("+userTokenCols+`) VALUES ($1, $2, $3, $4, $5, $6, $7)
ON CONFLICT (user_id, host) DO UPDATE SET preview = $3, key_id = $4, sealed = $5, updated_at = $7`,
		t.UserID, t.Host, t.Preview, t.KeyID, t.Sealed, t.CreatedAt, t.UpdatedAt)
	return mapErr(err, "git token")
}

func scanUserToken(r pgx.CollectableRow) (accounts.UserGitToken, error) {
	var t accounts.UserGitToken
	err := r.Scan(&t.UserID, &t.Host, &t.Preview, &t.KeyID, &t.Sealed, &t.CreatedAt, &t.UpdatedAt)
	t.CreatedAt, t.UpdatedAt = t.CreatedAt.UTC(), t.UpdatedAt.UTC()
	return t, err
}

// UserGitToken implements accounts.Store.
func (s *Store) UserGitToken(ctx context.Context, userID, host string) (accounts.UserGitToken, error) {
	rows, err := s.db.Query(ctx, "SELECT "+userTokenCols+" FROM user_git_tokens WHERE user_id = $1 AND host = $2", userID, host)
	if err != nil {
		return accounts.UserGitToken{}, err
	}
	t, err := pgx.CollectExactlyOneRow(rows, scanUserToken)
	if errors.Is(err, pgx.ErrNoRows) {
		return accounts.UserGitToken{}, fmt.Errorf("git token: %w", accounts.ErrNotFound)
	}
	return t, err
}

// UserGitTokens implements accounts.Store.
func (s *Store) UserGitTokens(ctx context.Context, userID string) ([]accounts.UserGitToken, error) {
	rows, err := s.db.Query(ctx, "SELECT "+userTokenCols+" FROM user_git_tokens WHERE user_id = $1 ORDER BY host", userID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, scanUserToken)
}

// DeleteUserGitToken implements accounts.Store.
func (s *Store) DeleteUserGitToken(ctx context.Context, userID, host string) error {
	tag, err := s.db.Exec(ctx, "DELETE FROM user_git_tokens WHERE user_id = $1 AND host = $2", userID, host)
	return affected(tag, err, "git token")
}
