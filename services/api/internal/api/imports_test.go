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
	"github.com/astrona-io/codesynapse/internal/store/memory"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

func TestImportListShowsPublicRepositories(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	acc := accounts.NewService(acctmem.New(), true)
	bootstrapAccounts(t, acc)
	srv := New(ctx, Config{Accounts: acc, Workspace: t.TempDir(), ForgeAPI: fakeGitHub(t).URL}, memory.New(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)

	bob := newClient(t, ts)
	var signed struct{ User accounts.User }
	require.Equal(t, bob.do("POST", "/api/v1/auth/signup", `{"email":"bob@example.com","name":"Bob","password":"`+pw+`"}`, &signed), 201)
	var org accounts.Org
	require.Equal(t, bob.do("POST", "/api/v1/accounts/orgs", `{"name":"Platform Team"}`, &org), 201)
	require.NoError(t, acc.St.AddRepository(ctx, accounts.Repository{OrgID: org.ID, Graph: "acme/api", Remote: "https://github.com/acme/api.git",
		Visibility: accounts.Public, AddedBy: signed.User.ID, AddedAt: time.Now().UTC()}))

	list := func(c *client, body string, out any) int {
		return c.do("POST", "/api/v1/imports/list", body, out, OrgHeader, org.Slug)
	}
	var got struct {
		Host  string
		Org   string
		Repos []ImportCandidate
	}
	require.Equal(t, list(bob, `{"org":"https://github.com/acme/*"}`, &got), 200)
	assert.Equal(t, got.Host, "github.com")
	assert.Equal(t, got.Org, "acme")
	require.Len(t, got.Repos, 1, "private, archived and forked repositories are left out")
	assert.Equal(t, got.Repos[0], ImportCandidate{Name: "api", Repo: "acme/api", URL: "https://github.com/acme/api.git", DefaultBranch: "main", Added: true})

	require.Equal(t, list(bob, `{"org":"acme","include_archived":true,"include_forks":true}`, &got), 200)
	assert.Equal(t, len(got.Repos), 3, "archived and forks on request; private never")
	assert.Equal(t, list(bob, `{"org":"nobody"}`, nil), 404)
	assert.Equal(t, list(bob, `{"org":"../etc"}`, nil), 400)
	assert.Equal(t, list(bob, `{"org":"team","host":"127.0.0.1"}`, nil), 400, "user-given hosts pass the SSRF check")

	var created struct{ Key string }
	require.Equal(t, bob.do("POST", "/api/v1/me/keys", `{"org":"`+org.Slug+`","name":"laptop"}`, &created), 201)
	agent := newClient(t, ts)
	agent.auth = "Bearer " + created.Key
	assert.Equal(t, list(agent, `{"org":"acme"}`, nil), 403, "importing is a signed-in session's work")
	eve := newClient(t, ts)
	require.Equal(t, eve.do("POST", "/api/v1/auth/signup", `{"email":"eve@example.com","name":"Eve","password":"`+pw+`"}`, nil), 201)
	assert.Equal(t, list(eve, `{"org":"acme"}`, nil), 404, "other organisations cannot use it")
}
