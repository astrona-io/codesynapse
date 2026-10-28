package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/astrona-io/codesynapse/internal/accounts"
	"github.com/astrona-io/codesynapse/internal/sso"
	"github.com/astrona-io/codesynapse/internal/telemetry"
)

// Single sign-on (docs/SSO_PLAN.md). The dashboard starts a sign-in with
// POST /auth/sso/{org}/start, which returns the provider's URL and sets a
// short-lived HTTP-only binder cookie. The provider sends the browser to the
// dashboard's /sso/callback page, which posts code and state to
// /auth/sso/callback; the API checks state and binder, verifies the ID token
// and signs the person in. No step redirects through the API, so the
// dashboard proxy never follows a redirect.

// SSOCookie binds a pending sign-in to the browser that started it.
const SSOCookie = "cs_sso"

func (s *Server) ssoClient() *http.Client {
	if s.cfg.SSOClient != nil {
		return s.cfg.SSOClient
	}
	return sso.Client(s.cfg.SSOAllowPrivate)
}

func (s *Server) ssoRedirect() string {
	return strings.TrimSuffix(s.cfg.PublicURL, "/") + "/sso/callback"
}

func (s *Server) discover(ctx context.Context, idp accounts.IdentityProvider, secret string) (*sso.Provider, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	return sso.Discover(ctx, s.ssoClient(), idp, secret, s.ssoRedirect())
}

// handleGetSSO shows an organisation's provider to its admins; the client
// secret never leaves the store, only its preview.
func (s *Server) handleGetSSO(w http.ResponseWriter, r *http.Request) {
	p, o, ok := s.orgFor(w, r, accounts.Admin)
	if !ok {
		return
	}
	idp, err := s.cfg.Accounts.IdentityProviderOf(r.Context(), p, o.ID)
	if errors.Is(err, accounts.ErrNotFound) {
		writeJSON(w, http.StatusOK, map[string]any{"provider": nil, "redirect_uri": s.ssoRedirect()})
		return
	}
	if err != nil {
		s.accountError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"provider": idp, "redirect_uri": s.ssoRedirect()})
}

// handleSetSSO connects or updates the provider after checking that the
// issuer answers OpenID discovery through the guarded client.
func (s *Server) handleSetSSO(w http.ResponseWriter, r *http.Request) {
	p, o, ok := s.orgFor(w, r, accounts.Admin)
	if !ok {
		return
	}
	var in accounts.IdentityProviderInput
	if err := decodeJSON(w, r, &in); err != nil {
		s.problem(w, http.StatusBadRequest, err)
		return
	}
	issuer, err := accounts.CheckIssuer(in.Issuer)
	if err != nil {
		s.accountError(w, err)
		return
	}
	if _, err := s.discover(r.Context(), accounts.IdentityProvider{Issuer: issuer, ClientID: in.ClientID}, ""); err != nil {
		s.problem(w, http.StatusBadGateway, errors.New("the issuer did not answer OpenID discovery: "+firstLineOf(err.Error())))
		return
	}
	idp, err := s.cfg.Accounts.SetIdentityProvider(r.Context(), p, o.ID, in)
	if err != nil {
		s.accountError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"provider": idp, "redirect_uri": s.ssoRedirect()})
}

