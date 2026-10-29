package pgstore

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/astrona-io/codesynapse/internal/accounts"
)

// PromptLogSetting implements accounts.Store.
func (s *Store) PromptLogSetting(ctx context.Context, orgID string) (accounts.PromptLogSetting, error) {
	var set accounts.PromptLogSetting
	err := s.db.QueryRow(ctx, "SELECT enabled, retention_days FROM org_prompt_log WHERE org_id = $1", orgID).
		Scan(&set.Enabled, &set.RetentionDays)
	if errors.Is(err, pgx.ErrNoRows) {
		return accounts.DefaultPromptLogSetting(), nil
	}
	return set, err
}

// SetPromptLogSetting implements accounts.Store.
func (s *Store) SetPromptLogSetting(ctx context.Context, orgID string, set accounts.PromptLogSetting) error {
	tag, err := s.db.Exec(ctx, `INSERT INTO org_prompt_log (org_id, enabled, retention_days) SELECT id, $2, $3 FROM orgs WHERE id = $1
ON CONFLICT (org_id) DO UPDATE SET enabled = EXCLUDED.enabled, retention_days = EXCLUDED.retention_days, updated_at = now()`,
		orgID, set.Enabled, set.RetentionDays)
	return affected(tag, err, "organisation")
}

const promptLogCols = `id, org_id, at, audit_id, user_id, key_id, session_id, path, model, route, status, duration_ms,
  shortened, fallthrough, prompt_sha256, prompt_bytes, forwarded_sha256, forwarded_bytes, prompt, forwarded, answer`

// AddPromptLog implements accounts.Store.
func (s *Store) AddPromptLog(ctx context.Context, e accounts.PromptLogEntry) (accounts.PromptLogEntry, error) {
	err := s.db.QueryRow(ctx, `INSERT INTO gateway_prompt_log (org_id, at, audit_id, user_id, key_id, session_id, path, model, route, status,
  duration_ms, shortened, fallthrough, prompt_sha256, prompt_bytes, forwarded_sha256, forwarded_bytes, prompt, forwarded, answer)
SELECT id, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20 FROM orgs WHERE id = $1 RETURNING id`,
		e.OrgID, e.At, e.AuditID, e.UserID, e.KeyID, e.SessionID, e.Path, e.Model, e.Route, e.Status,
		e.DurationMS, e.Shortened, e.Fallthrough, e.PromptSHA256, e.PromptBytes, e.ForwardedSHA256, e.ForwardedBytes, e.Prompt, e.Forwarded, e.Answer).Scan(&e.ID)
	if errors.Is(err, pgx.ErrNoRows) {
		return e, fmt.Errorf("organisation: %w", accounts.ErrNotFound)
	}
	return e, err
}

// ListPromptLog implements accounts.Store.
func (s *Store) ListPromptLog(ctx context.Context, f accounts.PromptLogFilter) ([]accounts.PromptLogEntry, error) {
	if f.OrgID == "" {
		return nil, fmt.Errorf("org: %w", accounts.ErrInvalidInput)
	}
	limit := f.Limit
	if limit <= 0 || limit > accounts.MaxPromptLogPage {
		limit = accounts.MaxPromptLogPage
	}
	var since, until *time.Time
	if !f.Since.IsZero() {
		since = &f.Since
	}
	if !f.Until.IsZero() {
		until = &f.Until
	}
	var shortened *bool
	if f.Shortened != nil {
		shortened = f.Shortened
	}
	order := "DESC"
	if f.Ascending {
		order = "ASC"
	}
	q := "SELECT " + promptLogCols + ` FROM gateway_prompt_log
WHERE org_id = $1
  AND ($2::timestamptz IS NULL OR at >= $2) AND ($3::timestamptz IS NULL OR at < $3)
  AND ($4 = '' OR user_id = $4) AND ($5 = '' OR session_id = $5) AND ($6 = '' OR model = $6)
  AND ($7 = '' OR route = $7) AND ($8 = '' OR fallthrough = $8) AND ($9::boolean IS NULL OR shortened = $9)
  AND ($10 = 0 OR id < $10) AND id > $11
ORDER BY id ` + order + " LIMIT $12"
	rows, err := s.db.Query(ctx, q, f.OrgID, since, until, f.UserID, f.SessionID, f.Model, f.Route, f.Fallthru, shortened, f.BeforeID, f.AfterID, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (accounts.PromptLogEntry, error) {
		var e accounts.PromptLogEntry
		err := r.Scan(&e.ID, &e.OrgID, &e.At, &e.AuditID, &e.UserID, &e.KeyID, &e.SessionID, &e.Path, &e.Model, &e.Route, &e.Status, &e.DurationMS,
			&e.Shortened, &e.Fallthrough, &e.PromptSHA256, &e.PromptBytes, &e.ForwardedSHA256, &e.ForwardedBytes, &e.Prompt, &e.Forwarded, &e.Answer)
		e.At = e.At.UTC()
		return e, err
	})
}

