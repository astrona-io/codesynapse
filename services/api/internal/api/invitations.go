package api

import (
	"net/http"
	"time"

	"github.com/astrona-io/codesynapse/internal/accounts"
)

// Invitations are how people join an organisation (accounts/invitations.go).
// Creating one answers the same, in the same time, whether or not the email
// has an account; the accept token is returned once and only its hash is
// kept. There is no mail: the inviter passes the link on.

type invitationRequest struct {
	Email string        `json:"email"`
	Role  accounts.Role `json:"role"`
}

type invitationToken struct {
	Token string `json:"token"`
}

func (s *Server) handleInvitations(w http.ResponseWriter, r *http.Request) {
	p, o, ok := s.orgFor(w, r, accounts.Viewer)
	if !ok {
		return
	}
	invs, err := s.cfg.Accounts.Invitations(r.Context(), p, o.ID)
	if err != nil {
		s.accountError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"org": o, "invitations": invs})
}

func (s *Server) handleCreateInvitation(w http.ResponseWriter, r *http.Request) {
	p, o, ok := s.orgFor(w, r, accounts.Viewer)
	if !ok {
		return
	}
	var req invitationRequest
	if err := decodeJSON(w, r, &req); err != nil {
		s.problem(w, 400, err)
		return
	}
	// The service never looks the email up; the floor (the sign-up one)
	// keeps the answer time free of anything that might still differ.
	start := time.Now()
	inv, token, err := s.cfg.Accounts.CreateInvitation(r.Context(), p, o.ID, req.Email, req.Role)
	s.waitSince(r.Context(), start, s.signupFloor)
	if err != nil {
		s.accountError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusCreated, map[string]any{"invitation": inv, "token": token, "accept_path": "/invite/" + token})
}

func (s *Server) handleRevokeInvitation(w http.ResponseWriter, r *http.Request) {
	p, o, ok := s.orgFor(w, r, accounts.Viewer)
	if !ok {
		return
	}
	if err := s.cfg.Accounts.RevokeInvitation(r.Context(), p, o.ID, r.PathValue("id")); err != nil {
		s.accountError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// The token travels in the body, not the path, so it stays out of access
// logs and traces.

func (s *Server) handlePreviewInvitation(w http.ResponseWriter, r *http.Request) {
	p, ok := s.caller(w, r)
	if !ok {
		return
	}
	var req invitationToken
	if err := decodeJSON(w, r, &req); err != nil {
		s.problem(w, 400, err)
		return
	}
	pre, err := s.cfg.Accounts.PreviewInvitation(r.Context(), p, req.Token)
	if err != nil {
		s.accountError(w, err)
		return
	}
	writeJSON(w, 200, pre)
}

func (s *Server) handleAcceptInvitation(w http.ResponseWriter, r *http.Request) {
	p, ok := s.caller(w, r)
	if !ok {
		return
	}
	var req invitationToken
	if err := decodeJSON(w, r, &req); err != nil {
		s.problem(w, 400, err)
		return
	}
	m, err := s.cfg.Accounts.AcceptInvitation(r.Context(), p, req.Token)
	if err != nil {
		s.accountError(w, err)
		return
	}
	writeJSON(w, 200, m)
}
