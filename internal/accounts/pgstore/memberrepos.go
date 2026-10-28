package pgstore

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
)

// MemberReposEnabled implements accounts.Store.
func (s *Store) MemberReposEnabled(ctx context.Context, orgID string) (bool, error) {
	var on bool
	err := s.db.QueryRow(ctx, "SELECT enabled FROM org_member_repos WHERE org_id = $1", orgID).Scan(&on)
	if errors.Is(err, pgx.ErrNoRows) {
		return true, nil // never chose: allowed by default
	}
	return on, err
}

// SetMemberReposEnabled implements accounts.Store.
func (s *Store) SetMemberReposEnabled(ctx context.Context, orgID string, enabled bool) error {
	tag, err := s.db.Exec(ctx, `INSERT INTO org_member_repos (org_id, enabled) SELECT id, $2 FROM orgs WHERE id = $1
ON CONFLICT (org_id) DO UPDATE SET enabled = EXCLUDED.enabled, updated_at = now()`, orgID, enabled)
	return affected(tag, err, "organisation")
}
