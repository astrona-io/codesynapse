package pgstore

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/astrona-io/codesynapse/internal/accounts"
)

// The statements below are constants with values only in parameters, and
// they return counts grouped by organisation ID and fixed columns, never a
// name, address or the ID of a user, key or session. $1 is the start of the
// day and $2 its end; the queries that need only the end take it as $1.
// Root (bootstrap_root) is never counted.

const statOrgsSQL = `SELECT count(*), count(*) FILTER (WHERE o.created_at >= $1),
 count(*) FILTER (WHERE NOT EXISTS (SELECT 1 FROM memberships m JOIN users u ON u.id = m.user_id WHERE m.org_id = o.id AND NOT u.bootstrap_root AND m.added_at < $2)),
 count(*) FILTER (WHERE EXISTS (SELECT 1 FROM identity_providers p WHERE p.org_id = o.id))
FROM orgs o WHERE o.created_at < $2`

const statUsersSQL = `WITH mem AS (
 SELECT m.user_id, count(*) AS n, bool_and(m.role = 'guest') AS guest_only FROM memberships m WHERE m.added_at < $2 GROUP BY m.user_id)
SELECT count(*), count(*) FILTER (WHERE u.created_at >= $1),
 count(*) FILTER (WHERE u.disabled_at < $2), count(*) FILTER (WHERE u.platform_admin),
 count(*) FILTER (WHERE mem.guest_only), count(*) FILTER (WHERE mem.n > 1)
FROM users u LEFT JOIN mem ON mem.user_id = u.id WHERE NOT u.bootstrap_root AND u.created_at < $2`

const statActiveSQL = `SELECT count(DISTINCT a.user_id) FROM (
 SELECT user_id FROM events WHERE action = 'user.login' AND outcome = 'success' AND user_id <> '' AND at >= $1 AND at < $2
 UNION ALL SELECT user_id FROM sessions WHERE created_at >= $1 AND created_at < $2
 UNION ALL SELECT user_id FROM api_keys WHERE last_used_at >= $1 AND last_used_at < $2
 UNION ALL SELECT user_id FROM execution_sessions WHERE created_at >= $1 AND created_at < $2
) a JOIN users u ON u.id = a.user_id WHERE NOT u.bootstrap_root`

const statMembershipsSQL = `SELECT m.org_id, m.role, count(*), count(*) FILTER (WHERE m.role <> 'guest' AND (u.disabled_at IS NULL OR u.disabled_at >= $1))
FROM memberships m JOIN users u ON u.id = m.user_id WHERE NOT u.bootstrap_root AND m.added_at < $1 GROUP BY m.org_id, m.role`

const statSeatsSQL = `SELECT count(DISTINCT m.user_id) FROM memberships m JOIN users u ON u.id = m.user_id
WHERE NOT u.bootstrap_root AND m.added_at < $1 AND m.role <> 'guest' AND (u.disabled_at IS NULL OR u.disabled_at >= $1)`

const statReposSQL = `SELECT r.org_id, count(*), count(*) FILTER (WHERE r.owner_id = ''), count(*) FILTER (WHERE r.owner_id <> ''),
 count(*) FILTER (WHERE r.visibility = 'public'), count(*) FILTER (WHERE r.visibility = 'private'),
 count(*) FILTER (WHERE r.visibility = 'local'), count(*) FILTER (WHERE r.access = 'lost')
FROM repositories r WHERE r.added_at < $1 GROUP BY r.org_id`

const statGraphsSQL = `SELECT count(DISTINCT graph) FROM repositories WHERE added_at < $1`

const statJobsSQL = `SELECT j.org_id, j.kind, j.state, count(*), EXISTS (SELECT 1 FROM orgs o WHERE o.id = j.org_id)
FROM jobs j WHERE j.state IN ('succeeded', 'failed') AND j.finished_at >= $1 AND j.finished_at < $2 GROUP BY j.org_id, j.kind, j.state`