// PromptLogOrgs implements accounts.Store.
func (s *Store) PromptLogOrgs(ctx context.Context) (map[string]int, error) {
	rows, err := s.db.Query(ctx, `SELECT l.org_id, coalesce(max(o.retention_days), $1)
FROM (SELECT DISTINCT org_id FROM gateway_prompt_log) l LEFT JOIN org_prompt_log o ON o.org_id = l.org_id GROUP BY l.org_id`, accounts.DefaultPromptLogDays)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var org string
		var days int
		if err := rows.Scan(&org, &days); err != nil {
			return nil, err
		}
		out[org] = days
	}
	return out, rows.Err()
}

// PrunePromptLog implements accounts.Store.
func (s *Store) PrunePromptLog(ctx context.Context, orgID string, before time.Time) (int, error) {
	tag, err := s.db.Exec(ctx, "DELETE FROM gateway_prompt_log WHERE org_id = $1 AND at < $2", orgID, before)
	return int(tag.RowsAffected()), err
}

// SealPromptLogSegment implements accounts.Store.
func (s *Store) SealPromptLogSegment(ctx context.Context, seg accounts.PromptLogSegment) (accounts.PromptLogSegment, error) {
	err := pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM (SELECT id FROM gateway_prompt_log WHERE org_id = $1 AND id BETWEEN $2 AND $3 FOR UPDATE) r`,
			seg.OrgID, seg.FirstID, seg.LastID).Scan(&n); err != nil {
			return err
		}
		if n == 0 || n != seg.Count {
			return accounts.ErrAuditConflict
		}
		if err := tx.QueryRow(ctx, `INSERT INTO gateway_prompt_log_segments (org_id, first_id, last_id, first_at, last_at, count, sha256, bytes, backend, key, sealed_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11) RETURNING id`,
			seg.OrgID, seg.FirstID, seg.LastID, seg.FirstAt, seg.LastAt, seg.Count, seg.SHA256, seg.Bytes, seg.Backend, seg.Key, seg.SealedAt).Scan(&seg.ID); err != nil {
			return mapErr(err, "prompt log segment")
		}
		_, err := tx.Exec(ctx, "DELETE FROM gateway_prompt_log WHERE org_id = $1 AND id BETWEEN $2 AND $3", seg.OrgID, seg.FirstID, seg.LastID)
		return err
	})
	seg.FirstAt, seg.LastAt, seg.SealedAt = seg.FirstAt.UTC(), seg.LastAt.UTC(), seg.SealedAt.UTC()
	return seg, err
}

// PromptLogSegments implements accounts.Store.
func (s *Store) PromptLogSegments(ctx context.Context, before time.Time) ([]accounts.PromptLogSegment, error) {
	rows, err := s.db.Query(ctx, `SELECT id, org_id, first_id, last_id, first_at, last_at, count, sha256, bytes, backend, key, sealed_at
FROM gateway_prompt_log_segments WHERE last_at < $1 ORDER BY last_at, id LIMIT $2`, before, accounts.MaxPromptLogSegments)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (accounts.PromptLogSegment, error) {
		var g accounts.PromptLogSegment
		err := r.Scan(&g.ID, &g.OrgID, &g.FirstID, &g.LastID, &g.FirstAt, &g.LastAt, &g.Count, &g.SHA256, &g.Bytes, &g.Backend, &g.Key, &g.SealedAt)
		g.FirstAt, g.LastAt, g.SealedAt = g.FirstAt.UTC(), g.LastAt.UTC(), g.SealedAt.UTC()
		return g, err
	})
}

// DropPromptLogSegment implements accounts.Store.
func (s *Store) DropPromptLogSegment(ctx context.Context, id int64) error {
	tag, err := s.db.Exec(ctx, "DELETE FROM gateway_prompt_log_segments WHERE id = $1", id)
	return affected(tag, err, "prompt log segment")
}
