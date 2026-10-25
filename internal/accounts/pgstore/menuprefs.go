package pgstore

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/astrona-io/codesynapse/internal/accounts"
)

// MenuPrefs implements accounts.Store.
func (s *Store) MenuPrefs(ctx context.Context, userID string) (accounts.MenuPrefs, error) {
	m := accounts.MenuPrefs{Pinned: []string{}, Hidden: []string{}}
	err := s.db.QueryRow(ctx, "SELECT pinned, hidden FROM menu_prefs WHERE user_id = $1", userID).Scan(&m.Pinned, &m.Hidden)
	if errors.Is(err, pgx.ErrNoRows) {
		return m, nil
	}
	return m, err
}

// SetMenuPrefs implements accounts.Store.
func (s *Store) SetMenuPrefs(ctx context.Context, userID string, m accounts.MenuPrefs) error {
	tag, err := s.db.Exec(ctx, `INSERT INTO menu_prefs (user_id, pinned, hidden) SELECT id, $2, $3 FROM users WHERE id = $1
ON CONFLICT (user_id) DO UPDATE SET pinned = EXCLUDED.pinned, hidden = EXCLUDED.hidden`, userID, m.Pinned, m.Hidden)
	return affected(tag, err, "user")
}
