package api

import (
	"cmp"
	"context"
	"crypto/subtle"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/astrona-io/codesynapse/internal/accounts"
	"github.com/astrona-io/codesynapse/internal/app"
	"github.com/astrona-io/codesynapse/internal/execution"
)

// SessionCookie holds the session token; HTTP-only, so scripts never see it.
const SessionCookie = "cs_session"

type principalKey struct{}

// principal returns who the request acts as (accounts mode only).
func principal(ctx context.Context) (accounts.Principal, bool) {
	p, ok := ctx.Value(principalKey{}).(accounts.Principal)
	return p, ok
}

// publicPaths need no login: health, version and the login flow itself.
var publicPaths = map[string]bool{
	"GET /healthz": true, "GET /api/v1/version": true, "GET /api/v1/auth/status": true,
	"POST /api/v1/auth/login": true, "POST /api/v1/auth/signup": true, "POST /api/v1/auth/logout": true,
}

// authenticate resolves the caller in accounts mode. It returns false after
// writing the error response.
func (s *Server) authenticate(w http.ResponseWriter, r *http.Request) (*http.Request, bool) {
	if publicPaths[r.Method+" "+r.URL.Path] {
		return r, true
	}
	// Single sign-on starts and finishes before anyone is signed in. Linking
	// an identity finishes on the same callback, so a valid session cookie
	// is still read there; without one the request stays anonymous.
	if r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/api/v1/auth/sso/") {
		if c, err := r.Cookie(SessionCookie); err == nil && sameOrigin(r) {
			if p, err := s.cfg.Accounts.Authenticate(r.Context(), c.Value); err == nil {
				return r.WithContext(context.WithValue(r.Context(), principalKey{}, p)), true
			}
		}
		return r, true
	}
	var p accounts.Principal
	var err error
	authz := r.Header.Get("Authorization")
	switch {
	case strings.HasPrefix(authz, "Bearer "+accounts.KeyPrefix):
		p, err = s.cfg.Accounts.AuthenticateKey(r.Context(), strings.TrimPrefix(authz, "Bearer "))
	case strings.HasPrefix(authz, "Bearer "):
		tok := strings.TrimPrefix(authz, "Bearer ")
		if s.cfg.ServiceToken == "" || subtle.ConstantTimeCompare([]byte(tok), []byte(s.cfg.ServiceToken)) != 1 {
			err = accounts.ErrUnauthorized
		} else {
			p = accounts.Principal{User: accounts.User{ID: "service", Name: "Service token", PlatformAdmin: true}, Via: "service"}
		}
	default:
		c, cerr := r.Cookie(SessionCookie)
		if cerr != nil {
			err = accounts.ErrUnauthorized
			break
		}
		if !sameOrigin(r) {
			s.problem(w, http.StatusForbidden, errors.New("cross-origin request refused"))
			return r, false
		}
		p, err = s.cfg.Accounts.Authenticate(r.Context(), c.Value)
	}
	if err != nil {
		status := http.StatusUnauthorized
		if errors.Is(err, accounts.ErrForbidden) {
			status = http.StatusForbidden
		}
		w.Header().Set("WWW-Authenticate", `Bearer realm="codesynapse"`)
		s.problem(w, status, errors.New("log in or use a valid API key"))
		return r, false
	}
	return r.WithContext(context.WithValue(r.Context(), principalKey{}, p)), true
}

// sameOrigin refuses cookie-authenticated requests from other sites. Safe
// methods pass; for the others an Origin, when sent, must match the host
// (the web app forwards requests from its own origin).
func sameOrigin(r *http.Request) bool {
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	}
	o := r.Header.Get("Origin")
	if o == "" {
		return r.Header.Get("Sec-Fetch-Site") != "cross-site"
	}
	u, err := url.Parse(o)
	return err == nil && strings.EqualFold(u.Host, r.Host)
}

