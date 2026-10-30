package pgstore

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/astrona-io/codesynapse/internal/accounts"
)

// Audit statistics: fixed SQL over events with the window in $1 and $2,
// served by the (at, action) index of migration 0037. The family is the
// action's prefix before the first dot, or "http" for "http <pattern>";
// the code folds it into accounts.AuditFamilies.

const auditHoursQuery = `SELECT hour, family, outcome, n FROM (
  SELECT date_trunc('hour', at AT TIME ZONE 'UTC') AS hour,
    CASE WHEN action LIKE 'http %' THEN 'http' ELSE left(split_part(action, '.', 1), 40) END AS family, outcome, count(*) AS n
  FROM events WHERE at >= $1 AND at < $2
  GROUP BY 1, 2, 3) h
ORDER BY hour, family COLLATE "C", outcome COLLATE "C" LIMIT $3`

const auditLoginsQuery = `SELECT reason, n FROM (
  SELECT left(detail, 40) AS reason, count(*) AS n FROM events
  WHERE action = 'user.login' AND outcome = 'failure' AND at >= $1 AND at < $2
  GROUP BY 1) l
ORDER BY reason COLLATE "C" LIMIT 100`

const auditSecurityQuery = `SELECT action, count(*) FROM events
WHERE action = ANY($3) AND at >= $1 AND at < $2
GROUP BY action ORDER BY action COLLATE "C"`

// AuditStats implements accounts.Store.
func (s *Store) AuditStats(ctx context.Context, since, until time.Time) (accounts.AuditCounts, error) {
	out := accounts.AuditCounts{}
	rows, err := s.db.Query(ctx, auditHoursQuery, since, until, accounts.MaxAuditCountRows)
	if err != nil {
		return out, err
	}
	out.Hours, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (accounts.AuditHourCount, error) {
		var h accounts.AuditHourCount
		err := r.Scan(&h.Hour, &h.Family, &h.Outcome, &h.N)
		h.Hour = time.Date(h.Hour.Year(), h.Hour.Month(), h.Hour.Day(), h.Hour.Hour(), 0, 0, 0, time.UTC)
		return h, err
	})
	if err != nil {
		return out, err
	}
	collect := func(r pgx.CollectableRow) (accounts.AuditNamedCount, error) {
		var c accounts.AuditNamedCount
		err := r.Scan(&c.Name, &c.N)
		return c, err
	}
	if rows, err = s.db.Query(ctx, auditLoginsQuery, since, until); err != nil {
		return out, err
	}
	if out.Logins, err = pgx.CollectRows(rows, collect); err != nil {
		return out, err
	}
	if rows, err = s.db.Query(ctx, auditSecurityQuery, since, until, accounts.AuditSecurityActionList()); err != nil {
		return out, err
	}
	out.Security, err = pgx.CollectRows(rows, collect)
	return out, err
}

const verificationCols = `id, at, trigger, deep, duration_ms, run_error, ok, checked, broken_at, reason, first_id, segments, sealed, archives`

// AddAuditVerification implements accounts.Store.
func (s *Store) AddAuditVerification(ctx context.Context, v accounts.AuditVerification) (accounts.AuditVerification, error) {
	v, err := accounts.PrepareAuditVerification(v)
	if err != nil {
		return v, err
	}
	err = s.db.QueryRow(ctx, `INSERT INTO audit_verifications (at, trigger, deep, duration_ms, run_error, ok, checked, broken_at, reason, first_id, segments, sealed, archives)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13) RETURNING id`,
		v.At, v.Trigger, v.Deep, v.DurationMS, v.RunError, v.OK, v.Checked, v.BrokenAt, v.Reason, v.FirstID, v.Segments, v.Sealed, v.Archives).Scan(&v.ID)
	if err != nil {
		return v, err
	}
	_, err = s.db.Exec(ctx, `DELETE FROM audit_verifications WHERE at < $1`, v.At.Add(-accounts.AuditVerificationRetention))
	return v, err
}

// AuditVerifications implements accounts.Store.
func (s *Store) AuditVerifications(ctx context.Context, limit int) ([]accounts.AuditVerification, error) {
	limit = min(max(limit, 1), accounts.MaxAuditVerifications)
	rows, err := s.db.Query(ctx, `SELECT `+verificationCols+` FROM audit_verifications ORDER BY at DESC, id DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (accounts.AuditVerification, error) {
		var v accounts.AuditVerification
		err := r.Scan(&v.ID, &v.At, &v.Trigger, &v.Deep, &v.DurationMS, &v.RunError, &v.OK, &v.Checked, &v.BrokenAt, &v.Reason,
			&v.FirstID, &v.Segments, &v.Sealed, &v.Archives)
		v.At = v.At.UTC()
		return v, err
	})
}

// TriageCounts implements accounts.Store.
func (s *Store) TriageCounts(ctx context.Context, now time.Time) (map[string]map[string]int, error) {
	rows, err := s.db.Query(ctx, `SELECT org_id, state, count(*) FROM finding_triage
WHERE expires_at IS NULL OR expires_at > $1 GROUP BY org_id, state`, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]map[string]int{}
	for rows.Next() {
		var org, state string
		var n int
		if err := rows.Scan(&org, &state, &n); err != nil {
			return nil, err
		}
		if out[org] == nil {
			out[org] = map[string]int{}
		}
		out[org][state] = n
	}
	return out, rows.Err()
}
