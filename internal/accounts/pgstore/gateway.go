package pgstore

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
)

// GatewayEnabled implements accounts.Store.
func (s *Store) GatewayEnabled(ctx context.Context, orgID string) (bool, error) {
	var on bool
	err := s.db.QueryRow(ctx, "SELECT enabled FROM org_gateway WHERE org_id = $1", orgID).Scan(&on)
	if errors.Is(err, pgx.ErrNoRows) {
		return true, nil // never chose: on by default
	}
	return on, err
}

// SetGatewayEnabled implements accounts.Store.
func (s *Store) SetGatewayEnabled(ctx context.Context, orgID string, enabled bool) error {
	tag, err := s.db.Exec(ctx, `INSERT INTO org_gateway (org_id, enabled) SELECT id, $2 FROM orgs WHERE id = $1
ON CONFLICT (org_id) DO UPDATE SET enabled = EXCLUDED.enabled, updated_at = now()`, orgID, enabled)
	return affected(tag, err, "organisation")
}

// GatewayLocalFirst implements accounts.Store.
func (s *Store) GatewayLocalFirst(ctx context.Context, orgID string) (bool, error) {
	var on bool
	err := s.db.QueryRow(ctx, "SELECT local_first FROM org_gateway WHERE org_id = $1", orgID).Scan(&on)
	if errors.Is(err, pgx.ErrNoRows) {
		return true, nil // never chose: local first by default
	}
	return on, err
}

// SetGatewayLocalFirst implements accounts.Store. A new row takes the
// column defaults, so the gateway itself stays on.
func (s *Store) SetGatewayLocalFirst(ctx context.Context, orgID string, on bool) error {
	tag, err := s.db.Exec(ctx, `INSERT INTO org_gateway (org_id, local_first) SELECT id, $2 FROM orgs WHERE id = $1
ON CONFLICT (org_id) DO UPDATE SET local_first = EXCLUDED.local_first, updated_at = now()`, orgID, on)
	return affected(tag, err, "organisation")
}

// GatewayShortenPrompts implements accounts.Store.
func (s *Store) GatewayShortenPrompts(ctx context.Context, orgID string) (bool, error) {
	var on bool
	err := s.db.QueryRow(ctx, "SELECT shorten_prompts FROM org_gateway WHERE org_id = $1", orgID).Scan(&on)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil // never chose: off, it alters the person's words
	}
	return on, err
}

// SetGatewayShortenPrompts implements accounts.Store. A new row takes the
// column defaults, so the gateway itself stays on.
func (s *Store) SetGatewayShortenPrompts(ctx context.Context, orgID string, on bool) error {
	tag, err := s.db.Exec(ctx, `INSERT INTO org_gateway (org_id, shorten_prompts) SELECT id, $2 FROM orgs WHERE id = $1
ON CONFLICT (org_id) DO UPDATE SET shorten_prompts = EXCLUDED.shorten_prompts, updated_at = now()`, orgID, on)
	return affected(tag, err, "organisation")
}
