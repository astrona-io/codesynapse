package fgasync

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/astrona-io/codesynapse/internal/accounts"
	"github.com/astrona-io/codesynapse/internal/accounts/memstore"
	"github.com/astrona-io/codesynapse/internal/authz/fga"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

// TestParityWithAccessOf is the heart of phase 5: it applies real policy
// changes to the accounts store, syncs them to a real OpenFGA server (`just
// test-openfga` starts a throwaway one) and checks after every step that
// OpenFGA answers exactly what the Go evaluator (AccessOf) decides, for
// every user and every repository entry, lost ones included, for every
// permission of each and for every organisation level. Custom roles are
// part of it: a direct custom grant replaces team grants and the default,
// team custom roles add up, a grant naming a missing role gives nothing,
// and changing a role rewrites what OpenFGA holds for its grants.
func TestParityWithAccessOf(t *testing.T) {
	url, token := os.Getenv("CODESYNAPSE_TEST_OPENFGA_URL"), os.Getenv("CODESYNAPSE_TEST_OPENFGA_TOKEN")
	if url == "" {
		t.Skip("CODESYNAPSE_TEST_OPENFGA_URL not set (just test-openfga)")
	}
	ctx := context.Background()
	c, err := fga.New(fga.Config{URL: url, Token: token, Timeout: 10 * time.Second})
	require.NoError(t, err)
	var b [6]byte
	_, _ = rand.Read(b[:])
	_, err = c.EnsureStore(ctx, "parity-"+hex.EncodeToString(b[:]))
	require.NoError(t, err)
	_, err = c.EnsureModel(ctx)
	require.NoError(t, err)

	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	st := memstore.New()
	svc := accounts.NewService(st, false)
	svc.Now = func() time.Time { return now }
	fs, ok := accounts.Store(st).(accounts.OpenFGASyncStore)
	require.True(t, ok)
	s := &Syncer{Client: c, Records: fs, Policy: svc.OrgPolicy}
	decided := accounts.NewService(st, false)
	decided.Now = svc.Now
	decided.Decider = &Decider{Records: fs, Select: func(context.Context) (Checker, error) { return c, nil }}

	users := []string{"owner", "alice", "bob", "carol", "dave", "erin"}
	for _, id := range users {
		require.NoError(t, st.CreateUser(ctx, accounts.User{ID: id, Name: id, Email: id + "@example.com", CreatedAt: now}))
	}
	orgs := []string{"o1", "o2"}
	require.NoError(t, st.CreateOrg(ctx, accounts.Org{ID: "o1", Name: "o1", Slug: "o1", CreatedAt: now}, "owner"))
	require.NoError(t, st.CreateOrg(ctx, accounts.Org{ID: "o2", Name: "o2", Slug: "o2", CreatedAt: now}, "dave"))
	for _, m := range []struct {
		org, user string
		role      accounts.Role
	}{
		{"o1", "alice", accounts.Write}, {"o1", "bob", accounts.Read}, {"o1", "carol", accounts.Guest},
		{"o1", "erin", accounts.Read}, {"o2", "alice", accounts.Read},
	} {
		require.NoError(t, st.SetMember(ctx, m.org, m.user, m.role))
	}
	require.NoError(t, st.SetDefaultRepoAccess(ctx, "o1", accounts.DefaultRepoReadOnly))
	require.NoError(t, st.CreateTeam(ctx, accounts.Team{ID: "t1", OrgID: "o1", Slug: "t1", Name: "t1", CreatedAt: now}))
	require.NoError(t, st.CreateTeam(ctx, accounts.Team{ID: "t2", OrgID: "o2", Slug: "t2", Name: "t2", CreatedAt: now}))
	type entry struct{ org, graph string }
	repos := []entry{{"o1", "g/shared"}, {"o1", "g/other"}, {"o1", "g/mine"}, {"o1", "g/extra"}, {"o2", "g/theirs"}}
	for _, r := range []accounts.Repository{
		{OrgID: "o1", Graph: "g/shared", Visibility: accounts.Public, AddedBy: "owner", AddedAt: now},
		{OrgID: "o1", Graph: "g/other", Visibility: accounts.Public, AddedBy: "owner", AddedAt: now},
		{OrgID: "o1", Graph: "g/extra", Visibility: accounts.Public, AddedBy: "owner", AddedAt: now},
		{OrgID: "o2", Graph: "g/theirs", Visibility: accounts.Public, AddedBy: "dave", AddedAt: now},
	} {
		require.NoError(t, st.AddRepository(ctx, r))
	}

	tuple := func(objType, obj, rel, subjType, subj, subjRel string) accounts.Tuple {
		return accounts.Tuple{ObjectType: objType, ObjectID: obj, Relation: rel, SubjectType: subjType, SubjectID: subj, SubjectRelation: subjRel, CreatedAt: now}
	}
	grant := func(org, graph, rel, subjType, subj string) accounts.Tuple {
		subjRel := ""
		if subjType == accounts.TypeTeam {
			subjRel = accounts.TeamRead
		}
		return tuple(accounts.TypeRepository, accounts.RepoObject(org, graph), rel, subjType, subj, subjRel)
	}
	write := func(ts ...accounts.Tuple) {
		for _, x := range ts {
			require.NoError(t, st.WriteTuple(ctx, x))
		}
	}
	role := func(id, org, resource string, ps ...accounts.Permission) accounts.CustomRole {
		return accounts.CustomRole{ID: id, OrgID: org, Name: id, Resource: resource, Permissions: ps,
			CreatedBy: "owner", CreatedAt: now, UpdatedAt: now}
	}
	custom := accounts.CustomRelation
	revoke := func(ts ...accounts.Tuple) {
		for _, x := range ts {
			_, err := st.DeleteTuples(ctx, accounts.TupleFilter{ObjectType: x.ObjectType, ObjectID: x.ObjectID,
				SubjectType: x.SubjectType, SubjectID: x.SubjectID, SubjectRelation: x.SubjectRelation})
			require.NoError(t, err)
		}
	}

	// fact pins what AccessOf decides after a step, so the fixtures keep
	// covering the cases they are named for rather than only agreeing.
	type fact struct {
		user  string
		repo  entry
		perm  accounts.Permission
		holds bool
	}
	facts := map[string][]fact{
		"custom roles": {
			{"bob", entry{"o1", "g/other"}, accounts.RepoReviewsWrite, true},
			{"bob", entry{"o1", "g/other"}, accounts.RepoMemoryWrite, false},
			{"carol", entry{"o1", "g/shared"}, accounts.RepoMemoryFull, true},
			{"carol", entry{"o1", "g/shared"}, accounts.RepoReviewsWrite, true},
			{"erin", entry{"o1", "g/shared"}, accounts.RepoReviewsWrite, true},
			{"erin", entry{"o1", "g/other"}, accounts.RepoCodeRead, false},
			{"erin", entry{"o1", "g/extra"}, accounts.RepoCodeRead, false},
			{"bob", entry{"o1", "g/extra"}, accounts.RepoSettingsFull, false},
		},
		"role changed": {
			{"bob", entry{"o1", "g/other"}, accounts.RepoReviewsWrite, false},
			{"bob", entry{"o1", "g/other"}, accounts.RepoFixesWrite, true},
		},
		"role deleted while granted": {
			{"carol", entry{"o1", "g/shared"}, accounts.RepoMemoryFull, false},
		},
	}
	steps := []struct {
		name   string
		change func()
	}{
		{"initial", func() {}},
		{"grant", func() {
			write(grant("o1", "g/shared", accounts.RepoFull, accounts.TypeUser, "bob"),
				grant("o1", "g/other", accounts.RepoRead, accounts.TypeUser, "carol"))
		}},
		{"revoke", func() { revoke(grant("o1", "g/shared", accounts.RepoFull, accounts.TypeUser, "bob")) }},
		{"team change", func() {
			write(tuple(accounts.TypeTeam, "t1", accounts.TeamRead, accounts.TypeUser, "alice", ""),
				tuple(accounts.TypeTeam, "t1", accounts.TeamFull, accounts.TypeUser, "bob", ""),
				tuple(accounts.TypeTeam, "t1", accounts.TeamRead, accounts.TypeUser, "carol", ""),
				tuple(accounts.TypeTeam, "t2", accounts.TeamRead, accounts.TypeUser, "alice", ""),
				grant("o1", "g/other", accounts.RepoWrite, accounts.TypeTeam, "t1"),
				grant("o2", "g/theirs", accounts.RepoFull, accounts.TypeTeam, "t2"),
				tuple(accounts.TypeOrganisation, "o1", string(accounts.Full), accounts.TypeTeam, "t1", accounts.TeamRead))
		}},
		{"team member removed", func() {
			_, err := st.DeleteTuples(ctx, accounts.TupleFilter{ObjectType: accounts.TypeTeam, ObjectID: "t1", SubjectType: accounts.TypeUser, SubjectID: "alice"})
			require.NoError(t, err)
		}},
		{"deny", func() { write(grant("o1", "g/shared", accounts.RepoDeny, accounts.TypeUser, "alice")) }},
		{"default change", func() { require.NoError(t, st.SetDefaultRepoAccess(ctx, "o1", accounts.DefaultRepoNone)) }},
		{"default back", func() { require.NoError(t, st.SetDefaultRepoAccess(ctx, "o1", accounts.DefaultRepoReadOnly)) }},
		{"suspension", func() {
			at := now
			require.NoError(t, st.SetUserDisabled(ctx, "erin", &at))
		}},
		{"member removal", func() { require.NoError(t, st.RemoveMember(ctx, "o1", "bob")) }},
		{"personal entry", func() {
			require.NoError(t, st.AddRepository(ctx, accounts.Repository{OrgID: "o1", Graph: "g/mine", Visibility: accounts.Public, AddedBy: "alice", OwnerID: "alice", AddedAt: now}))
			write(grant("o1", "g/mine", accounts.RepoRead, accounts.TypeUser, "owner"))
		}},
		{"lost entry", func() { require.NoError(t, st.SetRepositoryAccess(ctx, "o1", "g/shared", accounts.AccessLost, now)) }},
		{"restored", func() {
			require.NoError(t, st.SetRepositoryAccess(ctx, "o1", "g/shared", accounts.AccessOK, now))
			require.NoError(t, st.SetUserDisabled(ctx, "erin", nil))
			require.NoError(t, st.SetMember(ctx, "o1", "bob", accounts.Read))
		}},
		{"custom roles", func() {
			for _, r := range []accounts.CustomRole{
				role("r-rev", "o1", accounts.ResourceRepository, accounts.RepoCodeRead, accounts.RepoReviewsWrite, accounts.RepoSecurityRead),
				role("r-mem", "o1", accounts.ResourceRepository, accounts.RepoCodeRead, accounts.RepoMemoryFull),
				role("r-aud", "o1", accounts.ResourceOrganisation, accounts.OrgAuditRead, accounts.OrgMembersRead),
				role("r-far", "o2", accounts.ResourceRepository, accounts.RepoCodeRead, accounts.RepoSettingsFull),
			} {
				require.NoError(t, st.CreateCustomRole(ctx, r))
			}
			require.NoError(t, st.CreateTeam(ctx, accounts.Team{ID: "t3", OrgID: "o1", Slug: "t3", Name: "t3", CreatedAt: now}))
			write(tuple(accounts.TypeTeam, "t3", accounts.TeamRead, accounts.TypeUser, "erin", ""),
				tuple(accounts.TypeTeam, "t3", accounts.TeamRead, accounts.TypeUser, "carol", ""),
				// A direct custom role replaces bob's team grant on g/other.
				grant("o1", "g/other", custom("r-rev"), accounts.TypeUser, "bob"),
				// Team custom roles add up (carol is in t1 and t3) and replace
				// the default for erin.
				grant("o1", "g/shared", custom("r-mem"), accounts.TypeTeam, "t1"),
				grant("o1", "g/shared", custom("r-rev"), accounts.TypeTeam, "t3"),
				// A missing role and another organisation's give nothing, and
				// still replace the default.
				grant("o1", "g/other", custom("missing"), accounts.TypeUser, "erin"),
				grant("o1", "g/extra", custom("gone"), accounts.TypeTeam, "t3"),
				grant("o1", "g/extra", custom("r-far"), accounts.TypeUser, "bob"),
				// Organisation roles add to the built-in role, also for a guest
				// through a team; another organisation's role gives nothing.
				tuple(accounts.TypeOrganisation, "o1", custom("r-aud"), accounts.TypeUser, "bob", ""),
				tuple(accounts.TypeOrganisation, "o1", custom("r-aud"), accounts.TypeTeam, "t3", accounts.TeamRead),
				tuple(accounts.TypeOrganisation, "o2", custom("r-aud"), accounts.TypeUser, "alice", ""))
		}},
		{"role changed", func() {
			require.NoError(t, st.UpdateCustomRole(ctx, role("r-rev", "o1", accounts.ResourceRepository, accounts.RepoCodeRead, accounts.RepoFixesWrite)))
			require.NoError(t, st.UpdateCustomRole(ctx, role("r-aud", "o1", accounts.ResourceOrganisation, accounts.OrgRolesFull)))
		}},
		{"role deleted while granted", func() { require.NoError(t, st.DeleteCustomRole(ctx, "o1", "r-mem")) }},
		{"team deleted", func() { require.NoError(t, st.DeleteTeam(ctx, "t1")) }},
	}

	for _, step := range steps {
		step.change()
		for _, o := range orgs {
			require.NoError(t, st.TouchPolicy(ctx, o))
		}
		done, err := s.SyncPending(ctx, 10)
		require.NoError(t, err)
		require.Equal(t, done, len(orgs), "step %q: every organisation synced", step.name)

		var checks []fga.Tuple
		var want []bool
		for _, u := range users {
			a, err := svc.AccessOf(ctx, u)
			if errors.Is(err, accounts.ErrSuspended) {
				a = accounts.Access{}
			} else {
				require.NoError(t, err)
			}
			perms := map[entry]accounts.Perms{}
			for _, r := range a.Repos {
				perms[entry{r.OrgID, r.Graph}] = r.Perms
			}
			for _, f := range facts[step.name] {
				if f.user == u {
					assert.Equal(t, perms[f.repo].Has(f.perm), f.holds, fmt.Sprintf("step %q: AccessOf %s %s %v", step.name, u, f.perm, f.repo))
				}
			}
			for _, r := range repos {
				for _, p := range accounts.AllPermissions() {
					if p.Resource() == accounts.ResourceRepository {
						checks = append(checks, fga.Tuple{User: "user:" + u, Relation: Relation(p), Object: "repository:" + fga.RepositoryID(r.org, r.graph)})
						want = append(want, perms[r].Has(p))
					}
				}
			}
			for _, o := range orgs {
				for _, level := range []accounts.Role{accounts.Read, accounts.Write, accounts.Full, accounts.Owner} {
					checks = append(checks, fga.Tuple{User: "user:" + u, Relation: string(level), Object: "organisation:" + o})
					want = append(want, a.Orgs[o].AtLeast(level))
				}
				for _, p := range accounts.AllPermissions() {
					if p.Resource() == accounts.ResourceOrganisation && !p.Marker() {
						checks = append(checks, fga.Tuple{User: "user:" + u, Relation: Relation(p), Object: "organisation:" + o})
						want = append(want, a.OrgPerms[o].Has(p))
					}
				}
				_, member := a.Orgs[o]
				checks = append(checks, fga.Tuple{User: "user:" + u, Relation: "active", Object: "organisation:" + o})
				want = append(want, member)
			}
		}
		var got []bool
		for batch := range slices.Chunk(checks, fga.MaxBatchCheck) {
			g, err := c.BatchCheck(ctx, batch)
			require.NoError(t, err)
			got = append(got, g...)
		}
		for i := range checks {
			assert.Equal(t, got[i], want[i], fmt.Sprintf("step %q: %s %s %s", step.name, checks[i].User, checks[i].Relation, checks[i].Object))
		}

		for _, o := range orgs {
			res, err := s.Verify(ctx, o)
			require.NoError(t, err)
			assert.False(t, res.Drift(), "step %q: %s has no drift after a sync", step.name, o)
			org, err := st.OrgByID(ctx, o)
			require.NoError(t, err)
			require.NoError(t, Fresh(ctx, fs, c.StoreID(), c.ModelID(), org))
		}

		// The Decider gives every user the same access as the Go evaluator.
		for _, u := range users {
			want, werr := svc.AccessOf(ctx, u)
			got, gerr := decided.AccessOf(ctx, u)
			if errors.Is(werr, accounts.ErrSuspended) {
				assert.True(t, errors.Is(gerr, accounts.ErrSuspended), fmt.Sprintf("step %q: %s suspended", step.name, u))
				continue
			}
			require.NoError(t, werr)
			require.NoError(t, gerr)
			assert.Equal(t, fmt.Sprint(got.Orgs), fmt.Sprint(want.Orgs), fmt.Sprintf("step %q: %s roles", step.name, u))
			assert.Equal(t, fmt.Sprint(got.OrgPerms), fmt.Sprint(want.OrgPerms), fmt.Sprintf("step %q: %s organisation permissions", step.name, u))
			perms := func(a accounts.Access) map[entry]string {
				m := map[entry]string{}
				for _, r := range a.Repos {
					m[entry{r.OrgID, r.Graph}] = fmt.Sprint(r.Perms)
				}
				return m
			}
			assert.Equal(t, fmt.Sprint(perms(got)), fmt.Sprint(perms(want)), fmt.Sprintf("step %q: %s repository permissions", step.name, u))
		}
	}
}
