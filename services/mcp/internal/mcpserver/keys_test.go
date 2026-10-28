package mcpserver

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/astrona-io/codesynapse/internal/accounts"
	"github.com/astrona-io/codesynapse/internal/accounts/accountstest"
	acctmem "github.com/astrona-io/codesynapse/internal/accounts/memstore"
	"github.com/astrona-io/codesynapse/internal/execution"
	agentmem "github.com/astrona-io/codesynapse/internal/memory"
	"github.com/astrona-io/codesynapse/internal/reviews"
	"github.com/astrona-io/codesynapse/internal/store/memory"
	"github.com/astrona-io/codesynapse/internal/store/storetest"
	"github.com/astrona-io/codesynapse/internal/telemetry"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
	"github.com/astrona-io/codesynapse/internal/usage"
)

type bearer struct{ token, session string }

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	r.Header.Set(execution.SessionHeader, b.session)
	r.Header.Set(execution.ParentSessionHeader, b.session)
	return http.DefaultTransport.RoundTrip(r)
}

// keyServer serves the fixture graph over HTTP behind KeyVerifier and
// returns keys for three organisations: a and b use the repository, c does not.
func keyServer(t *testing.T) (url string, keys map[string]string, acc *accounts.Service, rec *usage.Recorder, s *memory.Store) {
	t.Helper()
	ctx := context.Background()
	s = memory.New()
	require.NoError(t, s.ReplaceRepo(ctx, storetest.Repo, storetest.Fixture()))
	acc = accounts.NewService(acctmem.New(), true)
	root, err := accountstest.Bootstrap(ctx, acc)
	require.NoError(t, err)
	keys = map[string]string{}
	for _, name := range []string{"a", "b", "c"} {
		u, org, err := acc.SignUp(ctx, name+"@example.com", name, "correct horse battery staple")
		require.NoError(t, err)
		if name == "a" { // a standard account with platform administration
			require.NoError(t, acc.SetPlatformAdmin(ctx, root, u.ID, true))
			u.PlatformAdmin = true
		}
		key, _, err := acc.CreateKey(ctx, accounts.Principal{User: u, Via: "session"}, org.ID, "agent", 0)
		require.NoError(t, err)
		keys[name] = key
		_, err = acc.RegisterExecutionSession(ctx, u.ID, org.ID, "chat-"+name)
		require.NoError(t, err)
		if name != "c" {
			require.NoError(t, acc.St.AddRepository(ctx, accounts.Repository{OrgID: org.ID, Graph: storetest.Repo,
				Visibility: accounts.Public, AddedBy: u.ID, AddedAt: time.Now().UTC().Truncate(time.Microsecond)}))
		}
	}
	url, rec = serveAccounts(t, acc, s)
	return url, keys, acc, rec, s
}

// serveAccounts serves s over HTTP behind acc's personal API keys.
func serveAccounts(t *testing.T, acc *accounts.Service, s *memory.Store) (string, *usage.Recorder) {
	t.Helper()
	rec := usage.NewRecorder(s)
	runtime := NewRuntime(AccountsAccess{Acc: acc}, "", "", "")
	server := New(s, "test", Options{Reviews: s, Memory: s, Usage: rec, Curated: s, CatalogWrites: true, Access: AccountsAccess{Acc: acc}, Runtime: runtime})
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil)
	ts := httptest.NewServer(RequestInfoHTTP(nil, auth.RequireBearerToken(KeyVerifier(acc, "svc-token"),
		&auth.RequireBearerTokenOptions{AllowMissingExpiration: true})(runtime.HTTP(MeasureHTTP(handler)))))
	t.Cleanup(ts.Close)
	return ts.URL, rec
}

func connectKey(t *testing.T, url, token string, sessions ...string) (*mcp.ClientSession, error) {
	t.Helper()
	session := "chat-a"
	if len(sessions) > 0 {
		session = sessions[0]
	}
	tr := &mcp.StreamableClientTransport{Endpoint: url, HTTPClient: &http.Client{Transport: bearer{token, session}}}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "key-client"}, nil).Connect(context.Background(), tr, nil)
	if err == nil {
		t.Cleanup(func() { _ = cs.Close() })
	}
	return cs, err
}

