package api

import (
	"net/http"

	"github.com/astrona-io/codesynapse/internal/history"
	"github.com/astrona-io/codesynapse/internal/model"
)

// handleHistory serves the snapshots taken after each ingest of a repository
// (?limit=, default 100) with what got worse and better since the one before.
func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request) {
	repo := r.PathValue("repo")
	if _, err := s.st.Node(r.Context(), model.NodeID(repo, model.KindRepository, repo)); err != nil {
		s.problem(w, 404, err)
		return
	}
	snaps, err := s.st.Snapshots(r.Context(), repo, intParam(r, "limit", 100))
	if err != nil {
		s.problem(w, 500, err)
		return
	}
	writeJSON(w, 200, history.Build(snaps))
}
