package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
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

// failingAudit refuses audit entries while broken is set.
type failingAudit struct {
	accounts.Store
	broken *atomic.Bool
}

func (f failingAudit) AddEvent(ctx context.Context, e accounts.Event) (accounts.Event, error) {
	if f.broken.Load() {
		return e, errors.New("audit store down")
	}
	return f.Store.AddEvent(ctx, e)
}

func TestEveryMCPRequestIsAudited(t *testing.T) {
	ctx := context.Background()
	s := memory.New()
	require.NoError(t, s.ReplaceRepo(ctx, storetest.Repo, storetest.Fixture()))
	broken := &atomic.Bool{}
	acc := accounts.NewService(failingAudit{Store: acctmem.New(), broken: broken}, true)
	bob := accounts.User{ID: "u-bob", Email: "bob@example.com", Name: "Bob", CreatedAt: time.Now().UTC()}
	require.NoError(t, acc.St.CreateUser(ctx, bob))
	org := accounts.Org{ID: "o-bob", Slug: "bob", Name: "Bob's", CreatedAt: time.Now().UTC()}
	require.NoError(t, acc.St.CreateOrg(ctx, org, bob.ID))
	require.NoError(t, acc.St.AddRepository(ctx, accounts.Repository{OrgID: org.ID, Graph: storetest.Repo, Visibility: accounts.Public, AddedBy: bob.ID, AddedAt: time.Now().UTC().Truncate(time.Microsecond)}))
	key, apiKey, err := acc.CreateKey(ctx, accounts.Principal{User: bob, Via: "session"}, org.ID, "agent", 0)
	require.NoError(t, err)
	_, err = acc.RegisterExecutionSession(ctx, bob.ID, org.ID, "chat-audit")
	require.NoError(t, err)
	url, _ := serveAccounts(t, acc, s)

	cs, err := connectKey(t, url, key, "chat-audit")
	require.NoError(t, err)
	const diff = "diff --git a/x.go b/x.go\n+func secretSauce() {}\n"
	_, err = cs.CallTool(ctx, &mcp.CallToolParams{Name: "get_pr_context", Arguments: map[string]any{"repo": storetest.Repo, "diff": diff}})
	require.NoError(t, err)
	_, err = cs.CallTool(ctx, &mcp.CallToolParams{Name: "search", Arguments: map[string]any{"repo": storetest.Repo, "query": "who paid with 4111 1111 1111 1111 using ghp_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "limit": 5}})
	require.NoError(t, err)
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "search", Arguments: map[string]any{"repo": "someone-elses", "query": "x"}})
	require.NoError(t, err)
	require.True(t, res.IsError, "a repository outside the caller's reach is refused")

	// No repo argument: the only repository the caller may use is picked, and
	// the entry names it, so the sessions page shows where the chat ran.
	_, err = cs.CallTool(ctx, &mcp.CallToolParams{Name: "repo_overview", Arguments: map[string]any{}})
	require.NoError(t, err)

	events, err := acc.St.ListEvents(ctx, accounts.EventFilter{Action: "mcp.", Limit: 100, Ascending: true})
	require.NoError(t, err)
	byName := map[string][]accounts.Event{}
	for _, e := range events {
		if e.MCP == nil {
			continue
		}
		byName[e.MCP.Method+" "+e.MCP.Name] = append(byName[e.MCP.Method+" "+e.MCP.Name], e)
		assert.Equal(t, e.UserID, bob.ID)
		assert.Equal(t, e.KeyID, apiKey.ID)
		assert.Equal(t, e.Via, "key")
		assert.Equal(t, e.OrgID, org.ID)
		assert.True(t, e.RequestID != "" && e.IP == "127.0.0.1" && e.UserAgent != "", "request details: %+v", e)
		assert.Contains(t, e.MCP.Client, "key-client")
		assert.True(t, e.MCP.TransportID != "", "transport session")
		raw, _ := json.Marshal(e)
		assert.False(t, strings.Contains(string(raw), key), "the key never reaches the log")
		assert.False(t, strings.Contains(string(raw), "secretSauce"), "bulk inputs are only digested")
		assert.False(t, strings.Contains(string(raw), "4111 1111") || strings.Contains(string(raw), "ghp_aaaa"), "free text is redacted")
	}
	assert.True(t, len(byName["initialize "])+len(byName["server/discover "]) > 0, "connecting is recorded")
	require.Len(t, byName["tools/call get_pr_context"], 1)
	pr := byName["tools/call get_pr_context"][0]
	assert.Equal(t, pr.MCP.SessionID, "chat-audit")
	assert.Equal(t, pr.MCP.Repo, storetest.Repo)
	assert.Equal(t, pr.Outcome, accounts.Success)
	assert.True(t, strings.HasPrefix(pr.MCP.Args["diff"], "sha256:"), pr.MCP.Args["diff"])
	assert.True(t, strings.HasPrefix(pr.MCP.ArgsDigest, "sha256:"))
	assert.True(t, pr.MCP.Bytes > 0)
	require.Len(t, byName["tools/call repo_overview"], 1)
	assert.Equal(t, byName["tools/call repo_overview"][0].MCP.Repo, storetest.Repo, "the resolved repository, not only the argument")
	searches := byName["tools/call search"]
	require.Len(t, searches, 2)
	assert.Equal(t, searches[0].MCP.Args["limit"], "5")
	assert.Contains(t, searches[0].MCP.Args["query"], "who paid with [CARD]")
	assert.Equal(t, searches[1].Outcome, accounts.Failure, "refusals are recorded")
	assert.Equal(t, searches[1].Status, 400)
	bySession, err := acc.St.ListEvents(ctx, accounts.EventFilter{Session: "chat-audit", Tool: "search", Limit: 10})
	require.NoError(t, err)
	assert.Len(t, bySession, 2, "filters by chat and tool")
	v, err := accounts.VerifyAudit(ctx, acc.St)
	require.NoError(t, err)
	assert.True(t, v.OK, v.Reason)

	// Without an audit entry, no result.
	broken.Store(true)
	_, err = cs.CallTool(ctx, &mcp.CallToolParams{Name: "repo_list", Arguments: map[string]any{}})
	broken.Store(false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "audit log is unavailable")
}
