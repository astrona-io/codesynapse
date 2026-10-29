package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/astrona-io/codesynapse/internal/accounts"
	acctmem "github.com/astrona-io/codesynapse/internal/accounts/memstore"
	"github.com/astrona-io/codesynapse/internal/execution"
	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/semantic"
	"github.com/astrona-io/codesynapse/internal/store/memory"
	"github.com/astrona-io/codesynapse/internal/store/storetest"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
	"github.com/astrona-io/codesynapse/internal/vector"
	"github.com/astrona-io/codesynapse/services/api/internal/llm"
)

// fakeEmbeddings puts "serve" texts on one axis and everything else on
// another, so a task about serving finds Handler by meaning.
func fakeEmbeddings(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Input []string }
		_ = json.NewDecoder(r.Body).Decode(&req)
		type item struct {
			Index     int       `json:"index"`
			Embedding []float32 `json:"embedding"`
		}
		var data []item
		for i, in := range req.Input {
			v := []float32{0, 0, 1}
			if strings.Contains(strings.ToLower(in), "serve") {
				v = []float32{1, 0.1, 0}
			}
			data = append(data, item{i, v})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestSemanticRanksAfterIngest(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	st := memory.New()
	g := storetest.Fixture()
	require.NoError(t, st.ReplaceRepo(ctx, storetest.Repo, g))
	srv := fakeEmbeddings(t)
	reg := llm.NewRegistry(llm.Config{Providers: []llm.ProviderConfig{{Name: "ollama", Kind: llm.KindOpenAI, BaseURL: srv.URL + "/v1",
		Model: "qwen", EmbeddingModel: "nomic", TimeoutSeconds: 5}}, Default: "ollama"})
	acc := accounts.NewService(acctmem.New(), true)
	bootstrapAccounts(t, acc)
	api := New(ctx, Config{Accounts: acc, LLM: reg}, st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ts := httptest.NewServer(api)
	t.Cleanup(ts.Close)

	id, err := execution.With(ctx, execution.Identity{TenantID: "org", SubjectID: "user", SessionID: "thread", ParentSessionID: "thread"})
	require.NoError(t, err)
	api.embedIngest(id, storetest.Repo, g, nil)

	c := newClient(t, ts)
	require.Equal(t, c.do("POST", "/api/v1/auth/signup", `{"email":"ada@example.com","name":"Ada","password":"`+pw+`"}`, nil), 201)
	var org accounts.Org
	require.Equal(t, c.do("POST", "/api/v1/accounts/orgs", `{"name":"Acme"}`, &org), 201)
	u, err := acc.St.UserByEmail(ctx, "ada@example.com")
	require.NoError(t, err)
	require.NoError(t, acc.St.AddRepository(ctx, accounts.Repository{OrgID: org.ID, Graph: storetest.Repo, AddedBy: u.ID}))

	var out struct {
		Enabled bool
		Hits    []semanticHit
	}
	path := "/api/v1/repos/" + url.PathEscape(storetest.Repo) + "/semantic"
	require.Equal(t, c.do("POST", path, `{"task":"who will serve requests","k":3}`, &out, OrgHeader, org.Slug), 200)
	assert.True(t, out.Enabled)
	require.True(t, len(out.Hits) > 0)
	assert.Equal(t, out.Hits[0].ID, storetest.ID(model.KindFunction, "app.Handler"), "found by meaning")
	assert.Equal(t, c.do("POST", path, `{"task":""}`, nil, OrgHeader, org.Slug), 400)
}

func TestSemanticOffWithoutEmbeddingModel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	st := memory.New()
	require.NoError(t, st.ReplaceRepo(ctx, storetest.Repo, storetest.Fixture()))
	acc := accounts.NewService(acctmem.New(), true)
	bootstrapAccounts(t, acc)
	ts := httptest.NewServer(New(ctx, Config{Accounts: acc}, st, slog.New(slog.NewTextHandler(io.Discard, nil))))
	t.Cleanup(ts.Close)
	c := newClient(t, ts)
	require.Equal(t, c.do("POST", "/api/v1/auth/signup", `{"email":"ada@example.com","name":"Ada","password":"`+pw+`"}`, nil), 201)
	var org accounts.Org
	require.Equal(t, c.do("POST", "/api/v1/accounts/orgs", `{"name":"Acme"}`, &org), 201)
	u, err := acc.St.UserByEmail(ctx, "ada@example.com")
	require.NoError(t, err)
	require.NoError(t, acc.St.AddRepository(ctx, accounts.Repository{OrgID: org.ID, Graph: storetest.Repo, AddedBy: u.ID}))
	var out struct{ Enabled bool }
	require.Equal(t, c.do("POST", "/api/v1/repos/"+url.PathEscape(storetest.Repo)+"/semantic", `{"task":"serve"}`, &out, OrgHeader, org.Slug), 200)
	assert.False(t, out.Enabled)
}

// vectorIDs lists the IDs a namespace of the symbol vector index holds.
func vectorIDs(t *testing.T, st *memory.Store, tenant, graph string) []string {
	t.Helper()
	page, err := st.CodeVectors().(vector.Sweeper).Scan(context.Background(), tenant, graph, "", vector.MaxScan)
	require.NoError(t, err)
	var out []string
	for _, it := range page {
		out = append(out, it.ID)
	}
	return out
}

func TestIngestPrunesStaleSymbolVectors(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	st := memory.New()
	g := storetest.Fixture()
	require.NoError(t, st.ReplaceRepo(ctx, storetest.Repo, g))
	srv := fakeEmbeddings(t)
	reg := llm.NewRegistry(llm.Config{Providers: []llm.ProviderConfig{{Name: "ollama", Kind: llm.KindOpenAI, BaseURL: srv.URL + "/v1",
		Model: "qwen", EmbeddingModel: "nomic", TimeoutSeconds: 5}}, Default: "ollama"})
	api := New(ctx, Config{LLM: reg}, st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	id, err := execution.With(ctx, execution.Identity{TenantID: "org", SubjectID: "user", SessionID: "thread", ParentSessionID: "thread"})
	require.NoError(t, err)
	api.embedIngest(id, storetest.Repo, g, nil)
	before := vectorIDs(t, st, semantic.Tenant, storetest.Repo)
	require.True(t, len(before) > 1)
	// A vector of a symbol that is gone, and one of another graph.
	require.NoError(t, st.CodeVectors().Upsert(ctx, "nomic", []vector.Item{
		{ID: storetest.ID(model.KindFunction, "app.Gone") + "#0011223344556677", Tenant: semantic.Tenant, Namespace: storetest.Repo, Vector: []float32{1, 0, 0}},
		{ID: "other#0011223344556677", Tenant: semantic.Tenant, Namespace: "other", Vector: []float32{1, 0, 0}},
	}))

	h, ok := g.Node(storetest.ID(model.KindFunction, "app.Handler"))
	require.True(t, ok)
	h.Summary = "a new description"
	require.NoError(t, st.ReplaceRepo(ctx, storetest.Repo, g))
	api.embedIngest(id, storetest.Repo, g, nil)

	after := vectorIDs(t, st, semantic.Tenant, storetest.Repo)
	assert.Equal(t, len(after), len(before), "one vector per symbol: the old description and the gone symbol went")
	assert.Equal(t, after, slices.Sorted(maps.Keys(semantic.Live(g.Nodes()))))
	assert.Len(t, vectorIDs(t, st, semantic.Tenant, "other"), 1, "another graph is left alone")
}

func TestDeletingARepositoryRemovesItsVectors(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	st := memory.New()
	require.NoError(t, st.ReplaceRepo(ctx, storetest.Repo, storetest.Fixture()))
	other := model.NewGraph()
	other.AddNode(model.Node{ID: model.NodeID("other", model.KindRepository, "other"), Kind: model.KindRepository, Repo: "other", Name: "other"})
	require.NoError(t, st.ReplaceRepo(ctx, "other", other))
	vec := []float32{1, 0, 0}
	require.NoError(t, st.CodeVectors().Upsert(ctx, "nomic", []vector.Item{
		{ID: "fixture-1", Tenant: semantic.Tenant, Namespace: storetest.Repo, Vector: vec},
		{ID: "fixture-2", Tenant: semantic.Tenant, Namespace: storetest.Repo, Vector: vec},
		{ID: "other-1", Tenant: semantic.Tenant, Namespace: "other", Vector: vec},
		{ID: "orphan-1", Tenant: semantic.Tenant, Namespace: "deleted-before", Vector: vec},
		// A vector of another tenant under the same namespace name.
		{ID: "memory-1", Tenant: "org-a", Namespace: storetest.Repo, Vector: vec},
	}))
	acc := accounts.NewService(acctmem.New(), true)
	bootstrapAccounts(t, acc)
	api := New(ctx, Config{Accounts: acc}, st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ts := httptest.NewServer(api)
	t.Cleanup(ts.Close)

	signup := func(email string) *client {
		c := newClient(t, ts)
		require.Equal(t, c.do("POST", "/api/v1/auth/signup", `{"email":"`+email+`","name":"`+email+`","password":"`+pw+`"}`, nil), 201)
		return c
	}
	bob, ada := signup("bob@example.com"), signup("ada@example.com")
	var team, side accounts.Org
	require.Equal(t, bob.do("POST", "/api/v1/accounts/orgs", `{"name":"Team"}`, &team), 201)
	require.Equal(t, ada.do("POST", "/api/v1/accounts/orgs", `{"name":"Side"}`, &side), 201)
	bobUser, err := acc.St.UserByEmail(ctx, "bob@example.com")
	require.NoError(t, err)
	adaUser, err := acc.St.UserByEmail(ctx, "ada@example.com")
	require.NoError(t, err)
	require.NoError(t, acc.St.AddRepository(ctx, accounts.Repository{OrgID: team.ID, Graph: storetest.Repo, AddedBy: bobUser.ID}))
	require.NoError(t, acc.St.AddRepository(ctx, accounts.Repository{OrgID: side.ID, Graph: storetest.Repo, AddedBy: adaUser.ID}))
	require.NoError(t, acc.St.AddRepository(ctx, accounts.Repository{OrgID: team.ID, Graph: "other", AddedBy: bobUser.ID}))

	require.Equal(t, bob.do("DELETE", "/api/v1/repos/"+storetest.Repo, "", nil), 204)
	assert.Len(t, vectorIDs(t, st, semantic.Tenant, storetest.Repo), 2, "another organisation still has the graph")

	require.Equal(t, ada.do("DELETE", "/api/v1/repos/"+storetest.Repo, "", nil), 204)
	assert.Len(t, vectorIDs(t, st, semantic.Tenant, storetest.Repo), 0, "the last link took the graph and its vectors")
	assert.Equal(t, vectorIDs(t, st, semantic.Tenant, "other"), []string{"other-1"}, "other repositories keep theirs")
	assert.Equal(t, vectorIDs(t, st, "org-a", storetest.Repo), []string{"memory-1"}, "and other tenants theirs")

	// The sweep finds the vectors of a graph deleted without them.
	vs, ok := api.codeVectors()
	require.True(t, ok)
	assert.Equal(t, api.sweepVectorsOnce(ctx, vs, ""), "")
	assert.Len(t, vectorIDs(t, st, semantic.Tenant, "deleted-before"), 0)
	assert.Equal(t, vectorIDs(t, st, semantic.Tenant, "other"), []string{"other-1"})
}
