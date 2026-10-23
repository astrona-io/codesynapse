package sso

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/astrona-io/codesynapse/internal/accounts"
	"github.com/astrona-io/codesynapse/internal/sso/ssotest"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

func TestClientRefusesInternalAddresses(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	defer srv.Close()
	res, err := Client(false).Get(srv.URL) //nolint:noctx // test
	if res != nil {
		_ = res.Body.Close()
	}
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrInternalAddress), err.Error())
	res, err = Client(true).Get(srv.URL) //nolint:noctx // test
	require.NoError(t, err, "a self-hosted provider is allowed when configured")
	_ = res.Body.Close()

	redirect := httptest.NewServer(http.RedirectHandler(srv.URL, http.StatusFound))
	defer redirect.Close()
	res, err = Client(true).Get(redirect.URL) //nolint:noctx // test
	require.NoError(t, err)
	_ = res.Body.Close()
	assert.Equal(t, res.StatusCode, http.StatusFound, "redirects are not followed")
}

func TestClaimsFrom(t *testing.T) {
	c := claimsFrom("s1", map[string]any{"email": "A@x.org", "email_verified": "true", "name": "A", "roles": []any{"admins", 7, "", "devs"}}, "roles")
	assert.Equal(t, c, accounts.SSOClaims{Subject: "s1", Email: "A@x.org", EmailVerified: true, Name: "A", Groups: []string{"admins", "devs"}})
	c = claimsFrom("s2", map[string]any{"email_verified": false, "groups": "single"}, "groups")
	assert.Equal(t, c.Groups, []string{"single"})
	assert.False(t, c.EmailVerified)
}

func provider(t *testing.T, idp *ssotest.Provider) *Provider {
	t.Helper()
	p, err := Discover(context.Background(), idp.Client(), accounts.IdentityProvider{Issuer: idp.Issuer(), ClientID: idp.ClientID,
		IdentityProviderSettings: accounts.IdentityProviderSettings{GroupsClaim: "groups"}}, idp.ClientSecret, "https://cs.example/sso/callback")
	require.NoError(t, err)
	return p
}

func TestExchange(t *testing.T) {
	idp := ssotest.New(t, "cs", "secret")
	p := provider(t, idp)
	url := p.AuthURL("state-1", "nonce-1", "verifier-verifier-verifier-verifier-verifier")
	code, state := idp.Authorize(t, url, map[string]any{"sub": "u1", "email": "ada@x.org", "email_verified": true, "groups": []any{"devs"}})
	assert.Equal(t, state, "state-1")
	c, err := p.Exchange(context.Background(), code, "verifier-verifier-verifier-verifier-verifier", "nonce-1")
	require.NoError(t, err)
	assert.Equal(t, c, accounts.SSOClaims{Subject: "u1", Email: "ada@x.org", EmailVerified: true, Groups: []string{"devs"}})
	_, err = p.Exchange(context.Background(), code, "verifier-verifier-verifier-verifier-verifier", "nonce-1")
	assert.Error(t, err, "a code works once")
}

func TestExchangeRejects(t *testing.T) {
	const verifier = "verifier-verifier-verifier-verifier-verifier"
	for name, tc := range map[string]struct {
		tamper   func(map[string]any)
		verifier string
		nonce    string
		secret   string
	}{
		"wrong nonce":    {nonce: "other"},
		"wrong verifier": {verifier: "another-verifier-another-verifier-another"},
		"wrong audience": {tamper: func(c map[string]any) { c["aud"] = "someone-else" }},
		"wrong issuer":   {tamper: func(c map[string]any) { c["iss"] = "https://evil.example" }},
		"expired":        {tamper: func(c map[string]any) { c["exp"] = 1 }},
		"wrong secret":   {secret: "guess"},
	} {
		t.Run(name, func(t *testing.T) {
			idp := ssotest.New(t, "cs", "secret")
			idp.Tamper = tc.tamper
			secret := idp.ClientSecret
			if tc.secret != "" {
				secret = tc.secret
			}
			p, err := Discover(context.Background(), idp.Client(), accounts.IdentityProvider{Issuer: idp.Issuer(), ClientID: idp.ClientID}, secret, "https://cs.example/sso/callback")
			require.NoError(t, err)
			code, _ := idp.Authorize(t, p.AuthURL("s", "nonce-1", verifier), map[string]any{"sub": "u1"})
			v, nonce := verifier, "nonce-1"
			if tc.verifier != "" {
				v = tc.verifier
			}
			if tc.nonce != "" {
				nonce = tc.nonce
			}
			_, err = p.Exchange(context.Background(), code, v, nonce)
			assert.Error(t, err)
		})
	}
}
