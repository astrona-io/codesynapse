package api

import (
	"errors"
	"net/http"
	"strings"

	"github.com/parisnakitakejser/codesynapse/internal/catalog"
	"github.com/parisnakitakejser/codesynapse/internal/store"
)

// The architecture catalog: records kept as files in the repository, read
// through the same internal/catalog service the MCP tools use, on a reader
// fenced to the repository the route was authorised for.

func (s *Server) catalogOf(repo string) catalog.Service {
	return catalog.Service{R: store.ForRepository(s.st, repo)}
}

func (s *Server) catalogError(w http.ResponseWriter, err error) {
	if errors.Is(err, store.ErrNotFound) {
		s.problem(w, http.StatusNotFound, err)
		return
	}
	s.problem(w, 500, err)
}

func commaList(v string) []string {
	var out []string
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// handleArchitecture lists the records: ?type= ?status= ?component= ?label=
// ?q= ?binding=1 ?limit= ?offset=.
func (s *Server) handleArchitecture(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	page, err := s.catalogOf(r.PathValue("repo")).List(r.Context(), r.PathValue("repo"), catalog.Filter{
		Type: q.Get("type"), Status: q.Get("status"), Component: q.Get("component"), Label: q.Get("label"), Q: q.Get("q"),
		BindingOnly: q.Get("binding") == "1", Limit: intParam(r, "limit", 50), Offset: intParam(r, "offset", 0),
	})
	if err != nil {
		s.catalogError(w, err)
		return
	}
	writeJSON(w, 200, page)
}

// handleArchitectureRecord returns one record: ?id=ADR-0001.
func (s *Server) handleArchitectureRecord(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	if id == "" || len(id) > 200 {
		s.problem(w, http.StatusBadRequest, errors.New("id is required"))
		return
	}
	d, err := s.catalogOf(r.PathValue("repo")).Get(r.Context(), r.PathValue("repo"), id)
	if err != nil {
		s.catalogError(w, err)
		return
	}
	writeJSON(w, 200, d)
}

// handleArchitectureIssues lists what a person should look at in the records.
func (s *Server) handleArchitectureIssues(w http.ResponseWriter, r *http.Request) {
	issues, src, err := s.catalogOf(r.PathValue("repo")).Issues(r.Context(), r.PathValue("repo"))
	if err != nil {
		s.catalogError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"issues": issues, "source": src})
}

// handleArchitectureContext finds the records that apply to a task:
// ?q= ?files=a,b ?symbols=x,y ?components=c ?limit=.
func (s *Server) handleArchitectureContext(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	res, err := s.catalogOf(r.PathValue("repo")).Context(r.Context(), r.PathValue("repo"), catalog.Task{
		Query: q.Get("q"), Files: commaList(q.Get("files")), Symbols: commaList(q.Get("symbols")), Components: commaList(q.Get("components")), Limit: intParam(r, "limit", 10),
	})
	if err != nil {
		s.catalogError(w, err)
		return
	}
	writeJSON(w, 200, res)
}