func TestAPIKeyScoping(t *testing.T) {
	url, keys, _, rec, s := keyServer(t)
	ctx := context.Background()

	_, err := connectKey(t, url, "csk_not-a-real-key")
	assert.Error(t, err, "unknown keys are refused")

	a, err := connectKey(t, url, keys["a"])
	require.NoError(t, err)
	b, err := connectKey(t, url, keys["b"], "chat-b")
	require.NoError(t, err)
	c, err := connectKey(t, url, keys["c"], "chat-c")
	require.NoError(t, err)
	_, err = connectKey(t, url, "svc-token")
	assert.Error(t, err, "the installation token cannot access an accounts deployment")

	var list repoListOut
	require.False(t, call(t, a, "repo_list", map[string]any{}, &list).IsError)
	assert.Len(t, list.Repos, 1)
	require.False(t, call(t, c, "repo_list", map[string]any{}, &list).IsError)
	assert.Len(t, list.Repos, 0, "an organisation sees only its repositories")
	assert.True(t, call(t, c, "repo_overview", map[string]any{"repo": storetest.Repo}, nil).IsError)

	t.Run("memory per organisation", func(t *testing.T) {
		res := call(t, a, "upsert_graph_triples", map[string]any{"triples": []map[string]any{
			{"subject": "Payments service", "predicate": "OWNED_BY", "object": "Team Blue"},
		}}, nil)
		require.False(t, res.IsError, "%v", res.Content)
		var g agentmem.Graph
		require.False(t, call(t, a, "search_graph_nodes", map[string]any{"query": "payments"}, &g).IsError)
		assert.Len(t, g.Facts, 1)
		for name, cs := range map[string]*mcp.ClientSession{"b": b} {
			g = agentmem.Graph{}
			require.False(t, call(t, cs, "search_graph_nodes", map[string]any{"query": "payments"}, &g).IsError)
			assert.Len(t, g.Facts, 0, name+" shares the graph, not the memory")
		}
	})

	t.Run("reviews per organisation", func(t *testing.T) {
		var sub struct{ ID string }
		res := call(t, a, "submit_review", map[string]any{"title": "a's review", "summary": "s"}, &sub)
		require.False(t, res.IsError, "%v", res.Content)
		var rl struct{ Reviews []struct{ ID string } }
		require.False(t, call(t, a, "list_reviews", map[string]any{}, &rl).IsError)
		assert.Len(t, rl.Reviews, 1)
		require.False(t, call(t, b, "list_reviews", map[string]any{}, &rl).IsError)
		assert.Len(t, rl.Reviews, 0)
		assert.True(t, call(t, b, "get_review", map[string]any{"id": sub.ID}, nil).IsError)
		assert.True(t, call(t, b, "update_review_comment", map[string]any{"review_id": sub.ID, "comment_id": "x", "status": "resolved"}, nil).IsError)
	})

	t.Run("legacy unscoped review", func(t *testing.T) {
		require.NoError(t, s.SaveReview(ctx, reviews.Review{ID: "legacy-review", Repo: storetest.Repo, Title: "Legacy"}))
		assert.True(t, callContext(ctx, t, a, "get_review", map[string]any{"id": "legacy-review"}, nil).IsError)
		assert.True(t, callContext(ctx, t, a, "update_review_comment", map[string]any{"review_id": "legacy-review", "comment_id": "x", "status": "resolved"}, nil).IsError)
		var listed struct{ Reviews []struct{ ID string } }
		require.False(t, callContext(ctx, t, a, "list_reviews", map[string]any{}, &listed).IsError)
		for _, review := range listed.Reviews {
			assert.False(t, review.ID == "legacy-review")
		}
	})

	t.Run("usage per organisation", func(t *testing.T) {
		require.NoError(t, rec.Flush(ctx))
		got, err := s.Usage(ctx, "2000-01-01")
		require.NoError(t, err)
		orgs := map[string]bool{}
		for _, bk := range got {
			orgs[bk.Org] = true
		}
		assert.Equal(t, len(orgs), 3, "only authenticated organisations")
	})
}