const statBrowserSQL = `SELECT count(*) FILTER (WHERE s.expires_at > $2), count(*) FILTER (WHERE s.created_at >= $1)
FROM sessions s JOIN users u ON u.id = s.user_id WHERE NOT u.bootstrap_root AND s.created_at < $2`

const statExecutionSQL = `SELECT e.org_id,
 count(*) FILTER (WHERE e.expires_at > $2 AND (e.revoked_at IS NULL OR e.revoked_at >= $2)),
 count(*) FILTER (WHERE e.created_at >= $1),
 count(*) FILTER (WHERE e.created_at >= $1 AND left(e.id, 4) = 'web:'),
 count(*) FILTER (WHERE e.created_at >= $1 AND left(e.id, 3) = 'gw:'),
 count(*) FILTER (WHERE e.created_at >= $1 AND left(e.id, 4) <> 'web:' AND left(e.id, 3) <> 'gw:'),
 count(*) FILTER (WHERE e.revoked_at >= $1 AND e.revoked_at < $2)
FROM execution_sessions e JOIN users u ON u.id = e.user_id
WHERE NOT u.bootstrap_root AND e.created_at < $2 AND (e.expires_at > $1 OR e.revoked_at >= $1) GROUP BY e.org_id`

const statKeysSQL = `SELECT k.org_id,
 count(*) FILTER (WHERE (k.revoked_at IS NULL OR k.revoked_at >= $2) AND (k.expires_at IS NULL OR k.expires_at > $2)),
 count(*) FILTER (WHERE k.last_used_at >= $1 AND k.last_used_at < $2),
 count(*) FILTER (WHERE (k.revoked_at IS NULL OR k.revoked_at >= $2) AND k.expires_at > $2 AND k.expires_at <= $3)
FROM api_keys k JOIN users u ON u.id = k.user_id WHERE NOT u.bootstrap_root AND k.created_at < $2 GROUP BY k.org_id`

// org_watches has no creation time: active is the state when the day is
// written, like the other state figures.
const statWatchesSQL = `SELECT (SELECT count(*) FROM org_watches WHERE NOT paused),
 (SELECT count(*) FROM events WHERE action = 'watch.repo_add' AND outcome = 'success' AND at >= $1 AND at < $2)`

