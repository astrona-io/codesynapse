package api

import (
	"net/http"

	"github.com/astrona-io/codesynapse/internal/accounts"
	"github.com/astrona-io/codesynapse/internal/model"
)

// A repository's house style (internal/conventions): how its tests assert,
// how errors and logs are made, which frameworks it uses. The plugin's
// SessionStart hook adds it to a session's context once, rather than every
// minimal_context answer paying for it.

func (s *Server) conventionsRoutes() {
	s.route("GET /api/v1/repos/{repo}/conventions", repoRole(accounts.Viewer), s.handleConventions)
}

func (s *Server) handleConventions(w http.ResponseWriter, r *http.Request) {
	repo := r.PathValue("repo")
	n, err := s.st.Node(r.Context(), model.NodeID(repo, model.KindRepository, repo))
	if err != nil {
		s.problem(w, http.StatusNotFound, err)
		return
	}
	out := model.ConventionsOf(n)
	if out == nil {
		out = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"repo": repo, "conventions": out})
}
