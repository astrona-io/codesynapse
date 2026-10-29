package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/astrona-io/codesynapse/internal/accounts"
	acctmem "github.com/astrona-io/codesynapse/internal/accounts/memstore"
	"github.com/astrona-io/codesynapse/internal/store/memory"
	"github.com/astrona-io/codesynapse/internal/telemetry"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

// flakyAudit refuses audit entries while broken is set.
type flakyAudit struct {
	accounts.Store
	broken atomic.Bool
}

func (f *flakyAudit) AddEvent(ctx context.Context, e accounts.Event) (accounts.Event, error) {
	if f.broken.Load() {
		return accounts.Event{}, errors.New("disk full")
	}
	return f.Store.AddEvent(ctx, e)
}

// statsReads are every platform read: the statistics routes and the
// older admin lists, all session-only and audited.
var statsReads = []string{
	"/api/v1/admin/stats/summary?range=24h",
	"/api/v1/admin/stats/tenants?range=7d",
	"/api/v1/admin/stats/sessions?range=1h",
	"/api/v1/admin/stats/export?section=tenants&range=30d&format=csv",
	"/api/v1/admin/stats/export?section=sessions&range=6h&format=json",
	"/api/v1/admin/stats/export?section=summary&range=24h&format=csv",
	"/api/v1/admin/stats/summary?range=90d",
	"/api/v1/admin/stats/sessions?range=365d",
	"/api/v1/admin/stats/export?section=tenants&range=365d&format=json",
	"/api/v1/admin/stats/jobs?range=24h",
	"/api/v1/admin/stats/jobs?range=90d",
	"/api/v1/admin/stats/ingest?range=7d",
	"/api/v1/admin/stats/ingest?range=365d",
	"/api/v1/admin/stats/export?section=jobs&range=24h&format=csv",
	"/api/v1/admin/stats/export?section=ingest&range=30d&format=json",
	"/api/v1/admin/users",
	"/api/v1/admin/orgs",
	"/api/v1/admin/health",
	"/api/v1/stats/series?panel=platform_api_requests&range=1h",
	"/api/v1/admin/stats/series?panel=org_tool_calls&range=1h",
	"/api/v1/admin/stats/series?panel=mcp_calls&range=24h",
	"/api/v1/admin/stats/series/batch?panels=sessions,api_routes,gateway_requests&range=7d",
	"/api/v1/admin/stats/usage?range=24h",
	"/api/v1/admin/stats/usage?range=90d",
	"/api/v1/admin/stats/export?section=usage&range=7d&format=csv",
}