func (s *Server) setSession(w http.ResponseWriter, token string) {
	// Secure is configurable only for plain-HTTP local development.
	http.SetCookie(w, &http.Cookie{Name: SessionCookie, Value: token, Path: "/", HttpOnly: true, Secure: s.cfg.SecureCookies, //nolint:gosec // see above
		SameSite: http.SameSiteLaxMode, MaxAge: int(accounts.SessionTTL / time.Second)})
}

func (s *Server) clearSession(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{Name: SessionCookie, Value: "", Path: "/", HttpOnly: true, Secure: s.cfg.SecureCookies, //nolint:gosec // as in setSession
		SameSite: http.SameSiteLaxMode, MaxAge: -1})
}

// clientAddr is the address login attempts are limited by.
// It is the connection's address, or, when the connection comes from a
// trusted proxy (the web app), the last X-Forwarded-For hop: behind the
// proxy every user would otherwise share one address, one login limit and
// one audit IP. Senders outside TrustedProxies cannot set it.
func (s *Server) clientAddr(r *http.Request) string {
	return app.ClientAddr(r, s.cfg.TrustedProxies)
}

// accountError maps account errors to HTTP statuses.
func (s *Server) accountError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, execution.ErrInvalid), errors.Is(err, accounts.ErrInvalidInput):
		s.problem(w, http.StatusBadRequest, err)
	case errors.Is(err, accounts.ErrUnauthorized), errors.Is(err, accounts.ErrKeyRevoked):
		s.problem(w, http.StatusUnauthorized, err)
	case errors.Is(err, accounts.ErrForbidden), errors.Is(err, accounts.ErrSignupClosed), errors.Is(err, accounts.ErrSuspended), errors.Is(err, accounts.ErrRootAccount):
		s.problem(w, http.StatusForbidden, err)
	case errors.Is(err, accounts.ErrNotFound):
		s.problem(w, http.StatusNotFound, err)
	case errors.Is(err, accounts.ErrIngestReportExpired):
		s.problem(w, http.StatusGone, err)
	case errors.Is(err, accounts.ErrIngestConflict):
		s.problem(w, http.StatusConflict, err)
	case errors.Is(err, execution.ErrConflict), errors.Is(err, accounts.ErrExecutionSessionClosed), errors.Is(err, accounts.ErrExists), errors.Is(err, accounts.ErrLastOwner), errors.Is(err, accounts.ErrNoSecretKey), errors.Is(err, accounts.ErrLastAdmin),
		errors.Is(err, accounts.ErrSetupComplete), errors.Is(err, accounts.ErrSetupRequired):
		s.problem(w, http.StatusConflict, err)
	case errors.Is(err, accounts.ErrExecutionSessionLimit), errors.Is(err, accounts.ErrIngestLimit), errors.Is(err, accounts.ErrRateLimited):
		w.Header().Set("Retry-After", "60")
		s.problem(w, http.StatusTooManyRequests, err)
	default:
		s.problem(w, 500, err)
	}
}

