package memory

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"unicode/utf8"

	agentmem "github.com/astrona-io/codesynapse/internal/memory"
	"github.com/astrona-io/codesynapse/internal/store"
	"github.com/astrona-io/codesynapse/internal/vector"
)

type memRepo struct {
	entities map[string]agentmem.Entity
	facts    map[string]agentmem.Fact
}

// UpsertMemory implements store.MemoryStore.
func (s *Store) UpsertMemory(ctx context.Context, b agentmem.Batch) error {
	b = b.StampOrigin(ctx)
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.mem[b.Repo]
	if m == nil {
		m = &memRepo{entities: map[string]agentmem.Entity{}, facts: map[string]agentmem.Fact{}}
	}
	aliasOf := map[string]string{}
	names := b.Names()
	for id, e := range m.entities {
		for _, a := range e.Aliases {
			if slices.Contains(names, a) {
				aliasOf[a] = id
			}
		}
	}
	b.Resolve(aliasOf)

	entityIDs, subjects, factIDs := b.Needs()
	st := agentmem.State{Entities: map[string]agentmem.Entity{}, Facts: map[string]agentmem.Fact{}, EntityCount: len(m.entities), FactCount: len(m.facts)}
	for _, id := range entityIDs {
		if e, ok := m.entities[id]; ok {
			st.Entities[id] = e
		}
	}
	for id, f := range m.facts {
		if slices.Contains(factIDs, id) || slices.Contains(subjects, f.Subject) || slices.Contains(subjects, f.Object) {
			st.Facts[id] = f
		}
	}
	p, err := b.Plan(st)
	if err != nil {
		return err
	}
	s.mem[b.Repo] = m
	for _, id := range p.DeleteFacts {
		delete(m.facts, id)
	}
	for _, e := range p.PutEntities {
		e.Aliases = slices.Clone(e.Aliases)
		m.entities[e.ID] = e
	}
	for _, f := range p.PutFacts {
		m.facts[f.ID] = f
	}
	for _, id := range p.DeleteEntities {
		delete(m.entities, id)
		delete(s.vecs, id)
	}
	return nil
}

// SearchMemory implements store.MemoryStore.
func (s *Store) SearchMemory(_ context.Context, repo, query string, limit int, history bool) (agentmem.Graph, error) {
	limit = store.ClampLimit(limit, 20)
	words := agentmem.Words(query)
	s.mu.RLock()
	defer s.mu.RUnlock()
	g := agentmem.Graph{Notice: agentmem.Notice, Entities: []agentmem.Entity{}, Facts: []agentmem.Fact{}}
	m := s.mem[repo]
	if m == nil {
		return g, nil
	}
	type scored struct {
		e     agentmem.Entity
		score int
	}
	var hits []scored
	for _, e := range m.entities {
		if sc := agentmem.Score(e, words); sc > 0 || len(words) == 0 {
			hits = append(hits, scored{e, sc})
		}
	}
	slices.SortFunc(hits, func(a, b scored) int {
		return cmp.Or(cmp.Compare(b.score, a.score), cmp.Compare(b.e.UpdatedAt, a.e.UpdatedAt), cmp.Compare(a.e.Name, b.e.Name))
	})
	matched := map[string]bool{}
	for _, h := range hits[:min(len(hits), limit)] {
		matched[h.e.ID] = true
		g.Entities = append(g.Entities, h.e)
	}
	g.Facts = m.factsTouching(matched, min(limit*5, store.MaxLimit), history)
	g.Entities = append(g.Entities, m.endpoints(g.Facts, matched)...)
	return g, nil
}

// MemorySubgraph implements store.MemoryStore.
func (s *Store) MemorySubgraph(ctx context.Context, repo string, ids []string, depth, limit int, history bool) (agentmem.Graph, error) {
	if err := ctx.Err(); err != nil {
		return agentmem.Graph{}, err
	}
	if len(ids) > store.MaxLimit {
		return agentmem.Graph{}, fmt.Errorf("memory roots exceed %d", store.MaxLimit)
	}
	limit = store.ClampLimit(limit, 50)
	depth = min(max(depth, 1), 2)
	s.mu.RLock()
	defer s.mu.RUnlock()
	g := agentmem.Graph{Notice: agentmem.Notice, Entities: []agentmem.Entity{}, Facts: []agentmem.Fact{}}
	m := s.mem[repo]
	if m == nil {
		return g, nil
	}
	reached := map[string]bool{}
	for _, id := range ids {
		if _, ok := m.entities[id]; ok {
			reached[id] = true
		}
	}
	if depth == 2 {
		for _, f := range m.factsTouching(reached, len(m.facts), history) {
			reached[f.Subject], reached[f.Object] = true, true
		}
	}
	g.Facts = m.factsTouching(reached, limit, history)
	g.Entities = m.endpoints(g.Facts, nil)
	seen := map[string]bool{}
	for _, e := range g.Entities {
		seen[e.ID] = true
	}
	for _, id := range ids {
		if e, ok := m.entities[id]; ok && !seen[id] {
			g.Entities = append(g.Entities, e)
			seen[id] = true
		}
	}
	slices.SortFunc(g.Entities, func(a, b agentmem.Entity) int {
		return cmp.Or(cmp.Compare(a.Name, b.Name), cmp.Compare(a.ID, b.ID))
	})
	return g, nil
}

// factsTouching returns facts with an endpoint in ids, most recent first.
func (m *memRepo) factsTouching(ids map[string]bool, limit int, history bool) []agentmem.Fact {
	out := []agentmem.Fact{}
	for _, f := range m.facts {
		if (ids[f.Subject] || ids[f.Object]) && (history || f.Current()) {
			out = append(out, f)
		}
	}
	slices.SortFunc(out, func(a, b agentmem.Fact) int {
		return cmp.Or(cmp.Compare(b.UpdatedAt, a.UpdatedAt), cmp.Compare(a.ID, b.ID))
	})
	return out[:min(len(out), limit)]
}

