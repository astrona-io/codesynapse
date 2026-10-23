package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/astrona-io/codesynapse/internal/accounts"
)

// Every route is classified for accounts limited to governance (the
// bootstrap root, docs/ROOT_BOOTSTRAP_PLAN.md): s.route through its policy,
// account routes through s.account. TestEveryRouteIsClassified keeps other
// registrations out.

// routeClass is what an account route allows a governance-only account.
type routeClass int

const (
	// publicRoute: sign-up, login, status; no account involved.
	publicRoute routeClass = iota
	// governanceRoute: settings, health, audit, account administration.
	governanceRoute
	// businessRoute: organisation work, refused to root.
	businessRoute
)

// account registers an account route with its class.
func (s *Server) account(pattern string, class routeClass, h http.HandlerFunc) {
	s.classify(pattern, class)
	if class == businessRoute {
		h = s.business(h)
	}
	s.mux.HandleFunc(pattern, h)
}

func (s *Server) classify(pattern string, class routeClass) {
	if s.classes == nil {
		s.classes = map[string]routeClass{}
	}
	s.classes[pattern] = class
}

// governance marks a policy as allowed for governance-only accounts.
func governance(p policy) policy {
	p.governance = true
	return p
}

// business refuses governance-only callers before h runs.
func (s *Server) business(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if p, ok := principal(r.Context()); ok && s.cfg.Accounts != nil {
			restricted, err := s.restricted(r.Context(), p)
			if err != nil {
				s.accountError(w, err)
				return
			}
			if restricted {
				s.denyRoot(w, r, p)
				return
			}
		}
		h(w, r)
	}
}

// restricted reports whether p may do governance only. The installation
// token is not an account and is scoped by membershipScope instead.
func (s *Server) restricted(ctx context.Context, p accounts.Principal) (bool, error) {
	if p.Via == "service" {
		return false, nil
	}
	return s.cfg.Accounts.Restricted(ctx, p.User.ID)
}

// denyRoot refuses organisation work to a governance-only account and
// audits the route, never the request content.
func (s *Server) denyRoot(w http.ResponseWriter, r *http.Request, p accounts.Principal) {
	s.record(r.Context(), accounts.Event{UserID: p.User.ID, ActorEmail: p.User.Email, Via: p.Via, KeyID: p.KeyID,
		Action: "root.denied", Target: r.Pattern, Outcome: accounts.Failure})
	s.problem(w, http.StatusForbidden, accounts.ErrRootAccount)
}

// SetupComplete is shown once root has handed the installation over.
const SetupComplete = "Setup is complete. Sign in with the standard account for organisation work."

// handleSetup completes the bootstrap handover and ends root's session.
func (s *Server) handleSetup(w http.ResponseWriter, r *http.Request) {
	p, ok := s.caller(w, r)
	if !ok {
		return
	}
	var in accounts.SetupInput
	if err := decodeJSON(w, r, &in); err != nil {
		s.problem(w, http.StatusBadRequest, err)
		return
	}
	setup, err := s.cfg.Accounts.CompleteSetup(r.Context(), p, in)
	if err != nil {
		s.accountError(w, err)
		return
	}
	s.clearSession(w)
	writeJSON(w, http.StatusOK, map[string]any{"message": SetupComplete, "setup": setup})
}

type ownerRecovery struct {
	UserID string `json:"user_id"`
}

// handleRecoverOwner promotes an existing standard member of an
// organisation whose owners lost access (platform administration).
func (s *Server) handleRecoverOwner(w http.ResponseWriter, r *http.Request) {
	p, ok := s.caller(w, r)
	if !ok {
		return
	}
	var in ownerRecovery
	if err := decodeJSON(w, r, &in); err != nil {
		s.problem(w, http.StatusBadRequest, err)
		return
	}
	if in.UserID == "" {
		s.problem(w, http.StatusBadRequest, errors.New("user_id is required"))
		return
	}
	if err := s.cfg.Accounts.RecoverOrgOwner(r.Context(), p, r.PathValue("id"), in.UserID); err != nil {
		s.accountError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
