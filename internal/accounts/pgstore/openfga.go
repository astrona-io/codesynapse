package pgstore

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/astrona-io/codesynapse/internal/accounts"
)

var _ accounts.OpenFGASyncStore = (*Store)(nil)

// OpenFGAPending implements accounts.OpenFGASyncStore.
func (s *Store) OpenFGAPending(ctx context.Context, storeID, modelID string, limit int) ([]accounts.Org, error) {
	rows, err := s.db.Query(ctx, `SELECT o.id,o.slug,o.name,o.created_at,o.default_repo_access,o.policy_revision
FROM orgs o LEFT JOIN openfga_sync f ON f.org_id = o.id
WHERE f.org_id IS NULL OR f.store_id <> $1 OR f.model_id <> $2 OR f.revision < o.policy_revision OR f.rebuild_requested_at IS NOT NULL
ORDER BY o.id LIMIT $3`, storeID, modelID, min(max(limit, 1), accounts.MaxTuples))
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (accounts.Org, error) { return scanOrg(r) })
}

// OpenFGASynced implements accounts.OpenFGASyncStore.
func (s *Store) OpenFGASynced(ctx context.Context, orgID string) (accounts.OpenFGASync, error) {
	out := accounts.OpenFGASync{OrgID: orgID}
	var synced *time.Time
	err := s.db.QueryRow(ctx, "SELECT store_id, model_id, revision, synced_at, rebuild_requested_at FROM openfga_sync WHERE org_id = $1 AND synced_at IS NOT NULL", orgID).
		Scan(&out.StoreID, &out.ModelID, &out.Revision, &synced, &out.RebuildRequested)
	if err != nil {
		return out, mapErr(err, "openfga sync")
	}
	out.SyncedAt = synced.UTC()
	if out.RebuildRequested != nil {
		t := out.RebuildRequested.UTC()
		out.RebuildRequested = &t
	}
	return out, nil
}

// RecordOpenFGASync implements accounts.OpenFGASyncStore.
func (s *Store) RecordOpenFGASync(ctx context.Context, r accounts.OpenFGASync, started time.Time) error {
	tag, err := s.db.Exec(ctx, `INSERT INTO openfga_sync (org_id, store_id, model_id, revision, synced_at) VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (org_id) DO UPDATE SET
  revision = CASE WHEN openfga_sync.store_id = EXCLUDED.store_id AND openfga_sync.model_id = EXCLUDED.model_id
    THEN GREATEST(openfga_sync.revision, EXCLUDED.revision) ELSE EXCLUDED.revision END,
  store_id = EXCLUDED.store_id,
  model_id = EXCLUDED.model_id,
  synced_at = EXCLUDED.synced_at,
  rebuild_requested_at = CASE WHEN openfga_sync.rebuild_requested_at <= $6 THEN NULL ELSE openfga_sync.rebuild_requested_at END`,
		r.OrgID, r.StoreID, r.ModelID, r.Revision, r.SyncedAt, started)
	if err != nil {
		return mapErr(err, "openfga sync")
	}
	if tag.RowsAffected() == 0 {
		return accounts.ErrNotFound
	}
	return nil
}

// RequestOpenFGARebuild implements accounts.OpenFGASyncStore.
func (s *Store) RequestOpenFGARebuild(ctx context.Context, orgID string, at time.Time) (int, error) {
	tag, err := s.db.Exec(ctx, `INSERT INTO openfga_sync (org_id, rebuild_requested_at)
SELECT id, $2 FROM orgs WHERE $1 = '' OR id = $1
ON CONFLICT (org_id) DO UPDATE SET rebuild_requested_at = EXCLUDED.rebuild_requested_at`, orgID, at)
	if err != nil {
		return 0, err
	}
	if orgID != "" && tag.RowsAffected() == 0 {
		return 0, accounts.ErrNotFound
	}
	return int(tag.RowsAffected()), nil
}