// endpoints returns the entities facts refer to, except those in skip, by name.
func (m *memRepo) endpoints(facts []agentmem.Fact, skip map[string]bool) []agentmem.Entity {
	seen := map[string]bool{}
	var out []agentmem.Entity
	for _, f := range facts {
		for _, id := range []string{f.Subject, f.Object} {
			if !seen[id] && !skip[id] {
				seen[id] = true
				out = append(out, m.entities[id])
			}
		}
	}
	slices.SortFunc(out, func(a, b agentmem.Entity) int { return cmp.Or(cmp.Compare(a.Name, b.Name), cmp.Compare(a.ID, b.ID)) })
	return out
}

// DeleteMemoryFact implements store.MemoryStore.
func (s *Store) DeleteMemoryFact(_ context.Context, repo, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.mem[repo]
	if m == nil {
		return fmt.Errorf("fact %q: %w", id, store.ErrNotFound)
	}
	if _, ok := m.facts[id]; !ok {
		return fmt.Errorf("fact %q: %w", id, store.ErrNotFound)
	}
	delete(m.facts, id)
	return nil
}

// DeleteMemoryEntity implements store.MemoryStore.
func (s *Store) DeleteMemoryEntity(_ context.Context, repo, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.mem[repo]
	if m == nil {
		return fmt.Errorf("entity %q: %w", id, store.ErrNotFound)
	}
	if _, ok := m.entities[id]; !ok {
		return fmt.Errorf("entity %q: %w", id, store.ErrNotFound)
	}
	delete(m.entities, id)
	delete(s.vecs, id)
	for fid, f := range m.facts {
		if f.Subject == id || f.Object == id {
			delete(m.facts, fid)
		}
	}
	return nil
}

// MemoryStats implements store.MemoryStore.
func (s *Store) MemoryStats(context.Context) ([]agentmem.Stats, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := []agentmem.Stats{}
	for repo, m := range s.mem {
		if len(m.entities) == 0 && len(m.facts) == 0 {
			continue
		}
		st := agentmem.Stats{Repo: repo, Entities: len(m.entities), Sources: map[string]int{}}
		for _, e := range m.entities {
			st.LastUpdated = max(st.LastUpdated, e.UpdatedAt)
		}
		for _, f := range m.facts {
			if !f.Current() {
				st.History++
				continue
			}
			st.Facts++
			st.Confirmations += f.Confirmations
			st.FactChars += utf8.RuneCountInString(m.entities[f.Subject].Name) + utf8.RuneCountInString(f.Predicate) + utf8.RuneCountInString(m.entities[f.Object].Name)
			st.Sources[f.Source]++
			st.LastUpdated = max(st.LastUpdated, f.UpdatedAt)
		}
		out = append(out, st)
	}
	slices.SortFunc(out, func(a, b agentmem.Stats) int { return cmp.Compare(a.Repo, b.Repo) })
	return out, nil
}

// embedding is an entity's vector and the model that made it.
type embedding struct {
	model string
	vec   []float32
}

// MemoryEmbeddingTodo implements store.MemoryStore.
func (s *Store) MemoryEmbeddingTodo(_ context.Context, model string, limit int) ([]agentmem.Entity, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []agentmem.Entity
	for _, m := range s.mem {
		for id, e := range m.entities {
			if v, ok := s.vecs[id]; !ok || v.model != model {
				out = append(out, e)
			}
		}
	}
	slices.SortFunc(out, func(a, b agentmem.Entity) int {
		return cmp.Or(cmp.Compare(a.UpdatedAt, b.UpdatedAt), cmp.Compare(a.ID, b.ID))
	})
	return out[:min(len(out), store.ClampLimit(limit, 64))], nil
}

// SetMemoryEmbeddings implements store.MemoryStore.
func (s *Store) SetMemoryEmbeddings(_ context.Context, model string, vecs map[string][]float32) error {
	if err := agentmem.CheckVectors(vecs); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, v := range vecs {
		s.vecs[id] = embedding{model: model, vec: slices.Clone(v)}
	}
	return nil
}

// MemoryLexical implements store.MemoryStore.
func (s *Store) MemoryLexical(_ context.Context, repo, query string, k int) ([]agentmem.Scored, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	m := s.mem[repo]
	if m == nil {
		return []agentmem.Scored{}, nil
	}
	ents := make([]agentmem.Entity, 0, len(m.entities))
	for _, e := range m.entities {
		ents = append(ents, e)
	}
	return agentmem.RankBM25(ents, query, vector.ClampK(k)), nil
}

// MemoryNearest implements store.MemoryStore (exact cosine search).
func (s *Store) MemoryNearest(_ context.Context, repo, model string, vec []float32, k int) ([]agentmem.Scored, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := []agentmem.Scored{}
	m := s.mem[repo]
	if m == nil {
		return out, nil
	}
	for id, e := range m.entities {
		if v, ok := s.vecs[id]; ok && v.model == model && len(v.vec) == len(vec) {
			out = append(out, agentmem.Scored{Entity: e, Score: agentmem.Similarity(vec, v.vec)})
		}
	}
	slices.SortFunc(out, func(a, b agentmem.Scored) int { return cmp.Or(cmp.Compare(b.Score, a.Score), cmp.Compare(a.ID, b.ID)) })
	return out[:min(len(out), vector.ClampK(k))], nil
}
