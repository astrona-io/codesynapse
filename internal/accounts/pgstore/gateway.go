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

// GatewayLocalExclude implements accounts.Store.
func (s *Store) GatewayLocalExclude(ctx context.Context, orgID string) ([]string, error) {
	var list []string
	err := s.db.QueryRow(ctx, "SELECT local_exclude FROM org_gateway WHERE org_id = $1", orgID).Scan(&list)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && list == nil) {
		return []string{}, nil // never chose: nothing excluded
	}
	return list, err
}

// SetGatewayLocalExclude implements accounts.Store. A new row takes the
// column defaults, so the gateway and local first stay on.
func (s *Store) SetGatewayLocalExclude(ctx context.Context, orgID string, patterns []string) error {
	if patterns == nil {
		patterns = []string{}
	}
	tag, err := s.db.Exec(ctx, `INSERT INTO org_gateway (org_id, local_exclude) SELECT id, $2 FROM orgs WHERE id = $1
ON CONFLICT (org_id) DO UPDATE SET local_exclude = EXCLUDED.local_exclude, updated_at = now()`, orgID, patterns)
	return affected(tag, err, "organisation")
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
		return true, nil // never chose: on, like the column default
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

// GatewayLocalThinking implements accounts.Store.
func (s *Store) GatewayLocalThinking(ctx context.Context, orgID string) (bool, error) {
	var on bool
	err := s.db.QueryRow(ctx, "SELECT local_thinking FROM org_gateway WHERE org_id = $1", orgID).Scan(&on)
	if errors.Is(err, pgx.ErrNoRows) {
		return true, nil // never chose: on, like the column default
	}
	return on, err
}

// SetGatewayLocalThinking implements accounts.Store. A new row takes the
// column defaults, so the gateway itself stays on.
func (s *Store) SetGatewayLocalThinking(ctx context.Context, orgID string, on bool) error {
	tag, err := s.db.Exec(ctx, `INSERT INTO org_gateway (org_id, local_thinking) SELECT id, $2 FROM orgs WHERE id = $1
ON CONFLICT (org_id) DO UPDATE SET local_thinking = EXCLUDED.local_thinking, updated_at = now()`, orgID, on)
	return affected(tag, err, "organisation")
}
