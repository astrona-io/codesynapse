package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/astrona-io/codesynapse/internal/accounts"
	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/semantic"
	"github.com/astrona-io/codesynapse/services/api/internal/llm"
)

// Search by meaning over code symbols (docs/adr/0002-code-symbol-embeddings.md).
// A local embedding provider embeds each symbol's description after an
// ingest; /semantic ranks them for a task. Off, and answering enabled:false,
// unless a local provider with an embedding model is routed to "embeddings".

func (s *Server) semanticRoutes() {
	s.route("POST /api/v1/repos/{repo}/semantic", repoRole(accounts.Viewer), s.withExecution(s.handleSemantic))
}

// registryEmbedder embeds through the registry's local embedding provider.
type registryEmbedder struct {
	reg  *llm.Registry
	name string
}

func (e registryEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	return e.reg.Embed(ctx, e.name, texts)
}

// semanticStore returns the symbol vector store and its embedder, or false
// when this installation has no semantic leg.
func (s *Server) semanticStore() (semantic.Store, semantic.Embedder, bool) {
	if s.cfg.LLM == nil {
		return semantic.Store{}, nil, false
	}
	ix, ok := s.st.(semantic.Indexer)
	if !ok || ix.CodeVectors() == nil {
		return semantic.Store{}, nil, false
	}
	name, cfg, err := s.cfg.LLM.EmbeddingProvider()
	if err != nil {
		return semantic.Store{}, nil, false
	}
	return semantic.Store{Idx: ix.CodeVectors(), Model: cfg.EmbeddingModel}, registryEmbedder{s.cfg.LLM, name}, true
}

// embedIngest makes vectors for the symbols an ingest added or changed. It
// is best effort: without a local embedding model it does nothing, and a
// failure leaves search lexical.
func (s *Server) embedIngest(ctx context.Context, repo string, g *model.Graph, authorize func(context.Context) error) {
	vs, emb, ok := s.semanticStore()
	if !ok {
		return
	}
	if authorize != nil {
		if err := authorize(ctx); err != nil {
			return
		}
	}
	st, err := vs.Sync(ctx, emb, repo, g.Nodes())
	if err != nil {
		if !errors.Is(err, llm.ErrNoEmbeddings) {
			s.logger.Warn("symbol embedding deferred", "error", err)
		}
		return
	}
	s.logger.Info("symbols embedded", "symbols", st.Symbols, "embedded", st.Embedded, "skipped", st.Skipped)
}

// afterIngest runs every post-ingest step.
func (s *Server) afterIngest(ctx context.Context, repo string, g *model.Graph, authorize func(context.Context) error) {
	s.annotateIngest(ctx, repo, g, authorize)
	s.embedIngest(ctx, repo, g, authorize)
}

type semanticHit struct {
	ID    string  `json:"id"`
	Score float64 `json:"score"`
}

func (s *Server) handleSemantic(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Task string `json:"task"`
		K    int    `json:"k"`
	}
	if err := decodeJSON(w, r, &in); err != nil {
		s.problem(w, http.StatusBadRequest, err)
		return
	}
	if in.Task == "" || len(in.Task) > 2000 {
		s.problem(w, http.StatusBadRequest, errors.New("task must be 1 to 2000 characters"))
		return
	}
	hits := []semanticHit{}
	vs, emb, ok := s.semanticStore()
	if !ok {
		writeJSON(w, 200, map[string]any{"enabled": false, "hits": hits})
		return
	}
	ranked, err := vs.Rank(r.Context(), emb, r.PathValue("repo"), in.Task, in.K)
	if err != nil {
		// Search by meaning is an extra: the caller falls back to names.
		s.logger.Warn("semantic ranking failed", "error", err)
		writeJSON(w, 200, map[string]any{"enabled": false, "hits": hits})
		return
	}
	for _, h := range ranked {
		hits = append(hits, semanticHit{ID: h.ID, Score: h.Score})
	}
	writeJSON(w, 200, map[string]any{"enabled": true, "hits": hits})
}
