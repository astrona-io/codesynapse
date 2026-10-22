package api

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/astrona-io/codesynapse/internal/accounts"
	repoconfig "github.com/astrona-io/codesynapse/internal/config"
	"github.com/astrona-io/codesynapse/internal/execution"
	"github.com/astrona-io/codesynapse/internal/ingest"
	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/store/storetest"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

func TestFilteredGraphLinkRequiresMatchingTenantApproval(t *testing.T) {
	f := newRuntimeFixture(t, Config{})
	ctx := t.Context()
	principal := accounts.Principal{User: f.user, Via: "session"}
	link := func(entry accounts.Repository) error {
		id, err := f.accounts.RegisterExecutionSession(ctx, f.user.ID, entry.OrgID, "link-"+entry.OrgID)
		require.NoError(t, err)
		workCtx, err := execution.With(context.WithValue(ctx, principalKey{}, principal), id)
		require.NoError(t, err)
		return f.server.linkPolicyRepository(workCtx, entry, accounts.Member)
	}
	other, err := f.accounts.CreateOrg(ctx, principal, "Other graph owner", "")
	require.NoError(t, err)
	policy, err := repoconfig.Parse([]byte("version: 1\nignore:\n  paths: [generated/**]\n"))
	require.NoError(t, err)
	graph := storetest.Fixture()
	repo, ok := graph.Node(model.NodeID(storetest.Repo, model.KindRepository, storetest.Repo))
	require.True(t, ok)
	meta, err := json.Marshal(map[string]any{"repository_policy": ingest.PolicyMetadata{Present: true, Digest: policy.Digest(), Config: policy.Config(), ExclusionsApproved: true}})
	require.NoError(t, err)
	repo.Meta = string(meta)
	require.NoError(t, f.store.ReplaceRepo(ctx, storetest.Repo, graph))
	entry := accounts.Repository{OrgID: other.ID, Graph: storetest.Repo, Visibility: accounts.Private, AddedBy: f.user.ID, AddedAt: time.Now()}
	assert.True(t, errors.Is(link(entry), accounts.ErrForbidden))
	rows, err := f.accounts.St.OrgRepositories(ctx, other.ID)
	require.NoError(t, err)
	assert.Len(t, rows, 0, "a new tenant must not inherit someone else's excluded coverage")

	entry.OrgID = f.org.ID
	assert.True(t, errors.Is(link(entry), accounts.ErrForbidden))
	canonical, err := json.Marshal(policy.Config())
	require.NoError(t, err)
	require.NoError(t, f.accounts.SetRepoPolicy(ctx, principal, accounts.RepoPolicyApproval{OrgID: f.org.ID, Graph: storetest.Repo, Digest: policy.Digest(), ConfigJSON: string(canonical), Reason: "Reviewed generated paths", AllowExclusions: true}))
	assert.True(t, errors.Is(link(entry), accounts.ErrExists), "an existing matching tenant remains linked")
	require.NoError(t, f.accounts.DeleteRepoPolicy(ctx, principal, f.org.ID, storetest.Repo))
	assert.True(t, errors.Is(link(entry), accounts.ErrForbidden))

	repo.Meta = ""
	require.NoError(t, f.store.ReplaceRepo(ctx, storetest.Repo, graph))
	entry.OrgID = other.ID
	require.NoError(t, link(entry), "unfiltered graphs can be shared without an exclusion approval")
}
