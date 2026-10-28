package api

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
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

func TestImportListUsesTheOrganisationsToken(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	var seen []string
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Header.Get("Authorization"))
		if r.URL.Path != "/orgs/work/repos" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") == "" {
			_, _ = fmt.Fprint(w, `[{"name":"open","default_branch":"main"}]`)
			return
		}
		w.Header().Set("X-GitHub-SSO", "partial-results; organizations=1")
		_, _ = fmt.Fprint(w, `[
			{"name":"open","default_branch":"main","permissions":{"admin":false,"push":false,"pull":true}},
			{"name":"closed","default_branch":"main","private":true,"permissions":{"admin":false,"push":true,"pull":true}},
			{"name":"owned","private":true,"permissions":{"admin":true,"push":true,"pull":true}}
		]`)
	}))
	t.Cleanup(gh.Close)
	acc := accounts.NewService(acctmem.New(), true)
	bootstrapAccounts(t, acc)
	key := make([]byte, 32)
	key[3] = 7
	sealer, err := accounts.NewSealer(key)
	require.NoError(t, err)
	acc.Secrets = sealer
	srv := New(ctx, Config{Accounts: acc, Workspace: t.TempDir(), ForgeAPI: gh.URL}, memory.New(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)

	bob := newClient(t, ts)
	require.Equal(t, bob.do("POST", "/api/v1/auth/signup", `{"email":"bob@example.com","name":"Bob","password":"`+pw+`"}`, nil), 201)
	var org accounts.Org
	require.Equal(t, bob.do("POST", "/api/v1/accounts/orgs", `{"name":"Work"}`, &org), 201)
	type answer struct {
		Repos         []ImportCandidate
		Authenticated bool
		SSORestricted bool `json:"sso_restricted"`
	}
	list := func() answer {
		var got answer
		require.Equal(t, bob.do("POST", "/api/v1/imports/list", `{"org":"work"}`, &got, OrgHeader, org.Slug), 200)
		return got
	}

	got := list()
	assert.False(t, got.Authenticated)
	require.Len(t, got.Repos, 1, "without a token only public repositories are listed")
	assert.Equal(t, seen[len(seen)-1], "", "anonymous without a token")

	require.Equal(t, bob.do("PUT", "/api/v1/accounts/orgs/"+org.Slug+"/git-tokens/github.com", `{"token":"ghp_listing_token_123456"}`, nil), 200)
	got = list()
	assert.True(t, got.Authenticated)
	assert.True(t, got.SSORestricted, "GitHub's SAML hint is passed on")
	require.Len(t, got.Repos, 3, "the organisation's token lists private repositories")
	assert.Equal(t, seen[len(seen)-1], "Bearer ghp_listing_token_123456")
	perms := map[string]string{}
	for _, r := range got.Repos {
		perms[r.Name] = r.Permission
		assert.Equal(t, r.URL, "https://github.com/work/"+r.Name+".git", "clone URLs are built, never taken from the answer")
	}
	assert.Equal(t, perms, map[string]string{"open": "read", "closed": "write", "owned": "admin"})
	assert.True(t, got.Repos[1].Private)

	events, err := acc.St.ListEvents(ctx, accounts.EventFilter{Action: "git_token.list"})
	require.NoError(t, err)
	assert.Len(t, events, 1, "listing with the token is audited")
}
