package memory

import (
	"errors"
	"slices"
	"strings"
)

// ErrFull is returned when an upsert would take a repository past
// MaxFactsPerRepo or MaxEntitiesPerRepo (history counts).
var ErrFull = errors.New("agent memory for this repository is full; delete stale facts first")

// Upserts run in three steps so both stores behave identically:
//
//  1. the store looks up which of b.Names() are aliases (AliasOf),
//  2. b.Resolve rewrites the batch onto canonical entities,
//  3. the store loads the State that b.Needs() and applies b.Plan(state).
//
// Only step 1 and the loading in step 3 touch the database.

// Names returns the normalised names an upsert may need to resolve.
func (b Batch) Names() []string {
	var out []string
	add := func(n string) {
		if n != "" && !slices.Contains(out, n) {
			out = append(out, n)
		}
	}
	for _, e := range b.Entities {
		add(normName(e.Name))
	}
	for _, a := range b.Aliases {
		add(a.Name)
	}
	return out
}

// Resolve rewrites entity and fact IDs onto canonical entities. aliasOf maps
// a normalised name to the entity ID it is an alias of.
func (b *Batch) Resolve(aliasOf map[string]string) {
	canon := map[string]string{}
	for _, e := range b.Entities {
		if c, ok := aliasOf[normName(e.Name)]; ok && c != e.ID {
			canon[e.ID] = c
		}
	}
	if len(canon) == 0 {
		return
	}
	id := func(s string) string {
		if c, ok := canon[s]; ok {
			return c
		}
		return s
	}
	ents := b.Entities[:0]
	for _, e := range b.Entities {
		if _, aliased := canon[e.ID]; !aliased {
			ents = append(ents, e)
		}
	}
	b.Entities = ents
	fix := func(fs []Fact) []Fact {
		seen := map[string]bool{}
		out := fs[:0]
		for _, f := range fs {
			f.Subject, f.Object = id(f.Subject), id(f.Object)
			f.ID = FactID(f.Subject, f.Predicate, f.Object)
			if !seen[f.ID] {
				seen[f.ID] = true
				out = append(out, f)
			}
		}
		return out
	}
	b.Facts = fix(b.Facts)
	b.Replace = fix(b.Replace)
	for i := range b.Aliases {
		b.Aliases[i].Canonical = id(b.Aliases[i].Canonical)
	}
}

// Needs lists what State must contain: entity IDs and the subjects whose
// facts the upsert may close or move.
func (b Batch) Needs() (entityIDs, factSubjects []string, factIDs []string) {
	add := func(list *[]string, v string) {
		if !slices.Contains(*list, v) {
			*list = append(*list, v)
		}
	}
	for _, e := range b.Entities {
		add(&entityIDs, e.ID)
	}
	for _, a := range b.Aliases {
		add(&entityIDs, a.EntityID)
		add(&entityIDs, a.Canonical)
		add(&factSubjects, a.EntityID) // facts in either direction
	}
	for _, r := range b.Replace {
		add(&factSubjects, r.Subject)
	}
	for _, f := range b.Facts {
		add(&factIDs, f.ID)
	}
	return entityIDs, factSubjects, factIDs
}

// State is what the store holds that an upsert touches: the entities from
// Needs, the facts with those IDs, and every fact touching the subjects
// (in either direction), plus the repository's totals.
type State struct {
	Entities    map[string]Entity
	Facts       map[string]Fact
	EntityCount int
	FactCount   int
}

// Plan is the set of writes an upsert makes. Puts are complete records.
type Plan struct {
	PutEntities    []Entity
	PutFacts       []Fact
	DeleteFacts    []string
	DeleteEntities []string
}

