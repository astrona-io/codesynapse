package accountstest

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/astrona-io/codesynapse/internal/accounts"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

// SeedAccessBenchmark creates a maximum-size team set without password hashing.
func SeedAccessBenchmark(tb testing.TB, st accounts.Store, suffix ...string) (string, string) {
	tb.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	prefix := "bounded"
	if len(suffix) > 0 {
		prefix += "-" + suffix[0]
	}
	userID, orgID := prefix+"-user", prefix+"-org"
	require.NoError(tb, st.PolicyTransaction(ctx, "", func(tx accounts.Store) error {
		if err := tx.CreateUser(ctx, accounts.User{ID: userID, Email: prefix + "@example.com", Name: "Bounded", CreatedAt: now}); err != nil {
			return err
		}
		if err := tx.CreateOrg(ctx, accounts.Org{ID: orgID, Slug: orgID, Name: "Bounded", CreatedAt: now}, userID); err != nil {
			return err
		}
		for i := range 100 {
			if err := tx.AddRepository(ctx, accounts.Repository{OrgID: orgID, Graph: fmt.Sprintf(prefix+"/repo-%03d", i), Visibility: accounts.Public, AddedBy: userID, AddedAt: now}); err != nil {
				return err
			}
		}
		for i := range accounts.MaxTeams {
			id := fmt.Sprintf(prefix+"-team-%03d", i)
			if err := tx.CreateTeam(ctx, accounts.Team{ID: id, OrgID: orgID, Slug: id, Name: id, CreatedAt: now}); err != nil {
				return err
			}
			if err := tx.WriteTuple(ctx, accounts.Tuple{ObjectType: accounts.TypeTeam, ObjectID: id, Relation: accounts.RelMember, SubjectType: accounts.TypeUser, SubjectID: userID, CreatedAt: now}); err != nil {
				return err
			}
			if err := tx.WriteTuple(ctx, accounts.Tuple{ObjectType: accounts.TypeRepository, ObjectID: accounts.RepoObject(orgID, prefix+"/repo-000"), Relation: accounts.RepoAdmin, SubjectType: accounts.TypeTeam, SubjectID: id, SubjectRelation: accounts.RelMember, CreatedAt: now}); err != nil {
				return err
			}
		}
		return nil
	}))
	return userID, orgID
}

// RunAccessBounds checks that growing the membership set cannot cause partial access.
func RunAccessBounds(t *testing.T, st accounts.Store) {
	ctx := context.Background()
	userID, orgID := SeedAccessBenchmark(t, st)
	calls := 0
	counted := countingAccessStore{Store: st, calls: &calls}
	svc := accounts.NewService(counted, true)
	a, err := svc.AccessOf(ctx, userID)
	require.NoError(t, err)
	assert.Len(t, a.Repos, 100)
	assert.Equal(t, calls, 7, "bounded bulk reads at 128 teams and 100 repositories")
	now := time.Now().UTC()
	extra := "bounded-extra"
	require.NoError(t, st.CreateTeam(ctx, accounts.Team{ID: extra, OrgID: orgID, Slug: extra, Name: extra, CreatedAt: now}))
	require.NoError(t, st.WriteTuple(ctx, accounts.Tuple{ObjectType: accounts.TypeTeam, ObjectID: extra, Relation: accounts.RelMember, SubjectType: accounts.TypeUser, SubjectID: userID, CreatedAt: now}))
	_, err = svc.AccessOf(ctx, userID)
	assert.Equal(t, err, accounts.ErrAccessLimit)
}

type countingAccessStore struct {
	accounts.Store
	calls *int
}

func (s countingAccessStore) PolicyTransaction(ctx context.Context, orgID string, fn func(accounts.Store) error) error {
	return s.Store.PolicyTransaction(ctx, orgID, func(tx accounts.Store) error { return fn(countingAccessStore{Store: tx, calls: s.calls}) })
}
func (s countingAccessStore) UserByID(ctx context.Context, id string) (accounts.User, error) {
	*s.calls++
	return s.Store.UserByID(ctx, id)
}
func (s countingAccessStore) UserMemberships(ctx context.Context, id string) ([]accounts.Membership, error) {
	*s.calls++
	return s.Store.UserMemberships(ctx, id)
}
func (s countingAccessStore) ReadTuples(ctx context.Context, f accounts.TupleFilter) ([]accounts.Tuple, error) {
	*s.calls++
	return s.Store.ReadTuples(ctx, f)
}
func (s countingAccessStore) TeamsByIDs(ctx context.Context, ids []string) ([]accounts.Team, error) {
	*s.calls++
	return s.Store.TeamsByIDs(ctx, ids)
}
func (s countingAccessStore) RepositoriesForOrgs(ctx context.Context, ids []string) ([]accounts.Repository, error) {
	*s.calls++
	return s.Store.RepositoriesForOrgs(ctx, ids)
}

func (s countingAccessStore) ReadSnapshot(ctx context.Context, fn func(accounts.Store) error) error {
	return s.Store.ReadSnapshot(ctx, func(tx accounts.Store) error { return fn(countingAccessStore{Store: tx, calls: s.calls}) })
}
