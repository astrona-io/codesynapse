package api

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/astrona-io/codesynapse/internal/accounts"
	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/semantic"
	"github.com/astrona-io/codesynapse/internal/store"
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
	s.pruneVectors(ctx, vs, repo, g)
}

// Bounds of vector cleaning: one prune, delete or sweep run, and how often
// the sweep runs and how far it reads each time.
const (
	vectorCleanTimeout = 2 * time.Minute
	vectorSweepEvery   = 6 * time.Hour
	vectorSweepPages   = 20
)

// pruneVectors removes the vectors of symbols and descriptions g no longer
// has, once g's vectors are all there. It leaves them when another ingest
// published a newer graph in the meantime: that ingest prunes against its own.
func (s *Server) pruneVectors(ctx context.Context, vs semantic.Store, repo string, g *model.Graph) {
	id := model.NodeID(repo, model.KindRepository, repo)
	want, ok := g.Node(id)
	if !ok {
		return
	}
	if live, err := s.st.Node(ctx, id); err != nil || live.Head != want.Head || live.Time != want.Time {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, vectorCleanTimeout)
	defer cancel()
	st, err := vs.Prune(ctx, repo, g.Nodes())
	if err != nil {
		if !errors.Is(err, semantic.ErrNoSweeper) {
			s.logger.Warn("removing stale symbol vectors", "error", err, "removed", st.Removed)
		}
		return
	}
	if st.Removed > 0 || !st.Complete {
		s.logger.Info("stale symbol vectors removed", "scanned", st.Scanned, "removed", st.Removed, "complete", st.Complete)
	}
}

// codeVectors is the symbol vector index for cleaning, which needs no
// embedding model: vectors stay after a model is unconfigured.
func (s *Server) codeVectors() (semantic.Store, bool) {
	ix, ok := s.st.(semantic.Indexer)
	if !ok || ix.CodeVectors() == nil {
		return semantic.Store{}, false
	}
	return semantic.Store{Idx: ix.CodeVectors()}, true
}

// forgetVectors removes a deleted graph's symbol vectors. A failure is
// logged; sweepVectors removes them later.
func (s *Server) forgetVectors(ctx context.Context, repo string) {
	vs, ok := s.codeVectors()
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), vectorCleanTimeout)
	defer cancel()
	if err := vs.Forget(ctx, repo); err != nil && !errors.Is(err, semantic.ErrNoSweeper) {
		s.logger.Warn("removing the symbol vectors of a deleted repository; the sweep retries", "error", err)
	}
}

// sweepVectors removes, every few hours under the api-loops lease, the
// symbol vectors of graphs that no longer exist: deletes whose vector
// removal failed, and graphs deleted before vectors went with them. Each
// run reads a bounded number of pages and the next run carries on.
func (s *Server) sweepVectors(ctx context.Context) {
	vs, ok := s.codeVectors()
	if !ok {
		return
	}
	t := time.NewTicker(vectorSweepEvery)
	defer t.Stop()
	next := ""
	for {
		next = s.sweepVectorsOnce(ctx, vs, next)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (s *Server) sweepVectorsOnce(ctx context.Context, vs semantic.Store, after string) string {
	ctx, cancel := context.WithTimeout(ctx, vectorCleanTimeout)
	defer cancel()
	st, err := vs.Sweep(ctx, after, vectorSweepPages, s.graphExists)
	if err != nil {
		if !errors.Is(err, semantic.ErrNoSweeper) && ctx.Err() == nil {
			s.logger.Warn("sweeping symbol vectors", "error", err, "graphs", st.Graphs)
		}
		return st.Next
	}
	if st.Graphs > 0 {
		s.logger.Info("symbol vectors of deleted repositories removed", "graphs", st.Graphs, "scanned", st.Scanned)
	}
	return st.Next
}

// graphExists reports whether graph is a published repository graph.
func (s *Server) graphExists(ctx context.Context, graph string) (bool, error) {
	_, err := s.st.Node(ctx, model.NodeID(graph, model.KindRepository, graph))
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	return err == nil, err
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
