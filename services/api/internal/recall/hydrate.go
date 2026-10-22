package recall

import (
	"encoding/json"
	"math"
	"slices"
	"unicode/utf8"

	"github.com/astrona-io/codesynapse/internal/memory"
	"github.com/astrona-io/codesynapse/internal/vector"
)

// Search metadata may lag deletion or alias merging. Only current, scoped
// graph records can supply the entities and facts returned to the caller.
func candidates(repo string, hits []memory.Scored, limit int) []memory.Scored {
	out := []memory.Scored{}
	seen := map[string]bool{}
	for _, h := range hits[:min(len(hits), limit)] {
		if !validEntity(repo, h.Entity) || seen[h.ID] || math.IsNaN(h.Score) || math.IsInf(h.Score, 0) || h.Score < 0 {
			continue
		}
		seen[h.ID] = true
		out = append(out, h)
	}
	return out
}

func validEntity(repo string, e memory.Entity) bool {
	if e.Repo != repo || e.Name == "" || len(e.Name) > 4*memory.MaxNameLen || utf8.RuneCountInString(e.Name) > memory.MaxNameLen ||
		e.ID != memory.EntityID(repo, e.Name) || len(e.Type) > 40 || len(e.CreatedAt) > 64 || len(e.UpdatedAt) > 64 {
		return false
	}
	return true
}

func hydrate(res Result, graph memory.Graph, legs map[string][]memory.Scored, repo, query string, depth, limit int, history bool) Result {
	const maxEntities = 2*maxFacts + lexicalCandidates + vector.MaxK
	known := map[string]memory.Entity{}
	for _, e := range graph.Entities[:min(len(graph.Entities), maxEntities)] {
		if validEntity(repo, e) {
			known[e.ID] = e
		}
	}
	currentLegs := map[string][]memory.Scored{}
	for leg, hits := range legs {
		for _, hit := range hits {
			e, ok := known[hit.ID]
			if !ok || leg == "name" && len(memory.RankBM25([]memory.Entity{e}, query, 1)) == 0 {
				continue
			}
			currentLegs[leg] = append(currentLegs[leg], memory.Scored{Entity: e, Score: hit.Score})
		}
	}
	current := memory.FuseRRF(lexicalCandidates+vector.MaxK, currentLegs)
	slices.SortStableFunc(current, func(a, b memory.Fused) int {
		if memory.ExactMatch(a.Entity, query) != memory.ExactMatch(b.Entity, query) {
			if memory.ExactMatch(a.Entity, query) {
				return -1
			}
			return 1
		}
		return 0
	})
	res.Truncated = res.Truncated || len(current) > entryLimit || len(graph.Facts) >= limit
	reached := map[string]bool{}
	for _, f := range current[:min(len(current), entryLimit)] {
		e, omitted := boundedAliases(f.Entity, query)
		via := f.Legs[0]
		if len(f.Legs) > 1 {
			via = "both"
		}
		res.Entries = append(res.Entries, Entry{Entity: e, Score: f.Score, Via: via})
		res.Entities = append(res.Entities, e)
		if !fits(res) {
			res.Entries = res.Entries[:len(res.Entries)-1]
			res.Entities = res.Entities[:len(res.Entities)-1]
			res.Truncated = true
			continue
		}
		res.Truncated = res.Truncated || omitted
		reached[e.ID] = true
	}
	facts := []memory.Fact{}
	seen := map[string]bool{}
	for _, f := range graph.Facts[:min(len(graph.Facts), maxFacts)] {
		if f.Repo != repo || f.ID != memory.FactID(f.Subject, f.Predicate, f.Object) || seen[f.ID] ||
			known[f.Subject].ID == "" || known[f.Object].ID == "" || !history && !f.Current() ||
			len(f.Source) > 240 || len(f.CreatedAt) > 64 || len(f.UpdatedAt) > 64 || len(f.ValidFrom) > 64 || len(f.ValidTo) > 64 ||
			!memory.ValidPredicate(f.Predicate) ||
			math.IsNaN(f.Confidence) || math.IsInf(f.Confidence, 0) || f.Confidence < 0 || f.Confidence > 1 {
			continue
		}
		seen[f.ID] = true
		facts = append(facts, f)
	}
	added := map[string]bool{}
	for range depth {
		next := map[string]bool{}
		for _, f := range facts {
			if added[f.ID] || !reached[f.Subject] && !reached[f.Object] {
				continue
			}
			if len(res.Facts) == limit {
				res.Truncated = true
				continue
			}
			before := len(res.Entities)
			for _, id := range []string{f.Subject, f.Object} {
				if !slices.ContainsFunc(res.Entities, func(e memory.Entity) bool { return e.ID == id }) {
					e, omitted := boundedAliases(known[id], query)
					res.Entities = append(res.Entities, e)
					res.Truncated = res.Truncated || omitted
				}
			}
			res.Facts = append(res.Facts, f)
			if !fits(res) {
				res.Entities = res.Entities[:before]
				res.Facts = res.Facts[:len(res.Facts)-1]
				res.Truncated = true
				continue
			}
			added[f.ID] = true
			next[f.Subject], next[f.Object] = true, true
		}
		for id := range next {
			reached[id] = true
		}
	}
	if len(graph.Entities) > maxEntities || len(graph.Facts) > maxFacts {
		res.Truncated = true
	}
	return res
}

// Alias merges can accumulate many names. Keep current evidence for ranking,
// then bound the emitted metadata without losing the alias queried exactly.
func boundedAliases(e memory.Entity, query string) (memory.Entity, bool) {
	const limit = 64
	aliases := make([]string, 0, min(len(e.Aliases), limit))
	exact := false
	omitted := false
	for _, a := range e.Aliases {
		if len(a) > 4*memory.MaxNameLen || utf8.RuneCountInString(a) > memory.MaxNameLen {
			omitted = true
			continue
		}
		matches := memory.ExactMatch(memory.Entity{Name: a}, query)
		if len(aliases) < limit {
			aliases = append(aliases, a)
			exact = exact || matches
		} else {
			omitted = true
			if matches && !exact {
				aliases[len(aliases)-1] = a
				exact = true
			}
		}
	}
	e.Aliases = aliases
	return e, omitted
}

func fits(res Result) bool {
	// Reserve room for the truncation flag if a later record exceeds the cap.
	res.Truncated = true
	b, err := json.Marshal(res)
	return err == nil && len(b) <= maxResponseBytes
}
