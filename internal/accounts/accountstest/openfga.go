package accountstest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/astrona-io/codesynapse/internal/accounts"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

// RunOpenFGASync covers the OpenFGA sync's records (pending, revisions that
// never go back, rebuild requests) and OrgPolicy, the policy the sync
// writes: eligible members only, usable entries only, the organisation's
// own teams only, and invalid policy refused as a whole.
func RunOpenFGASync(t *testing.T, newStore func(*testing.T) accounts.Store) {
	ctx := context.Background()
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	setup := func(t *testing.T) (accounts.Store, accounts.OpenFGASyncStore, *accounts.Service) {
		st := newStore(t)
		fs, ok := st.(accounts.OpenFGASyncStore)
		require.True(t, ok, "the store records OpenFGA syncs")
		for _, id := range []string{"fs-owner", "fs-member", "fs-guest", "fs-gone", "fs-other"} {
			require.NoError(t, st.CreateUser(ctx, accounts.User{ID: id, Name: id, Email: id + "@example.com", CreatedAt: now}))
		}
		require.NoError(t, st.CreateOrg(ctx, accounts.Org{ID: "fs-a", Name: "a", Slug: "fs-a", CreatedAt: now}, "fs-owner"))
		require.NoError(t, st.CreateOrg(ctx, accounts.Org{ID: "fs-b", Name: "b", Slug: "fs-b", CreatedAt: now}, "fs-other"))
		require.NoError(t, st.SetMember(ctx, "fs-a", "fs-member", accounts.Write))
		require.NoError(t, st.SetMember(ctx, "fs-a", "fs-guest", accounts.Guest))
		require.NoError(t, st.SetMember(ctx, "fs-a", "fs-gone", accounts.Write))
		svc := accounts.NewService(st, false)
		svc.Now = func() time.Time { return now }
		return st, fs, svc
	}

	t.Run("records", func(t *testing.T) {
		st, fs, _ := setup(t)
		ids := func(orgs []accounts.Org) []string {
			out := []string{}
			for _, o := range orgs {
				out = append(out, o.ID)
			}
			return out
		}
		pending, err := fs.OpenFGAPending(ctx, "s1", "m1", 10)
		require.NoError(t, err)
		assert.Equal(t, ids(pending), []string{"fs-a", "fs-b"}, "never synced is pending")
		_, err = fs.OpenFGASynced(ctx, "fs-a")
		assert.True(t, errors.Is(err, accounts.ErrNotFound))

		a, err := st.OrgByID(ctx, "fs-a")
		require.NoError(t, err)
		b, err := st.OrgByID(ctx, "fs-b")
		require.NoError(t, err)
		require.NoError(t, fs.RecordOpenFGASync(ctx, accounts.OpenFGASync{OrgID: "fs-a", StoreID: "s1", ModelID: "m1", Revision: a.PolicyRevision, SyncedAt: now}, now))
		require.NoError(t, fs.RecordOpenFGASync(ctx, accounts.OpenFGASync{OrgID: "fs-b", StoreID: "s1", ModelID: "m1", Revision: b.PolicyRevision, SyncedAt: now}, now))
		pending, err = fs.OpenFGAPending(ctx, "s1", "m1", 10)
		require.NoError(t, err)
		assert.Len(t, pending, 0)
		got, err := fs.OpenFGASynced(ctx, "fs-a")
		require.NoError(t, err)
		assert.Equal(t, got.Revision, a.PolicyRevision)
		assert.Equal(t, got.StoreID, "s1")
		assert.True(t, got.SyncedAt.Equal(now))

		pending, err = fs.OpenFGAPending(ctx, "s2", "m1", 10)
		require.NoError(t, err)
		assert.Len(t, pending, 2, "another store needs every organisation")
		pending, err = fs.OpenFGAPending(ctx, "s1", "m2", 10)
		require.NoError(t, err)
		assert.Len(t, pending, 2, "another model needs every organisation")

		require.NoError(t, st.TouchPolicy(ctx, "fs-a"))
		pending, err = fs.OpenFGAPending(ctx, "s1", "m1", 10)
		require.NoError(t, err)
		assert.Equal(t, ids(pending), []string{"fs-a"}, "a policy change makes the organisation pending")

		// A slow sync of an older revision never lowers the record.
		require.NoError(t, fs.RecordOpenFGASync(ctx, accounts.OpenFGASync{OrgID: "fs-a", StoreID: "s1", ModelID: "m1", Revision: a.PolicyRevision + 1, SyncedAt: now}, now))
		require.NoError(t, fs.RecordOpenFGASync(ctx, accounts.OpenFGASync{OrgID: "fs-a", StoreID: "s1", ModelID: "m1", Revision: a.PolicyRevision, SyncedAt: now}, now))
		got, err = fs.OpenFGASynced(ctx, "fs-a")
		require.NoError(t, err)
		assert.Equal(t, got.Revision, a.PolicyRevision+1)
		// A new store starts over.
		require.NoError(t, fs.RecordOpenFGASync(ctx, accounts.OpenFGASync{OrgID: "fs-a", StoreID: "s2", ModelID: "m1", Revision: 1, SyncedAt: now}, now))
		got, err = fs.OpenFGASynced(ctx, "fs-a")
		require.NoError(t, err)
		assert.Equal(t, got.Revision, int64(1))
		assert.Equal(t, got.StoreID, "s2")

		// Rebuild requests: kept until a sync that started after them.
		n, err := fs.RequestOpenFGARebuild(ctx, "fs-b", now.Add(time.Minute))
		require.NoError(t, err)
		assert.Equal(t, n, 1)
		got, err = fs.OpenFGASynced(ctx, "fs-b")
		require.NoError(t, err)
		assert.Equal(t, got.Revision, b.PolicyRevision, "a request never lowers the revision")
		require.True(t, got.RebuildRequested != nil)
		pending, err = fs.OpenFGAPending(ctx, "s1", "m1", 10)
		require.NoError(t, err)
		assert.Equal(t, ids(pending), []string{"fs-a", "fs-b"})
		require.NoError(t, fs.RecordOpenFGASync(ctx, accounts.OpenFGASync{OrgID: "fs-b", StoreID: "s1", ModelID: "m1", Revision: b.PolicyRevision, SyncedAt: now}, now))
		got, err = fs.OpenFGASynced(ctx, "fs-b")
		require.NoError(t, err)
		assert.True(t, got.RebuildRequested != nil, "a sync that started before the request does not clear it")
		require.NoError(t, fs.RecordOpenFGASync(ctx, accounts.OpenFGASync{OrgID: "fs-b", StoreID: "s1", ModelID: "m1", Revision: b.PolicyRevision, SyncedAt: now}, now.Add(2*time.Minute)))
		got, err = fs.OpenFGASynced(ctx, "fs-b")
		require.NoError(t, err)
		assert.True(t, got.RebuildRequested == nil)

		n, err = fs.RequestOpenFGARebuild(ctx, "", now)
		require.NoError(t, err)
		assert.Equal(t, n, 2, "every organisation")
		_, err = fs.RequestOpenFGARebuild(ctx, "fs-nope", now)
		assert.True(t, errors.Is(err, accounts.ErrNotFound))
		err = fs.RecordOpenFGASync(ctx, accounts.OpenFGASync{OrgID: "fs-nope", StoreID: "s1", ModelID: "m1", Revision: 1, SyncedAt: now}, now)
		assert.True(t, errors.Is(err, accounts.ErrNotFound))
		pending, err = fs.OpenFGAPending(ctx, "s1", "m1", 1)
		require.NoError(t, err)
		assert.Len(t, pending, 1, "the limit holds")
	})

	t.Run("account changes", func(t *testing.T) {
		// Suspending or resuming an account changes what its memberships
		// grant, so every organisation it belongs to needs a new sync.
		st, _, svc := setup(t)
		require.NoError(t, st.CreateUser(ctx, accounts.User{ID: "fs-admin", Name: "admin", Email: "fs-admin@example.com", CreatedAt: now}))
		require.NoError(t, st.SetPlatformAdmin(ctx, "fs-admin", true))
		admin, err := st.UserByID(ctx, "fs-admin")
		require.NoError(t, err)
		p := accounts.Principal{User: admin, Via: "session"}
		revisions := func() (int64, int64) {
			a, err := st.OrgByID(ctx, "fs-a")
			require.NoError(t, err)
			b, err := st.OrgByID(ctx, "fs-b")
			require.NoError(t, err)
			return a.PolicyRevision, b.PolicyRevision
		}
		a0, b0 := revisions()
		require.NoError(t, svc.SuspendUser(ctx, p, "fs-member"))
		a1, b1 := revisions()
		assert.True(t, a1 > a0, "suspension bumps the member's organisation")
		assert.Equal(t, b1, b0, "and no other")
		require.NoError(t, svc.ResumeUser(ctx, p, "fs-member"))
		a2, _ := revisions()
		assert.True(t, a2 > a1, "so does resuming")
		require.NoError(t, svc.SetPlatformAdmin(ctx, p, "fs-member", true))
		a3, _ := revisions()
		assert.True(t, a3 > a2, "and the platform administrator flag")
	})

	t.Run("policy", func(t *testing.T) {
		st, _, svc := setup(t)
		require.NoError(t, st.CreateTeam(ctx, accounts.Team{ID: "fs-team", OrgID: "fs-a", Slug: "t", Name: "t", CreatedAt: now}))
		require.NoError(t, st.CreateTeam(ctx, accounts.Team{ID: "fs-team-b", OrgID: "fs-b", Slug: "t", Name: "t", CreatedAt: now}))
		tuple := func(objType, obj, rel, subjType, subj, subjRel string) accounts.Tuple {
			return accounts.Tuple{ObjectType: objType, ObjectID: obj, Relation: rel, SubjectType: subjType, SubjectID: subj, SubjectRelation: subjRel, CreatedAt: now}
		}
		for _, x := range []accounts.Tuple{
			tuple(accounts.TypeTeam, "fs-team", accounts.TeamFull, accounts.TypeUser, "fs-member", ""),
			tuple(accounts.TypeTeam, "fs-team", accounts.TeamRead, accounts.TypeUser, "fs-gone", ""),
			tuple(accounts.TypeTeam, "fs-team", accounts.TeamRead, accounts.TypeUser, "fs-other", ""),
			tuple(accounts.TypeOrganisation, "fs-a", string(accounts.Full), accounts.TypeTeam, "fs-team", accounts.TeamRead),
			tuple(accounts.TypeOrganisation, "fs-a", string(accounts.Owner), accounts.TypeTeam, "fs-team-b", accounts.TeamRead),
		} {
			require.NoError(t, st.WriteTuple(ctx, x))
		}
		for _, r := range []accounts.Repository{
			{OrgID: "fs-a", Graph: "fs/shared", Visibility: accounts.Public, AddedBy: "fs-owner", AddedAt: now},
			{OrgID: "fs-a", Graph: "fs/mine", Visibility: accounts.Public, AddedBy: "fs-member", OwnerID: "fs-member", AddedAt: now},
			{OrgID: "fs-a", Graph: "fs/lost", Visibility: accounts.Public, AddedBy: "fs-owner", AddedAt: now},
		} {
			require.NoError(t, st.AddRepository(ctx, r))
		}
		for _, x := range []accounts.Tuple{
			tuple(accounts.TypeRepository, accounts.RepoObject("fs-a", "fs/shared"), accounts.RepoDeny, accounts.TypeUser, "fs-member", ""),
			tuple(accounts.TypeRepository, accounts.RepoObject("fs-a", "fs/shared"), accounts.RepoWrite, accounts.TypeTeam, "fs-team", accounts.TeamRead),
			tuple(accounts.TypeRepository, accounts.RepoObject("fs-a", "fs/shared"), accounts.RepoRead, accounts.TypeUser, "fs-gone", ""),
			tuple(accounts.TypeRepository, accounts.RepoObject("fs-a", "fs/lost"), accounts.RepoFull, accounts.TypeUser, "fs-member", ""),
		} {
			require.NoError(t, st.WriteTuple(ctx, x))
		}
		require.NoError(t, st.SetRepositoryAccess(ctx, "fs-a", "fs/lost", accounts.AccessLost, now))
		disabled := now
		require.NoError(t, st.SetUserDisabled(ctx, "fs-gone", &disabled))

		p, err := svc.OrgPolicy(ctx, "fs-a")
		require.NoError(t, err)
		assert.Equal(t, p.Org.ID, "fs-a")
		assert.Equal(t, p.Members, []accounts.PolicyMember{{UserID: "fs-guest", Role: accounts.Guest}, {UserID: "fs-member", Role: accounts.Write}, {UserID: "fs-owner", Role: accounts.Owner}}, "suspended members are left out")
		assert.Equal(t, p.Teams, []accounts.PolicyTeam{{ID: "fs-team", Role: accounts.Full, Members: []accounts.PolicyTeamMember{{UserID: "fs-member", Maintainer: true}}}},
			"only eligible members of the organisation's own teams; another organisation's team holds no role here")
		require.Len(t, p.Repos, 2, "lost entries are left out")
		assert.Equal(t, p.Repos[0], accounts.PolicyRepo{Graph: "fs/mine", Personal: true, Users: []accounts.PolicyGrant{{SubjectID: "fs-member", Role: accounts.Full}}}, "adding grants the adder full")
		assert.Equal(t, p.Repos[1], accounts.PolicyRepo{Graph: "fs/shared",
			Users: []accounts.PolicyGrant{{SubjectID: "fs-member", Role: accounts.Deny}, {SubjectID: "fs-owner", Role: accounts.Full}},
			Teams: []accounts.PolicyGrant{{SubjectID: "fs-team", Role: accounts.Write}}})

		_, err = svc.OrgPolicy(ctx, "fs-nope")
		assert.True(t, errors.Is(err, accounts.ErrNotFound))

		// Invalid policy is refused as a whole, never written in part.
		require.NoError(t, st.WriteTuple(ctx, tuple(accounts.TypeTeam, "fs-team", accounts.TeamRead, accounts.TypeUser, "fs-member", "")))
		_, err = svc.OrgPolicy(ctx, "fs-a")
		assert.True(t, errors.Is(err, accounts.ErrInvalidInput), "two memberships of one team")
	})
}