func TestTelemetryCarriesTenant(t *testing.T) {
	url, keys, acc, _, _ := keyServer(t)
	p, err := acc.AuthenticateKey(context.Background(), keys["a"])
	require.NoError(t, err)
	a, err := connectKey(t, url, keys["a"])
	require.NoError(t, err)
	require.False(t, call(t, a, "repo_list", map[string]any{}, nil).IsError)
	require.False(t, call(t, a, "search_graph_nodes", map[string]any{"repo": storetest.Repo}, nil).IsError)
	require.True(t, call(t, a, "file_outline", map[string]any{"repo": "someone/hidden", "file": "x.go"}, nil).IsError)
	_, err = a.CallTool(context.Background(), &mcp.CallToolParams{Name: "no_such_tool_" + p.OrgID, Arguments: map[string]any{}})
	assert.Error(t, err)

	var b strings.Builder
	require.NoError(t, telemetry.Default.WriteText(&b))
	out := b.String()
	assert.Contains(t, out, `mcp_tool_calls_total{tenant_id="`+p.OrgID+`",mcp_tool="repo_list",outcome="ok",llm_provider="none",llm_model="none"}`)
	assert.Contains(t, out, `mcp_tool_calls_total{tenant_id="`+p.OrgID+`",mcp_tool="unknown",outcome="refused",llm_provider="none",llm_model="none"}`, "names a client invents are not labels, and a call without a policy is refused")
	assert.Contains(t, out, `mcp_tool_calls_total{tenant_id="`+p.OrgID+`",mcp_tool="file_outline",outcome="refused",llm_provider="none",llm_model="none"}`, "a repository outside the caller's scope is a refusal, not an error")
	assert.False(t, strings.Contains(out, "no_such_tool_"), "the invented name never reaches a series")
	assert.Contains(t, out, `agent_memory_recalls_total{tenant_id="`+p.OrgID+`",mcp_tool="search_graph_nodes",llm_provider="none",llm_model="none"}`, "accepted tool context reaches memory instrumentation")
}

func TestLostPrivateAccessLeavesScope(t *testing.T) {
	url, keys, acc, _, _ := keyServer(t)
	ctx := context.Background()
	p, err := acc.AuthenticateKey(ctx, keys["a"])
	require.NoError(t, err)
	require.NoError(t, acc.St.SetRepositoryAccess(ctx, p.OrgID, storetest.Repo, accounts.AccessLost, time.Now().UTC()))
	a, err := connectKey(t, url, keys["a"])
	require.NoError(t, err)
	var list repoListOut
	require.False(t, call(t, a, "repo_list", map[string]any{}, &list).IsError)
	assert.Len(t, list.Repos, 0, "an organisation whose token lost access no longer sees the graph")
	assert.True(t, call(t, a, "repo_overview", map[string]any{"repo": storetest.Repo}, nil).IsError)
}

