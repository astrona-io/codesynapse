// Package ssotest is a minimal OpenID Connect provider for tests: discovery,
// JWKS, an authorization step driven by the test, and a token endpoint that
// checks client credentials, the redirect URI and PKCE before returning an
// RS256-signed ID token. It uses only the standard library.
package ssotest

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"
)

// Provider is a running test identity provider.
type Provider struct {
	Server       *httptest.Server
	ClientID     string
	ClientSecret string
	key          *rsa.PrivateKey
	mu           sync.Mutex
	grants       map[string]grant
	// Tamper, when set, edits the ID-token claims after the test chose them
	// (wrong audience, nonce or issuer).
	Tamper func(map[string]any)
}

type grant struct {
	redirect, challenge, nonce string
	claims                     map[string]any
}

// New starts a provider over TLS. Close it with t.Cleanup (done here).
func New(t testing.TB, clientID, clientSecret string) *Provider {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	p := &Provider{ClientID: clientID, ClientSecret: clientSecret, key: key, grants: map[string]grant{}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", p.discovery)
	mux.HandleFunc("GET /keys", p.keys)
	mux.HandleFunc("POST /token", p.token)
	p.Server = httptest.NewTLSServer(mux)
	t.Cleanup(p.Server.Close)
	return p
}

// Issuer is the provider's issuer URL.
func (p *Provider) Issuer() string { return p.Server.URL }

// Client trusts the provider's TLS certificate.
func (p *Provider) Client() *http.Client { return p.Server.Client() }

func (p *Provider) discovery(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, map[string]any{
		"issuer": p.Issuer(), "authorization_endpoint": p.Issuer() + "/authorize", "token_endpoint": p.Issuer() + "/token",
		"jwks_uri": p.Issuer() + "/keys", "id_token_signing_alg_values_supported": []string{"RS256"},
		"response_types_supported": []string{"code"}, "subject_types_supported": []string{"public"},
	})
}

func (p *Provider) keys(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, map[string]any{"keys": []map[string]string{{
		"kty": "RSA", "alg": "RS256", "use": "sig", "kid": "test",
		"n": b64(p.key.N.Bytes()), "e": b64(big.NewInt(int64(p.key.E)).Bytes()),
	}}})
}

// Authorize plays the sign-in page: it reads the authorization URL the
// application built, checks it, and returns a code and the state for the
// callback. claims are the ID token's (sub, email, groups, ...).
func (p *Provider) Authorize(t testing.TB, authURL string, claims map[string]any) (code, state string) {
	t.Helper()
	u, err := url.Parse(authURL)
	if err != nil || u.Scheme+"://"+u.Host != p.Issuer() || u.Path != "/authorize" {
		t.Fatalf("authorization URL %q is not this provider's", authURL)
	}
	q := u.Query()
	if q.Get("client_id") != p.ClientID || q.Get("response_type") != "code" || q.Get("code_challenge_method") != "S256" ||
		q.Get("code_challenge") == "" || q.Get("nonce") == "" || q.Get("state") == "" {
		t.Fatalf("incomplete authorization request: %v", q)
	}
	code = b64(random(t))
	p.mu.Lock()
	p.grants[code] = grant{redirect: q.Get("redirect_uri"), challenge: q.Get("code_challenge"), nonce: q.Get("nonce"), claims: claims}
	p.mu.Unlock()
	return code, q.Get("state")
}

func (p *Provider) token(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	id, secret, ok := r.BasicAuth()
	if !ok {
		id, secret = r.PostForm.Get("client_id"), r.PostForm.Get("client_secret")
	}
	if id != p.ClientID || secret != p.ClientSecret {
		w.WriteHeader(http.StatusUnauthorized)
		writeJSON(w, map[string]string{"error": "invalid_client"})
		return
	}
	code := r.PostForm.Get("code")
	p.mu.Lock()
	g, ok := p.grants[code]
	delete(p.grants, code) // codes work once
	p.mu.Unlock()
	sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
	if !ok || r.PostForm.Get("redirect_uri") != g.redirect || b64(sum[:]) != g.challenge {
		w.WriteHeader(http.StatusBadRequest)
		writeJSON(w, map[string]string{"error": "invalid_grant"})
		return
	}
	now := time.Now()
	claims := map[string]any{"iss": p.Issuer(), "aud": p.ClientID, "iat": now.Unix(), "exp": now.Add(5 * time.Minute).Unix(), "nonce": g.nonce}
	for k, v := range g.claims {
		claims[k] = v
	}
	if p.Tamper != nil {
		p.Tamper(claims)
	}
	writeJSON(w, map[string]any{"access_token": "at-" + code, "token_type": "Bearer", "expires_in": 300, "id_token": p.sign(claims)})
}

func (p *Provider) sign(claims map[string]any) string {
	header, _ := json.Marshal(map[string]string{"alg": "RS256", "typ": "JWT", "kid": "test"}) //nolint:errchkjson // fixed map
	payload, _ := json.Marshal(claims)                                                        //nolint:errchkjson // test claims
	signing := b64(header) + "." + b64(payload)
	digest := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, p.key, crypto.SHA256, digest[:])
	if err != nil {
		panic(err) // a fresh 2048-bit key always signs
	}
	return signing + "." + b64(sig)
}

func random(t testing.TB) []byte {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return b
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v) //nolint:errcheck // test server
}