func TestPlatformStats(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	vm := httptest.NewServer(&fakeVM{})
	t.Cleanup(vm.Close)
	st := &flakyAudit{Store: acctmem.New()}
	acc := accounts.NewService(st, false)
	bootstrapAccounts(t, acc)
	acc.SignupOpen = true
	ts := httptest.NewServer(New(ctx, Config{Accounts: acc, ServiceToken: "svc-token", Metrics: &telemetry.Querier{URL: vm.URL}}, memory.New(), slog.New(slog.NewTextHandler(io.Discard, nil))))
	t.Cleanup(ts.Close)

	signup := func(email string) (*client, accounts.User, accounts.Org) {
		c := newClient(t, ts)
		var signed struct {
			User accounts.User
			Org  accounts.Org
		}
		require.Equal(t, c.signUp(`{"email":"`+email+`","name":"N","password":"`+pw+`"}`, &signed), 202)
		return c, signed.User, signed.Org
	}
	// Pat is in three organisations and holds the repository.
	pat, patUser, patOrg := signup("a@example.com")
	_, _, org2 := signup("owner2@example.com")
	_, _, org3 := signup("owner3@example.com")
	require.NoError(t, st.SetMember(ctx, org2.ID, patUser.ID, accounts.Member))
	require.NoError(t, st.SetMember(ctx, org3.ID, patUser.ID, accounts.Guest))
	require.NoError(t, st.AddRepository(ctx, accounts.Repository{OrgID: patOrg.ID, Graph: "secret-repo", Remote: "https://example.com/acme/secret-repo.git", Visibility: accounts.Private, AddedAt: time.Now(), AddedBy: patUser.ID}))
	var key struct{ Key string }
	require.Equal(t, pat.do("POST", "/api/v1/me/keys", `{"org":"`+patOrg.Slug+`","name":"laptop"}`, &key), 201)
	patKey := newClient(t, ts)
	patKey.auth = "Bearer " + key.Key

	// Ada administers the platform and belongs to no organisation.
	ada, adaUser, adaOrg := signup("ada@example.com")
	require.NoError(t, acc.SetPlatformAdmin(ctx, rootSession(t, acc), adaUser.ID, true))
	require.NoError(t, st.RemoveMember(ctx, adaOrg.ID, adaUser.ID))
	var adminKey struct{ Key string }
	sue, sueUser, _ := signup("sue@example.com")
	require.NoError(t, acc.SetPlatformAdmin(ctx, rootSession(t, acc), sueUser.ID, true))
	require.Equal(t, sue.do("POST", "/api/v1/me/keys", `{"org":"`+mustSlug(t, st, sueUser.ID)+`","name":"laptop"}`, &adminKey), 201)
	sueKey := newClient(t, ts)
	sueKey.auth = "Bearer " + adminKey.Key
	service := newClient(t, ts)
	service.auth = "Bearer svc-token"
	root := rootClient(t, ts)

	t.Run("scope", func(t *testing.T) {
		for _, path := range statsReads {
			t.Run(path, func(t *testing.T) {
				assert.Equal(t, pat.do("GET", path, "", nil), 403, "not a platform admin")
				assert.Equal(t, patKey.do("GET", path, "", nil), 403, "API key")
				assert.Equal(t, sueKey.do("GET", path, "", nil), 403, "an administrator's API key")
				assert.Equal(t, service.do("GET", path, "", nil), 403, "installation token")
				assert.Equal(t, root.do("GET", path, "", nil), 200, "root")
				assert.Equal(t, ada.do("GET", path, "", nil), 200, "admin without memberships")
			})
		}
		now := time.Now()
		require.NoError(t, st.SetUserDisabled(ctx, sueUser.ID, &now))
		for _, path := range statsReads {
			assert.Equal(t, sue.do("GET", path, "", nil), 401, "suspended: "+path)
		}
	})

	t.Run("views are audited", func(t *testing.T) {
		for _, action := range []string{"admin.users.view", "admin.orgs.view", "admin.health.view", "admin.stats.view", "admin.stats.export"} {
			evs, err := st.ListEvents(ctx, accounts.EventFilter{Action: action, Limit: 100})
			require.NoError(t, err)
			assert.True(t, len(evs) >= 2, action+": root and ada")
			for _, e := range evs {
				assert.True(t, e.UserID != patUser.ID && e.UserID != sueUser.ID, "refused views are not recorded as views")
			}
		}
		before, err := st.ListEvents(ctx, accounts.EventFilter{Action: "admin.stats.view", Limit: 1000})
		require.NoError(t, err)
		var batch struct{ Panels map[string]SeriesBatchItem }
		require.Equal(t, ada.do("GET", "/api/v1/stats/series/batch?panels=platform_api_requests,platform_heap,tool_calls&range=1h", "", &batch), 200)
		assert.Equal(t, batch.Panels["platform_api_requests"].Status, 200)
		after, err := st.ListEvents(ctx, accounts.EventFilter{Action: "admin.stats.view", Limit: 1000})
		require.NoError(t, err)
		assert.Equal(t, len(after), len(before)+1, "one batch is one view")
		batch.Panels = nil
		require.Equal(t, ada.do("GET", "/api/v1/admin/stats/series/batch?panels=org_tool_calls,mcp_calls,mcp_calls,api_routes,tenants&range=1h", "", &batch), 200)
		require.Len(t, batch.Panels, 4, "duplicates count once")
		assert.Equal(t, batch.Panels["org_tool_calls"].Status, 200)
		assert.Equal(t, batch.Panels["api_routes"].Status, 200)
		assert.Equal(t, batch.Panels["tenants"].Status, 404, "unknown admin panel")
		again, err := st.ListEvents(ctx, accounts.EventFilter{Action: "admin.stats.view", Limit: 1000})
		require.NoError(t, err)
		assert.Equal(t, len(again), len(after)+1, "one admin batch is one view")
		require.Equal(t, pat.do("GET", "/api/v1/stats/series/batch?panels=platform_api_requests,tool_calls&range=1h", "", &batch), 200)
		assert.Equal(t, batch.Panels["platform_api_requests"].Status, 403)
		assert.Equal(t, batch.Panels["tool_calls"].Status, 200)
	})

	t.Run("figures", func(t *testing.T) {
		var tenants struct {
			Totals accounts.TenantTotals
			Rows   []accounts.OrgTenantRow
			Page   int
			Pages  int
			Total  int
		}
		require.Equal(t, ada.do("GET", "/api/v1/admin/stats/tenants?range=24h", "", &tenants), 200)
		users, err := st.ListUsers(ctx, 100)
		require.NoError(t, err)
		assert.Equal(t, tenants.Totals.Users, len(users)-1, "every account but root")
		assert.Equal(t, tenants.Totals.MultiOrgUsers, 1, "pat counts once")
		members := 0
		pats := 0
		for _, o := range tenants.Rows {
			members += o.Members
			if o.OrgID == patOrg.ID || o.OrgID == org2.ID || o.OrgID == org3.ID {
				pats++
			}
		}
		assert.Equal(t, members, tenants.Totals.Memberships)
		assert.Equal(t, pats, 3, "three memberships")
		assert.Equal(t, tenants.Page, 1)
		assert.Equal(t, tenants.Pages, 1)

		var one struct {
			Rows []accounts.OrgTenantRow
		}
		require.Equal(t, ada.do("GET", "/api/v1/admin/stats/tenants?range=24h&org="+org2.ID, "", &one), 200)
		require.Len(t, one.Rows, 1)
		assert.Equal(t, ada.do("GET", "/api/v1/admin/stats/tenants?range=24h&org="+org2.Slug, "", nil), 400, "org is an ID checked against PostgreSQL")
		assert.Equal(t, ada.do("GET", "/api/v1/admin/stats/tenants?range=2y", "", nil), 400)
		assert.Equal(t, ada.do("GET", "/api/v1/admin/stats/export?section=users&format=csv", "", nil), 400)
		assert.Equal(t, ada.do("GET", "/api/v1/admin/stats/export?section=tenants&format=xlsx", "", nil), 400)

		var summary map[string]any
		require.Equal(t, ada.do("GET", "/api/v1/admin/stats/summary", "", &summary), 200)
		for _, k := range []string{"graphs", "cost", "alerts_firing"} {
			v, ok := summary[k]
			assert.True(t, ok && v == nil, k+" is null until a later phase")
		}
		assert.Equal(t, summary["jobs_waiting"], any(float64(0)), "the queue is read from PostgreSQL")
		assert.True(t, summary["jobs_failed"] != nil, "failed jobs come from metrics, which this server has")
	})

	t.Run("long ranges read the daily rollup", func(t *testing.T) {
		today := accounts.StatDay(time.Now())
		put := func(daysAgo int, rows ...accounts.PlatformStat) {
			day := today.AddDate(0, 0, -daysAgo)
			for i := range rows {
				rows[i].Day = day
			}
			require.NoError(t, st.PutPlatformStats(ctx, day, rows))
		}
		stat := func(metric, dim, org string, v int64) accounts.PlatformStat {
			return accounts.PlatformStat{Metric: metric, Dimension: dim, OrgID: org, Value: v}
		}
		put(1, stat(accounts.StatOrgs, "created", "", 2), stat(accounts.StatUsers, "created", "", 3), stat(accounts.StatActiveUsers, "30d", "", 7),
			stat(accounts.StatBrowserSessions, "created", "", 5),
			stat(accounts.StatExecutionSessions, "created", "", 4), stat(accounts.StatExecutionSessions, "created_runtime", "", 4),
			stat(accounts.StatExecutionSessions, "created", patOrg.ID, 4), stat(accounts.StatExecutionSessions, "created_runtime", patOrg.ID, 4))
		put(10, stat(accounts.StatOrgs, "created", "", 1), stat(accounts.StatUsers, "created", "", 1), stat(accounts.StatActiveUsers, "30d", "", 9),
			stat(accounts.StatExecutionSessions, "revoked", patOrg.ID, 2), stat(accounts.StatExecutionSessions, "revoked", "", 2))
		put(200, stat(accounts.StatOrgs, "created", "", 5), stat(accounts.StatUsers, "created", "", 10))
		put(500, stat(accounts.StatOrgs, "created", "", 100)) // past retention and every range

		var live StatsSummary
		require.Equal(t, ada.do("GET", "/api/v1/admin/stats/summary?range=24h", "", &live), 200)
		assert.True(t, live.Rollup == nil, "short ranges stay live")
		for _, c := range []struct {
			rng                      string
			orgs, users, days, wrote int
		}{{"90d", 3, 4, 90, 2}, {"365d", 8, 14, 365, 3}} {
			var sum StatsSummary
			require.Equal(t, ada.do("GET", "/api/v1/admin/stats/summary?range="+c.rng, "", &sum), 200)
			assert.Equal(t, sum.OrgsCreated, c.orgs, c.rng+": orgs created")
			assert.Equal(t, sum.UsersCreated, c.users, c.rng+": users created")
			assert.Equal(t, sum.ActiveUsers, 7, c.rng+": the 30-day figure of the latest day")
			assert.Equal(t, sum.ExecutionCreated, 4, c.rng)
			assert.Equal(t, sum.Orgs, live.Orgs, c.rng+": state stays live")
			assert.Equal(t, sum.Users, live.Users, c.rng)
			assert.Equal(t, sum.Seats, live.Seats, c.rng)
			require.True(t, sum.Rollup != nil, c.rng)
			assert.Equal(t, sum.Rollup.Days, c.days)
			assert.Equal(t, sum.Rollup.Written, c.wrote)
			assert.Equal(t, sum.Rollup.ActiveWindow, "30d")
		}

		var sessions struct {
			Totals accounts.SessionTotals
			Rows   []accounts.OrgSessionRow
			Rollup *StatsRollup
		}
		require.Equal(t, ada.do("GET", "/api/v1/admin/stats/sessions?range=90d&org="+patOrg.ID, "", &sessions), 200)
		assert.Equal(t, sessions.Totals.BrowserCreated, 5)
		assert.Equal(t, sessions.Totals.Execution.Created, 4)
		assert.Equal(t, sessions.Totals.Execution.Runtime, 4)
		assert.Equal(t, sessions.Totals.Execution.Revoked, 2)
		assert.Equal(t, sessions.Totals.Keys.Active, live.KeysActive, "state stays live")
		require.Len(t, sessions.Rows, 1)
		assert.Equal(t, sessions.Rows[0].Execution.Created, 4)
		assert.Equal(t, sessions.Rows[0].Execution.Revoked, 2)
		require.True(t, sessions.Rollup != nil)
		assert.Equal(t, sessions.Rollup.Unavailable, []string{"keys_used"})

		var tenants struct {
			Totals accounts.TenantTotals
			Rollup *StatsRollup
		}
		require.Equal(t, ada.do("GET", "/api/v1/admin/stats/tenants?range=365d", "", &tenants), 200)
		assert.Equal(t, tenants.Totals.OrgsCreated, 8)
		assert.Equal(t, tenants.Rollup.Unavailable, []string{"watch_adds", "active_members"})

		var csv, js string
		require.Equal(t, ada.do("GET", "/api/v1/admin/stats/export?section=sessions&range=90d&format=csv", "", &csv), 200)
		assert.Contains(t, csv, ",1,,") // keys active 1, used not available
		require.Equal(t, ada.do("GET", "/api/v1/admin/stats/export?section=summary&range=365d&format=json", "", &js), 200)
		assert.Contains(t, js, `"rollup":{"days":365,"written":3`)
		require.Equal(t, ada.do("GET", "/api/v1/admin/stats/export?section=tenants&range=24h&format=json", "", &js), 200)
		assert.False(t, strings.Contains(js, "rollup"), "short ranges export as before")

		evs, err := st.ListEvents(ctx, accounts.EventFilter{Action: "admin.stats.view", Limit: 1000})
		require.NoError(t, err)
		found := false
		for _, e := range evs {
			found = found || strings.Contains(e.Target, "section=summary range=365d")
		}
		assert.True(t, found, "long ranges are audited like the others")

		for _, path := range []string{
			"/api/v1/admin/stats/summary?range=180d",
			"/api/v1/admin/stats/tenants?range=90",
			"/api/v1/admin/stats/sessions?range=1y",
			"/api/v1/admin/stats/export?section=tenants&range=730d&format=csv",
			"/api/v1/admin/stats/series?panel=mcp_calls&range=90d",
			"/api/v1/admin/stats/series/batch?panels=mcp_calls&range=2y",
			"/api/v1/admin/stats/series/batch?panels=&range=1h",
			"/api/v1/admin/stats/usage?range=180d",
		} {
			assert.Equal(t, ada.do("GET", path, "", nil), 400, path)
		}
		assert.Equal(t, pat.do("GET", "/api/v1/admin/stats/summary?range=365d", "", nil), 403, "not a platform admin")
		assert.Equal(t, sueKey.do("GET", "/api/v1/admin/stats/summary?range=365d", "", nil), 401, "suspended, and an API key")
	})

	t.Run("no names, addresses or IDs", func(t *testing.T) {
		for _, path := range statsReads[:9] {
			var body string
			require.Equal(t, ada.do("GET", path, "", &body), 200)
			for _, secret := range []string{"secret-repo", "a@example.com", patUser.ID, adaUser.ID, key.Key[:12]} {
				assert.False(t, strings.Contains(body, secret), path+" leaks "+secret)
			}
		}
		var csv string
		require.Equal(t, ada.do("GET", "/api/v1/admin/stats/export?section=tenants&range=24h&format=csv", "", &csv), 200)
		assert.True(t, strings.HasPrefix(csv, "slug,name,created_at,members,owners"), csv)
	})

	t.Run("no figures without an audit entry", func(t *testing.T) {
		st.broken.Store(true)
		defer st.broken.Store(false)
		for _, path := range statsReads {
			var body string
			assert.Equal(t, ada.do("GET", path, "", &body), 503, path)
			assert.False(t, strings.Contains(body, "totals") || strings.Contains(body, "rows") || strings.Contains(body, "orgs\""), path)
			var problem struct{ Detail string }
			_ = json.Unmarshal([]byte(body), &problem) //nolint:errcheck // asserted below
			assert.Contains(t, problem.Detail, "audit log")
		}
	})
}

func mustSlug(t *testing.T, st accounts.Store, userID string) string {
	t.Helper()
	ms, err := st.UserMemberships(context.Background(), userID)
	require.NoError(t, err)
	require.Len(t, ms, 1)
	return ms[0].Org.Slug
}
