package neo4jstore

import (
	"context"
	"fmt"

	"github.com/neo4j/neo4j-go-driver/v5/neo4j"

	"github.com/parisnakitakejser/codesynapse/internal/memory"
	"github.com/parisnakitakejser/codesynapse/internal/store"
)

// Agent memory lives in :MemoryEntity nodes joined by :FACT relationships.
// Neither carries the :Node label, so ReplaceRepo and DeleteRepo never touch
// it. The predicate is a property, not the relationship type: agent text only
// ever reaches Cypher as a parameter.

// UpsertMemory implements store.MemoryStore.
func (s *Store) UpsertMemory(ctx context.Context, b memory.Batch) error {
	entityIDs := make([]string, len(b.Entities))
	entities := make([]map[string]any, len(b.Entities))
	for i, e := range b.Entities {
		entityIDs[i] = e.ID
		entities[i] = map[string]any{"id": e.ID, "repo": e.Repo, "name": e.Name, "type": e.Type, "created_at": e.CreatedAt, "updated_at": e.UpdatedAt}
	}
	factIDs := make([]string, len(b.Facts))
	facts := make([]map[string]any, len(b.Facts))
	for i, f := range b.Facts {
		factIDs[i] = f.ID
		facts[i] = map[string]any{"id": f.ID, "repo": f.Repo, "subject": f.Subject, "predicate": f.Predicate, "object": f.Object,
			"source": f.Source, "created_at": f.CreatedAt, "updated_at": f.UpdatedAt}
	}
	keep := []string{}
	replace := make([]map[string]any, len(b.Replace))
	for i, r := range b.Replace {
		keep = append(keep, r.ID)
		replace[i] = map[string]any{"subject": r.Subject, "predicate": r.Predicate}
	}
	p := map[string]any{"repo": b.Repo, "entity_ids": entityIDs, "fact_ids": factIDs, "entities": entities, "facts": facts, "replace": replace, "keep": keep}

	sess := s.driver.NewSession(ctx, neo4j.SessionConfig{DatabaseName: s.db, AccessMode: neo4j.AccessModeWrite})
	defer sess.Close(ctx) //nolint:errcheck // nothing to do on close failure
	_, err := sess.ExecuteWrite(ctx, func(tx neo4j.ManagedTransaction) (any, error) {
		count := func(q string) (int64, error) {
			res, err := tx.Run(ctx, q, p)
			if err != nil {
				return 0, err
			}
			rec, err := res.Single(ctx)
			if err != nil {
				return 0, err
			}
			n, _ := rec.Values[0].(int64)
			return n, nil
		}
		var n [6]int64
		for i, q := range []string{
			"MATCH (e:MemoryEntity {repo: $repo}) RETURN count(e)",
			"MATCH (e:MemoryEntity) WHERE e.id IN $entity_ids RETURN count(e)",
			"MATCH ()-[f:FACT {repo: $repo}]->() RETURN count(f)",
			"MATCH ()-[f:FACT]->() WHERE f.id IN $fact_ids RETURN count(f)",
			`UNWIND $replace AS r
MATCH (:MemoryEntity {id: r.subject})-[f:FACT {predicate: r.predicate}]->() WHERE NOT f.id IN $keep
RETURN count(DISTINCT f)`,
		} {
			v, err := count(q)
			if err != nil {
				return nil, err
			}
			n[i] = v
		}
		total, existing, facts, existingFacts, removed := n[0], n[1], n[2], n[3], n[4]
		if total+int64(len(entityIDs))-existing > memory.MaxEntitiesPerRepo ||
			facts-removed+int64(len(factIDs))-existingFacts > memory.MaxFactsPerRepo {
			return nil, store.ErrMemoryFull
		}
		for _, q := range []string{
			`UNWIND $replace AS r
MATCH (:MemoryEntity {id: r.subject})-[f:FACT {predicate: r.predicate}]->() WHERE NOT f.id IN $keep
DELETE f`,
			`UNWIND $entities AS e
MERGE (n:MemoryEntity {id: e.id})
ON CREATE SET n.repo = e.repo, n.name = e.name, n.type = e.type, n.created_at = e.created_at, n.updated_at = e.updated_at
ON MATCH SET n.name = e.name, n.updated_at = e.updated_at, n.type = CASE WHEN e.type = 'Thing' THEN n.type ELSE e.type END`,
			`UNWIND $facts AS f
MATCH (s:MemoryEntity {id: f.subject}), (o:MemoryEntity {id: f.object})
MERGE (s)-[r:FACT {id: f.id}]->(o)
ON CREATE SET r.repo = f.repo, r.predicate = f.predicate, r.source = f.source, r.confirmations = 1,
  r.created_at = f.created_at, r.updated_at = f.updated_at
ON MATCH SET r.confirmations = r.confirmations + 1, r.source = f.source, r.updated_at = f.updated_at`,
		} {
			if _, err := tx.Run(ctx, q, p); err != nil {
				return nil, err
			}
		}
		return nil, nil
	})
	return err
}

