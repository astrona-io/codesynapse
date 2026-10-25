package hugegraphstore

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/astrona-io/codesynapse/internal/curation"
	"github.com/astrona-io/codesynapse/internal/history"
	"github.com/astrona-io/codesynapse/internal/memory"
	"github.com/astrona-io/codesynapse/internal/reviews"
	"github.com/astrona-io/codesynapse/internal/store/storetest"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
	"github.com/astrona-io/codesynapse/internal/usage"
)

func TestProvisionRoleRejects(t *testing.T) {
	s := &Store{}
	for _, r := range []Role{
		{User: "admin", Password: "0123456789abcdef"},
		{User: "Bad User", Password: "0123456789abcdef"},
		{User: "cs_mcp", Password: "short"},
		{User: "cs_mcp", Password: "0123456789abcdef", WriteVertexLabels: []string{"x') or ('1"}},
	} {
		assert.Error(t, s.ProvisionRole(context.Background(), r), "%+v", r)
	}
}

// TestLimitedRole needs a HugeGraph with authentication on (just
// test-hugegraph-auth). The limited user reads everything, writes the labels
// of the MCP role and is refused on code nodes.
func TestLimitedRole(t *testing.T) {
	url, pw := os.Getenv("CODESYNAPSE_TEST_HUGEGRAPH_URL"), os.Getenv("CODESYNAPSE_TEST_HUGEGRAPH_PASSWORD")
	if url == "" || pw == "" {
		t.Skip("needs CODESYNAPSE_TEST_HUGEGRAPH_URL and _PASSWORD (an auth-enabled HugeGraph)")
	}
	ctx := context.Background()
	admin, err := New(ctx, Config{URL: url, User: "admin", Password: pw, TxCapacity: testTxCapacity()})
	require.NoError(t, err)
	t.Cleanup(func() { _ = admin.Close(ctx) })
	require.NoError(t, admin.EnsureSchema(ctx))
	require.NoError(t, admin.ReplaceRepo(ctx, storetest.Repo, storetest.Fixture()))

	const rolePW = "role-password-0123456789"
	role := MCPRole("cs_mcp_test", rolePW)
	require.NoError(t, admin.ProvisionRole(ctx, role))
	require.NoError(t, admin.ProvisionRole(ctx, role), "provisioning is idempotent")

	lim, err := New(ctx, Config{URL: url, User: role.User, Password: rolePW, TxCapacity: testTxCapacity()})
	require.NoError(t, err)
	t.Cleanup(func() { _ = lim.Close(ctx) })

	repos, err := lim.Repos(ctx)
	require.NoError(t, err)
	assert.True(t, len(repos) > 0, "the limited user reads code nodes")

	it := curation.Item{ID: "role-test", Repo: storetest.Repo, Kind: "link", CreatedAt: "2026-01-01T00:00:00Z", UpdatedAt: "2026-01-01T00:00:00Z"}
	assert.NoError(t, lim.SaveCurated(ctx, it), "curated is writable")
	assert.NoError(t, lim.DeleteCurated(ctx, storetest.Repo, it.ID))

	// The rest of what MCP tools write: agent memory, reviews and usage counters.
	now := time.Now().UTC()
	mb, err := memory.Prepare(storetest.Repo, []memory.Triple{{Subject: "Role Test", Predicate: "USES", Object: "Limited User"}}, "role-test", now)
	require.NoError(t, err)
	require.NoError(t, lim.UpsertMemory(ctx, mb), "memory is writable")
	g, err := lim.SearchMemory(ctx, storetest.Repo, "role test", 10, false)
	require.NoError(t, err)
	assert.True(t, len(g.Facts) > 0)
	for _, e := range g.Entities {
		assert.NoError(t, lim.DeleteMemoryEntity(ctx, storetest.Repo, e.ID))
	}
	rv, err := reviews.Prepare(reviews.Review{Repo: storetest.Repo, Title: "Role", Comments: []reviews.CommentItem{{File: "app/app.go", Line: 9, Message: "m"}}}, now)
	require.NoError(t, err)
	require.NoError(t, lim.SaveReview(ctx, rv), "reviews are writable")
	_, err = lim.GetReview(ctx, rv.ID)
	assert.NoError(t, err)
	assert.NoError(t, lim.DeleteReview(ctx, rv.ID))
	require.NoError(t, lim.AddUsage(ctx, []usage.Bucket{{Day: "2000-01-01", Repo: storetest.Repo, Tool: "search", Client: "role-test", Calls: 1}}), "usage is writable")
	assert.NoError(t, lim.PruneUsage(ctx, "2000-01-02"))
	// History is written by indexing, not by MCP.
	assert.Error(t, lim.AddSnapshot(ctx, storetest.Repo, history.Snapshot{At: now.Format(time.RFC3339)}), "snapshots are not writable")

	assert.Error(t, lim.ReplaceRepo(ctx, storetest.Repo, storetest.Fixture()), "code nodes are not writable")
	assert.Error(t, lim.DeleteRepo(ctx, storetest.Repo), "code nodes are not deletable")
	again, err := admin.Repos(ctx)
	require.NoError(t, err)
	assert.Equal(t, len(repos), len(again))
}
