package neo4jstore

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/neo4j/neo4j-go-driver/v5/neo4j"

	"github.com/astrona-io/codesynapse/internal/memory"
	"github.com/astrona-io/codesynapse/internal/store"
	"github.com/astrona-io/codesynapse/internal/store/nodeprops"
)

// Agent memory lives in :MemoryEntity nodes joined by :FACT relationships.
// Neither carries the :Node label, so ReplaceRepo and DeleteRepo never touch
// it. The predicate is a property, not the relationship type: agent text only
// ever reaches Cypher as a parameter. What an upsert writes is computed by
// memory.Batch.Plan, shared with the in-memory store.

const factProj = `f.id AS id, f.repo AS repo, a.id AS subject, f.predicate AS predicate, b.id AS object, f.source AS source, f.origin_session_id AS origin_session_id,
  f.confirmations AS confirmations, f.confidence AS confidence, f.valid_from AS valid_from, f.valid_to AS valid_to,
  f.created_at AS created_at, f.updated_at AS updated_at`

const entityProj = "e{.id, .repo, .name, .type, .aliases, .created_at, .updated_at} AS e"

// current keeps facts that were not replaced, unless $history is set.
const current = "($history OR coalesce(f.valid_to, '') = '')"

// UpsertMemory implements store.MemoryStore.
func (s *Store) UpsertMemory(ctx context.Context, b memory.Batch) error {
	b = b.StampOrigin(ctx)
	defer observe(ctx, "write", time.Now())
	sess := s.driver.NewSession(ctx, neo4j.SessionConfig{DatabaseName: s.db, AccessMode: neo4j.AccessModeWrite})
	defer sess.Close(ctx) //nolint:errcheck // nothing to do on close failure
	_, err := sess.ExecuteWrite(ctx, func(tx neo4j.ManagedTransaction) (any, error) {
		rows := func(q string, p map[string]any) ([]*neo4j.Record, error) {
			res, err := tx.Run(ctx, q, p)
			if err != nil {
				return nil, err
			}
			return res.Collect(ctx)
		}
		recs, err := rows(`MATCH (e:MemoryEntity {repo: $repo}) WHERE any(x IN coalesce(e.aliases, []) WHERE x IN $names)
UNWIND e.aliases AS name WITH e, name WHERE name IN $names RETURN name, e.id AS id`, map[string]any{"repo": b.Repo, "names": b.Names()})
		if err != nil {
			return nil, err
		}
		aliasOf := map[string]string{}
		for _, r := range recs {
			aliasOf[str(r, "name")] = str(r, "id")
		}
		b.Resolve(aliasOf)

		entityIDs, subjects, factIDs := b.Needs()
		st := memory.State{Entities: map[string]memory.Entity{}, Facts: map[string]memory.Fact{}}
		p := map[string]any{"repo": b.Repo, "ids": entityIDs, "subjects": subjects, "fact_ids": factIDs, "history": true}
		for q, dst := range map[string]*int{
			"MATCH (e:MemoryEntity {repo: $repo}) RETURN count(e) AS n": &st.EntityCount,
			"MATCH ()-[f:FACT {repo: $repo}]->() RETURN count(f) AS n":  &st.FactCount,
		} {
			recs, err := rows(q, p)
			if err != nil {
				return nil, err
			}
			n, _ := recs[0].AsMap()["n"].(int64)
			*dst = int(n)
		}
		if recs, err = rows("MATCH (e:MemoryEntity) WHERE e.id IN $ids RETURN "+entityProj, p); err != nil {
			return nil, err
		}
		for _, r := range recs {
			e := toEntity(r)
			st.Entities[e.ID] = e
		}
		if recs, err = rows(`MATCH (a:MemoryEntity)-[f:FACT {repo: $repo}]->(b:MemoryEntity)
WHERE f.id IN $fact_ids OR a.id IN $subjects OR b.id IN $subjects RETURN `+factProj, p); err != nil {
			return nil, err
		}
		for _, r := range recs {
			f := toFact(r)
			st.Facts[f.ID] = f
		}

		plan, err := b.Plan(st)
		if err != nil {
			return nil, err
		}
		entities := make([]map[string]any, len(plan.PutEntities))
		for i, e := range plan.PutEntities {
			aliases := e.Aliases
			if aliases == nil {
				aliases = []string{}
			}
			entities[i] = map[string]any{"id": e.ID, "repo": e.Repo, "name": e.Name, "type": e.Type, "aliases": aliases,
				"created_at": e.CreatedAt, "updated_at": e.UpdatedAt}
		}
		facts := make([]map[string]any, len(plan.PutFacts))
		for i, f := range plan.PutFacts {
			facts[i] = map[string]any{"id": f.ID, "repo": f.Repo, "subject": f.Subject, "predicate": f.Predicate, "object": f.Object,
				"source": f.Source, "origin_session_id": f.OriginSessionID, "confirmations": f.Confirmations, "confidence": f.Confidence, "valid_from": f.ValidFrom,
				"valid_to": f.ValidTo, "created_at": f.CreatedAt, "updated_at": f.UpdatedAt}
		}
		w := map[string]any{"del_facts": plan.DeleteFacts, "del_entities": plan.DeleteEntities, "entities": entities, "facts": facts}
		for _, q := range []string{
			"MATCH ()-[f:FACT]->() WHERE f.id IN $del_facts DELETE f",
			`UNWIND $entities AS e MERGE (n:MemoryEntity {id: e.id})
SET n.repo = e.repo, n.name = e.name, n.type = e.type, n.aliases = e.aliases, n.created_at = e.created_at, n.updated_at = e.updated_at`,
			`UNWIND $facts AS f
MATCH (s:MemoryEntity {id: f.subject}), (o:MemoryEntity {id: f.object})
MERGE (s)-[r:FACT {id: f.id}]->(o)
SET r.repo = f.repo, r.predicate = f.predicate, r.source = f.source, r.confirmations = f.confirmations,
  r.origin_session_id = f.origin_session_id, r.confidence = f.confidence, r.valid_from = f.valid_from, r.valid_to = f.valid_to, r.created_at = f.created_at, r.updated_at = f.updated_at`,
			"MATCH (e:MemoryEntity) WHERE e.id IN $del_entities DETACH DELETE e",
		} {
			if _, err := tx.Run(ctx, q, w); err != nil {
				return nil, err
			}
		}
		return nil, nil
	})
	failed(ctx, "write", err)
	return err
}

