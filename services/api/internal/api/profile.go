package api

import (
	"net/http"

	"github.com/astrona-io/codesynapse/internal/accounts"
)

// A person's own account from the profile page: name, email and sessions
// (accounts/profile.go has the rules; every change needs a signed-in
// session).

func (s *Server) profileRoutes() {
	s.account("PATCH /api/v1/me", governanceRoute, s.handleUpdateProfile)
	s.account("POST /api/v1/me/email", governanceRoute, s.handleChangeEmail)
	s.account("GET /api/v1/me/sessions", governanceRoute, s.handleMySessions)
	s.account("DELETE /api/v1/me/sessions/{id}", governanceRoute, s.handleEndSession)
	s.account("POST /api/v1/me/sessions/end-others", governanceRoute, s.handleEndOtherSessions)
	s.account("GET /api/v1/me/menu", businessRoute, s.handleMenuPrefs)
	s.account("PUT /api/v1/me/menu", businessRoute, s.handleSetMenuPrefs)
}

func (s *Server) handleUpdateProfile(w http.ResponseWriter, r *http.Request) {
	p, ok := s.caller(w, r)
	if !ok {
		return
	}
	var in struct {
		Name     string  `json:"name"`
		Username *string `json:"username"` // nil: unchanged; "": remove
	}
	if err := decodeJSON(w, r, &in); err != nil {
		s.problem(w, 400, err)
		return
	}
	u := p.User
	var err error
	if in.Name != "" || in.Username == nil {
		if u, err = s.cfg.Accounts.UpdateName(r.Context(), p, in.Name); err != nil {
			s.accountError(w, err)
			return
		}
	}
	if in.Username != nil {
		if u, err = s.cfg.Accounts.UpdateUsername(r.Context(), p, *in.Username); err != nil {
			s.accountError(w, err)
			return
		}
	}
	writeJSON(w, 200, u)
}

func (s *Server) handleChangeEmail(w http.ResponseWriter, r *http.Request) {
	p, ok := s.caller(w, r)
	if !ok {
		return
	}
	var in struct {
		Password string `json:"password"`
		Email    string `json:"email"`
	}
	if err := decodeJSON(w, r, &in); err != nil {
		s.problem(w, 400, err)
		return
	}
	u, err := s.cfg.Accounts.ChangeEmail(r.Context(), p, in.Password, in.Email)
	if err != nil {
		s.accountError(w, err)
		return
	}
	writeJSON(w, 200, u)
}

func (s *Server) handleMySessions(w http.ResponseWriter, r *http.Request) {
	p, ok := s.caller(w, r)
	if !ok {
		return
	}
	list, err := s.cfg.Accounts.Sessions(r.Context(), p)
	if err != nil {
		s.accountError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"sessions": list})
}

func (s *Server) handleEndSession(w http.ResponseWriter, r *http.Request) {
	p, ok := s.caller(w, r)
	if !ok {
		return
	}
	list, err := s.cfg.Accounts.Sessions(r.Context(), p)
	if err != nil {
		s.accountError(w, err)
		return
	}
	current := false
	for _, sess := range list {
		current = current || sess.ID == r.PathValue("id") && sess.Current
	}
	if err := s.cfg.Accounts.EndSession(r.Context(), p, r.PathValue("id")); err != nil {
		s.accountError(w, err)
		return
	}
	if current {
		s.clearSession(w)
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleEndOtherSessions(w http.ResponseWriter, r *http.Request) {
	p, ok := s.caller(w, r)
	if !ok {
		return
	}
	if err := s.cfg.Accounts.EndOtherSessions(r.Context(), p); err != nil {
		s.accountError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleMenuPrefs(w http.ResponseWriter, r *http.Request) {
	p, ok := s.caller(w, r)
	if !ok {
		return
	}
	m, err := s.cfg.Accounts.MenuPrefs(r.Context(), p)
	if err != nil {
		s.accountError(w, err)
		return
	}
	writeJSON(w, 200, m)
}

func (s *Server) handleSetMenuPrefs(w http.ResponseWriter, r *http.Request) {
	p, ok := s.caller(w, r)
	if !ok {
		return
	}
	var in accounts.MenuPrefs
	if err := decodeJSON(w, r, &in); err != nil {
		s.problem(w, 400, err)
		return
	}
	m, err := s.cfg.Accounts.SetMenuPrefs(r.Context(), p, in)
	if err != nil {
		s.accountError(w, err)
		return
	}
	writeJSON(w, 200, m)
}
