package recall

import (
	"context"
	"errors"
	"time"

	"github.com/astrona-io/codesynapse/internal/memory"
	"github.com/astrona-io/codesynapse/internal/sessionmemory"
	"github.com/astrona-io/codesynapse/internal/store"
	"github.com/astrona-io/codesynapse/internal/telemetry"
	"github.com/astrona-io/codesynapse/internal/vector"
	"github.com/astrona-io/codesynapse/services/api/internal/llm"
)

// RecallSession never falls back to repository memory, including on model failure.
func (s *Service) RecallSession(ctx context.Context, scope sessionmemory.Scope, q Query) (Result, error) {
	if err := scope.Check(ctx); err != nil {
		return Result{}, err
	}
	ctx = telemetry.WithTenant(ctx, scope.TenantID)
	telemetry.AgentMemoryRecalls.IncContext(ctx, telemetry.Tenant(ctx))
	res, err := s.recallSession(ctx, scope, q)
	if err == nil {
		telemetry.MemoryRecalls.IncContext(ctx, telemetry.Tenant(ctx), res.Mode)
	}
	return res, err
}

func (s *Service) recallSession(ctx context.Context, scope sessionmemory.Scope, q Query) (Result, error) {
	res := Result{Mode: "names", Entries: []Entry{}, Graph: sessionmemory.EmptyGraph()}
	if err := scope.Check(ctx); err != nil {
		return res, err
	}
	if len(q.Text) > maxQueryBytes {
		return res, sessionmemory.ErrLimit
	}
	ctx, cancel := context.WithTimeout(ctx, recallTimeout)
	defer cancel()
	st, ok := s.St.(store.SessionMemoryStore)
	if !ok {
		return res, errors.New("session memory is unavailable")
	}
	if why := memory.RecallGate(q.Text); why != "" {
		res.Skipped = why
		return res, nil
	}
	g, err := st.ReadSessionMemory(ctx, scope, "", sessionmemory.MaxFacts)
	if err != nil {
		return res, err
	}
	legs := map[string][]memory.Scored{"name": memory.RankBM25(g.Entities, q.Text, lexicalCandidates)}
	if hits, err := s.sessionNearest(ctx, st, scope, q.Text); err == nil {
		res.Mode = "hybrid"
		legs["vector"] = hits
	}
	if err := ctx.Err(); err != nil {
		return res, err
	}
	// A second graph read rejects entities deleted or renamed during vector search.
	g, err = st.ReadSessionMemory(ctx, scope, "", sessionmemory.MaxFacts)
	if err != nil {
		return res, err
	}
	fused := memory.FuseRRF(lexicalCandidates+vector.MaxK, legs)
	roots := make(map[string]bool, len(fused))
	for _, hit := range fused {
		roots[hit.ID] = true
	}
	depth, limit := min(max(q.Depth, 1), 2), min(store.ClampLimit(q.Limit, 40), maxFacts)
	sub, truncated := sessionSubgraph(g, roots, depth, limit)
	res.Truncated = truncated || len(legs["name"]) >= lexicalCandidates || len(legs["vector"]) >= vector.MaxK
	return hydrate(res, sub, legs, scope.Key(), q.Text, depth, limit, false), nil
}

func (s *Service) sessionNearest(ctx context.Context, st store.SessionMemoryStore, scope sessionmemory.Scope, query string) ([]memory.Scored, error) {
	if s.LLM == nil {
		return nil, llm.ErrNoEmbeddings
	}
	name, cfg, err := s.LLM.EmbeddingProvider()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, vectorTimeout)
	defer cancel()
	vecs, err := s.LLM.Embed(ctx, name, []string{query})
	if err != nil {
		return nil, err
	}
	if len(vecs) != 1 {
		return nil, errors.New("embedding response has the wrong vector count")
	}
	if err := memory.CheckVectors(map[string][]float32{"query": vecs[0]}); err != nil {
		return nil, err
	}
	hits, err := st.SessionMemoryNearest(ctx, scope, cfg.EmbeddingModel, vecs[0], vector.MaxK)
	if err != nil {
		return nil, err
	}
	out := []memory.Scored{}
	for _, hit := range candidates(scope.Key(), hits, vector.MaxK) {
		if hit.Score >= MinScore && hit.Score <= 1 {
			out = append(out, hit)
		}
	}
	return out, nil
}

// IndexSession runs under the writing request or worker's inherited identity.
func (s *Service) IndexSession(ctx context.Context, scope sessionmemory.Scope, authorize func(context.Context) error) error {
	if err := scope.Check(ctx); err != nil {
		return err
	}
	if authorize == nil {
		return errors.New("session indexing requires an authorization check")
	}
	if err := authorize(ctx); err != nil {
		return err
	}
	if s.LLM == nil {
		return llm.ErrNoEmbeddings
	}
	name, cfg, err := s.LLM.EmbeddingProvider()
	if err != nil {
		return err
	}
	st, ok := s.St.(store.SessionMemoryStore)
	if !ok {
		return errors.New("session memory is unavailable")
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	g, err := st.ReadSessionMemory(ctx, scope, "", sessionmemory.MaxFacts)
	if err != nil {
		return err
	}
	for start := 0; start < len(g.Entities); start += embedBatch {
		batch := g.Entities[start:min(start+embedBatch, len(g.Entities))]
		names := make([]string, len(batch))
		for i, e := range batch {
			names[i] = e.Name
		}
		vecs, err := s.LLM.Embed(ctx, name, names)
		if err != nil {
			return err
		}
		if len(vecs) != len(batch) {
			return errors.New("embedding response has the wrong vector count")
		}
		values := map[string][]float32{}
		for i, e := range batch {
			values[e.ID] = vecs[i]
		}
		if err := memory.CheckVectors(values); err != nil {
			return err
		}
		if err := authorize(ctx); err != nil {
			return err
		}
		if err := st.SetSessionMemoryEmbeddings(ctx, scope, cfg.EmbeddingModel, values); err != nil {
			return err
		}
	}
	return nil
}

func sessionSubgraph(g memory.Graph, roots map[string]bool, depth, limit int) (memory.Graph, bool) {
	out := sessionmemory.EmptyGraph()
	reached := map[string]bool{}
	for _, e := range g.Entities {
		if roots[e.ID] {
			reached[e.ID] = true
		}
	}
	seen := map[string]bool{}
	truncated := false
	for range depth {
		next := map[string]bool{}
		for _, f := range g.Facts {
			if seen[f.ID] || !f.Current() || !reached[f.Subject] && !reached[f.Object] {
				continue
			}
			if len(out.Facts) == limit {
				truncated = true
				continue
			}
			out.Facts = append(out.Facts, f)
			seen[f.ID] = true
			next[f.Subject], next[f.Object] = true, true
		}
		for id := range next {
			reached[id] = true
		}
	}
	for _, e := range g.Entities {
		if reached[e.ID] {
			out.Entities = append(out.Entities, e)
		}
	}
	return out, truncated
}
