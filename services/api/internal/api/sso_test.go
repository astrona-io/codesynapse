package api

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/astrona-io/codesynapse/internal/accounts"
	"github.com/astrona-io/codesynapse/internal/accounts/accountstest"
	acctmem "github.com/astrona-io/codesynapse/internal/accounts/memstore"
	"github.com/astrona-io/codesynapse/internal/sso/ssotest"
	"github.com/astrona-io/codesynapse/internal/store/memory"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

type ssoFixture struct {
	ts    *httptest.Server
	acc   *accounts.Service
	idp   *ssotest.Provider
	owner *client
	org   accounts.Org
	team  string
}

func newSSOFixture(t *testing.T) ssoFixture {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	acc := accounts.NewService(acctmem.New(), true)
	sealer, err := accounts.NewSealer([]byte("0123456789abcdef0123456789abcdef"))
	require.NoError(t, err)
	acc.Secrets = sealer
	bootstrapAccounts(t, acc)
	idp := ssotest.New(t, "codesynapse", "client-secret-value")
	srv := New(ctx, Config{Accounts: acc, PublicURL: "https://cs.example", SSOClient: idp.Client()}, memory.New(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	owner := newClient(t, ts)
	var signed struct{ Org accounts.Org }
	require.Equal(t, owner.do("POST", "/api/v1/auth/signup", `{"email":"owner@corp.example","name":"Owner","password":"`+pw+`"}`, &signed), 201)
	var team accounts.Team
	require.Equal(t, owner.do("POST", "/api/v1/accounts/orgs/"+signed.Org.Slug+"/teams", `{"name":"Developers"}`, &team), 201)
	return ssoFixture{ts: ts, acc: acc, idp: idp, owner: owner, org: signed.Org, team: team.ID}
}

func (f ssoFixture) configure(t *testing.T) {
	t.Helper()
	body := `{"name":"Corp IdP","issuer":"` + f.idp.Issuer() + `","client_id":"codesynapse","client_secret":"client-secret-value","enabled":true,` +
		`"role_groups":{"cs-admins":"admin","cs-devs":"member"},"team_groups":{"cs-devs":"` + f.team + `"}}`
	var out map[string]any
	require.Equal(t, f.owner.do("PUT", "/api/v1/accounts/orgs/"+f.org.Slug+"/sso", body, &out), 200)
}

// signIn runs the browser side of a sign-in and returns the callback status.
func (f ssoFixture) signIn(t *testing.T, browser *client, claims map[string]any) (int, map[string]any) {
	t.Helper()
	var start struct{ URL string }
	require.Equal(t, browser.do("POST", "/api/v1/auth/sso/"+f.org.Slug+"/start", `{"next":"/repos"}`, &start), 200)
	code, state := f.idp.Authorize(t, start.URL, claims)
	var out map[string]any
	status := browser.do("POST", "/api/v1/auth/sso/callback", `{"code":"`+code+`","state":"`+state+`"}`, &out)
	t.Logf("callback %d: %v", status, out)
	return status, out
}

func TestSSOConfiguration(t *testing.T) {
	f := newSSOFixture(t)
	f.configure(t)
	var raw string
	require.Equal(t, f.owner.do("GET", "/api/v1/accounts/orgs/"+f.org.Slug+"/sso", "", &raw), 200)
	assert.False(t, strings.Contains(raw, "client-secret-value"), "the client secret is never returned")
	assert.Contains(t, raw, "https://cs.example/sso/callback")
	idp, err := f.acc.St.IdentityProvider(context.Background(), f.org.ID)
	require.NoError(t, err)
	assert.False(t, strings.Contains(string(idp.SealedSecret), "client-secret-value"), "stored sealed")
	assert.Equal(t, f.owner.do("PUT", "/api/v1/accounts/orgs/"+f.org.Slug+"/sso", `{"name":"x","issuer":"http://insecure.example","client_id":"a","client_secret":"b"}`, nil), 400)
	assert.Equal(t, f.owner.do("PUT", "/api/v1/accounts/orgs/"+f.org.Slug+"/sso", `{"name":"x","issuer":"https://cs.example","client_id":"a","client_secret":"b","team_groups":{"g":"missing"}}`, nil), 502,
		"an issuer that does not answer discovery is refused before anything is stored")

	stranger := newClient(t, f.ts)
	f.acc.SignupOpen = true
	require.Equal(t, stranger.do("POST", "/api/v1/auth/signup", `{"email":"stranger@x.org","name":"S","password":"`+pw+`"}`, nil), 201)
	assert.Equal(t, stranger.do("GET", "/api/v1/accounts/orgs/"+f.org.Slug+"/sso", "", nil), 404, "other organisations' providers are invisible")
}

func TestSSOSignIn(t *testing.T) {
	f := newSSOFixture(t)
	f.configure(t)
	ctx := context.Background()
	alice := map[string]any{"sub": "alice-1", "email": "Alice@Corp.example", "email_verified": true, "name": "Alice", "groups": []any{"cs-devs"}}

	browser := newClient(t, f.ts)
	status, out := f.signIn(t, browser, alice)
	require.Equal(t, status, 200, "%v", out)
	assert.Equal(t, out["next"], any("/repos"))
	var me struct {
		User        accounts.User
		Memberships []accounts.Membership
	}
	require.Equal(t, browser.do("GET", "/api/v1/me", "", &me), 200, "signed in")
	assert.Equal(t, me.User.Email, "alice@corp.example")
	require.Len(t, me.Memberships, 1)
	assert.Equal(t, me.Memberships[0].Role, accounts.Member, "the group decides the role")
	inTeam, err := f.acc.St.ReadTuples(ctx, accounts.TupleFilter{ObjectType: accounts.TypeTeam, ObjectID: f.team, SubjectType: accounts.TypeUser, SubjectID: me.User.ID})
	require.NoError(t, err)
	assert.Len(t, inTeam, 1, "the group decides the team")
	assert.Equal(t, browser.do("POST", "/api/v1/auth/login", `{"email":"alice@corp.example","password":"`+pw+`"}`, nil), 401, "a just-in-time account has no password")

	// The state works once and only in the browser that started it.
	var start struct{ URL string }
	require.Equal(t, browser.do("POST", "/api/v1/auth/sso/"+f.org.Slug+"/start", `{}`, &start), 200)
	code, state := f.idp.Authorize(t, start.URL, alice)
	other := newClient(t, f.ts)
	assert.Equal(t, other.do("POST", "/api/v1/auth/sso/callback", `{"code":"`+code+`","state":"`+state+`"}`, nil), 400, "another browser")
	assert.Equal(t, browser.do("POST", "/api/v1/auth/sso/callback", `{"code":"`+code+`","state":"`+state+`"}`, nil), 400, "a used state")

	// Promotion and revocation follow the groups.
	alice["groups"] = []any{"cs-admins"}
	status, _ = f.signIn(t, newClient(t, f.ts), alice)
	require.Equal(t, status, 200)
	role, err := f.acc.St.MemberRole(ctx, f.org.ID, me.User.ID)
	require.NoError(t, err)
	assert.Equal(t, role, accounts.Admin)
	inTeam, err = f.acc.St.ReadTuples(ctx, accounts.TupleFilter{ObjectType: accounts.TypeTeam, ObjectID: f.team, SubjectType: accounts.TypeUser, SubjectID: me.User.ID})
	require.NoError(t, err)
	assert.Len(t, inTeam, 0, "a mapped team the provider no longer grants is left")
	alice["groups"] = []any{}
	status, _ = f.signIn(t, newClient(t, f.ts), alice)
	assert.Equal(t, status, 403)
	_, err = f.acc.St.MemberRole(ctx, f.org.ID, me.User.ID)
	assert.True(t, errors.Is(err, accounts.ErrNotFound), "the membership the provider created ends with its grant")

	// Nobody gets an account the provider does not let in.
	status, _ = f.signIn(t, newClient(t, f.ts), map[string]any{"sub": "nobody", "email": "nobody@corp.example", "email_verified": true})
	assert.Equal(t, status, 403)
	_, err = f.acc.St.UserByEmail(ctx, "nobody@corp.example")
	assert.True(t, errors.Is(err, accounts.ErrNotFound))
	status, _ = f.signIn(t, newClient(t, f.ts), map[string]any{"sub": "unverified", "email": "u@corp.example", "email_verified": false, "groups": []any{"cs-devs"}})
	assert.Equal(t, status, 400, "a new account needs a verified email")

	var events struct{ Events []accounts.Event }
	require.Equal(t, rootClient(t, f.ts).do("GET", "/api/v1/admin/audit?action=sso.&limit=100", "", &events), 200)
	actions := map[string]bool{}
	for _, e := range events.Events {
		actions[e.Action+" "+e.Outcome] = true
	}
	for _, a := range []string{"sso.login success", "sso.login failure", "sso.link success", "sso.member.add success", "sso.member.remove success", "sso.team.add success", "sso.team.remove success"} {
		assert.True(t, actions[a], "audited: "+a)
	}
}

func TestSSOLinking(t *testing.T) {
	f := newSSOFixture(t)
	f.configure(t)
	f.acc.SignupOpen = true
	ctx := context.Background()

	// An account outside the organisation is never taken over.
	bob := newClient(t, f.ts)
	require.Equal(t, bob.do("POST", "/api/v1/auth/signup", `{"email":"bob@corp.example","name":"Bob","password":"`+pw+`"}`, nil), 201)
	status, _ := f.signIn(t, newClient(t, f.ts), map[string]any{"sub": "bob-1", "email": "bob@corp.example", "email_verified": true, "groups": []any{"cs-admins"}})
	assert.Equal(t, status, 409)
	bobUser, err := f.acc.St.UserByEmail(ctx, "bob@corp.example")
	require.NoError(t, err)
	_, err = f.acc.St.MemberRole(ctx, f.org.ID, bobUser.ID)
	assert.True(t, errors.Is(err, accounts.ErrNotFound))

	// A member the owner added links on a verified email and keeps the
	// owner's role.
	carol := newClient(t, f.ts)
	require.Equal(t, carol.do("POST", "/api/v1/auth/signup", `{"email":"carol@corp.example","name":"Carol","password":"`+pw+`"}`, nil), 201)
	require.Equal(t, f.owner.do("PUT", "/api/v1/accounts/orgs/"+f.org.Slug+"/members", `{"email":"carol@corp.example","role":"viewer"}`, nil), 200)
	status, _ = f.signIn(t, newClient(t, f.ts), map[string]any{"sub": "carol-1", "email": "carol@corp.example", "email_verified": true, "groups": []any{"cs-admins"}})
	require.Equal(t, status, 200)
	carolUser, err := f.acc.St.UserByEmail(ctx, "carol@corp.example")
	require.NoError(t, err)
	role, err := f.acc.St.MemberRole(ctx, f.org.ID, carolUser.ID)
	require.NoError(t, err)
	assert.Equal(t, role, accounts.Viewer, "hand-added memberships stay the owner's decision")
	status, _ = f.signIn(t, newClient(t, f.ts), map[string]any{"sub": "carol-1", "email": "carol@corp.example", "email_verified": true})
	assert.Equal(t, status, 200, "the provider cannot remove a hand-added member")

	// Root never signs in through a provider.
	status, out := f.signIn(t, newClient(t, f.ts), map[string]any{"sub": "root-1", "email": accountstest.RootEmail, "email_verified": true, "groups": []any{"cs-admins"}})
	assert.Equal(t, status, 403)
	assert.Equal(t, out["detail"], any(accounts.ErrRootAccount.Error()))

	// Explicit linking from the profile, then unlinking.
	var start struct{ URL string }
	require.Equal(t, bob.do("POST", "/api/v1/me/sso/"+f.org.Slug+"/link", `{}`, &start), 200)
	code, state := f.idp.Authorize(t, start.URL, map[string]any{"sub": "bob-1", "email": "bob@corp.example", "email_verified": true})
	var linked map[string]any
	require.Equal(t, bob.do("POST", "/api/v1/auth/sso/callback", `{"code":"`+code+`","state":"`+state+`"}`, &linked), 200)
	assert.Equal(t, linked["next"], any("/settings/profile"))
	var ids struct{ Identities []accounts.ExternalIdentity }
	require.Equal(t, bob.do("GET", "/api/v1/me/identities", "", &ids), 200)
	require.Len(t, ids.Identities, 1)
	status, _ = f.signIn(t, newClient(t, f.ts), map[string]any{"sub": "bob-1", "email": "bob@corp.example", "email_verified": true, "groups": []any{"cs-devs"}})
	assert.Equal(t, status, 200, "the linked identity signs in")
	assert.Equal(t, bob.do("DELETE", "/api/v1/me/identities/"+ids.Identities[0].ID, "", nil), 204, "bob still has a password")

	// Disconnecting the provider stops sign-ins.
	require.Equal(t, f.owner.do("DELETE", "/api/v1/accounts/orgs/"+f.org.Slug+"/sso", "", nil), 204)
	assert.Equal(t, newClient(t, f.ts).do("POST", "/api/v1/auth/sso/"+f.org.Slug+"/start", `{}`, nil), 404)
}

func TestSSOGuards(t *testing.T) {
	f := newSSOFixture(t)
	ctx := context.Background()
	body := `{"name":"Corp IdP","issuer":"` + f.idp.Issuer() + `","client_id":"codesynapse","client_secret":"client-secret-value","enabled":true,` +
		`"role_groups":{"cs-owners":"owner","cs-devs":"member"}}`
	require.Equal(t, f.owner.do("PUT", "/api/v1/accounts/orgs/"+f.org.Slug+"/sso", body, nil), 200)

	alice := newClient(t, f.ts)
	claims := map[string]any{"sub": "alice-1", "email": "alice@corp.example", "email_verified": true, "groups": []any{"cs-owners"}}
	status, _ := f.signIn(t, alice, claims)
	require.Equal(t, status, 200)
	var me struct{ User accounts.User }
	require.Equal(t, alice.do("GET", "/api/v1/me", "", &me), 200)

	// A passwordless account keeps its only way in.
	var ids struct{ Identities []accounts.ExternalIdentity }
	require.Equal(t, alice.do("GET", "/api/v1/me/identities", "", &ids), 200)
	require.Len(t, ids.Identities, 1)
	assert.Equal(t, alice.do("DELETE", "/api/v1/me/identities/"+ids.Identities[0].ID, "", nil), 403)

	// The provider never removes or demotes the last owner: the change and
	// the sign-in are refused, the membership stays.
	var owner struct{ User accounts.User }
	require.Equal(t, f.owner.do("GET", "/api/v1/me", "", &owner), 200)
	require.NoError(t, f.acc.St.RemoveMember(ctx, f.org.ID, owner.User.ID))
	for name, groups := range map[string][]any{"removed": {}, "demoted": {"cs-devs"}} {
		claims["groups"] = groups
		status, _ = f.signIn(t, newClient(t, f.ts), claims)
		assert.Equal(t, status, 403, name)
		role, err := f.acc.St.MemberRole(ctx, f.org.ID, me.User.ID)
		require.NoError(t, err)
		assert.Equal(t, role, accounts.Owner, name+": the last owner stays")
	}

	// A suspended account cannot sign in through its provider.
	claims["groups"] = []any{"cs-owners"}
	require.NoError(t, f.acc.SuspendUser(ctx, rootSession(t, f.acc), me.User.ID))
	late := newClient(t, f.ts)
	status, _ = f.signIn(t, late, claims)
	assert.Equal(t, status, 403)
	assert.Equal(t, late.do("GET", "/api/v1/me", "", nil), 401, "no session")
}
