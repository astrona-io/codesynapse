package mcpserver

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/parisnakitakejser/codesynapse/internal/accounts"
	acctmem "github.com/parisnakitakejser/codesynapse/internal/accounts/memstore"
	agentmem "github.com/parisnakitakejser/codesynapse/internal/memory"
	"github.com/parisnakitakejser/codesynapse/internal/store/memory"
	"github.com/parisnakitakejser/codesynapse/internal/store/storetest"
	"github.com/parisnakitakejser/codesynapse/internal/testutil/assert"
	"github.com/parisnakitakejser/codesynapse/internal/testutil/require"
	"github.com/parisnakitakejser/codesynapse/internal/usage"
)

type bearer struct{ token string }

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
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
	keys = map[string]string{}
	for _, name := range []string{"a", "b", "c"} {
		u, org, err := acc.SignUp(ctx, name+"@example.com", name, "correct horse battery staple")
		require.NoError(t, err)
		key, _, err := acc.CreateKey(ctx, accounts.Principal{User: u, Via: "session"}, org.ID, "agent", 0)
		require.NoError(t, err)
		keys[name] = key
		if name != "c" {
			require.NoError(t, acc.St.AddRepository(ctx, accounts.Repository{OrgID: org.ID, Graph: storetest.Repo,
				Visibility: accounts.Public, AddedBy: u.ID, AddedAt: time.Now().UTC().Truncate(time.Microsecond)}))
		}
	}
	rec = usage.NewRecorder(s)
	server := New(s, "test", Options{Reviews: s, Memory: s, Usage: rec, Access: OrgAccess{St: acc.St}})
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil)
	ts := httptest.NewServer(auth.RequireBearerToken(KeyVerifier(acc, "svc-token"),
		&auth.RequireBearerTokenOptions{AllowMissingExpiration: true})(handler))
	t.Cleanup(ts.Close)
	return ts.URL, keys, acc, rec, s
}

func connectKey(t *testing.T, url, token string) (*mcp.ClientSession, error) {
	t.Helper()
	tr := &mcp.StreamableClientTransport{Endpoint: url, HTTPClient: &http.Client{Transport: bearer{token}}}
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
	b, err := connectKey(t, url, keys["b"])
	require.NoError(t, err)
	c, err := connectKey(t, url, keys["c"])
	require.NoError(t, err)
	svc, err := connectKey(t, url, "svc-token")
	require.NoError(t, err)

	var list repoListOut
	require.False(t, call(t, a, "repo_list", map[string]any{}, &list).IsError)
	assert.Len(t, list.Repos, 1)
	require.False(t, call(t, c, "repo_list", map[string]any{}, &list).IsError)
	assert.Len(t, list.Repos, 0, "an organisation sees only its repositories")
	assert.True(t, call(t, c, "repo_overview", map[string]any{"repo": storetest.Repo}, nil).IsError)
	assert.False(t, call(t, svc, "repo_overview", map[string]any{"repo": storetest.Repo}, nil).IsError, "the MCP token sees everything")

	t.Run("memory per organisation", func(t *testing.T) {
		res := call(t, a, "upsert_graph_triples", map[string]any{"triples": []map[string]any{
			{"subject": "Payments service", "predicate": "OWNED_BY", "object": "Team Blue"},
		}}, nil)
		require.False(t, res.IsError, "%v", res.Content)
		var g agentmem.Graph
		require.False(t, call(t, a, "search_graph_nodes", map[string]any{"query": "payments"}, &g).IsError)
		assert.Len(t, g.Facts, 1)
		for name, cs := range map[string]*mcp.ClientSession{"b": b, "svc": svc} {
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

	t.Run("usage per organisation", func(t *testing.T) {
		require.NoError(t, rec.Flush(ctx))
		got, err := s.Usage(ctx, "2000-01-01")
		require.NoError(t, err)
		orgs := map[string]bool{}
		for _, bk := range got {
			orgs[bk.Org] = true
		}
		assert.Equal(t, len(orgs), 4, "three organisations and the MCP token (no organisation)")
	})
}

func TestRevokedKeyStopsWorking(t *testing.T) {
	url, keys, acc, _, _ := keyServer(t)
	ctx := context.Background()
	p, err := acc.AuthenticateKey(ctx, keys["a"])
	require.NoError(t, err)
	_, err = connectKey(t, url, keys["a"])
	require.NoError(t, err)
	require.NoError(t, acc.RevokeKey(ctx, accounts.Principal{User: p.User, Via: "session"}, p.KeyID))
	_, err = connectKey(t, url, keys["a"])
	assert.Error(t, err)
}
