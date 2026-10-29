package mcpserver

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/astrona-io/codesynapse/internal/accounts"
	"github.com/astrona-io/codesynapse/internal/accounts/accountstest"
	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/store/memory"
	"github.com/astrona-io/codesynapse/internal/store/storetest"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

func TestEveryToolHasPolicy(t *testing.T) {
	ctx := context.Background()
	s := memory.New()
	server := newTestServer(t, s, "test", Options{Memory: s, Reviews: s, History: s, Curated: s, CatalogWrites: true, Offload: &APIClient{}})
	ct, st := mcp.NewInMemoryTransports()
	ss, err := server.Connect(ctx, st, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = ss.Close() })
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "policy-test"}, nil).Connect(ctx, ct, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = cs.Close() })
	listed, err := cs.ListTools(ctx, nil)
	require.NoError(t, err)
	assert.Equal(t, len(listed.Tools), len(toolPolicies))
	for _, tool := range listed.Tools {
		policy, ok := toolPolicies[tool.Name]
		assert.True(t, ok, tool.Name)
		assert.Equal(t, tool.Annotations.ReadOnlyHint, policy.role == accounts.Viewer, tool.Name)
	}

	// Registration alone cannot enable a tool without an explicit policy.
	invoked := false
	mcp.AddTool(server, &mcp.Tool{Name: "unclassified"}, func(context.Context, *mcp.CallToolRequest, empty) (*mcp.CallToolResult, empty, error) {
		invoked = true
		return nil, empty{}, nil
	})
	_, err = cs.CallTool(ctx, &mcp.CallToolParams{Name: "unclassified", Arguments: map[string]any{}})
	assert.Error(t, err)
	assert.False(t, invoked)
}

func TestAccountsStdioAuthentication(t *testing.T) {
	_, keys, acc, _, s := keyServer(t)
	ctx := context.Background()
	for _, tc := range []struct {
		name, token string
		allowed     bool
	}{
		{"missing", "", false},
		{"service", "svc-token", false},
		{"personal", keys["a"], true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := New(s, "test", Options{Access: AccountsAccess{Acc: acc}, StdioToken: tc.token, Runtime: NewRuntime(AccountsAccess{Acc: acc}, tc.token, "chat-a", "chat-a")})
			ct, st := mcp.NewInMemoryTransports()
			ss, err := server.Connect(ctx, st, nil)
			require.NoError(t, err)
			t.Cleanup(func() { _ = ss.Close() })
			cs, err := mcp.NewClient(&mcp.Implementation{Name: "stdio-test"}, nil).Connect(ctx, ct, nil)
			if !tc.allowed {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
			t.Cleanup(func() { _ = cs.Close() })
			assert.False(t, callContext(ctx, t, cs, "repo_overview", map[string]any{"repo": storetest.Repo}, nil).IsError)
			p, err := acc.AuthenticateKey(ctx, tc.token)
			require.NoError(t, err)
			require.NoError(t, acc.RevokeKey(ctx, accounts.Principal{User: p.User, Via: "session"}, p.KeyID))
			_, err = cs.CallTool(ctx, &mcp.CallToolParams{Name: "repo_overview", Arguments: map[string]any{"repo": storetest.Repo}})
			assert.Error(t, err, "stdio reauthenticates a live session on every call")
		})
	}
}

func TestPlatformFlagDoesNotGrantMCPReach(t *testing.T) {
	_, keys, acc, _, _ := keyServer(t)
	ctx := context.Background()
	admin, err := acc.AuthenticateKey(ctx, keys["a"])
	require.NoError(t, err)
	require.True(t, admin.User.PlatformAdmin, "a has platform administration")
	other, err := acc.AuthenticateKey(ctx, keys["b"])
	require.NoError(t, err)
	reach, err := (AccountsAccess{Acc: acc}).Reach(ctx, admin.User.ID, other.OrgID)
	require.NoError(t, err)
	assert.Len(t, reach, 0)
}