// Plan computes the writes for b against st, or ErrFull.
func (b Batch) Plan(st State) (Plan, error) {
	ents := map[string]Entity{}
	facts := map[string]Fact{}
	delFacts := map[string]bool{}
	var delEnts []string
	entity := func(id string) (Entity, bool) {
		if e, ok := ents[id]; ok {
			return e, true
		}
		e, ok := st.Entities[id]
		return e, ok
	}
	fact := func(id string) (Fact, bool) {
		if f, ok := facts[id]; ok {
			return f, true
		}
		f, ok := st.Facts[id]
		return f, ok && !delFacts[id]
	}

	// Entities from the batch.
	newEntities := 0
	for _, e := range b.Entities {
		if old, ok := entity(e.ID); ok {
			e.CreatedAt = old.CreatedAt
			e.Aliases = old.Aliases
			if e.Type == "Thing" {
				e.Type = old.Type
			}
		} else {
			newEntities++
		}
		ents[e.ID] = e
	}

	// Aliases: record the name on the canonical entity and merge an
	// existing alias entity into it, facts included.
	for _, a := range b.Aliases {
		c, ok := entity(a.Canonical)
		if !ok {
			continue // Prepare always adds the canonical entity
		}
		if !slices.Contains(c.Aliases, a.Name) {
			c.Aliases = append(slices.Clone(c.Aliases), a.Name)
		}
		c.UpdatedAt = b.At
		if dup, ok := entity(a.EntityID); ok && a.EntityID != a.Canonical {
			for _, n := range dup.Aliases {
				if !slices.Contains(c.Aliases, n) {
					c.Aliases = append(c.Aliases, n)
				}
			}
			delEnts = append(delEnts, a.EntityID)
			delete(ents, a.EntityID)
			for id, f := range st.Facts {
				if f.Subject != a.EntityID && f.Object != a.EntityID {
					continue
				}
				delFacts[id] = true
				if f.Subject == a.EntityID {
					f.Subject = a.Canonical
				}
				if f.Object == a.EntityID {
					f.Object = a.Canonical
				}
				f.ID = FactID(f.Subject, f.Predicate, f.Object)
				if into, ok := fact(f.ID); ok {
					f = combine(into, f)
				}
				facts[f.ID] = f
			}
		}
		ents[c.ID] = c
	}

	// Replace closes the other current facts of the same subject and predicate.
	for _, r := range b.Replace {
		for id, f := range st.Facts {
			if f.Subject == r.Subject && f.Predicate == r.Predicate && f.Current() && id != r.ID && !delFacts[id] {
				if cur, ok := facts[id]; ok {
					f = cur
				}
				f.ValidTo, f.UpdatedAt = b.At, b.At
				facts[id] = f
			}
		}
	}

	// Facts from the batch: confirm, reopen or create.
	newFacts := 0
	for _, f := range b.Facts {
		if old, ok := fact(f.ID); ok {
			f.CreatedAt = old.CreatedAt
			f.Confirmations = old.Confirmations + 1
			f.Confidence = max(old.Confidence, f.Confidence)
			if old.Current() {
				f.ValidFrom = old.ValidFrom
			} // else: reopened, valid from now
		} else {
			newFacts++
		}
		facts[f.ID] = f
	}

	// Moved facts whose new ID did not exist are new as well.
	for id := range facts {
		if _, existed := st.Facts[id]; !existed && !slices.ContainsFunc(b.Facts, func(f Fact) bool { return f.ID == id }) {
			newFacts++
		}
	}
	removedFacts := 0
	for id := range delFacts {
		if _, rewritten := facts[id]; !rewritten {
			removedFacts++
		}
	}
	if st.EntityCount+newEntities-len(delEnts) > MaxEntitiesPerRepo || st.FactCount+newFacts-removedFacts > MaxFactsPerRepo {
		return Plan{}, ErrFull
	}

	p := Plan{DeleteEntities: delEnts}
	for id := range delFacts {
		if _, rewritten := facts[id]; !rewritten {
			p.DeleteFacts = append(p.DeleteFacts, id)
		}
	}
	for _, e := range ents {
		p.PutEntities = append(p.PutEntities, e)
	}
	for _, f := range facts {
		p.PutFacts = append(p.PutFacts, f)
	}
	slices.SortFunc(p.PutEntities, func(x, y Entity) int { return strings.Compare(x.ID, y.ID) })
	slices.SortFunc(p.PutFacts, func(x, y Fact) int { return strings.Compare(x.ID, y.ID) })
	slices.Sort(p.DeleteFacts)
	return p, nil
}

// combine merges two records of the same fact (after an entity merge).
func combine(a, b Fact) Fact {
	out := a
	out.Confirmations = a.Confirmations + b.Confirmations
	out.Confidence = max(a.Confidence, b.Confidence)
	out.CreatedAt = min(a.CreatedAt, b.CreatedAt)
	out.ValidFrom = min(a.ValidFrom, b.ValidFrom)
	if b.UpdatedAt > a.UpdatedAt {
		out.UpdatedAt, out.Source, out.OriginSessionID = b.UpdatedAt, b.Source, b.OriginSessionID
	}
	if a.Current() || b.Current() {
		out.ValidTo = ""
	} else {
		out.ValidTo = max(a.ValidTo, b.ValidTo)
	}
	return out
}