const entityProj = "e{.id, .repo, .name, .type, .created_at, .updated_at} AS e"

// SearchMemory implements store.MemoryStore.
func (s *Store) SearchMemory(ctx context.Context, repo, query string, limit int) (memory.Graph, error) {
	limit = store.ClampLimit(limit, 20)
	res, err := s.read(ctx, `MATCH (e:MemoryEntity {repo: $repo})
WITH e, size([w IN $words WHERE toLower(e.name) CONTAINS w OR toLower(e.type) = w]) AS score
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
		e := toEntity(rec, "e")
		g.Entities = append(g.Entities, e)
		ids = append(ids, e.ID)
		matched[e.ID] = true
	}
	if g.Facts, err = s.factsTouching(ctx, repo, ids, min(limit*5, store.MaxLimit)); err != nil {
		return g, err
	}
	more, err := s.endpoints(ctx, repo, g.Facts, matched)
	g.Entities = append(g.Entities, more...)
	return g, err
}

// MemorySubgraph implements store.MemoryStore.
func (s *Store) MemorySubgraph(ctx context.Context, repo string, ids []string, depth, limit int) (memory.Graph, error) {
	limit = store.ClampLimit(limit, 50)
	g := memory.Graph{Notice: memory.Notice, Entities: []memory.Entity{}, Facts: []memory.Fact{}}
	if min(max(depth, 1), 2) == 2 {
		res, err := s.read(ctx, `MATCH (a:MemoryEntity {repo: $repo})-[:FACT]-(b:MemoryEntity)
WHERE a.id IN $ids RETURN DISTINCT b.id AS id`, map[string]any{"repo": repo, "ids": ids})
		if err != nil {
			return g, err
		}
		for _, rec := range res.Records {
			ids = append(ids, str(rec, "id"))
		}
	}
	var err error
	if g.Facts, err = s.factsTouching(ctx, repo, ids, limit); err != nil {
		return g, err
	}
	g.Entities, err = s.endpoints(ctx, repo, g.Facts, nil)
	return g, err
}

func (s *Store) factsTouching(ctx context.Context, repo string, ids []string, limit int) ([]memory.Fact, error) {
	out := []memory.Fact{}
	if len(ids) == 0 {
		return out, nil
	}
	res, err := s.read(ctx, `MATCH (a:MemoryEntity)-[f:FACT {repo: $repo}]->(b:MemoryEntity)
WHERE a.id IN $ids OR b.id IN $ids
RETURN f.id AS id, f.repo AS repo, a.id AS subject, f.predicate AS predicate, b.id AS object, f.source AS source,
  f.confirmations AS confirmations, f.created_at AS created_at, f.updated_at AS updated_at
ORDER BY f.updated_at DESC, f.id ASC LIMIT $limit`, map[string]any{"repo": repo, "ids": ids, "limit": limit})
	if err != nil {
		return nil, err
	}
	for _, rec := range res.Records {
		n, _ := rec.AsMap()["confirmations"].(int64)
		out = append(out, memory.Fact{ID: str(rec, "id"), Repo: str(rec, "repo"), Subject: str(rec, "subject"), Predicate: str(rec, "predicate"),
			Object: str(rec, "object"), Source: str(rec, "source"), Confirmations: int(n), CreatedAt: str(rec, "created_at"), UpdatedAt: str(rec, "updated_at")})
	}
	return out, nil
}

// endpoints loads the entities facts refer to, except those in skip, by name.
func (s *Store) endpoints(ctx context.Context, repo string, facts []memory.Fact, skip map[string]bool) ([]memory.Entity, error) {
	var ids []string
	seen := map[string]bool{}
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
		out = append(out, toEntity(rec, "e"))
	}
	return out, nil
}

func toEntity(rec *neo4j.Record, key string) memory.Entity {
	m, _ := rec.AsMap()[key].(map[string]any)
	s := func(k string) string { v, _ := m[k].(string); return v }
	return memory.Entity{ID: s("id"), Repo: s("repo"), Name: s("name"), Type: s("type"), CreatedAt: s("created_at"), UpdatedAt: s("updated_at")}
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
