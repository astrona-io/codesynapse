package api

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/astrona-io/codesynapse/internal/accounts"
	acctmem "github.com/astrona-io/codesynapse/internal/accounts/memstore"
	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/store/memory"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

func TestCustomRolesOverHTTP(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	st := memory.New()
	for _, name := range []string{"acme/api", "acme/web"} {
		g := model.NewGraph()
		g.AddNode(model.Node{ID: model.NodeID(name, model.KindRepository, name), Kind: model.KindRepository, Repo: name, Name: name})
		require.NoError(t, st.ReplaceRepo(ctx, name, g))
	}
	acc := accounts.NewService(acctmem.New(), true)
	bootstrapAccounts(t, acc)
	srv := New(ctx, Config{Accounts: acc}, st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	signup := func(email string) *client {
		c := newClient(t, ts)
		require.Equal(t, c.signUp(`{"email":"`+email+`","name":"`+email+`","password":"`+pw+`"}`, nil), 202)
		return c
	}
	bob, carol := signup("bob@example.com"), signup("carol@example.com")
	var acme accounts.Org
	require.Equal(t, bob.do("POST", "/api/v1/accounts/orgs", `{"name":"Acme"}`, &acme), 201)
	bobUser, err := acc.St.UserByEmail(ctx, "bob@example.com")
	require.NoError(t, err)
	for _, g := range []string{"acme/api", "acme/web"} {
		require.NoError(t, acc.St.AddRepository(ctx, accounts.Repository{OrgID: acme.ID, Graph: g, AddedBy: bobUser.ID, AddedAt: time.Now()}))
		_, err := acc.SetRepoGrant(ctx, accounts.Principal{User: bobUser, Via: "session"}, acme.ID, g, accounts.Tuple{SubjectType: accounts.TypeUser, SubjectID: bobUser.ID}, accounts.Full)
		require.NoError(t, err)
	}
	invite(t, bob, acme.Slug, carol, "carol@example.com", "guest")
	inAcme := []string{OrgHeader, acme.Slug}
	roles := "/api/v1/accounts/orgs/" + acme.Slug + "/roles"

	var catalog struct {
		Permissions []struct{ Permission, Resource, Area, Level string }
	}
	require.Equal(t, bob.do("GET", "/api/v1/accounts/permissions", "", &catalog), 200)
	assert.True(t, len(catalog.Permissions) > 20)
	for _, p := range catalog.Permissions {
		assert.True(t, p.Level != "", "markers are not offered", p.Permission)
	}

	var role accounts.CustomRole
	body := `{"name":"Security triager","resource":"repository","permissions":["repository.security:write"]}`
	require.Equal(t, bob.do("POST", roles, body, &role), 201)
	assert.Equal(t, bob.do("POST", roles, body, nil), 409, "names are unique")
	assert.Equal(t, bob.do("POST", roles, `{"name":"x","resource":"repository","permissions":["repository.code:delete"]}`, nil), 400)
	assert.Equal(t, carol.do("POST", roles, body, nil), 404, "guests do not see roles")

	access := "/api/v1/accounts/orgs/" + acme.Slug + "/repos/" + url.PathEscape("acme/web") + "/access"
	require.Equal(t, bob.do("PUT", access, `{"user":"carol@example.com","role":"role:`+role.ID+`"}`, nil), 200)
	var list struct{ Repos []struct{ Name string } }
	require.Equal(t, carol.do("GET", "/api/v1/repos", "", &list, inAcme...), 200)
	require.Len(t, list.Repos, 1)
	assert.Equal(t, list.Repos[0].Name, "acme/web")
	assert.Equal(t, bob.do("DELETE", roles+"/"+role.ID, "", nil), 409, "a granted role stays")
	require.Equal(t, bob.do("PUT", roles+"/"+role.ID, `{"name":"Triager","resource":"repository","permissions":["repository.security:read"]}`, &role), 200)
	assert.Equal(t, role.Name, "Triager")

	// A scoped key: code reading only, on what Bob holds now.
	var created struct {
		Key    string
		APIKey accounts.APIKey `json:"api_key"`
	}
	assert.Equal(t, carol.do("POST", "/api/v1/me/keys", `{"org":"`+acme.Slug+`","name":"wide","scope":["repository.fixes:write"]}`, nil), 403, "beyond what she holds")
	require.Equal(t, bob.do("POST", "/api/v1/me/keys", `{"org":"`+acme.Slug+`","name":"reader","scope":["repository.code:read"]}`, &created), 201)
	assert.Equal(t, created.APIKey.Scope, []accounts.Permission{accounts.RepoCodeRead})
	agent := newClient(t, ts)
	agent.auth = "Bearer " + created.Key
	require.Equal(t, agent.do("GET", "/api/v1/repos", "", &list), 200)
	assert.Len(t, list.Repos, 2)
	assert.Equal(t, agent.do("POST", "/api/v1/repos/"+url.PathEscape("acme/web")+"/sync", "", nil), 403, "the key may only read code")
	assert.Equal(t, agent.do("GET", "/api/v1/accounts/orgs/"+acme.Slug+"/members", "", nil), 404, "nor read members")
	assert.Equal(t, agent.do("POST", roles, `{"name":"k","resource":"repository","permissions":["repository.code:read"]}`, nil), 404, "keys never manage roles")
}
