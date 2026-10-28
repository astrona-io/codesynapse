package api

import (
	"net/http"

	"github.com/astrona-io/codesynapse/internal/accounts"
)

// handleOrgMemberRepos reads (any member) or sets (owner or admin, signed-in
// session only) whether members may add repositories to their own space (on
// unless switched off). Switching it off keeps what members already added.
func (s *Server) handleOrgMemberRepos(w http.ResponseWriter, r *http.Request) {
	least := accounts.Member
	if r.Method == http.MethodPut {
		least = accounts.Admin
	}
	p, o, ok := s.orgFor(w, r, least)
	if !ok {
		return
	}
	if r.Method == http.MethodPut {
		var req struct {
			Enabled bool `json:"enabled"`
		}
		if err := decodeJSON(w, r, &req); err != nil {
			s.problem(w, http.StatusBadRequest, err)
			return
		}
		if err := s.cfg.Accounts.SetMemberRepos(r.Context(), p, o.ID, req.Enabled); err != nil {
			s.accountError(w, err)
			return
		}
	}
	on, err := s.cfg.Accounts.MemberReposSetting(r.Context(), p, o.ID)
	if err != nil {
		s.accountError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"enabled": on})
}