// CountPlatformStats implements accounts.Store. It reads in one repeatable
// read snapshot so the figures of a day agree with each other.
func (s *Store) CountPlatformStats(ctx context.Context, day time.Time) ([]accounts.PlatformStat, error) {
	if !day.Equal(accounts.StatDay(day)) {
		return nil, accounts.ErrInvalidInput
	}
	day = accounts.StatDay(day)
	start, end := day, day.AddDate(0, 0, 1)
	set := accounts.NewPlatformStatSet(day)
	count := func(tx pgx.Tx) error {
		var n [6]int64
		if err := tx.QueryRow(ctx, statOrgsSQL, start, end).Scan(&n[0], &n[1], &n[2], &n[3]); err != nil {
			return err
		}
		for i, d := range []string{"total", "created", "no_members", "sso"} {
			set.Total(accounts.StatOrgs, d, n[i])
		}
		if err := tx.QueryRow(ctx, statUsersSQL, start, end).Scan(&n[0], &n[1], &n[2], &n[3], &n[4], &n[5]); err != nil {
			return err
		}
		for i, d := range []string{"total", "created", "suspended", "platform_admins", "guest_only", "multi_org"} {
			set.Total(accounts.StatUsers, d, n[i])
		}
		for dim, days := range accounts.StatActiveWindows {
			var a int64
			if err := tx.QueryRow(ctx, statActiveSQL, end.AddDate(0, 0, -days), end).Scan(&a); err != nil {
				return err
			}
			set.Total(accounts.StatActiveUsers, dim, a)
		}
		if err := scanRows(ctx, tx, statMembershipsSQL, []any{end}, func(r pgx.Rows) error {
			var org, role string
			var total, seats int64
			if err := r.Scan(&org, &role, &total, &seats); err != nil {
				return err
			}
			set.Both(org, accounts.StatMemberships, "total", total)
			set.Both(org, accounts.StatMemberships, role, total)
			set.Org(org, accounts.StatSeats, "total", seats)
			return nil
		}); err != nil {
			return err
		}
		var seats, graphs int64
		if err := tx.QueryRow(ctx, statSeatsSQL, end).Scan(&seats); err != nil {
			return err
		}
		set.Total(accounts.StatSeats, "total", seats)
		if err := scanRows(ctx, tx, statReposSQL, []any{end}, func(r pgx.Rows) error {
			var org string
			var v [7]int64
			if err := r.Scan(&org, &v[0], &v[1], &v[2], &v[3], &v[4], &v[5], &v[6]); err != nil {
				return err
			}
			for i, d := range []string{"total", "organisation", "personal", "public", "private", "local", "lost"} {
				set.Both(org, accounts.StatRepoEntries, d, v[i])
			}
			return nil
		}); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, statGraphsSQL, end).Scan(&graphs); err != nil {
			return err
		}
		set.Total(accounts.StatGraphs, "total", graphs)
		if err := scanRows(ctx, tx, statJobsSQL, []any{start, end}, func(r pgx.Rows) error {
			var org, kind, state string
			var c int64
			var orgExists bool
			if err := r.Scan(&org, &kind, &state, &c, &orgExists); err != nil {
				return err
			}
			dim, ok := accounts.StatJobDimension(kind, state)
			if !ok {
				return nil
			}
			set.Total(accounts.StatJobsFinished, dim, c)
			if orgExists {
				set.Org(org, accounts.StatJobsFinished, dim, c)
			}
			return nil
		}); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, statBrowserSQL, start, end).Scan(&n[0], &n[1]); err != nil {
			return err
		}
		set.Total(accounts.StatBrowserSessions, "active", n[0])
		set.Total(accounts.StatBrowserSessions, "created", n[1])
		if err := tx.QueryRow(ctx, statWatchesSQL, start, end).Scan(&n[0], &n[1]); err != nil {
			return err
		}
		set.Total(accounts.StatWatches, "active", n[0])
		set.Total(accounts.StatWatches, "added", n[1])
		if err := scanRows(ctx, tx, statExecutionSQL, []any{start, end}, func(r pgx.Rows) error {
			var org string
			var v [6]int64
			if err := r.Scan(&org, &v[0], &v[1], &v[2], &v[3], &v[4], &v[5]); err != nil {
				return err
			}
			for i, d := range []string{"active", "created", "created_dashboard", "created_gateway", "created_runtime", "revoked"} {
				set.Both(org, accounts.StatExecutionSessions, d, v[i])
			}
			return nil
		}); err != nil {
			return err
		}
		ingest, err := ingestStats(ctx, tx, start, end)
		if err != nil {
			return err
		}
		accounts.AddIngestStats(set, ingest)
		return scanRows(ctx, tx, statKeysSQL, []any{start, end, end.AddDate(0, 0, 14)}, func(r pgx.Rows) error {
			var org string
			var v [3]int64
			if err := r.Scan(&org, &v[0], &v[1], &v[2]); err != nil {
				return err
			}
			for i, d := range []string{"active", "used", "expiring_14d"} {
				set.Both(org, accounts.StatAPIKeys, d, v[i])
			}
			return nil
		})
	}
	var err error
	if s.policy {
		err = pgx.BeginFunc(ctx, s.db, count)
	} else {
		err = pgx.BeginTxFunc(ctx, s.pool, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, count)
	}
	if err != nil {
		return nil, err
	}
	return set.Rows(), nil
}

func scanRows(ctx context.Context, tx pgx.Tx, sql string, args []any, row func(pgx.Rows) error) error {
	rows, err := tx.Query(ctx, sql, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		if err := row(rows); err != nil {
			return err
		}
	}
	return rows.Err()
}

