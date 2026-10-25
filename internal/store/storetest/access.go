package storetest

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/store"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

func accessContract(t *testing.T, db interface {
	store.AccessStore
	store.Reader
}) {
	ctx := context.Background()
	x := model.AccessSnapshot{TenantID: fmt.Sprintf("access-fixture-%d", time.Now().UnixNano()), Revision: 1, Default: "READ_ONLY",
		Users: []model.AccessUser{{ID: "reader", Active: true, InheritsDefault: true}, {ID: "denied", Active: true, InheritsDefault: true}, {ID: "team", Active: true}, {ID: "guest", Active: true}, {ID: "default", Active: true, InheritsDefault: true}, {ID: "suspended", InheritsDefault: true}},
		Teams: []string{"admins", "writers"}, Repos: []model.AccessRepo{{ID: "r", GraphID: "shared", Usable: true}, {ID: "lost", GraphID: "lost", Usable: false}},
		Members: []model.AccessMembership{{UserID: "reader", TeamID: "admins"}, {UserID: "denied", TeamID: "admins"}, {UserID: "team", TeamID: "writers"}, {UserID: "team", TeamID: "admins"}},
		Grants:  []model.AccessGrant{{SubjectKind: model.KindTeam, SubjectID: "admins", RepoID: "r", Role: "ADMIN"}, {SubjectKind: model.KindTeam, SubjectID: "writers", RepoID: "r", Role: "WRITE"}, {SubjectKind: model.KindUser, SubjectID: "reader", RepoID: "r", Role: "READ"}, {SubjectKind: model.KindUser, SubjectID: "denied", RepoID: "r", Role: "NONE"}}}
	require.NoError(t, db.ReplaceAccess(ctx, x))
	require.NoError(t, db.ReplaceAccess(ctx, x), "retry is idempotent")
	reordered := x
	reordered.Users = slices.Clone(x.Users)
	reordered.Grants = slices.Clone(x.Grants)
	slices.Reverse(reordered.Users)
	slices.Reverse(reordered.Grants)
	require.NoError(t, db.ReplaceAccess(ctx, reordered), "row order does not change a policy")
	conflict := x
	conflict.Grants = nil
	assert.True(t, db.ReplaceAccess(ctx, conflict) != nil, "a published revision is immutable")

	for _, tc := range []struct {
		user  string
		rank  int
		graph string
	}{{"reader", 1, "shared"}, {"denied", 0, "shared"}, {"team", 3, "shared"}, {"guest", 0, "shared"}, {"default", 1, "shared"}, {"suspended", 0, ""}, {"platform", 0, ""}} {
		t.Run(tc.user, func(t *testing.T) {
			got, err := db.AccessAt(ctx, x.TenantID, 1, tc.user, "r")
			require.NoError(t, err)
			assert.Equal(t, got, model.AccessDecision{GraphID: tc.graph, Rank: tc.rank})
		})
	}
	for _, args := range []struct {
		tenant, user, repo string
		revision           int64
	}{{x.TenantID, "reader", "lost", 1}, {x.TenantID, "reader", "r", 2}, {x.TenantID + "foreign", "reader", "r", 1}, {x.TenantID, "reader", "' OR 1=1", 1}} {
		got, err := db.AccessAt(ctx, args.tenant, args.revision, args.user, args.repo)
		require.NoError(t, err)
		assert.Equal(t, got.Rank, 0)
	}
	y := x
	y.TenantID += "-second"
	y.Default = "NONE"
	y.Grants = nil
	y.Members = nil
	require.NoError(t, db.ReplaceAccess(ctx, y))
	got, err := db.AccessAt(ctx, y.TenantID, 1, "reader", "r")
	require.NoError(t, err)
	assert.Equal(t, got.Rank, 0)
	x.Revision = 2
	x.Default = "NONE"
	x.Grants = nil
	require.NoError(t, db.ReplaceAccess(ctx, x))
	got, err = db.AccessAt(ctx, x.TenantID, 1, "reader", "r")
	require.NoError(t, err)
	assert.Equal(t, got.Rank, 0, "old revision cannot explain access")
	got, err = db.AccessAt(ctx, x.TenantID, 2, "reader", "r")
	require.NoError(t, err)
	assert.Equal(t, got.Rank, 0, "revocation reaches projection")
	x.Revision = 1
	x.Default = "READ_ONLY"
	require.NoError(t, db.ReplaceAccess(ctx, x))
	got, err = db.AccessAt(ctx, x.TenantID, 1, "reader", "r")
	require.NoError(t, err)
	assert.Equal(t, got.Rank, 0, "late delivery cannot restore an old revision")
	// Overlapping rebuilds must never move the visible revision backwards.
	older, newer := x, x
	older.Revision, newer.Revision = 3, 4
	older.Default, newer.Default = "READ_ONLY", "NONE"
	errors := make(chan error, 2)
	go func() { errors <- db.ReplaceAccess(ctx, older) }()
	go func() { errors <- db.ReplaceAccess(ctx, newer) }()
	for range 2 {
		require.NoError(t, <-errors)
	}
	got, err = db.AccessAt(ctx, x.TenantID, 4, "reader", "r")
	require.NoError(t, err)
	assert.Equal(t, got, model.AccessDecision{GraphID: "shared", Rank: 0})
	got, err = db.AccessAt(ctx, x.TenantID, 3, "reader", "r")
	require.NoError(t, err)
	assert.Equal(t, got, model.AccessDecision{})
	x.Revision = 5
	x.Grants = []model.AccessGrant{{SubjectKind: model.KindUser, SubjectID: "foreign", RepoID: "r", Role: "ADMIN"}}
	assert.True(t, db.ReplaceAccess(ctx, x) != nil, "foreign subject rejected")
	// Access vertices never appear in normal code queries or repository lists.
	_, err = db.Node(ctx, model.AccessVertexID(x.TenantID, 2, x.Digest(), model.KindUser, "reader"))
	assert.True(t, err != nil)
}
