package accountstest

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/astrona-io/codesynapse/internal/accounts"
	"github.com/astrona-io/codesynapse/internal/execution"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

// failingAudit refuses every audit entry.
type failingAudit struct{ accounts.Store }

func (failingAudit) AddEvent(context.Context, accounts.Event) (accounts.Event, error) {
	return accounts.Event{}, errors.New("disk full")
}

// RunPlatformStats checks the platform statistics: who may read them, that
// every view is audited first, and how people, memberships, seats,
// entries, sessions and keys are counted. Root is in no count; one person
// in three organisations is one user with three memberships.
func RunPlatformStats(t *testing.T, newStore func(*testing.T) accounts.Store) {
	t.Helper()
	ctx := context.Background()
	st := newStore(t)
	now := time.Date(2026, 10, 22, 14, 0, 0, 0, time.UTC)
	svc := accounts.NewService(st, true)
	svc.Now = func() time.Time { return now }
	ago := func(d time.Duration) time.Time { return now.Add(-d) }
	at := func(d time.Duration) *time.Time { v := now.Add(d); return &v }
	day := 24 * time.Hour

	users := map[string]accounts.User{
		"root": {ID: "u-root", Email: "root@example.com", Name: "Root", BootstrapRoot: true, PlatformAdmin: true, CreatedAt: ago(60 * day)},
		"ada":  {ID: "u-ada", Email: "a@example.com", Name: "Ada", PlatformAdmin: true, CreatedAt: ago(60 * day)},
		"pat":  {ID: "u-pat", Email: "pat@example.com", Name: "Pat", CreatedAt: ago(2 * time.Hour)},
		"gus":  {ID: "u-gus", Email: "gus@example.com", Name: "Gus", CreatedAt: ago(40 * day)},
		"sue":  {ID: "u-sue", Email: "sue@example.com", Name: "Sue", CreatedAt: ago(40 * day)},
	}
	for _, u := range users {
		require.NoError(t, st.CreateUser(ctx, u))
	}
	require.NoError(t, st.SetUserDisabled(ctx, "u-sue", at(-day)))
	orgs := []struct {
		org   accounts.Org
		owner string
	}{
		{accounts.Org{ID: "o-a", Slug: "alpha", Name: "Alpha", CreatedAt: ago(40 * day)}, "u-pat"},
		{accounts.Org{ID: "o-b", Slug: "beta", Name: "Beta", CreatedAt: ago(40 * day)}, "u-pat"},
		{accounts.Org{ID: "o-c", Slug: "gamma", Name: "Gamma", CreatedAt: ago(time.Hour)}, "u-pat"},
		{accounts.Org{ID: "o-d", Slug: "delta", Name: "Delta", CreatedAt: ago(40 * day)}, "u-root"}, // root only: no members
	}
	for _, o := range orgs {
		require.NoError(t, st.CreateOrg(ctx, o.org, o.owner))
	}
	require.NoError(t, st.SetMember(ctx, "o-a", "u-sue", accounts.Member))
	require.NoError(t, st.SetMember(ctx, "o-b", "u-pat", accounts.Member))
	require.NoError(t, st.SetMember(ctx, "o-b", "u-gus", accounts.Guest))
	require.NoError(t, st.SetMember(ctx, "o-c", "u-pat", accounts.Guest))
	require.NoError(t, st.CreateTeam(ctx, accounts.Team{ID: "t-1", OrgID: "o-a", Slug: "devs", Name: "Devs", CreatedAt: now}))
	for _, r := range []accounts.Repository{
		{OrgID: "o-a", Graph: "secret-repo", Visibility: accounts.Public, Access: accounts.AccessOK, AddedAt: now, AddedBy: "u-pat"},
		{OrgID: "o-a", Graph: "other-graph", Visibility: accounts.Local, Access: accounts.AccessOK, AddedAt: now, AddedBy: "u-pat"},
		{OrgID: "o-b", Graph: "secret-repo", Visibility: accounts.Private, Access: accounts.AccessLost, AddedAt: now, AddedBy: "u-pat", OwnerID: "u-pat"},
	} {
		require.NoError(t, st.AddRepository(ctx, r))
	}
	for _, idp := range []accounts.IdentityProvider{
		{OrgID: "o-a", Name: "Corp", Issuer: "https://idp.example", ClientID: "cs", SecretPreview: "abcd", KeyID: "k1", SealedSecret: []byte{1}, Enabled: true, CreatedBy: "u-pat", UpdatedAt: now},
		{OrgID: "o-b", Name: "Corp", Issuer: "https://idp.example", ClientID: "cs", SecretPreview: "abcd", KeyID: "k1", SealedSecret: []byte{1}, Enabled: false, CreatedBy: "u-pat", UpdatedAt: now},
	} {
		require.NoError(t, st.PutIdentityProvider(ctx, idp))
	}
	for i, org := range []string{"o-a", "o-b"} {
		id := execution.Identity{TenantID: org, SubjectID: "u-pat", SessionID: "web:1", ParentSessionID: "web:1"}
		require.NoError(t, st.PutOrgWatch(ctx, accounts.OrgWatch{OrgID: org, ActorID: "u-pat", Execution: id, Provider: "github", Host: "github.com", Login: "acme", Paused: i == 1, NextScan: now}))
	}
	for _, s := range []accounts.Session{
		{TokenHash: "h-ada", UserID: "u-ada", CreatedAt: ago(time.Hour), ExpiresAt: now.Add(7 * day)},
		{TokenHash: "h-root", UserID: "u-root", CreatedAt: ago(time.Hour), ExpiresAt: now.Add(7 * day)},
		{TokenHash: "h-gus", UserID: "u-gus", CreatedAt: ago(3 * day), ExpiresAt: ago(day)},
	} {
		require.NoError(t, st.CreateSession(ctx, s))
	}
	for _, x := range []accounts.ExecutionSession{
		{ID: "web:abc", UserID: "u-pat", OrgID: "o-a", CreatedAt: ago(time.Hour)},
		{ID: "runtime-1", UserID: "u-pat", OrgID: "o-a", CreatedAt: ago(2 * time.Hour)},
		{ID: "gw:1", UserID: "u-pat", OrgID: "o-b", CreatedAt: ago(3 * day)},
	} {
		x.ExpiresAt = x.CreatedAt.Add(accounts.ExecutionSessionTTL)
		_, _, err := st.RegisterExecutionSession(ctx, x)
		require.NoError(t, err)
	}
	require.NoError(t, st.RevokeExecutionSession(ctx, "runtime-1", "o-a", "u-pat", ago(30*time.Minute)))
	for _, k := range []accounts.APIKey{
		{ID: "k-pat", UserID: "u-pat", OrgID: "o-a", Name: "a", Prefix: "csk_1", Hash: "hash-1", CreatedAt: ago(10 * day)},
		{ID: "k-sue", UserID: "u-sue", OrgID: "o-a", Name: "b", Prefix: "csk_2", Hash: "hash-2", CreatedAt: ago(10 * day), ExpiresAt: at(7 * day)},
		{ID: "k-old", UserID: "u-pat", OrgID: "o-b", Name: "c", Prefix: "csk_3", Hash: "hash-3", CreatedAt: ago(10 * day)},
	} {
		require.NoError(t, st.CreateAPIKey(ctx, k))
	}
	require.NoError(t, st.TouchAPIKey(ctx, "k-pat", ago(time.Hour)))
	require.NoError(t, st.TouchAPIKey(ctx, "k-sue", ago(3*day)))
	require.NoError(t, st.RevokeAPIKey(ctx, "u-pat", "k-old", ago(day)))
	for _, e := range []accounts.Event{
		{At: ago(time.Hour), UserID: "u-gus", Via: "session", Action: "user.login", Outcome: accounts.Success},
		{At: ago(time.Hour), UserID: "u-root", Via: "session", Action: "user.login", Outcome: accounts.Success},
		{At: ago(time.Hour), UserID: "u-sue", Via: "sso", Action: "sso.login", Outcome: accounts.Failure},
		{At: ago(time.Hour), UserID: "u-pat", Via: "automation", OrgID: "o-a", Action: "watch.repo_add", Target: "secret-repo", Outcome: accounts.Success},
		{At: ago(2 * day), UserID: "u-pat", Via: "automation", OrgID: "o-a", Action: "watch.repo_add", Target: "secret-repo", Outcome: accounts.Success},
	} {
		_, err := st.AddEvent(ctx, e)
		require.NoError(t, err)
	}
	ada := accounts.Principal{User: users["ada"], Via: "session"}
	root := accounts.Principal{User: users["root"], Via: "session"}
	view := accounts.StatsRequest{Section: "tenants", Range: "24h"}

	t.Run("access", func(t *testing.T) {
		cases := map[string]struct {
			p    accounts.Principal
			want error
		}{
			"platform admin without memberships": {ada, nil},
			"root":                               {root, nil},
			"api key":                            {accounts.Principal{User: users["ada"], Via: "key", KeyID: "k-pat", OrgID: "o-a"}, accounts.ErrForbidden},
			"installation token":                 {accounts.Principal{Via: "service"}, accounts.ErrForbidden},
			"member":                             {accounts.Principal{User: users["pat"], Via: "session"}, accounts.ErrForbidden},
			"suspended":                          {accounts.Principal{User: users["sue"], Via: "session"}, accounts.ErrUnauthorized},
		}
		for name, c := range cases {
			t.Run(name, func(t *testing.T) {
				_, err := svc.PlatformStats(ctx, c.p, view)
				if c.want == nil {
					assert.NoError(t, err)
				} else {
					assert.True(t, errors.Is(err, c.want), err)
				}
			})
		}
		bad := map[string]accounts.StatsRequest{
			"range":   {Section: "tenants", Range: "2y"},
			"section": {Section: "secrets", Range: "24h"},
			"format":  {Section: "tenants", Range: "24h", Format: "xlsx"},
			"org":     {Section: "tenants", Range: "24h", OrgID: "o-missing"},
		}
		for name, r := range bad {
			_, err := svc.PlatformStats(ctx, ada, r)
			assert.True(t, errors.Is(err, accounts.ErrInvalidInput), name)
		}
	})

	t.Run("audited before answering", func(t *testing.T) {
		q, err := svc.PlatformStats(ctx, ada, accounts.StatsRequest{Section: "sessions", Range: "7d", OrgID: "o-b", Format: "csv"})
		require.NoError(t, err)
		assert.Equal(t, q.OrgID, "o-b")
		assert.True(t, q.Since.Equal(now.Add(-7*day)))
		evs, err := st.ListEvents(ctx, accounts.EventFilter{Action: "admin.stats.export", Limit: 10})
		require.NoError(t, err)
		require.Len(t, evs, 1)
		assert.Equal(t, evs[0].Target, "section=sessions range=7d org=o-b format=csv")
		assert.Equal(t, evs[0].UserID, "u-ada")
		for section, action := range map[string]string{"users": "admin.users.view", "orgs": "admin.orgs.view", "health": "admin.health.view", "summary": "admin.stats.view"} {
			_, err := svc.PlatformStats(ctx, ada, accounts.StatsRequest{Section: section})
			require.NoError(t, err)
			evs, err := st.ListEvents(ctx, accounts.EventFilter{Action: action, Limit: 100})
			require.NoError(t, err)
			assert.True(t, len(evs) > 0, action)
		}

		broken := accounts.NewService(failingAudit{st}, true)
		_, err = broken.PlatformStats(ctx, ada, view)
		assert.True(t, errors.Is(err, accounts.ErrAuditUnavailable), "no figures without an audit entry")
	})

	t.Run("tenants", func(t *testing.T) {
		q := accounts.StatsQuery{Since: ago(day), Now: now}
		got, err := st.TenantStats(ctx, q)
		require.NoError(t, err)
		assert.Equal(t, got.Totals, accounts.TenantTotals{
			Orgs: 4, OrgsCreated: 1, OrgsWithoutMembers: 1, OrgsWithSSO: 1,
			Users: 4, UsersCreated: 1, UsersSuspended: 1, PlatformAdmins: 1, GuestOnlyUsers: 1, MultiOrgUsers: 1, ActiveUsers: 3,
			Memberships: 5, Roles: map[string]int{"owner": 1, "member": 2, "guest": 2}, Seats: 2, SeatedPeople: 1, Teams: 1,
			Entries: 3, OrgEntries: 2, PersonalEntries: 1, PublicEntries: 1, PrivateEntries: 1, LocalEntries: 1, LostEntries: 1, Graphs: 2,
			Watches: 1, WatchAdds: 1,
		})
		require.Len(t, got.Orgs, 4)
		names := []string{}
		for _, o := range got.Orgs {
			names = append(names, o.Name)
		}
		assert.Equal(t, names, []string{"Alpha", "Beta", "Delta", "Gamma"}, "ordered by name")
		assert.Equal(t, got.Orgs[0], accounts.OrgTenantRow{OrgID: "o-a", Slug: "alpha", Name: "Alpha", CreatedAt: ago(40 * day),
			Members: 2, Roles: map[string]int{"owner": 1, "member": 1}, Seats: 1, ActiveMembers: 1, Teams: 1, Entries: 2, SSO: true, Watches: 1})
		assert.Equal(t, got.Orgs[1], accounts.OrgTenantRow{OrgID: "o-b", Slug: "beta", Name: "Beta", CreatedAt: ago(40 * day),
			Members: 2, Roles: map[string]int{"member": 1, "guest": 1}, Seats: 1, ActiveMembers: 2, Entries: 1, PersonalEntries: 1, PrivateEntries: 1, LostEntries: 1})
		assert.Equal(t, got.Orgs[2].Members, 0, "root is never a member")
		assert.Equal(t, got.Orgs[2].Roles, map[string]int{})
		assert.Equal(t, got.Orgs[3].Members, 1)
		assert.Equal(t, got.Orgs[3].Seats, 0, "guests hold no seat")
		members := 0
		for _, o := range got.Orgs {
			members += o.Members
		}
		assert.Equal(t, members, got.Totals.Memberships, "one person in three organisations is three memberships")

		q.OrgID = "o-b"
		one, err := st.TenantStats(ctx, q)
		require.NoError(t, err)
		require.Len(t, one.Orgs, 1)
		assert.Equal(t, one.Orgs[0].OrgID, "o-b")
		assert.Equal(t, one.Totals, got.Totals, "totals stay the platform's")

		older, err := st.TenantStats(ctx, accounts.StatsQuery{Since: ago(30 * day), Now: now})
		require.NoError(t, err)
		assert.Equal(t, older.Totals.WatchAdds, 2)
		assert.Equal(t, older.Totals.ActiveUsers, 4, "sue's key use counts, root never does")
		assert.Equal(t, older.Totals.OrgsCreated, 1)
		noLeak(t, got)
	})

	t.Run("sessions", func(t *testing.T) {
		got, err := st.SessionStats(ctx, accounts.StatsQuery{Since: ago(day), Now: now})
		require.NoError(t, err)
		assert.Equal(t, got.Totals, accounts.SessionTotals{
			BrowserActive: 1, BrowserCreated: 1,
			Execution: accounts.ExecutionCounts{Active: 2, Created: 2, Revoked: 1, Dashboard: 1, Runtime: 1},
			Keys:      accounts.KeyCounts{Active: 2, Used: 1, Expiring: 1},
		})
		require.Len(t, got.Orgs, 4)
		assert.Equal(t, got.Orgs[0], accounts.OrgSessionRow{OrgID: "o-a", Slug: "alpha", Name: "Alpha",
			Execution: accounts.ExecutionCounts{Active: 1, Created: 2, Revoked: 1, Dashboard: 1, Runtime: 1},
			Keys:      accounts.KeyCounts{Active: 2, Used: 1, Expiring: 1}})
		assert.Equal(t, got.Orgs[1], accounts.OrgSessionRow{OrgID: "o-b", Slug: "beta", Name: "Beta", Execution: accounts.ExecutionCounts{Active: 1}})
		one, err := st.SessionStats(ctx, accounts.StatsQuery{Since: ago(day), Now: now, OrgID: "o-a"})
		require.NoError(t, err)
		require.Len(t, one.Orgs, 1)
		assert.Equal(t, one.Totals, got.Totals)
		noLeak(t, got)
	})
}

// noLeak fails when a statistic carries a repository name, an e-mail
// address or a user, key or session ID of the fixture.
func noLeak(t *testing.T, v any) {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	for _, s := range []string{"secret-repo", "other-graph", "@example.com", "u-ada", "u-pat", "u-root", "k-pat", "web:abc", "runtime-1", "h-ada"} {
		assert.False(t, strings.Contains(string(b), s), s)
	}
}
