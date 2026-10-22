package api

import (
	"net/http"

	"github.com/astrona-io/codesynapse/internal/curation"
	"github.com/astrona-io/codesynapse/internal/query"
	"github.com/astrona-io/codesynapse/internal/store"
)

func (s *Server) handleIdentities(w http.ResponseWriter, r *http.Request) {
	repo := r.PathValue("repo")
	svc := query.Service{R: store.ForRepository(s.st, repo)}
	page, err := svc.Identities(r.Context(), repo, r.URL.Query().Get("id"), r.URL.Query().Get("review") == "1", intParam(r, "offset", 0), intParam(r, "limit", 20))
	if err != nil {
		s.problem(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, page)
}

func (s *Server) handleCodeRecords(w http.ResponseWriter, r *http.Request) {
	repo := r.PathValue("repo")
	page, err := s.catalogOf(r.Context(), repo).CodeRecords(r.Context(), repo, r.URL.Query().Get("symbol"), intParam(r, "offset", 0), intParam(r, "limit", 20))
	if err != nil {
		s.catalogError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, page)
}

func (s *Server) handleRecordCode(w http.ResponseWriter, r *http.Request) {
	repo := r.PathValue("repo")
	var in curation.CodeInput
	if err := decodeBody(w, r, &in); err != nil {
		s.catalogError(w, err)
		return
	}
	v, err := s.catalogOf(r.Context(), repo).RecordCode(r.Context(), repo, actorOf(r.Context(), repo), in)
	if err != nil {
		s.catalogError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, v)
}
