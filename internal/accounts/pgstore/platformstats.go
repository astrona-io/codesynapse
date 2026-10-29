package pgstore

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/astrona-io/codesynapse/internal/accounts"
)

// Platform statistics: fixed SQL, aggregates only. $1 and $2 are the
// window, $3 the optional organisation filter. Root (bootstrap_root) is left
// out of every person, membership, session and key count.

const statsPeople = `people AS (SELECT id, created_at, disabled_at, platform_admin FROM users WHERE NOT bootstrap_root),
m AS (SELECT m.org_id, m.user_id, m.role, p.disabled_at FROM memberships m JOIN people p ON p.id = m.user_id),
active AS (
  SELECT user_id FROM events WHERE action IN ('user.login', 'sso.login') AND outcome <> 'failure' AND user_id <> '' AND at >= $1 AND at <= $2
  UNION SELECT user_id FROM sessions WHERE created_at >= $1 AND created_at <= $2
  UNION SELECT user_id FROM api_keys WHERE last_used_at >= $1 AND last_used_at <= $2
  UNION SELECT user_id FROM execution_sessions WHERE created_at >= $1 AND created_at <= $2
)`

const tenantTotalsQuery = `WITH ` + statsPeople + `
SELECT
  (SELECT count(*) FROM orgs),
  (SELECT count(*) FROM orgs WHERE created_at >= $1 AND created_at <= $2),
  (SELECT count(*) FROM orgs o WHERE NOT EXISTS (SELECT 1 FROM m WHERE m.org_id = o.id)),
  (SELECT count(*) FROM identity_providers WHERE enabled),
  (SELECT count(*) FROM people),
  (SELECT count(*) FROM people WHERE created_at >= $1 AND created_at <= $2),
  (SELECT count(*) FROM people WHERE disabled_at IS NOT NULL),
  (SELECT count(*) FROM people WHERE platform_admin),
  (SELECT count(*) FROM (SELECT user_id FROM m GROUP BY user_id HAVING bool_and(role = 'guest')) g),
  (SELECT count(*) FROM (SELECT user_id FROM m GROUP BY user_id HAVING count(*) > 1) x),
  (SELECT count(*) FROM people WHERE id IN (SELECT user_id FROM active)),
  (SELECT count(*) FROM m),
  (SELECT count(*) FROM m WHERE role <> 'guest' AND disabled_at IS NULL),
  (SELECT count(DISTINCT user_id) FROM m WHERE role <> 'guest' AND disabled_at IS NULL),
  (SELECT count(*) FROM teams),
  (SELECT count(*) FROM repositories),
  (SELECT count(*) FROM repositories WHERE owner_id = ''),
  (SELECT count(*) FROM repositories WHERE owner_id <> ''),
  (SELECT count(*) FROM repositories WHERE visibility = 'public'),
  (SELECT count(*) FROM repositories WHERE visibility = 'private'),
  (SELECT count(*) FROM repositories WHERE visibility = 'local'),
  (SELECT count(*) FROM repositories WHERE access = 'lost'),
  (SELECT count(DISTINCT graph) FROM repositories),
  (SELECT count(*) FROM org_watches WHERE NOT paused),
  (SELECT count(*) FROM events WHERE action = 'watch.repo_add' AND outcome = 'success' AND at >= $1 AND at <= $2)`

const tenantRolesQuery = `SELECT m.org_id, m.role, count(*) FROM memberships m JOIN users u ON u.id = m.user_id
WHERE NOT u.bootstrap_root GROUP BY m.org_id, m.role`