func TestSharesAndReadOnlyKeys(t *testing.T) {
	url, keys, acc, _, _ := keyServer(t)
	ctx := context.Background()
	pa, err := acc.AuthenticateKey(ctx, keys["a"])
	require.NoError(t, err)
	pc, err := acc.AuthenticateKey(ctx, keys["c"])
	require.NoError(t, err)
	ownerA := accounts.Principal{User: pa.User, Via: "session"}

	// An external recipient must first join the owning organisation explicitly.
	_, err = acc.SetRepoGrant(ctx, ownerA, pa.OrgID, storetest.Repo, accounts.Tuple{SubjectType: accounts.TypeUser, SubjectID: pc.User.ID}, accounts.Viewer)
	assert.Error(t, err, "a grant cannot replace explicit membership")
	_, err = acc.AddMember(ctx, ownerA, pa.OrgID, pc.User.Email, accounts.Guest)
	require.NoError(t, err)
	_, err = acc.SetRepoGrant(ctx, ownerA, pa.OrgID, storetest.Repo, accounts.Tuple{SubjectType: accounts.TypeUser, SubjectID: pc.User.ID}, accounts.Viewer)
	require.NoError(t, err)
	a, err := connectKey(t, url, keys["a"])
	require.NoError(t, err)
	b, err := connectKey(t, url, keys["b"], "chat-b")
	require.NoError(t, err)
	foreign, err := connectKey(t, url, keys["c"], "chat-c")
	require.NoError(t, err)
	assert.True(t, call(t, foreign, "repo_overview", map[string]any{"repo": storetest.Repo}, nil).IsError, "a key from another organisation cannot use the grant")
	guestKey, _, err := acc.CreateKey(ctx, accounts.Principal{User: pc.User, Via: "session"}, pa.OrgID, "guest", 0)
	require.NoError(t, err)
	_, err = acc.RegisterExecutionSession(ctx, pc.User.ID, pa.OrgID, "chat-guest")
	require.NoError(t, err)
	c, err := connectKey(t, url, guestKey, "chat-guest")
	require.NoError(t, err)
	for name, cs := range map[string]*mcp.ClientSession{"a": a, "b": b} {
		res := call(t, cs, "upsert_graph_triples", map[string]any{"triples": []map[string]any{{"subject": "Billing " + name, "predicate": "OWNED_BY", "object": "Team " + name}}}, nil)
		require.False(t, res.IsError, "%v", res.Content)
	}
	var list repoListOut
	require.False(t, call(t, c, "repo_list", map[string]any{}, &list).IsError)
	require.Len(t, list.Repos, 1, "the owning organisation key sees its explicit grant")
	var g agentmem.Graph
	require.False(t, call(t, c, "search_graph_nodes", map[string]any{"query": "billing"}, &g).IsError)
	require.Len(t, g.Entities, 2)
	names := g.Entities[0].Name + " " + g.Entities[1].Name
	assert.Contains(t, names, "Billing a", "c reads the sharing organisation's memory")
	assert.False(t, strings.Contains(names, "Billing b"), "not a third organisation's")
	res := call(t, c, "upsert_graph_triples", map[string]any{"triples": []map[string]any{{"subject": "x", "predicate": "P", "object": "y"}}}, nil)
	assert.True(t, res.IsError, "shares are read-only")
	res = call(t, c, "submit_review", map[string]any{"title": "t", "summary": "s"}, nil)
	assert.True(t, res.IsError)

	// A viewer needs an explicit read grant when the organisation default is NONE.
	viewer, _, err := acc.SignUp(ctx, "viewer@example.com", "viewer", "correct horse battery staple")
	require.NoError(t, err)
	_, err = acc.AddMember(ctx, ownerA, pa.OrgID, viewer.Email, accounts.Viewer)
	require.NoError(t, err)
	_, err = acc.SetRepoGrant(ctx, ownerA, pa.OrgID, storetest.Repo, accounts.Tuple{SubjectType: accounts.TypeUser, SubjectID: viewer.ID}, accounts.Viewer)
	require.NoError(t, err)
	vkey, _, err := acc.CreateKey(ctx, accounts.Principal{User: viewer, Via: "session"}, pa.OrgID, "v", 0)
	require.NoError(t, err)
	_, err = acc.RegisterExecutionSession(ctx, viewer.ID, pa.OrgID, "chat-viewer")
	require.NoError(t, err)
	v, err := connectKey(t, url, vkey, "chat-viewer")
	require.NoError(t, err)
	require.False(t, call(t, v, "search_graph_nodes", map[string]any{"query": "billing"}, &g).IsError)
	assert.Len(t, g.Entities, 2, "viewers read their organisation's memory")
	res = call(t, v, "upsert_graph_triples", map[string]any{"triples": []map[string]any{{"subject": "x", "predicate": "P", "object": "y"}}}, nil)
	assert.True(t, res.IsError, "viewers do not write")
	require.NoError(t, acc.RemoveMember(ctx, ownerA, pa.OrgID, viewer.ID))
	_, err = v.CallTool(ctx, &mcp.CallToolParams{Name: "repo_list", Arguments: map[string]any{}})
	assert.Error(t, err, "removed membership revokes a live session")
}

func TestRevokedKeyStopsWorking(t *testing.T) {
	url, keys, acc, _, _ := keyServer(t)
	ctx := context.Background()
	p, err := acc.AuthenticateKey(ctx, keys["a"])
	require.NoError(t, err)
	live, err := connectKey(t, url, keys["a"])
	require.NoError(t, err)
	require.False(t, call(t, live, "repo_list", map[string]any{}, nil).IsError)
	require.NoError(t, acc.RevokeKey(ctx, accounts.Principal{User: p.User, Via: "session"}, p.KeyID))
	_, err = live.CallTool(ctx, &mcp.CallToolParams{Name: "repo_list", Arguments: map[string]any{}})
	assert.Error(t, err, "revocation also stops a live transport session")
	_, err = connectKey(t, url, keys["a"])
	assert.Error(t, err)
}
