// Package sso talks OpenID Connect to an organisation's identity provider:
// discovery, the authorization URL (with PKCE and a nonce), the code
// exchange and ID-token verification. The issuer comes from an organisation
// owner, so every connection goes through Client, which refuses internal
// addresses at connect time and follows no redirects. Account rules live in
// package accounts.
package sso

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"syscall"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"github.com/astrona-io/codesynapse/internal/accounts"
)

// maxResponse bounds every answer from an identity provider.
const maxResponse = 1 << 20

// ErrInternalAddress refuses a provider on a private network.
var ErrInternalAddress = errors.New("the identity provider resolves to an internal address")

// Client returns the HTTP client for identity providers. Unless
// allowPrivate (a self-hosted provider on the same network), connections to
// loopback, private, link-local and unspecified addresses are refused when
// they are made, which also covers DNS answers that change after a check.
func Client(allowPrivate bool) *http.Client {
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	if !allowPrivate {
		dialer.Control = func(_, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return err
			}
			if ip := net.ParseIP(host); ip == nil || internal(ip) {
				return ErrInternalAddress
			}
			return nil
		}
	}
	transport := &http.Transport{DialContext: dialer.DialContext, TLSHandshakeTimeout: 5 * time.Second, ResponseHeaderTimeout: 10 * time.Second, MaxIdleConns: 10, IdleConnTimeout: time.Minute}
	return &http.Client{
		Timeout:       15 * time.Second,
		Transport:     limited{transport},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

func internal(ip net.IP) bool {
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified()
}

// limited caps response bodies.
type limited struct{ next http.RoundTripper }

func (l limited) RoundTrip(r *http.Request) (*http.Response, error) {
	res, err := l.next.RoundTrip(r)
	if err == nil {
		res.Body = struct {
			io.Reader
			io.Closer
		}{io.LimitReader(res.Body, maxResponse), res.Body}
	}
	return res, err
}

// Provider is a discovered identity provider configured for one
// organisation's client.
type Provider struct {
	oauth       oauth2.Config
	verifier    *oidc.IDTokenVerifier
	client      *http.Client
	groupsClaim string
}

// Discover fetches the issuer's discovery document through client. go-oidc
// checks that the document names the same issuer; the endpoints must be
// https.
func Discover(ctx context.Context, client *http.Client, idp accounts.IdentityProvider, secret, redirectURL string) (*Provider, error) {
	ctx = oidc.ClientContext(ctx, client)
	p, err := oidc.NewProvider(ctx, idp.Issuer)
	if err != nil {
		return nil, fmt.Errorf("discovering %s: %w", idp.Issuer, err)
	}
	ep := p.Endpoint()
	for _, raw := range []string{ep.AuthURL, ep.TokenURL} {
		if u, err := url.Parse(raw); err != nil || u.Scheme != "https" || u.User != nil {
			return nil, fmt.Errorf("discovering %s: endpoints must be https URLs", idp.Issuer)
		}
	}
	scopes := []string{oidc.ScopeOpenID, "email", "profile"}
	scopes = append(scopes, idp.Scopes...)
	return &Provider{
		oauth:       oauth2.Config{ClientID: idp.ClientID, ClientSecret: secret, Endpoint: ep, RedirectURL: redirectURL, Scopes: scopes},
		verifier:    p.Verifier(&oidc.Config{ClientID: idp.ClientID}),
		client:      client,
		groupsClaim: idp.GroupsClaim,
	}, nil
}

// AuthURL is where the browser goes to sign in: the state, a nonce and a
// PKCE S256 challenge travel with it.
func (p *Provider) AuthURL(state, nonce, verifier string) string {
	return p.oauth.AuthCodeURL(state, oidc.Nonce(nonce), oauth2.S256ChallengeOption(verifier))
}

// Exchange trades the code for tokens and verifies the ID token: signature
// against the provider's keys, issuer, audience, expiry and nonce.
func (p *Provider) Exchange(ctx context.Context, code, verifier, nonce string) (accounts.SSOClaims, error) {
	if code == "" || len(code) > 4096 {
		return accounts.SSOClaims{}, errors.New("missing authorization code")
	}
	ctx = oidc.ClientContext(ctx, p.client)
	tok, err := p.oauth.Exchange(ctx, code, oauth2.VerifierOption(verifier))
	if err != nil {
		return accounts.SSOClaims{}, fmt.Errorf("exchanging the code: %w", err)
	}
	raw, ok := tok.Extra("id_token").(string)
	if !ok || raw == "" {
		return accounts.SSOClaims{}, errors.New("the provider returned no ID token")
	}
	idt, err := p.verifier.Verify(ctx, raw)
	if err != nil {
		return accounts.SSOClaims{}, fmt.Errorf("verifying the ID token: %w", err)
	}
	if idt.Nonce != nonce {
		return accounts.SSOClaims{}, errors.New("the ID token does not belong to this sign-in")
	}
	var claims map[string]any
	if err := idt.Claims(&claims); err != nil {
		return accounts.SSOClaims{}, err
	}
	return claimsFrom(idt.Subject, claims, p.groupsClaim), nil
}

// claimsFrom reads the claims a sign-in uses. Providers differ in types:
// email_verified may be a string, groups a single string.
func claimsFrom(subject string, claims map[string]any, groupsClaim string) accounts.SSOClaims {
	c := accounts.SSOClaims{Subject: subject}
	c.Email, _ = claims["email"].(string)
	switch v := claims["email_verified"].(type) {
	case bool:
		c.EmailVerified = v
	case string:
		c.EmailVerified = strings.EqualFold(v, "true")
	}
	if name, ok := claims["name"].(string); ok {
		c.Name = name
	}
	switch v := claims[groupsClaim].(type) {
	case string:
		c.Groups = []string{v}
	case []any:
		for _, g := range v {
			if s, ok := g.(string); ok && s != "" && len(s) <= 256 {
				c.Groups = append(c.Groups, s)
			}
			if len(c.Groups) >= accounts.MaxClaimGroups {
				break
			}
		}
	}
	return c
}
