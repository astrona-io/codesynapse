package api

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/astrona-io/codesynapse/internal/accounts"
	acctmem "github.com/astrona-io/codesynapse/internal/accounts/memstore"
	"github.com/astrona-io/codesynapse/internal/gitx"
	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/store/memory"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

func TestUserGitTokens(t *testing.T) {
	const (
		mine       = "https://127.0.0.1/carol/mine.git"
		shared     = "https://127.0.0.1/acme/shared.git"
		carolToken = "glpat-carol-token-0001"
		orgToken   = "glpat-team-token-00001"
		mineRepo   = "carol/mine"
		sharedRepo = "acme/shared"
	)
	host := &fakeHost{public: map[string]bool{}, readers: map[string]map[string]bool{}}
	// The organisation's token reads both: a personal entry must still
	// never use it.
	host.allow(mine, orgToken, true)
	host.allow(shared, orgToken, true)
	prev := canRead
	canRead = host.canRead
	t.Cleanup(func() { canRead = prev })

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	st := memory.New()
	for name, url := range map[string]string{mineRepo: mine, sharedRepo: shared} {
		g := model.NewGraph()
		g.AddNode(model.Node{ID: model.NodeID(name, model.KindRepository, name), Kind: model.KindRepository, Repo: name, Name: name, URL: url})
		require.NoError(t, st.ReplaceRepo(ctx, name, g))
	}
	acc := accounts.NewService(acctmem.New(), true)
	bootstrapAccounts(t, acc)
	key := make([]byte, 32)
	key[7] = 3
	sealer, err := accounts.NewSealer(key)
	require.NoError(t, err)
	acc.Secrets = sealer
	srv := New(ctx, Config{Accounts: acc, Workspace: t.TempDir(), AllowPrivateGit: true}, st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)

	signup := func(email string) *client {
		c := newClient(t, ts)
		require.Equal(t, c.signUp(`{"email":"`+email+`","name":"`+email+`","password":"`+pw+`"}`, nil), 202)
		return c
	}
	bob := signup("bob@example.com")
	carol := signup("carol@example.com")
	var team accounts.Org
	require.Equal(t, bob.do("POST", "/api/v1/accounts/orgs", `{"name":"Team"}`, &team), 201)
	require.Equal(t, bob.do("PUT", "/api/v1/accounts/orgs/"+team.Slug+"/members", `{"email":"carol@example.com","role":"member"}`, nil), 200)
	inTeam := []string{OrgHeader, team.Slug}
	register := func(c *client, url, name, space string) (int, string) {
		var body string
		session := "register-" + strings.ReplaceAll(name, "/", "-")
		c.do("POST", "/api/v1/execution/sessions", `{"session_id":"`+session+`"}`, nil, inTeam...)
		headers := append(append([]string{}, inTeam...), runtimeHeaders(session)...)
		code := c.do("POST", "/api/v1/repos", `{"url":"`+url+`","name":"`+name+`","space":"`+space+`"}`, &body, headers...)
		return code, body
	}
	visible := func(c *client) []string {
		var list struct{ Repos []struct{ Name string } }
		require.Equal(t, c.do("GET", "/api/v1/repos", "", &list, inTeam...), 200)
		var names []string
		for _, r := range list.Repos {
			names = append(names, r.Name)
		}
		return names
	}
	require.Equal(t, bob.do("PUT", "/api/v1/accounts/orgs/"+team.Slug+"/git-tokens/127.0.0.1", `{"token":"`+orgToken+`"}`, nil), 200)

	t.Run("only a signed-in session manages its own tokens", func(t *testing.T) {
		var created struct{ Key string }
		require.Equal(t, carol.do("POST", "/api/v1/me/keys", `{"org":"`+team.Slug+`","name":"agent"}`, &created), 201)
		agent := newClient(t, ts)
		agent.auth = "Bearer " + created.Key
		assert.Equal(t, agent.do("GET", "/api/v1/me/git-tokens", "", nil), 403)
		assert.Equal(t, agent.do("PUT", "/api/v1/me/git-tokens/127.0.0.1", `{"token":"`+carolToken+`"}`, nil), 403)
		assert.Equal(t, agent.do("DELETE", "/api/v1/me/git-tokens/127.0.0.1", "", nil), 403)
		assert.Equal(t, newClient(t, ts).do("GET", "/api/v1/me/git-tokens", "", nil), 401)
	})

	t.Run("a personal private repository needs the owner's own token", func(t *testing.T) {
		code, body := register(carol, mine, mineRepo, spaceMe)
		assert.Equal(t, code, 403, body)
		assert.Contains(t, body, "your profile (Git tokens)", "the organisation's token is never used for a personal entry")

		var set, listed string
		require.Equal(t, carol.do("PUT", "/api/v1/me/git-tokens/127.0.0.1", `{"token":"`+carolToken+`"}`, &set), 200)
		assert.False(t, strings.Contains(set, carolToken), "the token is never returned")
		require.Equal(t, carol.do("GET", "/api/v1/me/git-tokens", "", &listed), 200)
		assert.Contains(t, listed, "127.0.0.1")
		assert.False(t, strings.Contains(listed, carolToken))
		var others string
		require.Equal(t, bob.do("GET", "/api/v1/me/git-tokens", "", &others), 200)
		assert.False(t, strings.Contains(others, "127.0.0.1"), "tokens are per person")

		code, body = register(carol, mine, mineRepo, spaceMe)
		assert.Equal(t, code, 403, body)
		assert.Contains(t, body, "your token for 127.0.0.1 cannot read", "a token without access proves nothing")
		host.allow(mine, carolToken, true)
		code, body = register(carol, mine, mineRepo, spaceMe)
		require.Equal(t, code, 200, body)
		assert.Contains(t, body, `"visibility":"private"`)
		assert.Contains(t, strings.Join(visible(carol), ","), mineRepo)
	})

	t.Run("each entry uses only its own token", func(t *testing.T) {
		code, body := register(bob, shared, sharedRepo, spaceOrg)
		require.Equal(t, code, 200, body)
		reach := scope{reach: map[string]accounts.Reach{mineRepo: {OrgID: team.ID, Role: accounts.Admin}, sharedRepo: {OrgID: team.ID, Role: accounts.Admin}}}
		inScope := context.WithValue(ctx, scopeKey{}, reach)
		assert.Equal(t, srv.syncAuth(inScope, mineRepo, mine), gitx.Auth{Host: "127.0.0.1", Token: carolToken}, "personal: the owner's token")
		assert.Equal(t, srv.syncAuth(inScope, sharedRepo, shared), gitx.Auth{Host: "127.0.0.1", Token: orgToken}, "organisation: the organisation's token")
		push, err := srv.pushAuthFor(inScope, mineRepo, mine)
		require.NoError(t, err)
		assert.Equal(t, push.Token, carolToken)
		push, err = srv.pushAuthFor(inScope, sharedRepo, shared)
		require.NoError(t, err)
		assert.Equal(t, push.Token, orgToken)
	})

	t.Run("removing the owner's token loses the entry, setting it again restores it", func(t *testing.T) {
		require.Equal(t, carol.do("DELETE", "/api/v1/me/git-tokens/127.0.0.1", "", nil), 204)
		srv.verifyAccess(ctx, mineRepo)
		assert.False(t, strings.Contains(strings.Join(visible(carol), ","), mineRepo), "no fallback to the organisation's token")
		_, err := srv.pushAuthFor(context.WithValue(ctx, scopeKey{}, scope{reach: map[string]accounts.Reach{mineRepo: {OrgID: team.ID, Role: accounts.Admin}}}), mineRepo, mine)
		assert.Error(t, err)
		var restored struct{ Restored []string }
		require.Equal(t, carol.do("PUT", "/api/v1/me/git-tokens/127.0.0.1", `{"token":"`+carolToken+`"}`, &restored), 200)
		assert.Equal(t, restored.Restored, []string{mineRepo})
	})

	var audit string
	require.Equal(t, rootClient(t, ts).do("GET", "/api/v1/admin/audit?limit=500&format=jsonl", "", &audit), 200)
	for _, action := range []string{"user.git_token.set", "user.git_token.remove", "repo.access_lost", "repo.access_restored"} {
		assert.Contains(t, audit, action)
	}
	for _, tok := range []string{carolToken, orgToken} {
		assert.False(t, strings.Contains(audit, tok), "tokens never reach the audit log")
	}
}
