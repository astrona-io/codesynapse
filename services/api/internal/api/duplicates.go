package api

import (
	"context"
	"errors"
	"net/http"
	"regexp"

	"github.com/parisnakitakejser/codesynapse/internal/duplicates"
	"github.com/parisnakitakejser/codesynapse/internal/model"
	"github.com/parisnakitakejser/codesynapse/internal/refactorplan"
	"github.com/parisnakitakejser/codesynapse/internal/store"
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

// sharedCode compares the repositories the caller can see. Only those are
// read, so nothing leaks across organisations.
func (s *Server) sharedCode(ctx context.Context) (duplicates.CrossReport, error) {
	repos, err := s.st.Repos(ctx)
	if err != nil {
		return duplicates.CrossReport{}, err
	}
	reports := map[string]duplicates.Report{}
	for _, n := range visibleRepos(ctx, repos) {
		if n.Duplicates == "" {
			continue
		}
		rep, err := duplicates.Decode(n.Duplicates)
		if err != nil {
			return duplicates.CrossReport{}, err
		}
		reports[n.Name] = rep
	}
	return duplicates.Across(reports), nil
}

// handleSharedCode reports code the visible repositories have in common,
// with a recommendation where it is worth sharing.
func (s *Server) handleSharedCode(w http.ResponseWriter, r *http.Request) {
	rep, err := s.sharedCode(r.Context())
	if err != nil {
		s.problem(w, 500, err)
		return
	}
	writeJSON(w, 200, rep)
}

var groupHash = regexp.MustCompile(`^[0-9a-f]{16}$`)

// handleRefactorPlan says how to merge one group of duplicates: ?hash= and
// ?kind=exact|near, both as listed by the duplicates report.
func (s *Server) handleRefactorPlan(w http.ResponseWriter, r *http.Request) {
	repo, hash, kind := r.PathValue("repo"), r.URL.Query().Get("hash"), r.URL.Query().Get("kind")
	if !groupHash.MatchString(hash) || (kind != "" && kind != "exact" && kind != "near") {
		s.problem(w, http.StatusBadRequest, errors.New("hash (16 hex digits) is required; kind is exact or near"))
		return
	}
	plan, err := refactorplan.ForGroup(r.Context(), store.ForRepository(s.st, repo), repo, hash, kind)
	switch {
	case errors.Is(err, refactorplan.ErrNotFound):
		s.problem(w, http.StatusNotFound, err)
	case err != nil:
		s.problem(w, 500, err)
	default:
		writeJSON(w, 200, plan)
	}
}
