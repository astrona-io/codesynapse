package accountstest

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/astrona-io/codesynapse/internal/accounts"
	"github.com/astrona-io/codesynapse/internal/execution"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

// RunPlatformStatsDaily checks the daily platform statistics: what a day counts
// (root never), that the writer is idempotent, that only listed metrics and
// dimensions are stored, the bounded read and the 400-day pruning. Every
// store gives the same rows for the same fixture, so memstore and pgstore
// agree.
func RunPlatformStatsDaily(t *testing.T, newStore func(*testing.T) accounts.Store) {
	t.Helper()
	day := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	t.Run("counts", func(t *testing.T) {
		st := newStore(t)
		ctx := t.Context()
		svc, f := platformStatsFixture(ctx, t, st)
		day := f.day
		got := statValues(t, st, day, "")
		want := map[string]int64{
			"orgs/total": 4, "orgs/created": 4, "orgs/no_members": 1, "orgs/sso": 0,
			// One person in three organisations is one user and three
			// memberships; root is in no count.
			"users/total": 3, "users/created": 3, "users/suspended": 0, "users/guest_only": 0, "users/multi_org": 1,
			"users/platform_admins": f.platformAdmins,
			"active_users/1d":       2, "active_users/7d": 2, "active_users/30d": 2,
			"memberships/total": 5, "memberships/owner": 3, "memberships/member": 1, "memberships/guest": 1, "memberships/admin": 0, "memberships/viewer": 0,
			"seats/total":        3,
			"repo_entries/total": 3, "repo_entries/organisation": 2, "repo_entries/personal": 1, "repo_entries/public": 2, "repo_entries/private": 1, "repo_entries/local": 0, "repo_entries/lost": 1,
			"graphs/total":            2,
			"browser_sessions/active": 2, "browser_sessions/created": 2,
			"execution_sessions/active": 2, "execution_sessions/created": 2, "execution_sessions/created_dashboard": 1, "execution_sessions/created_gateway": 0, "execution_sessions/created_runtime": 1, "execution_sessions/revoked": 0,
			"api_keys/active": 1, "api_keys/used": 1, "api_keys/expiring_14d": 1,
			"jobs_finished/sync.failed": 1, "jobs_finished/other.succeeded": 1, "jobs_finished/sync.succeeded": 0,
			// A paused watch is not active; a failed addition and one of the
			// day before are not the day's.
			"watches/active": 1, "watches/added": 1,
			// Written by the API's sources, zero without them.
			"llm_calls/measured": 0, "llm_memory/embedded": 0,
		}
		for k, v := range want {
			assert.Equal(t, got[k], v, k)
		}
		for _, m := range accounts.PlatformStatMetrics() {
			for _, d := range accounts.PlatformStatDimensions(m) {
				if !accounts.ValidPlatformStat(m, d, "") || m == accounts.StatStoreRows || m == accounts.StatStoreBytes {
					continue // organisation rows only, or written only when sampled
				}
				_, ok := got[m+"/"+d]
				assert.True(t, ok, "every platform row is written, zeros included: "+m+"/"+d)
			}
		}

		ada := statValues(t, st, day, f.adaOrg)
		assert.Equal(t, ada["memberships/total"], int64(2))
		assert.Equal(t, ada["seats/total"], int64(2))
		assert.Equal(t, ada["repo_entries/personal"], int64(1))
		assert.Equal(t, ada["repo_entries/lost"], int64(1))
		assert.Equal(t, ada["execution_sessions/created_dashboard"], int64(1))
		assert.Equal(t, ada["api_keys/used"], int64(1))
		assert.Equal(t, ada["jobs_finished/sync.failed"], int64(1))
		_, zero := ada["repo_entries/local"]
		assert.False(t, zero, "organisation rows are written only when not zero")
		setup := statValues(t, st, day, f.setupOrg)
		assert.Equal(t, setup["memberships/guest"], int64(1))
		assert.Equal(t, setup["seats/total"], int64(1), "a guest holds no seat")

		// Every organisation's rows: opaque IDs only, and a job of an
		// organisation that is gone counts on the platform alone.
		all, err := st.ListPlatformStats(ctx, accounts.PlatformStatQuery{From: day, To: day.AddDate(0, 0, 1), PerOrg: true})
		require.NoError(t, err)
		require.True(t, len(all) > 0)
		for _, r := range all {
			assert.True(t, f.orgIDs[r.OrgID], "only existing organisations get rows: "+r.OrgID)
			assert.True(t, accounts.ValidPlatformStat(r.Metric, r.Dimension, r.OrgID))
		}
		text := fmt.Sprint(all)
		for _, s := range []string{"@", "acme/", "claude-1", "web:abc", "Ada"} {
			assert.False(t, strings.Contains(text, s), "no names, addresses, repositories or session IDs: "+s)
		}

		// Read path: platform administrators only, 90d and 365d only.
		_, err = svc.PlatformStatsHistory(ctx, f.ada, "90d", nil, "", false)
		assert.True(t, errors.Is(err, accounts.ErrForbidden), "a member who is not a platform admin is refused")
		_, err = svc.PlatformStatsHistory(ctx, f.root, "30d", nil, "", false)
		assert.True(t, errors.Is(err, accounts.ErrInvalidInput), "short ranges come from VictoriaMetrics")
		hist, err := svc.PlatformStatsHistory(ctx, f.root, "90d", []string{accounts.StatUsers}, "", false)
		require.NoError(t, err)
		assert.Len(t, hist, len(accounts.PlatformStatDimensions(accounts.StatUsers)))
		hist, err = svc.PlatformStatsHistory(ctx, f.root, "365d", []string{accounts.StatSeats}, f.adaOrg, false)
		require.NoError(t, err)
		require.Len(t, hist, 1)
		assert.Equal(t, hist[0].Value, int64(2))
	})

	t.Run("writer is idempotent", func(t *testing.T) {
		st := newStore(t)
		ctx := t.Context()
		svc, f := platformStatsFixture(ctx, t, st)
		day := f.day
		first := listDay(t, st, day)
		// A retry, or a second writer, finds the day written.
		written, err := svc.RollupPlatformStats(ctx)
		require.NoError(t, err)
		assert.False(t, written)
		// Writing the counted rows again leaves the same rows.
		rows, err := st.CountPlatformStats(ctx, day)
		require.NoError(t, err)
		require.NoError(t, st.PutPlatformStats(ctx, day, rows))
		require.NoError(t, st.PutPlatformStats(ctx, day, rows))
		assert.Equal(t, fmt.Sprint(listDay(t, st, day)), fmt.Sprint(first))
		// A write replaces the day: a row it no longer holds goes.
		var kept []accounts.PlatformStat
		for _, r := range rows {
			if r.OrgID == "" {
				kept = append(kept, r)
			}
		}
		require.NoError(t, st.PutPlatformStats(ctx, day, kept))
		assert.Len(t, listDay(t, st, day), len(kept))
	})

	t.Run("allowlist", func(t *testing.T) {
		st := newStore(t)
		ctx := t.Context()
		require.NoError(t, st.Migrate(ctx))
		ok := accounts.PlatformStat{Day: day, Metric: accounts.StatSeats, Dimension: "total", OrgID: "org-1", Value: 3}
		cases := []struct {
			name string
			day  time.Time
			edit func(*accounts.PlatformStat)
		}{
			{name: "unknown metric", edit: func(r *accounts.PlatformStat) { r.Metric = "repo_names" }},
			{name: "unknown dimension", edit: func(r *accounts.PlatformStat) { r.Dimension = "ada@example.com" }},
			{name: "organisation row of a platform-only metric", edit: func(r *accounts.PlatformStat) { r.Metric, r.Dimension = accounts.StatUsers, "total" }},
			{name: "negative count", edit: func(r *accounts.PlatformStat) { r.Value = -1 }},
			{name: "another day", edit: func(r *accounts.PlatformStat) { r.Day = day.AddDate(0, 0, 1) }},
			{name: "not a day", day: day.Add(time.Hour)},
		}
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				r, d := ok, day
				if c.edit != nil {
					c.edit(&r)
				}
				if !c.day.IsZero() {
					d, r.Day = c.day, c.day
				}
				assert.True(t, errors.Is(st.PutPlatformStats(ctx, d, []accounts.PlatformStat{r}), accounts.ErrInvalidInput))
			})
		}
		assert.True(t, errors.Is(st.PutPlatformStats(ctx, day, []accounts.PlatformStat{ok, ok}), accounts.ErrInvalidInput), "duplicates are refused")
		has, err := st.HasPlatformStats(ctx, day)
		require.NoError(t, err)
		assert.False(t, has, "a refused write stores nothing")
		require.NoError(t, st.PutPlatformStats(ctx, day, []accounts.PlatformStat{ok}))

		_, err = st.ListPlatformStats(ctx, accounts.PlatformStatQuery{From: day, To: day.AddDate(0, 0, 401)})
		assert.True(t, errors.Is(err, accounts.ErrInvalidInput), "reads are bounded to the retention")
		_, err = st.ListPlatformStats(ctx, accounts.PlatformStatQuery{From: day, To: day.AddDate(0, 0, 1), Metrics: []string{"secrets"}})
		assert.True(t, errors.Is(err, accounts.ErrInvalidInput))
		_, err = st.ListPlatformStats(ctx, accounts.PlatformStatQuery{From: day, To: day.AddDate(0, 0, 1), OrgID: "org-1", PerOrg: true})
		assert.True(t, errors.Is(err, accounts.ErrInvalidInput))
		rows, err := st.ListPlatformStats(ctx, accounts.PlatformStatQuery{From: day, To: day.AddDate(0, 0, 1), OrgID: "org-1"})
		require.NoError(t, err)
		require.Len(t, rows, 1)
		assert.Equal(t, rows[0].Value, int64(3))
		assert.True(t, rows[0].Day.Equal(day))
		rows, err = st.ListPlatformStats(ctx, accounts.PlatformStatQuery{From: day, To: day.AddDate(0, 0, 1)})
		require.NoError(t, err)
		assert.Len(t, rows, 0, "an organisation's row is not a platform total")
	})

	t.Run("pruning", func(t *testing.T) {
		st := newStore(t)
		ctx := t.Context()
		require.NoError(t, st.Migrate(ctx))
		now := day.Add(10 * time.Minute)
		row := func(d time.Time) []accounts.PlatformStat {
			return []accounts.PlatformStat{{Day: d, Metric: accounts.StatGraphs, Dimension: "total", Value: 1}}
		}
		old, kept := day.AddDate(0, 0, -accounts.PlatformStatsRetentionDays-1), day.AddDate(0, 0, -accounts.PlatformStatsRetentionDays)
		require.NoError(t, st.PutPlatformStats(ctx, old, row(old)))
		require.NoError(t, st.PutPlatformStats(ctx, kept, row(kept)))
		svc := accounts.NewService(st, false)
		svc.Now = func() time.Time { return now }
		written, err := svc.RollupPlatformStats(ctx)
		require.NoError(t, err)
		assert.True(t, written, "yesterday is written on an empty installation too")
		has, err := st.HasPlatformStats(ctx, old)
		require.NoError(t, err)
		assert.False(t, has, "rows older than 400 days are pruned")
		has, err = st.HasPlatformStats(ctx, kept)
		require.NoError(t, err)
		assert.True(t, has, "the 400th day is kept")
		n, err := st.PrunePlatformStats(ctx, kept)
		require.NoError(t, err)
		assert.Equal(t, n, 0)
	})

	t.Run("sources and sums", func(t *testing.T) {
		st := newStore(t)
		ctx := t.Context()
		require.NoError(t, st.Migrate(ctx))
		svc := accounts.NewService(st, false)
		first := day.AddDate(0, 0, -1)
		usage := func(_ context.Context, d time.Time, set *accounts.PlatformStatSet) error {
			set.Both("org-1", accounts.StatMCPUsage, "calls", 5)
			set.Org("org-1", accounts.StatMCPTools, "search.calls", 4)
			set.Org("org-2", accounts.StatMCPTools, "search.calls", 1)
			set.Org("org-2", accounts.StatMCPClients, "claude.sessions", int64(d.Day()))
			return nil
		}
		for _, d := range []time.Time{first, day} {
			svc.Now = func() time.Time { return d.AddDate(0, 0, 1).Add(time.Minute) }
			written, err := svc.RollupPlatformStats(ctx, usage)
			require.NoError(t, err)
			assert.True(t, written)
		}
		v := statValues(t, st, day, "")
		assert.Equal(t, v[accounts.StatMCPUsage+"/calls"], int64(5))
		_, ok := v[accounts.StatMCPTools+"/search.calls"]
		assert.False(t, ok, "tools and clients have organisation rows only")
		assert.Equal(t, statValues(t, st, day, "org-2")[accounts.StatMCPTools+"/search.calls"], int64(1))

		sums, err := st.ListPlatformStats(ctx, accounts.PlatformStatQuery{From: first, To: day.AddDate(0, 0, 1),
			Metrics: []string{accounts.StatMCPTools, accounts.StatMCPClients}, PerOrg: true, Sum: true})
		require.NoError(t, err)
		require.Len(t, sums, 3)
		want := []accounts.PlatformStat{
			{Day: first, Metric: accounts.StatMCPClients, Dimension: "claude.sessions", OrgID: "org-2", Value: int64(first.Day() + day.Day())},
			{Day: first, Metric: accounts.StatMCPTools, Dimension: "search.calls", OrgID: "org-1", Value: 8},
			{Day: first, Metric: accounts.StatMCPTools, Dimension: "search.calls", OrgID: "org-2", Value: 2},
		}
		for i, r := range sums {
			assert.True(t, r.Day.Equal(first), "a sum's day is the first of the read")
			r.Day = want[i].Day
			assert.Equal(t, r, want[i])
		}

		// A source that fails leaves the day unwritten, to be retried whole.
		next := day.AddDate(0, 0, 1)
		svc.Now = func() time.Time { return next.AddDate(0, 0, 1).Add(time.Minute) }
		_, err = svc.RollupPlatformStats(ctx, func(context.Context, time.Time, *accounts.PlatformStatSet) error {
			return errors.New("graph store down")
		})
		require.Error(t, err)
		has, err := st.HasPlatformStats(ctx, next)
		require.NoError(t, err)
		assert.False(t, has)
	})
}

