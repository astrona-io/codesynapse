package query

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/astrona-io/codesynapse/internal/accounts"
	"github.com/astrona-io/codesynapse/internal/accounts/accountstest"
	acctmem "github.com/astrona-io/codesynapse/internal/accounts/memstore"
	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/store"
	"github.com/astrona-io/codesynapse/internal/store/memory"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

func accessFixture(t *testing.T) (*accounts.Service, accounts.Principal, accounts.Org) {
	t.Helper()
	ctx := context.Background()
	acc := accounts.NewService(acctmem.New(), true)
	_, err := accountstest.Bootstrap(ctx, acc)
	require.NoError(t, err)
	// Start with no pending projections: the setup organisation's is not under test.
	pending, err := acc.St.PendingAccessPolicies(ctx, 20)
	require.NoError(t, err)
	for _, o := range pending {
		require.NoError(t, acc.St.AcknowledgeAccessPolicy(ctx, o.ID, o.PolicyRevision))
	}
	u, org, err := acc.SignUp(ctx, "owner@example.com", "Owner", "correct horse battery staple")
	require.NoError(t, err)
	require.NoError(t, acc.St.AddRepository(ctx, accounts.Repository{OrgID: org.ID, Graph: "example/repo", Visibility: accounts.Public, AddedBy: u.ID, AddedAt: time.Now()}))
	return acc, accounts.Principal{User: u, Via: "session"}, org
}

func TestAccessSnapshotMatchesAuthoritativePermissions(t *testing.T) {
	ctx := context.Background()
	acc, owner, org := accessFixture(t)
	require.NoError(t, acc.SetDefaultRepoAccess(ctx, owner, org.ID, accounts.DefaultRepoReadOnly))
	member, _, err := acc.SignUp(ctx, "member@example.com", "Member", "correct horse battery staple")
	require.NoError(t, err)
	guest, _, err := acc.SignUp(ctx, "guest@example.com", "Guest", "correct horse battery staple")
	require.NoError(t, err)
	for _, entry := range []struct {
		user accounts.User
		role accounts.Role
	}{{member, accounts.Member}, {guest, accounts.Guest}} {
		require.NoError(t, accountstest.Join(ctx, t, acc, owner, org.ID, entry.user, entry.role))
	}
	team, err := acc.CreateTeam(ctx, owner, org.ID, "Editors")
	require.NoError(t, err)
	_, err = acc.SetTeamMember(ctx, owner, team.ID, member.Email, accounts.RelMember)
	require.NoError(t, err)
	_, err = acc.SetRepoGrant(ctx, owner, org.ID, "example/repo", accounts.Tuple{SubjectType: accounts.TypeTeam, SubjectID: team.ID, SubjectRelation: accounts.RelMember}, accounts.Member)
	require.NoError(t, err)

	check := func(wantMember int) {
		t.Helper()
		snapshot, err := AccessSnapshot(ctx, acc.St, org.ID)
		require.NoError(t, err)
		assert.Equal(t, snapshot.Default, "READ_ONLY")
		assert.Len(t, snapshot.Users, 3)
		assert.Len(t, snapshot.Teams, 1)
		for _, tc := range []struct {
			user string
			rank int
		}{{owner.User.ID, 3}, {member.ID, wantMember}, {guest.ID, 0}, {"unknown", 0}} {
			decision := snapshot.Evaluate(tc.user, "example/repo")
			assert.Equal(t, decision.Rank, tc.rank, tc.user)
		}
	}
	check(2)
	_, err = acc.SetRepoGrant(ctx, owner, org.ID, "example/repo", accounts.Tuple{SubjectType: accounts.TypeUser, SubjectID: member.ID}, accounts.None)
	require.NoError(t, err)
	check(0)
	require.NoError(t, acc.St.SetRepositoryAccess(ctx, org.ID, "example/repo", accounts.AccessLost, time.Now()))
	snapshot, err := AccessSnapshot(ctx, acc.St, org.ID)
	require.NoError(t, err)
	assert.Equal(t, snapshot.Evaluate(owner.User.ID, "example/repo").Rank, 0)
}

type changingProjectionSource struct {
	accounts.Store
	reads int
}

func (s *changingProjectionSource) OrgByID(ctx context.Context, id string) (accounts.Org, error) {
	s.reads++
	if s.reads == 2 {
		if err := s.TouchPolicy(ctx, id); err != nil {
			return accounts.Org{}, err
		}
	}
	return s.Store.OrgByID(ctx, id)
}

func TestAccessSnapshotRejectsConcurrentPolicyChange(t *testing.T) {
	acc, _, org := accessFixture(t)
	_, err := AccessSnapshot(context.Background(), &changingProjectionSource{Store: acc.St}, org.ID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "changed during projection")
}

type projectionTarget struct {
	store.AccessStore
	failure error
	after   func(context.Context, model.AccessSnapshot) error
}

func (s *projectionTarget) ReplaceAccess(ctx context.Context, snapshot model.AccessSnapshot) error {
	if s.failure != nil {
		return s.failure
	}
	if err := s.AccessStore.ReplaceAccess(ctx, snapshot); err != nil {
		return err
	}
	if s.after != nil {
		return s.after(ctx, snapshot)
	}
	return nil
}

func TestProjectionOutboxRetriesAndKeepsNewerRevisions(t *testing.T) {
	ctx := context.Background()
	acc, owner, org := accessFixture(t)
	target := &projectionTarget{AccessStore: memory.New(), failure: errors.New("projection unavailable")}
	assert.Error(t, ProjectAccessPolicies(ctx, acc.St, target, 20))
	pending, err := acc.St.PendingAccessPolicies(ctx, 20)
	require.NoError(t, err)
	require.Len(t, pending, 1, "an unavailable graph must leave the revision pending")
	oldRevision := pending[0].PolicyRevision
	decision, err := target.AccessAt(ctx, org.ID, oldRevision, owner.User.ID, "example/repo")
	require.NoError(t, err)
	assert.Equal(t, decision.Rank, 0)

	// A change committed after publication must survive acknowledgement of the old revision.
	target.failure = nil
	target.after = func(ctx context.Context, s model.AccessSnapshot) error { return acc.St.TouchPolicy(ctx, s.TenantID) }
	require.NoError(t, ProjectAccessPolicies(ctx, acc.St, target, 20))
	pending, err = acc.St.PendingAccessPolicies(ctx, 20)
	require.NoError(t, err)
	require.Len(t, pending, 1)
	assert.True(t, pending[0].PolicyRevision > oldRevision)
	newRevision := pending[0].PolicyRevision
	decision, err = target.AccessAt(ctx, org.ID, newRevision, owner.User.ID, "example/repo")
	require.NoError(t, err)
	assert.Equal(t, decision.Rank, 0, "the published older revision cannot satisfy a current lookup")

	target.after = nil
	require.NoError(t, ProjectAccessPolicies(ctx, acc.St, target, 20))
	pending, err = acc.St.PendingAccessPolicies(ctx, 20)
	require.NoError(t, err)
	assert.Len(t, pending, 0)
	decision, err = target.AccessAt(ctx, org.ID, newRevision, owner.User.ID, "example/repo")
	require.NoError(t, err)
	assert.Equal(t, decision.Rank, 3)
	decision, err = target.AccessAt(ctx, org.ID, oldRevision, owner.User.ID, "example/repo")
	require.NoError(t, err)
	assert.Equal(t, decision.Rank, 0, "replaced revisions are no longer queryable")
}