// accountRoutes registers account, organisation and administration routes,
// each classified for governance-only accounts (root.go).
func (s *Server) accountRoutes() {
	s.account("GET /api/v1/auth/status", publicRoute, s.handleAuthStatus)
	s.account("POST /api/v1/auth/signup", publicRoute, s.handleSignup)
	s.account("POST /api/v1/auth/login", publicRoute, s.handleLogin)
	s.account("POST /api/v1/auth/logout", publicRoute, s.handleLogout)
	s.account("POST /api/v1/auth/setup", governanceRoute, s.handleSetup)
	s.account("POST /api/v1/auth/replace-admin", governanceRoute, s.handleReplaceAdmin)
	s.account("POST /api/v1/auth/sso/{org}/start", publicRoute, s.handleSSOStart(accounts.SSOLogin))
	s.account("POST /api/v1/auth/sso/callback", publicRoute, s.handleSSOCallback)
	s.account("POST /api/v1/me/sso/{org}/link", businessRoute, s.handleSSOStart(accounts.SSOLink))
	s.account("GET /api/v1/me/identities", businessRoute, s.handleSSOIdentities)
	s.account("DELETE /api/v1/me/identities/{id}", businessRoute, s.handleUnlinkIdentity)
	s.account("GET /api/v1/accounts/orgs/{org}/sso", businessRoute, s.handleGetSSO)
	s.account("PUT /api/v1/accounts/orgs/{org}/sso", businessRoute, s.handleSetSSO)
	s.account("DELETE /api/v1/accounts/orgs/{org}/sso", businessRoute, s.handleDeleteSSO)
	s.account("GET /api/v1/me", governanceRoute, s.handleMe)
	s.account("POST /api/v1/me/password", governanceRoute, s.handlePassword)
	s.profileRoutes()
	s.account("GET /api/v1/me/keys", businessRoute, s.handleListKeys)
	s.account("POST /api/v1/me/keys", businessRoute, s.handleCreateKey)
	s.account("DELETE /api/v1/me/keys/{id}", businessRoute, s.handleRevokeKey)
	s.account("GET /api/v1/accounts/orgs", businessRoute, s.handleMyOrgs)
	s.account("POST /api/v1/accounts/orgs", businessRoute, s.handleCreateOrg)
	s.account("GET /api/v1/accounts/orgs/{org}/members", businessRoute, s.handleMembers)
	s.account("PUT /api/v1/accounts/orgs/{org}/members", businessRoute, s.handleSetMember)
	s.account("DELETE /api/v1/accounts/orgs/{org}/members/{user}", businessRoute, s.handleRemoveMember)
	s.account("GET /api/v1/accounts/orgs/{org}/audit", businessRoute, s.handleOrgAudit)
	s.account("GET /api/v1/admin/audit", governanceRoute, s.handleAdminAudit)
	s.account("GET /api/v1/admin/audit/verify", governanceRoute, s.handleVerifyAudit)
	s.account("GET /api/v1/accounts/orgs/{org}/repos", businessRoute, s.handleOrgRepos)
	s.account("GET /api/v1/accounts/orgs/{org}/repo-policy", businessRoute, s.handleRepoPolicy)
	s.account("GET /api/v1/accounts/orgs/{org}/access-changes", businessRoute, s.handleAccessChanges)
	s.account("PUT /api/v1/accounts/orgs/{org}/repo-policy", businessRoute, s.handleRepoPolicy)
	s.account("POST /api/v1/accounts/orgs/{org}/repos/{repo}/recover-admin", businessRoute, s.handleRecoverRepoAdmin)
	s.account("POST /api/v1/accounts/orgs/{org}/repos", businessRoute, s.handleAttachRepo)
	s.account("GET /api/v1/me/access", businessRoute, s.handleMyAccess)
	s.account("GET /api/v1/accounts/orgs/{org}/teams", businessRoute, s.handleTeams)
	s.account("POST /api/v1/accounts/orgs/{org}/teams", businessRoute, s.handleCreateTeam)
	s.account("DELETE /api/v1/accounts/orgs/{org}/teams/{team}", businessRoute, s.handleDeleteTeam)
	s.account("PUT /api/v1/accounts/orgs/{org}/teams/{team}/members", businessRoute, s.handleSetTeamMember)
	s.account("DELETE /api/v1/accounts/orgs/{org}/teams/{team}/members/{user}", businessRoute, s.handleRemoveTeamMember)
	s.account("PUT /api/v1/accounts/orgs/{org}/teams/{team}/role", businessRoute, s.handleSetTeamRole)
	s.account("GET /api/v1/accounts/orgs/{org}/repos/{repo}/access", businessRoute, s.handleRepoGrants)
	s.account("PUT /api/v1/accounts/orgs/{org}/repos/{repo}/access", businessRoute, s.handleSetRepoGrant)
	s.account("DELETE /api/v1/accounts/orgs/{org}/repos/{repo}/access", businessRoute, s.handleRemoveRepoGrant)
	s.account("GET /api/v1/accounts/orgs/{org}/git-tokens", businessRoute, s.handleGitTokens)
	s.account("PUT /api/v1/accounts/orgs/{org}/git-tokens/{host}", businessRoute, s.handleSetGitToken)
	s.account("DELETE /api/v1/accounts/orgs/{org}/git-tokens/{host}", businessRoute, s.handleRemoveGitToken)
	s.account("GET /api/v1/admin/users", governanceRoute, s.handleAdminUsersDetail)
	s.account("GET /api/v1/admin/orgs", governanceRoute, s.handleAdminOrgsDetail)
	s.account("POST /api/v1/admin/users/{id}/suspend", governanceRoute, s.handleSuspendUser)
	s.account("POST /api/v1/admin/users/{id}/resume", governanceRoute, s.handleResumeUser)
	s.account("PUT /api/v1/admin/users/{id}/platform-admin", governanceRoute, s.handleSetPlatformAdmin)
	s.account("GET /api/v1/admin/health", governanceRoute, s.handleAdminHealth)
	s.account("POST /api/v1/admin/orgs/{id}/owner", governanceRoute, s.handleRecoverOwner)
}

