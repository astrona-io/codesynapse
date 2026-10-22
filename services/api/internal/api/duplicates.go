package api

import (
	"errors"
	"net/http"

	"github.com/parisnakitakejser/codesynapse/internal/duplicates"
	"github.com/parisnakitakejser/codesynapse/internal/model"
)

// handleDuplicates serves the duplicate-code report computed at ingest time.
// The fingerprints used to compare repositories are left out.
func (s *Server) handleDuplicates(w http.ResponseWriter, r *http.Request) {
	repo := r.PathValue("repo")
	n, err := s.st.Node(r.Context(), model.NodeID(repo, model.KindRepository, repo))
	if err != nil {
		s.problem(w, 500, err)
		return
	}
	if n.Duplicates == "" {
		s.problem(w, http.StatusNotFound, errors.New("no duplicate report stored; re-ingest (Sync) the repository"))
		return
	}
	rep, err := duplicates.Decode(n.Duplicates)
	if err != nil {
		s.problem(w, 500, err)
		return
	}
	rep.Fingerprints = nil
	writeJSON(w, 200, rep)
}

// handleSharedCode compares the repositories the caller can see and reports
// code they have in common, with a recommendation where it is worth sharing.
// Only visible repositories are read, so nothing leaks across organisations.
func (s *Server) handleSharedCode(w http.ResponseWriter, r *http.Request) {
	repos, err := s.st.Repos(r.Context())
	if err != nil {
		s.problem(w, 500, err)
		return
	}
	reports := map[string]duplicates.Report{}
	for _, n := range visibleRepos(r.Context(), repos) {
		if n.Duplicates == "" {
			continue
		}
		rep, err := duplicates.Decode(n.Duplicates)
		if err != nil {
			s.problem(w, 500, err)
			return
		}
		reports[n.Name] = rep
	}
	writeJSON(w, 200, duplicates.Across(reports))
}
