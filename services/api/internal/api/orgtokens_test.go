package api

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/parisnakitakejser/codesynapse/internal/accounts"
	acctmem "github.com/parisnakitakejser/codesynapse/internal/accounts/memstore"
	"github.com/parisnakitakejser/codesynapse/internal/gitx"
	"github.com/parisnakitakejser/codesynapse/internal/model"
	"github.com/parisnakitakejser/codesynapse/internal/store/memory"
	"github.com/parisnakitakejser/codesynapse/internal/testutil/assert"
	"github.com/parisnakitakejser/codesynapse/internal/testutil/require"
)

// fakeHost decides which tokens read which remotes.
type fakeHost struct {
	mu      sync.Mutex
	public  map[string]bool            // remote -> readable anonymously
	readers map[string]map[string]bool // remote -> tokens that read it
}

func (f *fakeHost) canRead(_ context.Context, remote string, auth gitx.Auth) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.public[remote] || f.readers[remote][auth.Token] {
		return nil
	}
	return gitx.ErrNoAccess
}

func (f *fakeHost) allow(remote, token string, ok bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.readers[remote] == nil {
		f.readers[remote] = map[string]bool{}
	}
	f.readers[remote][token] = ok
}

func TestPrivateRepositoriesWithOrgTokens(t *testing.T) {
	const (
		secret     = "https://127.0.0.1/acme/secret.git"
		public     = "https://127.0.0.1/acme/public.git"
		bobToken   = "glpat-team-token-0001"
		bobToken2  = "glpat-team-token-0002"
		daveToken  = "glpat-other-token-001"
		secretRepo = "acme/secret"
	)
	host := &fakeHost{public: map[string]bool{public: true}, readers: map[string]map[string]bool{}}
	host.allow(secret, bobToken, true)
	prev := canRead
	canRead = host.canRead
	t.Cleanup(func() { canRead = prev })

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	st := memory.New()
	g := model.NewGraph()
	g.AddNode(model.Node{ID: model.NodeID(secretRepo, model.KindRepository, secretRepo), Kind: model.KindRepository, Repo: secretRepo, Name: secretRepo, URL: secret})
	require.NoError(t, st.ReplaceRepo(ctx, secretRepo, g))
	acc := accounts.NewService(acctmem.New(), true)
	key := make([]byte, 32)
	key[5] = 9
	sealer, err := accounts.NewSealer(key)
	require.NoError(t, err)
	acc.Secrets = sealer
	srv := New(ctx, Config{Accounts: acc, Workspace: t.TempDir(), AllowPrivateGit: true}, st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)

	signup := func(email string) *client {
		c := newClient(t, ts)
		require.Equal(t, c.do("POST", "/api/v1/auth/signup", `{"email":"`+email+`","name":"`+email+`","password":"`+pw+`"}`, nil), 201)
		return c
	}
	ada := signup("ada@example.com") // platform admin
	bob := signup("bob@example.com")
	carol := signup("carol@example.com")
	dave := signup("dave@example.com")
	var team, other accounts.Org
	require.Equal(t, bob.do("POST", "/api/v1/accounts/orgs", `{"name":"Team"}`, &team), 201)
	require.Equal(t, dave.do("POST", "/api/v1/accounts/orgs", `{"name":"Other"}`, &other), 201)
	require.Equal(t, bob.do("PUT", "/api/v1/accounts/orgs/"+team.Slug+"/members", `{"email":"carol@example.com","role":"member"}`, nil), 200)
	inTeam := []string{OrgHeader, team.Slug}
	inOther := []string{OrgHeader, other.Slug}
	register := func(c *client, url, name string, org []string) (int, string) {
		var body string
		code := c.do("POST", "/api/v1/repos", `{"url":"`+url+`","name":"`+name+`"}`, &body, org...)
		return code, body
	}
	visible := func(c *client, org []string) []string {
		var list struct{ Repos []struct{ Name string } }
		require.Equal(t, c.do("GET", "/api/v1/repos", "", &list, org...), 200)
		var names []string
		for _, r := range list.Repos {
			names = append(names, r.Name)
		}
		return names
	}
	tokensPath := "/api/v1/accounts/orgs/" + team.Slug + "/git-tokens/"

	code, body := register(bob, secret, secretRepo, inTeam)
	assert.Equal(t, code, 403, body)
	assert.Contains(t, body, "add a Git token for 127.0.0.1", "says what to do")

	assert.Equal(t, carol.do("PUT", tokensPath+"127.0.0.1", `{"token":"`+bobToken+`"}`, nil), 404, "members do not manage tokens")
	var set string
	require.Equal(t, bob.do("PUT", tokensPath+"127.0.0.1", `{"token":"`+bobToken+`"}`, &set), 200)
	assert.Contains(t, set, "glpat***-0001")
	assert.False(t, strings.Contains(set, bobToken), "the token is never returned")
	var listed string
	require.Equal(t, carol.do("GET", "/api/v1/accounts/orgs/"+team.Slug+"/git-tokens", "", &listed), 200, "members see hosts and previews")
	assert.False(t, strings.Contains(listed, bobToken))

	code, body = register(bob, secret, secretRepo, inTeam)
	require.Equal(t, code, 200, body)
	assert.Contains(t, body, `"visibility":"private"`)
	assert.Equal(t, visible(bob, inTeam), []string{secretRepo}, "proved access: the private graph is shared with the organisation")
	assert.Equal(t, srv.syncAuth(ctx, secretRepo, secret), gitx.Auth{Host: "127.0.0.1", Token: bobToken}, "syncs use the organisation's token")

	code, body = register(dave, secret, secretRepo, inOther)
	assert.Equal(t, code, 403, body)
	require.Equal(t, dave.do("PUT", "/api/v1/accounts/orgs/"+other.Slug+"/git-tokens/127.0.0.1", `{"token":"`+daveToken+`"}`, nil), 200)
	code, body = register(dave, secret, secretRepo, inOther)
	assert.Equal(t, code, 403, body)
	assert.Contains(t, body, "cannot read this repository", "a token without access proves nothing")
	code, body = register(dave, public, secretRepo, inOther)
	assert.Equal(t, code, 409, body)
	assert.Contains(t, body, "already used for another source", "a public URL under a private graph's name opens nothing")
	assert.Equal(t, len(visible(dave, inOther)), 0)

	// The token stops working: the next check takes the graph away, a new
	// working token brings it back.
	host.allow(secret, bobToken, false)
	srv.verifyAccess(ctx, secretRepo)
	assert.Equal(t, len(visible(bob, inTeam)), 0, "lost access hides the graph")
	host.allow(secret, bobToken2, true)
	var restored struct{ Restored []string }
	require.Equal(t, bob.do("PUT", tokensPath+"127.0.0.1", `{"token":"`+bobToken2+`"}`, &restored), 200)
	assert.Equal(t, restored.Restored, []string{secretRepo})
	assert.Equal(t, visible(bob, inTeam), []string{secretRepo})

	// Fix branches: an organisation pushes with its own token.
	req := httptest.NewRequest("POST", "/", nil)
	req = req.WithContext(context.WithValue(ctx, scopeKey{}, scope{graphs: map[string]accounts.Role{secretRepo: accounts.Owner},
		orgs: []accounts.Membership{{Org: team, Role: accounts.Owner}}}))
	push, err := srv.pushAuth(req, secretRepo, secret)
	require.NoError(t, err)
	assert.Equal(t, push.Token, bobToken2)
	req = req.WithContext(context.WithValue(ctx, scopeKey{}, scope{graphs: map[string]accounts.Role{}, orgs: []accounts.Membership{{Org: other, Role: accounts.Owner}}}))
	_, err = srv.pushAuth(req, secretRepo, secret)
	assert.Error(t, err, "an organisation that does not use the repository cannot push to it")

	var audit string
	require.Equal(t, ada.do("GET", "/api/v1/admin/audit?limit=500&format=jsonl", "", &audit), 200)
	for _, action := range []string{"org.git_token.set", "repo.access_lost", "repo.access_restored"} {
		assert.Contains(t, audit, action)
	}
	for _, tok := range []string{bobToken, bobToken2, daveToken} {
		assert.False(t, strings.Contains(audit, tok), "tokens never reach the audit log")
	}
	require.Equal(t, bob.do("DELETE", tokensPath+"127.0.0.1", "", nil), 204)
	srv.verifyAccess(ctx, secretRepo)
	assert.Equal(t, len(visible(bob, inTeam)), 0, "no token, no access")
}