func (s *Server) handleAuthStatus(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Accounts == nil {
		writeJSON(w, 200, map[string]any{"mode": "single"})
		return
	}
	setup, err := s.cfg.Accounts.Status(r.Context())
	if err != nil {
		s.problem(w, 500, err)
		return
	}
	initial, err := s.cfg.Accounts.InitialAdminActive(r.Context())
	if err != nil {
		s.problem(w, 500, err)
		return
	}
	// initial_admin only lets the sign-in page say where the first-start
	// password is; the account name is public in the documentation anyway.
	writeJSON(w, 200, map[string]any{"mode": "accounts", "needs_setup": setup, "signup_open": s.cfg.Accounts.SignupOpen || setup, "initial_admin": initial})
}

type credentials struct {
	Email    string `json:"email"`
	Username string `json:"username"`
	// Login is an email or a username; the sign-in form sends it. Email is
	// still accepted so existing clients keep working.
	Login    string `json:"login"`
	Name     string `json:"name"`
	Password string `json:"password"`
}

func (s *Server) handleSignup(w http.ResponseWriter, r *http.Request) {
	if !s.accountsOn(w) {
		return
	}
	var c credentials
	if err := decodeJSON(w, r, &c); err != nil {
		s.problem(w, 400, err)
		return
	}
	u, org, err := s.cfg.Accounts.SignUpWith(r.Context(), accounts.SignUpInput{Email: c.Email, Username: c.Username, Name: c.Name, Password: c.Password})
	if err != nil {
		s.accountError(w, err)
		return
	}
	tok, _, err := s.cfg.Accounts.Login(r.Context(), u.Email, c.Password, s.clientAddr(r))
	if err != nil {
		s.accountError(w, err)
		return
	}
	s.setSession(w, tok)
	writeJSON(w, http.StatusCreated, map[string]any{"user": u, "org": org})
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if !s.accountsOn(w) {
		return
	}
	var c credentials
	if err := decodeJSON(w, r, &c); err != nil {
		s.problem(w, 400, err)
		return
	}
	tok, u, err := s.cfg.Accounts.Login(r.Context(), cmp.Or(c.Login, c.Email), c.Password, s.clientAddr(r))
	if err != nil {
		s.accountError(w, err)
		return
	}
	s.setSession(w, tok)
	writeJSON(w, 200, map[string]any{"user": u})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if !s.accountsOn(w) {
		return
	}
	if c, err := r.Cookie(SessionCookie); err == nil {
		_ = s.cfg.Accounts.Logout(r.Context(), c.Value) //nolint:errcheck // the cookie is cleared either way
	}
	s.clearSession(w)
	w.WriteHeader(http.StatusNoContent)
}