// SearchMemory implements store.MemoryStore.
func (s *Store) SearchMemory(ctx context.Context, repo, query string, limit int, history bool) (memory.Graph, error) {
	limit = store.ClampLimit(limit, 20)
	res, err := s.read(ctx, `MATCH (e:MemoryEntity {repo: $repo})
WITH e, size([w IN $words WHERE toLower(e.name) CONTAINS w OR toLower(e.type) = w
  OR any(x IN coalesce(e.aliases, []) WHERE x CONTAINS w)]) AS score
WHERE score > 0 OR size($words) = 0
RETURN `+entityProj+` ORDER BY score DESC, e.updated_at DESC, e.name ASC LIMIT $limit`,
		map[string]any{"repo": repo, "words": memory.Words(query), "limit": limit})
	if err != nil {
		return memory.Graph{}, err
	}
	g := memory.Graph{Notice: memory.Notice, Entities: []memory.Entity{}, Facts: []memory.Fact{}}
	var ids []string
	matched := map[string]bool{}
	for _, rec := range res.Records {
		e := toEntity(rec)
		g.Entities = append(g.Entities, e)
		ids = append(ids, e.ID)
		matched[e.ID] = true
	}
	if g.Facts, err = s.factsTouching(ctx, repo, ids, min(limit*5, store.MaxLimit), history); err != nil {
		return g, err
	}
	more, err := s.endpoints(ctx, repo, g.Facts, matched)
	g.Entities = append(g.Entities, more...)
	return g, err
}