const tenantRowsQuery = `WITH ` + statsPeople + `
SELECT o.id, o.slug, o.name, o.created_at,
  (SELECT count(*) FROM m WHERE m.org_id = o.id),
  (SELECT count(*) FROM m WHERE m.org_id = o.id AND m.role <> 'guest' AND m.disabled_at IS NULL),
  (SELECT count(*) FROM m WHERE m.org_id = o.id AND m.user_id IN (SELECT user_id FROM active)),
  (SELECT count(*) FROM teams t WHERE t.org_id = o.id),
  (SELECT count(*) FROM repositories r WHERE r.org_id = o.id),
  (SELECT count(*) FROM repositories r WHERE r.org_id = o.id AND r.owner_id <> ''),
  (SELECT count(*) FROM repositories r WHERE r.org_id = o.id AND r.visibility = 'private'),
  (SELECT count(*) FROM repositories r WHERE r.org_id = o.id AND r.access = 'lost'),
  COALESCE((SELECT i.enabled FROM identity_providers i WHERE i.org_id = o.id), false),
  (SELECT count(*) FROM org_watches w WHERE w.org_id = o.id AND NOT w.paused)
FROM orgs o WHERE $3::text = '' OR o.id = $3
ORDER BY o.name COLLATE "C", o.id LIMIT ` + maxStatsRowsConst

// TenantStats implements accounts.Store.
func (s *Store) TenantStats(ctx context.Context, q accounts.StatsQuery) (accounts.TenantStats, error) {
	out := accounts.TenantStats{Totals: accounts.TenantTotals{Roles: map[string]int{}}, Orgs: []accounts.OrgTenantRow{}}
	t := &out.Totals
	if err := s.db.QueryRow(ctx, tenantTotalsQuery, q.Since, q.Now).Scan(
		&t.Orgs, &t.OrgsCreated, &t.OrgsWithoutMembers, &t.OrgsWithSSO,
		&t.Users, &t.UsersCreated, &t.UsersSuspended, &t.PlatformAdmins, &t.GuestOnlyUsers, &t.MultiOrgUsers, &t.ActiveUsers,
		&t.Memberships, &t.Seats, &t.SeatedPeople, &t.Teams,
		&t.Entries, &t.OrgEntries, &t.PersonalEntries, &t.PublicEntries, &t.PrivateEntries, &t.LocalEntries, &t.LostEntries, &t.Graphs,
		&t.Watches, &t.WatchAdds); err != nil {
		return out, fmt.Errorf("tenant totals: %w", err)
	}
	roles := map[string]map[string]int{}
	rows, err := s.db.Query(ctx, tenantRolesQuery)
	if err != nil {
		return out, fmt.Errorf("tenant roles: %w", err)
	}
	var orgID, role string
	var n int
	if _, err := pgx.ForEachRow(rows, []any{&orgID, &role, &n}, func() error {
		t.Roles[role] += n
		if roles[orgID] == nil {
			roles[orgID] = map[string]int{}
		}
		roles[orgID][role] = n
		return nil
	}); err != nil {
		return out, fmt.Errorf("tenant roles: %w", err)
	}
	rows, err = s.db.Query(ctx, tenantRowsQuery, q.Since, q.Now, q.OrgID)
	if err != nil {
		return out, fmt.Errorf("tenant rows: %w", err)
	}
	out.Orgs, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (accounts.OrgTenantRow, error) {
		var o accounts.OrgTenantRow
		err := r.Scan(&o.OrgID, &o.Slug, &o.Name, &o.CreatedAt, &o.Members, &o.Seats, &o.ActiveMembers, &o.Teams,
			&o.Entries, &o.PersonalEntries, &o.PrivateEntries, &o.LostEntries, &o.SSO, &o.Watches)
		o.CreatedAt = o.CreatedAt.UTC()
		o.Roles = roles[o.OrgID]
		if o.Roles == nil {
			o.Roles = map[string]int{}
		}
		return o, err
	})
	if err != nil {
		return out, fmt.Errorf("tenant rows: %w", err)
	}
	return out, nil
}

const sessionBrowserQuery = `SELECT
  count(*) FILTER (WHERE s.expires_at > $2),
  count(*) FILTER (WHERE s.created_at >= $1 AND s.created_at <= $2)
FROM sessions s JOIN users u ON u.id = s.user_id WHERE NOT u.bootstrap_root`

// Every runtime session and key belongs to one organisation, so the
// platform totals are the sums of these groups.
const sessionExecutionQuery = `SELECT e.org_id,
  count(*) FILTER (WHERE e.revoked_at IS NULL AND e.expires_at > $2),
  count(*) FILTER (WHERE e.created_at >= $1 AND e.created_at <= $2),
  count(*) FILTER (WHERE e.revoked_at >= $1 AND e.revoked_at <= $2),
  count(*) FILTER (WHERE e.created_at >= $1 AND e.created_at <= $2 AND e.id LIKE 'web:%'),
  count(*) FILTER (WHERE e.created_at >= $1 AND e.created_at <= $2 AND e.id LIKE 'gw:%')
FROM execution_sessions e JOIN users u ON u.id = e.user_id WHERE NOT u.bootstrap_root GROUP BY e.org_id`

