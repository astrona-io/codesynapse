package api

import (
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"

	"github.com/parisnakitakejser/codesynapse/internal/memory/extract"
	"github.com/parisnakitakejser/codesynapse/internal/model"
	"github.com/parisnakitakejser/codesynapse/internal/store"
	"github.com/parisnakitakejser/codesynapse/services/api/internal/offload"
	"github.com/parisnakitakejser/codesynapse/services/api/internal/recall"
)

// handleMemory lists a repository's agent memory for review in the UI.
func (s *Server) handleMemory(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit")) //nolint:errcheck // bad values fall back to the default
	ns, err := s.memNamespace(r, r.PathValue("repo"))
	if err != nil {
		s.accountError(w, err)
		return
	}
	g, err := s.st.SearchMemory(r.Context(), ns, r.URL.Query().Get("q"), store.ClampLimit(limit, 100), r.URL.Query().Get("history") == "true")
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
	repo, nsErr := s.memNamespace(r, repo)
	if nsErr != nil {
		s.accountError(w, nsErr)
		return
	}
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

type offloadRequest struct {
	Turns  []extract.Turn `json:"turns"`
	Source string         `json:"source"`
}

// handleOffload queues extraction of memory triples from conversation turns
// and returns at once (202). Turns are held in memory until the job ran.
func (s *Server) handleOffload(w http.ResponseWriter, r *http.Request) {
	repo := r.PathValue("repo")
	if _, err := s.st.Node(r.Context(), model.NodeID(repo, model.KindRepository, repo)); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			s.problem(w, 404, fmt.Errorf("repository %q is not indexed", repo))
			return
		}
		s.problem(w, 500, err)
		return
	}
	var req offloadRequest
	if err := decodeJSON(w, r, &req); err != nil {
		s.problem(w, 400, err)
		return
	}
	ns, err := s.memNamespace(r, repo)
	if err != nil {
		s.accountError(w, err)
		return
	}
	job, err := s.offload.Submit(repo, ns, req.Source, req.Turns)
	switch {
	case errors.Is(err, offload.ErrNoLocalModel):
		s.problem(w, http.StatusConflict, err)
	case errors.Is(err, offload.ErrBusy):
		s.problem(w, http.StatusServiceUnavailable, err)
	case err != nil:
		s.problem(w, 400, err)
	default:
		writeJSON(w, http.StatusAccepted, job)
	}
}

func (s *Server) handleOffloadJobs(w http.ResponseWriter, r *http.Request) {
	_, err := s.offload.Provider()
	sc := scopeFrom(r.Context())
	jobs := slices.DeleteFunc(s.offload.List(r.URL.Query().Get("repo")), func(j offload.Job) bool { return !sc.sees(j.Repo) })
	writeJSON(w, 200, map[string]any{"jobs": jobs, "available": err == nil})
}

func (s *Server) handleOffloadJob(w http.ResponseWriter, r *http.Request) {
	j, ok := s.offload.Get(r.PathValue("id"))
	if !ok || !scopeFrom(r.Context()).sees(j.Repo) {
		s.problem(w, 404, errors.New("no such memory extraction job"))
		return
	}
	writeJSON(w, 200, j)
}

// handleRecall runs hybrid recall: intent gate, vector and name entry
// points, then a 1-2 hop expansion.
func (s *Server) handleRecall(w http.ResponseWriter, r *http.Request) {
	var q recall.Query
	if err := decodeJSON(w, r, &q); err != nil {
		s.problem(w, 400, err)
		return
	}
	if len(q.Text) > 2000 {
		s.problem(w, 400, errors.New("query: at most 2000 characters"))
		return
	}
	ns, err := s.memNamespace(r, r.PathValue("repo"))
	if err != nil {
		s.accountError(w, err)
		return
	}
	res, err := s.recall.Recall(r.Context(), ns, q)
	if err != nil {
		s.problem(w, 500, err)
		return
	}
	writeJSON(w, 200, res)
}

// handleMemoryStatus says which memory features have a local model.
func (s *Server) handleMemoryStatus(w http.ResponseWriter, _ *http.Request) {
	out := map[string]any{"extraction": false, "embeddings": false}
	if name, err := s.offload.Provider(); err == nil {
		out["extraction"], out["extraction_provider"] = true, name
	}
	if s.cfg.LLM != nil {
		if name, cfg, err := s.cfg.LLM.EmbeddingProvider(); err == nil {
			out["embeddings"], out["embedding_provider"], out["embedding_model"] = true, name, cfg.EmbeddingModel
		}
	}
	writeJSON(w, 200, out)
}
