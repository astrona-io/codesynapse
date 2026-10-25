package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/astrona-io/codesynapse/internal/accounts"
	acctmem "github.com/astrona-io/codesynapse/internal/accounts/memstore"
	"github.com/astrona-io/codesynapse/internal/execution"
	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/store/memory"
	"github.com/astrona-io/codesynapse/internal/store/storetest"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
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