// MemorySubgraph implements store.MemoryStore.
func (s *Store) MemorySubgraph(ctx context.Context, repo string, ids []string, depth, limit int, history bool) (memory.Graph, error) {
	if len(ids) > store.MaxLimit {
		return memory.Graph{}, fmt.Errorf("memory roots exceed %d", store.MaxLimit)
	}
	roots := slices.Clone(ids)
	ids = slices.Clone(ids)
	limit = store.ClampLimit(limit, 50)
	g := memory.Graph{Notice: memory.Notice, Entities: []memory.Entity{}, Facts: []memory.Fact{}}
	if min(max(depth, 1), 2) == 2 {
		res, err := s.read(ctx, `MATCH (a:MemoryEntity {repo: $repo})-[f:FACT {repo: $repo}]-(b:MemoryEntity {repo: $repo})
WHERE a.id IN $ids AND `+current+` RETURN DISTINCT b.id AS id ORDER BY id LIMIT $max`, map[string]any{"repo": repo, "ids": ids, "history": history, "max": memory.MaxEntitiesPerRepo})
		if err != nil {
			return g, err
		}
		for _, rec := range res.Records {
			ids = append(ids, str(rec, "id"))
		}
	}
	var err error
	if g.Facts, err = s.factsTouching(ctx, repo, ids, limit, history); err != nil {
		return g, err
	}
	g.Entities, err = s.endpoints(ctx, repo, g.Facts, nil, roots...)
	return g, err
}

func (s *Store) factsTouching(ctx context.Context, repo string, ids []string, limit int, history bool) ([]memory.Fact, error) {
	out := []memory.Fact{}
	if len(ids) == 0 {
		return out, nil
	}
	res, err := s.read(ctx, `MATCH (a:MemoryEntity {repo: $repo})-[f:FACT {repo: $repo}]->(b:MemoryEntity {repo: $repo})
WHERE (a.id IN $ids OR b.id IN $ids) AND `+current+`
RETURN `+factProj+`
ORDER BY f.updated_at DESC, f.id ASC LIMIT $limit`, map[string]any{"repo": repo, "ids": ids, "limit": limit, "history": history})
	if err != nil {
		return nil, err
	}
	for _, rec := range res.Records {
		out = append(out, toFact(rec))
	}
	return out, nil
}

