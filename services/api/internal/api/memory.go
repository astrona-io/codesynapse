package api

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/parisnakitakejser/codesynapse/internal/store"
)

// handleMemory lists a repository's agent memory for review in the UI.
func (s *Server) handleMemory(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit")) //nolint:errcheck // bad values fall back to the default
	g, err := s.st.SearchMemory(r.Context(), r.PathValue("repo"), r.URL.Query().Get("q"), store.ClampLimit(limit, 100))
	if err != nil {
		s.problem(w, 500, err)
		return
	}
	writeJSON(w, 200, g)
}

// handleDeleteMemory removes one fact (?fact=id) or an entity with its facts
// (?entity=id). IDs go in the query because entity IDs may contain slashes.
func (s *Server) handleDeleteMemory(w http.ResponseWriter, r *http.Request) {
	repo, q := r.PathValue("repo"), r.URL.Query()
	var err error
	switch {
	case q.Get("fact") != "":
		err = s.st.DeleteMemoryFact(r.Context(), repo, q.Get("fact"))
	case q.Get("entity") != "":
		err = s.st.DeleteMemoryEntity(r.Context(), repo, q.Get("entity"))
	default:
		s.problem(w, 400, errors.New("give fact=<id> or entity=<id>"))
		return
	}
	switch {
	case errors.Is(err, store.ErrNotFound):
		s.problem(w, 404, err)
	case err != nil:
		s.problem(w, 500, err)
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}