type statsFixture struct {
	day              time.Time
	root, ada        accounts.Principal
	adaOrg, setupOrg string
	orgIDs           map[string]bool
	platformAdmins   int64
}

// platformStatsFixture fills st today and writes the day the way the
// api-loops writer does just after the next midnight. Stores stamp some rows
// with their own clock, so the fixture's day is the real one.
func platformStatsFixture(ctx context.Context, t *testing.T, st accounts.Store) (*accounts.Service, statsFixture) {
	t.Helper()
	now := time.Now().UTC()
	if left := accounts.StatDay(now).AddDate(0, 0, 1).Sub(now); left < time.Minute {
		time.Sleep(left + time.Second) // never straddle midnight
		now = time.Now().UTC()
	}
	day := accounts.StatDay(now)
	svc := accounts.NewService(st, false)
	svc.Now = func() time.Time { return now }
	root, err := Bootstrap(ctx, svc)
	require.NoError(t, err)
	svc.SignupOpen = true
	adaUser, adaOrg, err := svc.SignUp(ctx, "stats-ada@example.com", "Ada", pw)
	require.NoError(t, err)
	bobUser, bobOrg, err := svc.SignUp(ctx, "stats-bob@example.com", "Bob", pw)
	require.NoError(t, err)
	owner, err := st.UserByEmail(ctx, SetupOwner)
	require.NoError(t, err)
	ms, err := st.UserMemberships(ctx, owner.ID)
	require.NoError(t, err)
	require.Len(t, ms, 1)
	setupOrg := ms[0].Org.ID
	ada := accounts.Principal{User: adaUser, Via: "session"}
	require.NoError(t, Join(ctx, t, svc, ada, adaOrg.ID, bobUser, accounts.Member))
	require.NoError(t, Join(ctx, t, svc, accounts.Principal{User: owner, Via: "session"}, setupOrg, bobUser, accounts.Guest))
	empty, err := svc.CreateOrg(ctx, ada, "Stats empty", "")
	require.NoError(t, err)
	require.NoError(t, st.RemoveMember(ctx, empty.ID, adaUser.ID))

	require.NoError(t, st.AddRepository(ctx, accounts.Repository{OrgID: adaOrg.ID, Graph: "acme/one", Visibility: accounts.Public, AddedBy: adaUser.ID, AddedAt: now}))
	require.NoError(t, st.AddRepository(ctx, accounts.Repository{OrgID: bobOrg.ID, Graph: "acme/one", Visibility: accounts.Public, AddedBy: bobUser.ID, AddedAt: now}))
	require.NoError(t, st.AddRepository(ctx, accounts.Repository{OrgID: adaOrg.ID, Graph: "acme/two", Visibility: accounts.Private, AddedBy: adaUser.ID, OwnerID: adaUser.ID, AddedAt: now}))
	require.NoError(t, st.SetRepositoryAccess(ctx, adaOrg.ID, "acme/two", accounts.AccessLost, now))

	_, key, err := svc.CreateKey(ctx, ada, adaOrg.ID, "ci", 7*24*time.Hour)
	require.NoError(t, err)
	require.NoError(t, st.TouchAPIKey(ctx, key.ID, now))
	_, err = svc.RegisterExecutionSession(ctx, adaUser.ID, adaOrg.ID, "claude-1")
	require.NoError(t, err)
	_, err = svc.RegisterExecutionSession(ctx, adaUser.ID, adaOrg.ID, "web:abc")
	require.NoError(t, err)
	for _, email := range []string{adaUser.Email, bobUser.Email} {
		_, _, err := svc.Login(ctx, email, pw, "1.2.3.4")
		require.NoError(t, err)
	}

	job := func(id, kind, org, repo string) accounts.JobRecord {
		return accounts.JobRecord{ID: id, Kind: kind, Repo: repo, OrgID: org, ActorID: adaUser.ID, Via: "session", Role: accounts.Admin,
			Execution: execution.Identity{TenantID: org, SubjectID: adaUser.ID, SessionID: "web:abc", ParentSessionID: "web:abc"},
			State:     accounts.JobQueued, LeaseOwner: "proc", LeaseUntil: now.Add(time.Minute), CreatedAt: now}
	}
	for _, j := range []struct {
		rec   accounts.JobRecord
		state string
	}{{job("stats-j1", "sync", adaOrg.ID, "acme/one"), accounts.JobFailed}, {job("stats-j2", "mystery", "org-gone", "acme/three"), accounts.JobSucceeded}} {
		require.NoError(t, st.AddJob(ctx, j.rec))
		require.NoError(t, st.StartJob(ctx, j.rec.ID, "proc", now.Add(time.Minute), now))
		require.NoError(t, st.FinishJob(ctx, j.rec.ID, "proc", accounts.JobOutcome{State: j.state, At: now}))
	}
	for i, login := range []string{"acme", "paused"} {
		id := execution.Identity{TenantID: adaOrg.ID, SubjectID: adaUser.ID, SessionID: "web:abc", ParentSessionID: "web:abc"}
		require.NoError(t, st.PutOrgWatch(ctx, accounts.OrgWatch{OrgID: adaOrg.ID, ActorID: adaUser.ID, Execution: id, Provider: "github", Host: "github.com",
			Login: login, Paused: i == 1, NextScan: now}))
	}
	for _, e := range []accounts.Event{
		{At: now, UserID: adaUser.ID, Via: "automation", OrgID: adaOrg.ID, Action: "watch.repo_add", Target: "acme/one", Outcome: accounts.Success},
		{At: now, UserID: adaUser.ID, Via: "automation", OrgID: adaOrg.ID, Action: "watch.repo_add", Target: "acme/two", Outcome: accounts.Failure},
		{At: day.Add(-time.Minute), UserID: adaUser.ID, Via: "automation", OrgID: adaOrg.ID, Action: "watch.repo_add", Target: "acme/three", Outcome: accounts.Success},
	} {
		_, err := st.AddEvent(ctx, e)
		require.NoError(t, err)
	}
	users, err := st.ListUsers(ctx, 100)
	require.NoError(t, err)
	var admins int64
	for _, u := range users {
		if u.PlatformAdmin && !u.BootstrapRoot {
			admins++
		}
	}

	// The writer runs just after midnight and writes the day before.
	svc.Now = func() time.Time { return day.AddDate(0, 0, 1).Add(10 * time.Minute) }
	written, err := svc.RollupPlatformStats(ctx)
	require.NoError(t, err)
	assert.True(t, written)
	return svc, statsFixture{day: day, root: root, ada: ada, adaOrg: adaOrg.ID, setupOrg: setupOrg, platformAdmins: admins,
		orgIDs: map[string]bool{adaOrg.ID: true, bobOrg.ID: true, setupOrg: true, empty.ID: true}}
}

func listDay(t *testing.T, st accounts.Store, day time.Time) []accounts.PlatformStat {
	t.Helper()
	var out []accounts.PlatformStat
	for _, perOrg := range []bool{false, true} {
		rows, err := st.ListPlatformStats(t.Context(), accounts.PlatformStatQuery{From: day, To: day.AddDate(0, 0, 1), PerOrg: perOrg})
		require.NoError(t, err)
		out = append(out, rows...)
	}
	return out
}

// statValues maps "metric/dimension" to the day's value for orgID ("" for
// the platform).
func statValues(t *testing.T, st accounts.Store, day time.Time, orgID string) map[string]int64 {
	t.Helper()
	rows, err := st.ListPlatformStats(t.Context(), accounts.PlatformStatQuery{From: day, To: day.AddDate(0, 0, 1), OrgID: orgID})
	require.NoError(t, err)
	out := map[string]int64{}
	for _, r := range rows {
		require.True(t, r.Day.Equal(day))
		out[r.Metric+"/"+r.Dimension] = r.Value
	}
	return out
}