// caller returns the principal or writes 401/409.
func (s *Server) caller(w http.ResponseWriter, r *http.Request) (accounts.Principal, bool) {
	if !s.accountsOn(w) {
		return accounts.Principal{}, false
	}
	p, ok := principal(r.Context())
	if !ok {
		s.problem(w, http.StatusUnauthorized, errors.New("log in first"))
	}
	return p, ok
}

func (s *Server) accountsOn(w http.ResponseWriter) bool {
	if s.cfg.Accounts == nil {
		s.problem(w, http.StatusConflict, errors.New("accounts are off: set CODESYNAPSE_DATABASE_URL to enable users and organisations"))
		return false
	}
	return true
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	p, ok := s.caller(w, r)
	if !ok {
		return
	}
	ms := []accounts.Membership{}
	restricted, err := s.restricted(r.Context(), p)
	if err != nil {
		s.accountError(w, err)
		return
	}
	out := map[string]any{"user": p.User, "via": p.Via, "org_id": p.OrgID, "memberships": ms, "restricted": restricted}
	if restricted {
		// Governance only: no organisation to switch to; the UI needs the
		// setup state to offer the handover.
		setup, err := s.cfg.Accounts.SetupState(r.Context())
		if err != nil {
			s.problem(w, 500, err)
			return
		}
		out["setup"] = setup.State
	} else if p.Via != "service" {
		if ms, err = s.cfg.Accounts.St.UserMemberships(r.Context(), p.User.ID); err != nil {
			s.problem(w, 500, err)
			return
		}
		out["memberships"] = ms
	}
	writeJSON(w, 200, out)
}

type passwordChange struct {
	Current string `json:"current"`
	Next    string `json:"next"`
}

func (s *Server) handlePassword(w http.ResponseWriter, r *http.Request) {
	p, ok := s.caller(w, r)
	if !ok {
		return
	}
	var c passwordChange
	if err := decodeJSON(w, r, &c); err != nil {
		s.problem(w, 400, err)
		return
	}
	if err := s.cfg.Accounts.ChangePassword(r.Context(), p, c.Current, c.Next); err != nil {
		s.accountError(w, err)
		return
	}
	s.clearSession(w)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleListKeys(w http.ResponseWriter, r *http.Request) {
	p, ok := s.caller(w, r)
	if !ok {
		return
	}
	keys, err := s.cfg.Accounts.St.ListAPIKeys(r.Context(), p.User.ID)
	if err != nil {
		s.problem(w, 500, err)
		return
	}
	writeJSON(w, 200, map[string]any{"keys": keys})
}

type keyRequest struct {
	Org     string `json:"org"` // slug
	Name    string `json:"name"`
	TTLDays int    `json:"ttl_days"`
}

func (s *Server) handleCreateKey(w http.ResponseWriter, r *http.Request) {
	p, ok := s.caller(w, r)
	if !ok {
		return
	}
	var req keyRequest
	if err := decodeJSON(w, r, &req); err != nil {
		s.problem(w, 400, err)
		return
	}
	if req.TTLDays < 0 || req.TTLDays > 3650 {
		s.problem(w, 400, errors.New("ttl_days: 0 (never) to 3650"))
		return
	}
	org, err := s.cfg.Accounts.St.OrgBySlug(r.Context(), req.Org)
	if err != nil {
		s.accountError(w, err)
		return
	}
	key, k, err := s.cfg.Accounts.CreateKey(r.Context(), p, org.ID, req.Name, time.Duration(req.TTLDays)*24*time.Hour)
	if err != nil {
		s.accountError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"key": key, "api_key": k, "notice": "Copy the key now: it is shown only once."})
}