// endpoints loads the entities facts refer to, except those in skip, by name.
func (s *Store) endpoints(ctx context.Context, repo string, facts []memory.Fact, skip map[string]bool, roots ...string) ([]memory.Entity, error) {
	var ids []string
	seen := map[string]bool{}
	for _, id := range roots {
		if !seen[id] && !skip[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	for _, f := range facts {
		for _, id := range []string{f.Subject, f.Object} {
			if !seen[id] && !skip[id] {
				seen[id] = true
				ids = append(ids, id)
			}
		}
	}
	if len(ids) == 0 {
		return nil, nil
	}
	res, err := s.read(ctx, "MATCH (e:MemoryEntity {repo: $repo}) WHERE e.id IN $ids RETURN "+entityProj+" ORDER BY e.name, e.id",
		map[string]any{"repo": repo, "ids": ids})
	if err != nil {
		return nil, err
	}
	out := make([]memory.Entity, 0, len(res.Records))
	for _, rec := range res.Records {
		out = append(out, toEntity(rec))
	}
	return out, nil
}

// toEntity decodes the "e" column of an entityProj row.
func toEntity(rec *neo4j.Record) memory.Entity {
	m, _ := rec.AsMap()["e"].(map[string]any)
	s := func(k string) string { v, _ := m[k].(string); return v }
	e := memory.Entity{ID: s("id"), Repo: s("repo"), Name: s("name"), Type: s("type"), CreatedAt: s("created_at"), UpdatedAt: s("updated_at")}
	e.Aliases = nodeprops.Strs(m["aliases"])
	return e
}

// toFact decodes a factProj row. Facts saved before history and confidence
// existed read as current, valid from their creation, with confidence 1.
func toFact(rec *neo4j.Record) memory.Fact {
	m := rec.AsMap()
	n, _ := m["confirmations"].(int64)
	conf, _ := m["confidence"].(float64)
	if conf == 0 {
		conf = 1
	}
	f := memory.Fact{ID: str(rec, "id"), Repo: str(rec, "repo"), Subject: str(rec, "subject"), Predicate: str(rec, "predicate"),
		Object: str(rec, "object"), Source: str(rec, "source"), Confirmations: int(n), Confidence: conf,
		OriginSessionID: str(rec, "origin_session_id"), ValidFrom: str(rec, "valid_from"), ValidTo: str(rec, "valid_to"), CreatedAt: str(rec, "created_at"), UpdatedAt: str(rec, "updated_at")}
	if f.ValidFrom == "" {
		f.ValidFrom = f.CreatedAt
	}
	return f
}

// DeleteMemoryFact implements store.MemoryStore.
func (s *Store) DeleteMemoryFact(ctx context.Context, repo, id string) error {
	res, err := s.write(ctx, "MATCH ()-[f:FACT {repo: $repo, id: $id}]->() DELETE f RETURN count(f) AS n", map[string]any{"repo": repo, "id": id})
	if err != nil {
		return err
	}
	if n, _ := res.Records[0].AsMap()["n"].(int64); n == 0 {
		return fmt.Errorf("fact %q: %w", id, store.ErrNotFound)
	}
	return nil
}

// DeleteMemoryEntity implements store.MemoryStore.
func (s *Store) DeleteMemoryEntity(ctx context.Context, repo, id string) error {
	res, err := s.write(ctx, "MATCH (e:MemoryEntity {repo: $repo, id: $id}) DETACH DELETE e RETURN count(e) AS n", map[string]any{"repo": repo, "id": id})
	if err != nil {
		return err
	}
	if n, _ := res.Records[0].AsMap()["n"].(int64); n == 0 {
		return fmt.Errorf("entity %q: %w", id, store.ErrNotFound)
	}
	return nil
}

// MemoryStats implements store.MemoryStore.
func (s *Store) MemoryStats(ctx context.Context) ([]memory.Stats, error) {
	res, err := s.read(ctx, `MATCH (e:MemoryEntity)
WITH e.repo AS repo, count(e) AS entities, max(e.updated_at) AS entity_updated
OPTIONAL MATCH (a:MemoryEntity {repo: repo})-[f:FACT]->(b:MemoryEntity)
WITH repo, entities, entity_updated, f, a, b, coalesce(f.valid_to, '') = '' AS cur
RETURN repo, entities, entity_updated,
  sum(CASE WHEN f IS NOT NULL AND cur THEN 1 ELSE 0 END) AS facts,
  sum(CASE WHEN f IS NOT NULL AND NOT cur THEN 1 ELSE 0 END) AS history,
  sum(CASE WHEN cur THEN coalesce(f.confirmations, 0) ELSE 0 END) AS confirmations,
  sum(CASE WHEN cur THEN size(a.name) + size(f.predicate) + size(b.name) ELSE 0 END) AS chars,
  max(CASE WHEN cur THEN f.updated_at END) AS fact_updated,
  collect(CASE WHEN cur THEN f.source END) AS sources
ORDER BY repo`, nil)
	if err != nil {
		return nil, err
	}
	out := []memory.Stats{}
	for _, rec := range res.Records {
		m := rec.AsMap()
		n := func(k string) int { v, _ := m[k].(int64); return int(v) }
		st := memory.Stats{Repo: str(rec, "repo"), Entities: n("entities"), Facts: n("facts"), History: n("history"), Confirmations: n("confirmations"),
			FactChars: n("chars"), Sources: map[string]int{}, LastUpdated: max(str(rec, "entity_updated"), str(rec, "fact_updated"))}
		sources, _ := m["sources"].([]any)
		for _, v := range sources {
			if src, ok := v.(string); ok {
				st.Sources[src]++
			}
		}
		out = append(out, st)
	}
	return out, nil
}
