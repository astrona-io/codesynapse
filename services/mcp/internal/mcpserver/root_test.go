package mcpserver

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/astrona-io/codesynapse/internal/accounts"
	acctmem "github.com/astrona-io/codesynapse/internal/accounts/memstore"
	"github.com/astrona-io/codesynapse/internal/store/memory"
	"github.com/astrona-io/codesynapse/internal/store/storetest"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

// TestRootIsDeniedEveryTool: a platform administrator from before bootstrap
// separation keeps a live MCP session; once the installation is classified
// and its root is ambiguous, every tool call in that session is refused,
// read-only tools included, and new connections are refused.
func TestRootIsDeniedEveryTool(t *testing.T) {
	ctx := context.Background()
	s := memory.New()
	require.NoError(t, s.ReplaceRepo(ctx, storetest.Repo, storetest.Fixture()))
	acc := accounts.NewService(acctmem.New(), true)
	hash, err := accounts.HashPassword("correct horse battery staple")
	require.NoError(t, err)
	ada := accounts.User{ID: "u-ada", Email: "ada@example.com", Name: "Ada", PlatformAdmin: true, CreatedAt: time.Now().UTC(), PasswordHash: hash}
	require.NoError(t, acc.St.CreateUser(ctx, ada))
	org := accounts.Org{ID: "o-ada", Slug: "ada", Name: "Ada's", CreatedAt: time.Now().UTC()}
	require.NoError(t, acc.St.CreateOrg(ctx, org, ada.ID))
	require.NoError(t, acc.St.AddRepository(ctx, accounts.Repository{OrgID: org.ID, Graph: storetest.Repo, Visibility: accounts.Public, AddedBy: ada.ID, AddedAt: time.Now().UTC().Truncate(time.Microsecond)}))
	key, _, err := acc.CreateKey(ctx, accounts.Principal{User: ada, Via: "session"}, org.ID, "agent", 0)
	require.NoError(t, err)
	_, err = acc.RegisterExecutionSession(ctx, ada.ID, org.ID, "chat-ada")
	require.NoError(t, err)
	url, _ := serveAccounts(t, acc, s)

	cs, err := connectKey(t, url, key, "chat-ada")
	require.NoError(t, err)
	_, err = cs.CallTool(ctx, &mcp.CallToolParams{Name: "repo_list", Arguments: map[string]any{}})
	require.NoError(t, err, "before classification ada works as before")

	setup, err := acc.ReconcileBootstrap(ctx)
	require.NoError(t, err)
	require.Equal(t, setup.State, accounts.SetupAmbiguous)

	_, err = cs.ListTools(ctx, nil)
	assert.Error(t, err, "the live session rechecks the key on every request")
	_, err = cs.CallTool(ctx, &mcp.CallToolParams{Name: "repo_list", Arguments: map[string]any{}})
	assert.Error(t, err, "read-only tools included")
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	body, _ := io.ReadAll(res.Body)
	_ = res.Body.Close()
	assert.Equal(t, res.StatusCode, http.StatusUnauthorized)
	assert.Contains(t, string(body), accounts.ErrRootAccount.Error(), "the refusal says why")
	// The per-tool authorization layer refuses root on its own as well.
	_, err = (AccountsAccess{Acc: acc}).Reach(ctx, ada.ID, org.ID)
	assert.True(t, errors.Is(err, accounts.ErrRootAccount))
	events, err := acc.St.ListEvents(ctx, accounts.EventFilter{Action: "root.denied"})
	require.NoError(t, err)
	assert.True(t, len(events) > 0, "audited")
	_, err = connectKey(t, url, key, "chat-ada")
	assert.Error(t, err, "new connections are refused")
}
