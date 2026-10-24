package api

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/astrona-io/codesynapse/internal/accounts"
	acctmem "github.com/astrona-io/codesynapse/internal/accounts/memstore"
	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/store/memory"
	"github.com/astrona-io/codesynapse/internal/store/storetest"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

func TestSiteSearch(t *testing.T) {
	ts, _ := newTestServer(t)
	var got SiteSearch
	require.Equal(t, get(t, ts, "/api/v1/search?q=Handler", &got), 200)
	require.NotEmpty(t, got.Results)
	assert.Equal(t, got.Results[0].Kind, model.KindFunction)
	assert.Equal(t, got.Results[0].Name, "app.Handler")
	assert.Equal(t, got.Results[0].Repo, storetest.Repo)
	for _, h := range got.Results {
		assert.NotEqual(t, h.Name, "app.TestHandler", "tests are left out")
	}
	assert.Greater(t, got.Searched, 0)

	require.Equal(t, get(t, ts, "/api/v1/search?q="+storetest.Repo[:4], &got), 200)
	require.NotEmpty(t, got.Results)
	assert.Equal(t, got.Results[0].Kind, model.KindRepository, "a repository whose name matches comes first")

	assert.Equal(t, get(t, ts, "/api/v1/search?q=a", nil), 400)
	assert.Equal(t, get(t, ts, "/api/v1/search", nil), 400)
}

// TestSiteSearchStaysInScope: another organisation's repository is never
// searched.
func TestIsTestPath(t *testing.T) {
	for file, want := range map[string]bool{
		"a/b_test.go": true, "tests/test_api.py": true, "pkg/api_test.py": true, "web/x.test.ts": true, "web/x.spec.js": true,
		"web/__tests__/x.ts": true, "a/b.go": false, "testing/helpers.go": false, "web/latest.ts": false,
	} {
		assert.Equal(t, isTestPath(file), want, file)
	}
}

func TestSiteSearchStaysInScope(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	st := memory.New()
	require.NoError(t, st.ReplaceRepo(ctx, storetest.Repo, storetest.Fixture()))
	other := model.NewGraph()
	other.AddNode(model.Node{ID: model.NodeID("ext/secret", model.KindRepository, "ext/secret"), Kind: model.KindRepository, Repo: "ext/secret", Name: "ext/secret"})
	other.AddNode(model.Node{ID: model.NodeID("ext/secret", model.KindFunction, "secret.Handler"), Kind: model.KindFunction, Repo: "ext/secret", Name: "Handler", QualifiedName: "secret.Handler", File: "s.go", StartLine: 1, EndLine: 2})
	require.NoError(t, st.ReplaceRepo(ctx, "ext/secret", other))
	acc := accounts.NewService(acctmem.New(), true)
	bootstrapAccounts(t, acc)
	ts := httptest.NewServer(New(ctx, Config{Accounts: acc}, st, slog.New(slog.NewTextHandler(io.Discard, nil))))
	t.Cleanup(ts.Close)
	signup := func(email string) *client {
		c := newClient(t, ts)
		require.Equal(t, c.do("POST", "/api/v1/auth/signup", `{"email":"`+email+`","name":"`+email+`","password":"`+pw+`"}`, nil), 201)
		return c
	}
	_ = signup("ada@example.com")
	bob, dave := signup("bob@example.com"), signup("dave@example.com")
	var acme, ext accounts.Org
	require.Equal(t, bob.do("POST", "/api/v1/accounts/orgs", `{"name":"Acme"}`, &acme), 201)
	require.Equal(t, dave.do("POST", "/api/v1/accounts/orgs", `{"name":"Ext"}`, &ext), 201)
	for _, link := range []struct{ org, email, graph string }{{acme.ID, "bob@example.com", storetest.Repo}, {ext.ID, "dave@example.com", "ext/secret"}} {
		u, err := acc.St.UserByEmail(ctx, link.email)
		require.NoError(t, err)
		require.NoError(t, acc.St.AddRepository(ctx, accounts.Repository{OrgID: link.org, Graph: link.graph, AddedBy: u.ID, AddedAt: time.Now()}))
	}

	var got SiteSearch
	require.Equal(t, bob.do("GET", "/api/v1/search?q=Handler", "", &got, OrgHeader, acme.Slug), 200)
	require.NotEmpty(t, got.Results)
	for _, h := range got.Results {
		assert.Equal(t, h.Repo, storetest.Repo, "only Acme's repository is searched")
	}
	require.Equal(t, dave.do("GET", "/api/v1/search?q=Handler", "", &got, OrgHeader, ext.Slug), 200)
	require.Len(t, got.Results, 1)
	assert.Equal(t, got.Results[0].Repo, "ext/secret")
}