func (s *Server) handleRevokeKey(w http.ResponseWriter, r *http.Request) {
	p, ok := s.caller(w, r)
	if !ok {
		return
	}
	if err := s.cfg.Accounts.RevokeKey(r.Context(), p, r.PathValue("id")); err != nil {
		s.accountError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleMyOrgs(w http.ResponseWriter, r *http.Request) {
	p, ok := s.caller(w, r)
	if !ok {
		return
	}
	ms, err := s.cfg.Accounts.St.UserMemberships(r.Context(), p.User.ID)
	if err != nil {
		s.problem(w, 500, err)
		return
	}
	if p.OrgID != "" {
		ms = slices.DeleteFunc(ms, func(m accounts.Membership) bool { return m.Org.ID != p.OrgID })
	}
	writeJSON(w, 200, map[string]any{"memberships": ms})
}

type orgRequest struct {
	Name string `json:"name"`
	Slug string `json:"slug"`
}

func (s *Server) handleCreateOrg(w http.ResponseWriter, r *http.Request) {
	p, ok := s.caller(w, r)
	if !ok {
		return
	}
	var req orgRequest
	if err := decodeJSON(w, r, &req); err != nil {
		s.problem(w, 400, err)
		return
	}
	if p.Via == "service" {
		s.problem(w, http.StatusForbidden, errors.New("the service token is not a person; log in to create organisations"))
		return
	}
	o, err := s.cfg.Accounts.CreateOrg(r.Context(), p, req.Name, req.Slug)
	if err != nil {
		s.accountError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, o)
}

// orgFor resolves the {org} slug and checks the caller has at least least.
func (s *Server) orgFor(w http.ResponseWriter, r *http.Request, least accounts.Role) (accounts.Principal, accounts.Org, bool) {
	p, ok := s.caller(w, r)
	if !ok {
		return p, accounts.Org{}, false
	}
	o, err := s.cfg.Accounts.St.OrgBySlug(r.Context(), r.PathValue("org"))
	if err == nil {
		err = s.cfg.Accounts.Require(r.Context(), p, o.ID, least)
	}
	if err != nil {
		if errors.Is(err, accounts.ErrForbidden) {
			err = accounts.ErrNotFound // do not reveal organisations one cannot see
		}
		s.accountError(w, err)
		return p, o, false
	}
	return p, o, true
}

func (s *Server) handleMembers(w http.ResponseWriter, r *http.Request) {
	_, o, ok := s.orgFor(w, r, accounts.Viewer)
	if !ok {
		return
	}
	ms, err := s.cfg.Accounts.St.OrgMembers(r.Context(), o.ID)
	if err != nil {
		s.problem(w, 500, err)
		return
	}
	writeJSON(w, 200, map[string]any{"org": o, "members": ms})
}

type memberRequest struct {
	Email string        `json:"email"`
	Role  accounts.Role `json:"role"`
}

func (s *Server) handleSetMember(w http.ResponseWriter, r *http.Request) {
	p, o, ok := s.orgFor(w, r, accounts.Viewer)
	if !ok {
		return
	}
	var req memberRequest
	if err := decodeJSON(w, r, &req); err != nil {
		s.problem(w, 400, err)
		return
	}
	m, err := s.cfg.Accounts.AddMember(r.Context(), p, o.ID, req.Email, req.Role)
	if err != nil {
		s.accountError(w, err)
		return
	}
	writeJSON(w, 200, m)
}

func (s *Server) handleRemoveMember(w http.ResponseWriter, r *http.Request) {
	p, o, ok := s.orgFor(w, r, accounts.Viewer)
	if !ok {
		return
	}
	if err := s.cfg.Accounts.RemoveMember(r.Context(), p, o.ID, r.PathValue("user")); err != nil {
		s.accountError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// platformAdmin writes 403 unless the caller operates the installation.
func (s *Server) platformAdmin(w http.ResponseWriter, r *http.Request) bool {
	p, ok := s.caller(w, r)
	if ok && !p.User.PlatformAdmin {
		s.problem(w, http.StatusForbidden, errors.New("platform admins only"))
		return false
	}
	return ok
}

func clamp(v, lo, hi, def int) int {
	if v <= 0 {
		return def
	}
	return min(max(v, lo), hi)
}