// PutPlatformStats implements accounts.Store. A lock per day serialises
// writers of the same day; the day's rows are replaced in one transaction,
// so readers see the old day or the new one.
func (s *Store) PutPlatformStats(ctx context.Context, day time.Time, rows []accounts.PlatformStat) error {
	rows, err := accounts.PreparePlatformStats(day, rows)
	if err != nil {
		return err
	}
	metrics, dims, orgs, values := make([]string, len(rows)), make([]string, len(rows)), make([]string, len(rows)), make([]int64, len(rows))
	for i, r := range rows {
		metrics[i], dims[i], orgs[i], values[i] = r.Metric, r.Dimension, r.OrgID, r.Value
	}
	return pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(1668244590, $1::int)", int32(day.Unix()/86400)); err != nil { //nolint:gosec // days since 1970 fit an int32
			return err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM platform_stats_daily WHERE day = $1 AND (metric, dimension, org_id) NOT IN
 (SELECT * FROM unnest($2::text[], $3::text[], $4::text[]))`, day, metrics, dims, orgs); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO platform_stats_daily (day, metric, dimension, org_id, value)
SELECT $1, * FROM unnest($2::text[], $3::text[], $4::text[], $5::bigint[])
ON CONFLICT (day, metric, dimension, org_id) DO UPDATE SET value = EXCLUDED.value WHERE platform_stats_daily.value <> EXCLUDED.value`,
			day, metrics, dims, orgs, values)
		return err
	})
}

// HasPlatformStats implements accounts.Store.
func (s *Store) HasPlatformStats(ctx context.Context, day time.Time) (bool, error) {
	var ok bool
	err := s.db.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM platform_stats_daily WHERE day = $1)`, accounts.StatDay(day)).Scan(&ok)
	return ok, err
}

const platformStatsWhere = `WHERE day >= $1 AND day < $2 AND (coalesce(cardinality($3::text[]), 0) = 0 OR metric = ANY ($3::text[]))
 AND (CASE WHEN $4 THEN org_id <> '' ELSE org_id = $5 END)`

const listPlatformStatsSQL = `SELECT day, metric, dimension, org_id, value FROM platform_stats_daily ` + platformStatsWhere + `
ORDER BY day, metric COLLATE "C", dimension COLLATE "C", org_id COLLATE "C" LIMIT $6`

// The sum keeps the read's order; $1 stands in for the day.
const sumPlatformStatsSQL = `SELECT $1::date, metric, dimension, org_id, sum(value)::bigint FROM platform_stats_daily ` + platformStatsWhere + `
GROUP BY metric, dimension, org_id ORDER BY metric COLLATE "C", dimension COLLATE "C", org_id COLLATE "C" LIMIT $6`

// ListPlatformStats implements accounts.Store.
func (s *Store) ListPlatformStats(ctx context.Context, q accounts.PlatformStatQuery) ([]accounts.PlatformStat, error) {
	q, err := accounts.PreparePlatformStatQuery(q)
	if err != nil {
		return nil, err
	}
	sql := listPlatformStatsSQL
	if q.Sum {
		sql = sumPlatformStatsSQL
	}
	rows, err := s.db.Query(ctx, sql, q.From, q.To, q.Metrics, q.PerOrg, q.OrgID, q.Limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []accounts.PlatformStat{}
	for rows.Next() {
		var r accounts.PlatformStat
		if err := rows.Scan(&r.Day, &r.Metric, &r.Dimension, &r.OrgID, &r.Value); err != nil {
			return nil, err
		}
		r.Day = accounts.StatDay(r.Day)
		out = append(out, r)
	}
	return out, rows.Err()
}

// PrunePlatformStats implements accounts.Store.
func (s *Store) PrunePlatformStats(ctx context.Context, before time.Time) (int, error) {
	tag, err := s.db.Exec(ctx, `DELETE FROM platform_stats_daily WHERE day < $1`, accounts.StatDay(before))
	if err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), nil
}