func (s *Server) handleDeleteSSO(w http.ResponseWriter, r *http.Request) {
	p, o, ok := s.orgFor(w, r, accounts.Admin)
	if !ok {
		return
	}
	if err := s.cfg.Accounts.RemoveIdentityProvider(r.Context(), p, o.ID); err != nil {
		s.accountError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type ssoStartRequest struct {
	Next string `json:"next"`
}

// handleSSOStart begins a sign-in (public) or, under /me, a link to the
// signed-in account.
func (s *Server) handleSSOStart(mode string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.accountsOn(w) {
			return
		}
		var in ssoStartRequest
		if r.ContentLength != 0 {
			if err := decodeJSON(w, r, &in); err != nil {
				s.problem(w, http.StatusBadRequest, err)
				return
			}
		}
		var p accounts.Principal
		if mode == accounts.SSOLink {
			var ok bool
			if p, ok = s.caller(w, r); !ok {
				return
			}
		}
		start, err := s.cfg.Accounts.BeginSSO(r.Context(), r.PathValue("org"), mode, p, in.Next)
		if err != nil {
			s.accountError(w, err)
			return
		}
		prov, err := s.discover(r.Context(), start.Provider, start.Secret)
		if err != nil {
			s.logger.Warn("sso discovery failed", "org", start.Provider.OrgID, "error", err)
			s.problem(w, http.StatusBadGateway, errors.New("the organisation's identity provider is unreachable"))
			return
		}
		http.SetCookie(w, &http.Cookie{Name: SSOCookie, Value: start.Binder, Path: "/", HttpOnly: true, Secure: s.cfg.SecureCookies, //nolint:gosec // as in setSession
			SameSite: http.SameSiteLaxMode, MaxAge: int(accounts.SSOStateTTL / time.Second)})
		writeJSON(w, http.StatusOK, map[string]string{"url": prov.AuthURL(start.State, start.Nonce, start.Verifier)})
	}
}

type ssoCallbackRequest struct {
	Code  string `json:"code"`
	State string `json:"state"`
}

// handleSSOCallback finishes a sign-in or link the provider sent back.
func (s *Server) handleSSOCallback(w http.ResponseWriter, r *http.Request) {
	if !s.accountsOn(w) {
		return
	}
	var in ssoCallbackRequest
	if err := decodeJSON(w, r, &in); err != nil {
		s.problem(w, http.StatusBadRequest, err)
		return
	}
	binder := ""
	if c, err := r.Cookie(SSOCookie); err == nil {
		binder = c.Value
	}
	// The binder works once, whatever happens next.
	http.SetCookie(w, &http.Cookie{Name: SSOCookie, Value: "", Path: "/", HttpOnly: true, Secure: s.cfg.SecureCookies, //nolint:gosec // as in setSession
		SameSite: http.SameSiteLaxMode, MaxAge: -1})
	st, idp, secret, err := s.cfg.Accounts.TakeSSO(r.Context(), in.State, binder)
	if err != nil {
		accounts.RecordAuth(r.Context(), telemetry.AuthSSO, "", err)
		s.ssoError(w, err)
		return
	}
	prov, err := s.discover(r.Context(), idp, secret)
	if err != nil {
		s.logger.Warn("sso discovery failed", "org", idp.OrgID, "error", err)
		s.problem(w, http.StatusBadGateway, errors.New("the organisation's identity provider is unreachable"))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	claims, err := prov.Exchange(ctx, in.Code, st.Verifier, st.Nonce)
	if err != nil {
		s.logger.Warn("sso token exchange failed", "org", idp.OrgID, "error", err)
		accounts.RecordAuth(r.Context(), telemetry.AuthSSO, "", accounts.ErrUnauthorized)
		s.record(r.Context(), accounts.Event{Via: "anonymous", OrgID: idp.OrgID, Action: "sso.login", Outcome: accounts.Failure, Detail: "token exchange or verification failed"})
		s.problem(w, http.StatusUnauthorized, errors.New("the identity provider's answer could not be verified; start again"))
		return
	}
	if st.Mode == accounts.SSOLink {
		p, ok := principal(r.Context())
		if !ok || p.Via != "session" || p.User.ID != st.UserID {
			accounts.RecordAuth(r.Context(), telemetry.AuthSSO, "", accounts.ErrSSOState)
			s.ssoError(w, accounts.ErrSSOState)
			return
		}
		err := s.cfg.Accounts.CompleteSSOLink(r.Context(), st, idp, claims)
		accounts.RecordAuth(r.Context(), telemetry.AuthSSO, idp.OrgID, err)
		if err != nil {
			s.ssoError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"next": "/settings/profile"})
		return
	}
	tok, _, err := s.cfg.Accounts.CompleteSSOLogin(r.Context(), st, idp, claims)
	// The organisation is the provider's, from the server-side sign-in state.
	accounts.RecordAuth(r.Context(), telemetry.AuthSSO, idp.OrgID, err)
	if err != nil {
		s.ssoError(w, err)
		return
	}
	s.setSession(w, tok)
	writeJSON(w, http.StatusOK, map[string]string{"next": st.Next})
}

func (s *Server) ssoError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, accounts.ErrSSOState):
		s.problem(w, http.StatusBadRequest, err)
	case errors.Is(err, accounts.ErrSSOLinkRequired):
		s.problem(w, http.StatusConflict, err)
	case errors.Is(err, accounts.ErrSSONoAccess):
		s.problem(w, http.StatusForbidden, err)
	default:
		s.accountError(w, err)
	}
}

func (s *Server) handleSSOIdentities(w http.ResponseWriter, r *http.Request) {
	p, ok := s.caller(w, r)
	if !ok {
		return
	}
	ids, err := s.cfg.Accounts.Identities(r.Context(), p)
	if err != nil {
		s.accountError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"identities": ids})
}

func (s *Server) handleUnlinkIdentity(w http.ResponseWriter, r *http.Request) {
	p, ok := s.caller(w, r)
	if !ok {
		return
	}
	if err := s.cfg.Accounts.UnlinkIdentity(r.Context(), p, r.PathValue("id")); err != nil {
		s.accountError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