func TestAccountsHandlersDenyMissingCaller(t *testing.T) {
	_, _, acc, _, _ := keyServer(t)
	h := &handlers{access: AccountsAccess{Acc: acc}}
	_, restricted, err := h.reach(context.Background())
	assert.True(t, restricted)
	assert.Error(t, err)
	assert.Error(t, h.canSeeReview(context.Background(), storetest.Repo, ""))
}

func TestLiveMCPPermissionsFollowDirectOverride(t *testing.T) {
	url, keys, acc, _, _ := keyServer(t)
	ctx := context.Background()
	owner, err := acc.AuthenticateKey(ctx, keys["a"])
	require.NoError(t, err)
	actor := accounts.Principal{User: owner.User, Via: "session"}
	backup, _, err := acc.SignUp(ctx, "backup@example.com", "Backup", "correct horse battery staple")
	require.NoError(t, err)
	require.NoError(t, accountstest.Join(ctx, t, acc, actor, owner.OrgID, backup, accounts.Member))
	_, err = acc.SetRepoGrant(ctx, actor, owner.OrgID, storetest.Repo, accounts.Tuple{SubjectType: accounts.TypeUser, SubjectID: backup.ID}, accounts.Admin)
	require.NoError(t, err)
	cs, err := connectKey(t, url, keys["a"])
	require.NoError(t, err)
	require.False(t, callContext(ctx, t, cs, "submit_review", map[string]any{"title": "Before downgrade"}, nil).IsError)
	_, err = acc.SetRepoGrant(ctx, actor, owner.OrgID, storetest.Repo, accounts.Tuple{SubjectType: accounts.TypeUser, SubjectID: owner.User.ID}, accounts.Viewer)
	require.NoError(t, err)
	assert.False(t, callContext(ctx, t, cs, "repo_overview", map[string]any{"repo": storetest.Repo}, nil).IsError)
	assert.True(t, callContext(ctx, t, cs, "submit_review", map[string]any{"title": "After downgrade"}, nil).IsError, "a platform administrator with READ cannot write")
	_, err = acc.SetRepoGrant(ctx, accounts.Principal{User: backup, Via: "session"}, owner.OrgID, storetest.Repo, accounts.Tuple{SubjectType: accounts.TypeUser, SubjectID: owner.User.ID}, accounts.None)
	require.NoError(t, err)
	assert.True(t, callContext(ctx, t, cs, "repo_overview", map[string]any{"repo": storetest.Repo}, nil).IsError, "existing sessions lose content access after a deny")
	var listed repoListOut
	assert.False(t, callContext(ctx, t, cs, "repo_list", map[string]any{}, &listed).IsError)
	assert.Len(t, listed.Repos, 0)
}

func TestMCPRejectsForeignFullSymbolID(t *testing.T) {
	url, keys, _, _, db := keyServer(t)
	ctx := context.Background()
	foreign := "private/other"
	g := model.NewGraph()
	g.AddNode(model.Node{ID: model.NodeID(foreign, model.KindRepository, foreign), Kind: model.KindRepository, Repo: foreign, Name: foreign})
	id := model.NodeID(foreign, model.KindFunction, "SecretFunction")
	g.AddNode(model.Node{ID: id, Kind: model.KindFunction, Repo: foreign, Name: "SecretFunction", Summary: "Foreign private summary"})
	require.NoError(t, db.ReplaceRepo(ctx, foreign, g))
	cs, err := connectKey(t, url, keys["a"])
	require.NoError(t, err)
	for _, tool := range []string{"get_symbol_context", "get_call_graph", "get_implementations"} {
		t.Run(tool, func(t *testing.T) {
			result := callContext(ctx, t, cs, tool, map[string]any{"repo": storetest.Repo, "symbol": id}, nil)
			assert.True(t, result.IsError)
			payload, err := json.Marshal(result)
			require.NoError(t, err)
			assert.False(t, strings.Contains(string(payload), "Foreign private summary"))
		})
	}
}
