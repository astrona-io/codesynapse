package pgstore

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/astrona-io/codesynapse/internal/accounts"
)

const inviteCols = "id, org_id, email, role, token_hash, invited_by, created_at, expires_at"

// PutInvitation implements accounts.Store.
func (s *Store) PutInvitation(ctx context.Context, inv accounts.Invitation) error {
	_, err := s.db.Exec(ctx, "INSERT INTO org_invitations ("+inviteCols+") VALUES ($1, $2, $3, $4, $5, $6, $7, $8)",
		inv.ID, inv.OrgID, inv.Email, inv.Role, inv.TokenHash, inv.InvitedBy, inv.CreatedAt, inv.ExpiresAt)
	return mapErr(err, "invitation")
}

func scanInvitation(r pgx.CollectableRow) (accounts.Invitation, error) {
	var inv accounts.Invitation
	err := r.Scan(&inv.ID, &inv.OrgID, &inv.Email, &inv.Role, &inv.TokenHash, &inv.InvitedBy, &inv.CreatedAt, &inv.ExpiresAt)
	inv.CreatedAt, inv.ExpiresAt = inv.CreatedAt.UTC(), inv.ExpiresAt.UTC()
	return inv, err
}

// InvitationByTokenHash implements accounts.Store.
func (s *Store) InvitationByTokenHash(ctx context.Context, tokenHash string) (accounts.Invitation, error) {
	rows, err := s.db.Query(ctx, "SELECT "+inviteCols+" FROM org_invitations WHERE token_hash = $1", tokenHash)
	if err != nil {
		return accounts.Invitation{}, err
	}
	inv, err := pgx.CollectExactlyOneRow(rows, scanInvitation)
	if errors.Is(err, pgx.ErrNoRows) {
		return accounts.Invitation{}, fmt.Errorf("invitation: %w", accounts.ErrNotFound)
	}
	return inv, err
}

// OrgInvitations implements accounts.Store.
func (s *Store) OrgInvitations(ctx context.Context, orgID string) ([]accounts.Invitation, error) {
	rows, err := s.db.Query(ctx, "SELECT "+inviteCols+" FROM org_invitations WHERE org_id = $1 ORDER BY created_at, id", orgID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, scanInvitation)
}

// DeleteInvitation implements accounts.Store.
func (s *Store) DeleteInvitation(ctx context.Context, orgID, id string) error {
	tag, err := s.db.Exec(ctx, "DELETE FROM org_invitations WHERE org_id = $1 AND id = $2", orgID, id)
	return affected(tag, err, "invitation")
}
