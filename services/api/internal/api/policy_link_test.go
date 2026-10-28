package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
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

// One entry per organisation and graph: a repository in a member's own
// space keeps the organisation from adding it too, and the reverse.
func TestRepositorySpaces(t *testing.T) {
	f := newRuntimeFixture(t, Config{})
	ctx := t.Context()
	principal := accounts.Principal{User: f.user, Via: "session"}
	id, err := f.accounts.RegisterExecutionSession(ctx, f.user.ID, f.org.ID, "spaces")
	require.NoError(t, err)
	workCtx, err := execution.With(context.WithValue(ctx, principalKey{}, principal), id)
	require.NoError(t, err)
	entry := func(graph, owner string) accounts.Repository {
		return accounts.Repository{OrgID: f.org.ID, Graph: graph, Visibility: accounts.Public, OwnerID: owner, AddedBy: f.user.ID, AddedAt: time.Now()}
	}

	require.NoError(t, f.server.linkPolicyRepository(workCtx, entry("space/mine", f.user.ID), accounts.Member))
	assert.True(t, errors.Is(f.server.linkPolicyRepository(workCtx, entry("space/mine", f.user.ID), accounts.Member), accounts.ErrExists), "the same space again")
	var taken errSpaceTaken
	assert.True(t, errors.As(f.server.linkPolicyRepository(workCtx, entry("space/mine", ""), accounts.Admin), &taken), "not again for the organisation")
	assert.Equal(t, taken.existing.OwnerID, f.user.ID)

	require.NoError(t, f.server.linkPolicyRepository(workCtx, entry("space/shared", ""), accounts.Admin))
	assert.True(t, errors.As(f.server.linkPolicyRepository(workCtx, entry("space/shared", f.user.ID), accounts.Member), &taken), "not again in a member's space")

	private := entry("space/private", f.user.ID)
	private.Visibility = accounts.Private
	assert.True(t, errors.Is(f.server.linkPolicyRepository(workCtx, private, accounts.Member), errPersonalPrivate), "proved with the organisation's token, so not personal")
	require.NoError(t, f.accounts.St.SetMemberReposEnabled(ctx, f.org.ID, false))
	err = f.server.linkPolicyRepository(workCtx, entry("space/later", f.user.ID), accounts.Member)
	assert.True(t, errors.Is(err, accounts.ErrForbidden), "members' own repositories are turned off")
	assert.True(t, watchSkip(err), "a member's watch skips it instead of failing every scan")
	assert.True(t, watchSkip(errSpaceTaken{}), "a watch skips what the organisation has")
	assert.False(t, watchSkip(accounts.ErrForbidden), "other refusals still fail the scan")
	require.NoError(t, f.server.linkPolicyRepository(workCtx, entry("space/later", ""), accounts.Admin), "the organisation can still add it")

	for err, code := range map[error]int{errSpaceTaken{}: 409, errMemberReposOff: 403, accounts.ErrInvalidInput: 400} {
		w := httptest.NewRecorder()
		f.server.linkError(w, err)
		assert.Equal(t, w.Code, code, err.Error())
	}
}