func TestOrgTokensNeedASecretKey(t *testing.T) {
	ts, _ := accountsServer(t)
	bob := newClient(t, ts)
	var signed struct{ Org accounts.Org }
	require.Equal(t, bob.do("POST", "/api/v1/auth/signup", `{"email":"bob@example.com","name":"Bob","password":"`+pw+`"}`, &signed), 201)
	var body string
	assert.Equal(t, bob.do("PUT", "/api/v1/accounts/orgs/"+signed.Org.Slug+"/git-tokens/github.com", `{"token":"ghp_abcdefghijklmnop"}`, &body), 409)
	assert.Contains(t, body, "CODESYNAPSE_SECRET_KEY")
}

func TestSettingsHideInstallationTokensFromOrganisations(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	acc := accounts.NewService(acctmem.New(), true)
	srv := New(ctx, Config{Accounts: acc, GitAuths: []gitx.Auth{{Host: "github.com", Token: "ghp_installation_secret_token"}}}, memory.New(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	ada, bob := newClient(t, ts), newClient(t, ts)
	require.Equal(t, ada.do("POST", "/api/v1/auth/signup", `{"email":"ada@example.com","name":"Ada","password":"`+pw+`"}`, nil), 201)
	require.Equal(t, bob.do("POST", "/api/v1/auth/signup", `{"email":"bob@example.com","name":"Bob","password":"`+pw+`"}`, nil), 201)
	var asAdmin, asBob string
	require.Equal(t, ada.do("GET", "/api/v1/settings", "", &asAdmin), 200)
	require.Equal(t, bob.do("GET", "/api/v1/settings", "", &asBob), 200)
	assert.Contains(t, asAdmin, "github.com", "the platform admin sees the installation's hosts")
	assert.False(t, strings.Contains(asBob, "github.com") || strings.Contains(asBob, "ghp_i"), "organisation users do not")
}
