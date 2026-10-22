package memory

import (
	"cmp"
	"context"
	"fmt"
	"slices"

	agentmem "github.com/parisnakitakejser/codesynapse/internal/memory"
	"github.com/parisnakitakejser/codesynapse/internal/store"
)

type memRepo struct {
	entities map[string]agentmem.Entity
	facts    map[string]agentmem.Fact
}

// UpsertMemory implements store.MemoryStore.
func (s *Store) UpsertMemory(_ context.Context, b agentmem.Batch) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.mem[b.Repo]
	if m == nil {
		m = &memRepo{entities: map[string]agentmem.Entity{}, facts: map[string]agentmem.Fact{}}
	}

	// Work out the effect first so a full memory changes nothing.
	keep := map[string]bool{}
	for _, f := range b.Replace {
		keep[f.ID] = true
	}
	removed := map[string]bool{}
	for _, r := range b.Replace {
		for id, f := range m.facts {
			if f.Subject == r.Subject && f.Predicate == r.Predicate && !keep[id] {
				removed[id] = true
			}
		}
	}
	newEntities, newFacts := 0, 0
	for _, e := range b.Entities {
		if _, ok := m.entities[e.ID]; !ok {
			newEntities++
		}
	}
	for _, f := range b.Facts {
		if _, ok := m.facts[f.ID]; !ok {
			newFacts++
		}
	}
	if len(m.entities)+newEntities > agentmem.MaxEntitiesPerRepo || len(m.facts)-len(removed)+newFacts > agentmem.MaxFactsPerRepo {
		return store.ErrMemoryFull
	}

	s.mem[b.Repo] = m
	for id := range removed {
		delete(m.facts, id)
	}
	for _, e := range b.Entities {
		if old, ok := m.entities[e.ID]; ok {
			e.CreatedAt = old.CreatedAt
			if e.Type == "Thing" {
				e.Type = old.Type
			}
		}
		m.entities[e.ID] = e
	}
	for _, f := range b.Facts {
		if old, ok := m.facts[f.ID]; ok {
			f.CreatedAt = old.CreatedAt
			f.Confirmations = old.Confirmations + 1
		}
		m.facts[f.ID] = f
	}
	return nil
}

// SearchMemory implements store.MemoryStore.
func (s *Store) SearchMemory(_ context.Context, repo, query string, limit int) (agentmem.Graph, error) {
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
	g.Facts = m.factsTouching(matched, min(limit*5, store.MaxLimit))
	g.Entities = append(g.Entities, m.endpoints(g.Facts, matched)...)
	return g, nil
}

// MemorySubgraph implements store.MemoryStore.
func (s *Store) MemorySubgraph(_ context.Context, repo string, ids []string, depth, limit int) (agentmem.Graph, error) {
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
		for _, f := range m.factsTouching(reached, len(m.facts)) {
			reached[f.Subject], reached[f.Object] = true, true
		}
	}
	g.Facts = m.factsTouching(reached, limit)
	g.Entities = m.endpoints(g.Facts, nil)
	return g, nil
}

// factsTouching returns facts with an endpoint in ids, most recent first.
func (m *memRepo) factsTouching(ids map[string]bool, limit int) []agentmem.Fact {
	out := []agentmem.Fact{}
	for _, f := range m.facts {
		if ids[f.Subject] || ids[f.Object] {
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
	for fid, f := range m.facts {
		if f.Subject == id || f.Object == id {
			delete(m.facts, fid)
		}
	}
	return nil
}
