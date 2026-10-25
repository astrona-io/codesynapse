package pgstore

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/astrona-io/codesynapse/internal/accounts"
)

const watchCols = "org_id, key, actor_id, execution, provider, host, login, include_archived, include_forks, paused, last_scan, last_status, last_error, next_scan, added, known"

func scanWatch(row pgx.Row) (accounts.OrgWatch, error) {
	var w accounts.OrgWatch
	var key string
	var exec, known []byte
	var lastScan *time.Time
	if err := row.Scan(&w.OrgID, &key, &w.ActorID, &exec, &w.Provider, &w.Host, &w.Login, &w.IncludeArchived, &w.IncludeForks, &w.Paused,
		&lastScan, &w.LastStatus, &w.LastError, &w.NextScan, &w.Added, &known); err != nil {
		return accounts.OrgWatch{}, err
	}
	if err := json.Unmarshal(exec, &w.Execution); err != nil {
		return accounts.OrgWatch{}, err
	}
	if err := json.Unmarshal(known, &w.Known); err != nil {
		return accounts.OrgWatch{}, err
	}
	if lastScan != nil {
		w.LastScan = lastScan.UTC()
	}
	w.NextScan = w.NextScan.UTC()
	return w, nil
}

func collectWatches(rows pgx.Rows) ([]accounts.OrgWatch, error) {
	defer rows.Close()
	out := []accounts.OrgWatch{}
	for rows.Next() {
		w, err := scanWatch(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// OrgWatches implements accounts.Store.
func (s *Store) OrgWatches(ctx context.Context, orgID string) ([]accounts.OrgWatch, error) {
	rows, err := s.db.Query(ctx, "SELECT "+watchCols+" FROM org_watches WHERE org_id = $1 ORDER BY key LIMIT $2", orgID, accounts.MaxWatchesPerOrg)
	if err != nil {
		return nil, err
	}
	return collectWatches(rows)
}

// PutOrgWatch implements accounts.Store. An existing watch keeps its scan
// state; only its settings and identity change.
func (s *Store) PutOrgWatch(ctx context.Context, w accounts.OrgWatch) error {
	exec, err := json.Marshal(w.Execution)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(ctx, `INSERT INTO org_watches (org_id, key, actor_id, execution, provider, host, login, include_archived, include_forks, paused, next_scan, known)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, '[]')
ON CONFLICT (org_id, key) DO UPDATE SET actor_id = $3, execution = $4, provider = $5, include_archived = $8, include_forks = $9, paused = $10`,
		w.OrgID, w.Key(), w.ActorID, exec, w.Provider, w.Host, w.Login, w.IncludeArchived, w.IncludeForks, w.Paused, w.NextScan)
	return mapErr(err, "watch")
}

// DeleteOrgWatch implements accounts.Store.
func (s *Store) DeleteOrgWatch(ctx context.Context, orgID, key string) error {
	_, err := s.db.Exec(ctx, "DELETE FROM org_watches WHERE org_id = $1 AND key = $2", orgID, key)
	return err
}

// DueOrgWatches implements accounts.Store.
func (s *Store) DueOrgWatches(ctx context.Context, now time.Time) ([]accounts.OrgWatch, error) {
	rows, err := s.db.Query(ctx, "SELECT "+watchCols+" FROM org_watches WHERE NOT paused AND next_scan <= $1 ORDER BY next_scan, org_id, key LIMIT $2", now, accounts.MaxWatches)
	if err != nil {
		return nil, err
	}
	return collectWatches(rows)
}

// RecordWatchScan implements accounts.Store. A removed watch is no error.
func (s *Store) RecordWatchScan(ctx context.Context, orgID, key string, scan accounts.WatchScan) error {
	var known []byte
	if scan.Known != nil {
		var err error
		if known, err = json.Marshal(scan.Known); err != nil {
			return err
		}
	}
	_, err := s.db.Exec(ctx, `UPDATE org_watches SET last_scan = COALESCE($3, last_scan), last_status = $4, last_error = $5,
  next_scan = COALESCE($6, next_scan), added = added + $7, known = COALESCE($8::jsonb, known)
WHERE org_id = $1 AND key = $2`, orgID, key, nullTime(scan.LastScan), scan.LastStatus, scan.LastError, nullTime(scan.NextScan), scan.Added, known)
	return err
}
