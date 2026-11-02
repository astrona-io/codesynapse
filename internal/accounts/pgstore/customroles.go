package pgstore

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/astrona-io/codesynapse/internal/accounts"
)

const roleCols = "id, org_id, name, description, resource, permissions, created_by, created_at, updated_at"

func toStrings(ps []accounts.Permission) []string {
	if ps == nil {
		return nil
	}
	out := make([]string, len(ps))
	for i, p := range ps {
		out[i] = string(p)
	}
	return out
}

func toPermissions(ss []string) []accounts.Permission {
	if ss == nil {
		return nil
	}
	out := make([]accounts.Permission, len(ss))
	for i, s := range ss {
		out[i] = accounts.Permission(s)
	}
	return out
}

// CustomRoles implements accounts.Store.
func (s *Store) CustomRoles(ctx context.Context, orgIDs []string) ([]accounts.CustomRole, error) {
	if len(orgIDs) > accounts.MaxMemberships {
		return nil, accounts.ErrAccessLimit
	}
	rows, err := s.db.Query(ctx, "SELECT "+roleCols+" FROM custom_roles WHERE org_id = ANY($1) ORDER BY org_id, lower(name)", orgIDs)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (accounts.CustomRole, error) {
		var c accounts.CustomRole
		var perms []string
		err := r.Scan(&c.ID, &c.OrgID, &c.Name, &c.Description, &c.Resource, &perms, &c.CreatedBy, &c.CreatedAt, &c.UpdatedAt)
		c.Permissions = toPermissions(perms)
		c.CreatedAt, c.UpdatedAt = c.CreatedAt.UTC(), c.UpdatedAt.UTC()
		return c, err
	})
	if out == nil {
		out = []accounts.CustomRole{}
	}
	return out, err
}

// CreateCustomRole implements accounts.Store.
func (s *Store) CreateCustomRole(ctx context.Context, r accounts.CustomRole) error {
	_, err := s.db.Exec(ctx, "INSERT INTO custom_roles ("+roleCols+") VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)",
		r.ID, r.OrgID, r.Name, r.Description, r.Resource, toStrings(r.Permissions), r.CreatedBy, r.CreatedAt, r.UpdatedAt)
	return mapErr(err, "role")
}

// UpdateCustomRole implements accounts.Store.
func (s *Store) UpdateCustomRole(ctx context.Context, r accounts.CustomRole) error {
	tag, err := s.db.Exec(ctx, "UPDATE custom_roles SET name = $3, description = $4, permissions = $5, updated_at = $6 WHERE id = $1 AND org_id = $2",
		r.ID, r.OrgID, r.Name, r.Description, toStrings(r.Permissions), r.UpdatedAt)
	if err != nil {
		return mapErr(err, "role")
	}
	return affected(tag, nil, "role")
}

// DeleteCustomRole implements accounts.Store.
func (s *Store) DeleteCustomRole(ctx context.Context, orgID, id string) error {
	tag, err := s.db.Exec(ctx, "DELETE FROM custom_roles WHERE id = $1 AND org_id = $2", id, orgID)
	return affected(tag, err, "role")
}