const sessionKeysQuery = `SELECT k.org_id,
  count(*) FILTER (WHERE k.revoked_at IS NULL AND (k.expires_at IS NULL OR k.expires_at > $2)),
  count(*) FILTER (WHERE k.last_used_at >= $1 AND k.last_used_at <= $2),
  count(*) FILTER (WHERE k.revoked_at IS NULL AND k.expires_at > $2 AND k.expires_at <= $3)
FROM api_keys k JOIN users u ON u.id = k.user_id WHERE NOT u.bootstrap_root GROUP BY k.org_id`

const sessionOrgsQuery = `SELECT id, slug, name FROM orgs WHERE $1::text = '' OR id = $1 ORDER BY name COLLATE "C", id LIMIT ` + maxStatsRowsConst

// maxStatsRowsConst is accounts.MaxStatsRows as statement text (constant
// SQL); TestStatsRowCap keeps them equal. Names sort bytewise, as in memstore.
const maxStatsRowsConst = "10000"

// SessionStats implements accounts.Store.
func (s *Store) SessionStats(ctx context.Context, q accounts.StatsQuery) (accounts.SessionStats, error) {
	out := accounts.SessionStats{Orgs: []accounts.OrgSessionRow{}}
	t := &out.Totals
	if err := s.db.QueryRow(ctx, sessionBrowserQuery, q.Since, q.Now).Scan(&t.BrowserActive, &t.BrowserCreated); err != nil {
		return out, fmt.Errorf("browser sessions: %w", err)
	}
	execs := map[string]accounts.ExecutionCounts{}
	rows, err := s.db.Query(ctx, sessionExecutionQuery, q.Since, q.Now)
	if err != nil {
		return out, fmt.Errorf("runtime sessions: %w", err)
	}
	var orgID string
	var c accounts.ExecutionCounts
	if _, err := pgx.ForEachRow(rows, []any{&orgID, &c.Active, &c.Created, &c.Revoked, &c.Dashboard, &c.Gateway}, func() error {
		t.Execution.Active += c.Active
		t.Execution.Created += c.Created
		t.Execution.Revoked += c.Revoked
		t.Execution.Dashboard += c.Dashboard
		t.Execution.Gateway += c.Gateway
		accounts.SplitExecution(&c)
		execs[orgID] = c
		return nil
	}); err != nil {
		return out, fmt.Errorf("runtime sessions: %w", err)
	}
	accounts.SplitExecution(&t.Execution)
	keys := map[string]accounts.KeyCounts{}
	rows, err = s.db.Query(ctx, sessionKeysQuery, q.Since, q.Now, q.Now.Add(accounts.KeyExpiryWindow))
	if err != nil {
		return out, fmt.Errorf("api keys: %w", err)
	}
	var k accounts.KeyCounts
	if _, err := pgx.ForEachRow(rows, []any{&orgID, &k.Active, &k.Used, &k.Expiring}, func() error {
		t.Keys.Active += k.Active
		t.Keys.Used += k.Used
		t.Keys.Expiring += k.Expiring
		keys[orgID] = k
		return nil
	}); err != nil {
		return out, fmt.Errorf("api keys: %w", err)
	}
	rows, err = s.db.Query(ctx, sessionOrgsQuery, q.OrgID)
	if err != nil {
		return out, fmt.Errorf("session rows: %w", err)
	}
	out.Orgs, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (accounts.OrgSessionRow, error) {
		var o accounts.OrgSessionRow
		err := r.Scan(&o.OrgID, &o.Slug, &o.Name)
		o.Execution, o.Keys = execs[o.OrgID], keys[o.OrgID]
		return o, err
	})
	if err != nil {
		return out, fmt.Errorf("session rows: %w", err)
	}
	return out, nil
}
