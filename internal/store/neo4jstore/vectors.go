package neo4jstore

import (
	"context"
	"fmt"
	"strings"

	"github.com/parisnakitakejser/codesynapse/internal/memory"
	"github.com/parisnakitakejser/codesynapse/internal/store"
	"github.com/parisnakitakejser/codesynapse/internal/telemetry"
	"github.com/parisnakitakejser/codesynapse/internal/vector"
)

// vectorIndex indexes MemoryEntity.embedding for nearest-neighbour search.
// It is created when the first vectors arrive, since only then the length is
// known, and recreated when a new embedding model changes the length.
const vectorIndex = "memory_entity_embedding"

// vectorDims returns the length the vector index was created with, 0 if
// there is none.
func (s *Store) vectorDims(ctx context.Context) (int, error) {
	res, err := s.read(ctx, "SHOW VECTOR INDEXES YIELD name, options WHERE name = $name RETURN options", map[string]any{"name": vectorIndex})
	if err != nil {
		return 0, err
	}
	if len(res.Records) == 0 {
		return 0, nil
	}
	opts, _ := res.Records[0].AsMap()["options"].(map[string]any)
	cfg, _ := opts["indexConfig"].(map[string]any)
	n, _ := cfg["vector.dimensions"].(int64)
	return int(n), nil
}

func (s *Store) ensureVectorIndex(ctx context.Context, dims int) error {
	// The length is the only value interpolated into Cypher: index options
	// take no parameters. It is an int checked against MaxDimensions.
	if dims < 1 || dims > memory.MaxDimensions {
		return fmt.Errorf("vector length %d out of range", dims)
	}
	have, err := s.vectorDims(ctx)
	if err != nil {
		return err
	}
	if have == dims {
		return nil
	}
	if have != 0 {
		if _, err := s.write(ctx, "DROP INDEX "+vectorIndex+" IF EXISTS", nil); err != nil {
			return err
		}
	}
	_, err = s.write(ctx, fmt.Sprintf("CREATE VECTOR INDEX %s IF NOT EXISTS FOR (e:MemoryEntity) ON (e.embedding) "+
		"OPTIONS {indexConfig: {`vector.dimensions`: %d, `vector.similarity_function`: 'cosine'}}", vectorIndex, dims), nil)
	return err
}

// MemoryEmbeddingTodo implements store.MemoryStore.
func (s *Store) MemoryEmbeddingTodo(ctx context.Context, model string, limit int) ([]memory.Entity, error) {
	res, err := s.read(ctx, `MATCH (e:MemoryEntity) WHERE coalesce(e.embedding_model, '') <> $model
RETURN `+entityProj+` ORDER BY e.updated_at, e.id LIMIT $limit`, map[string]any{"model": model, "limit": store.ClampLimit(limit, 64)})
	if err != nil {
		return nil, err
	}
	var out []memory.Entity
	for _, rec := range res.Records {
		out = append(out, toEntity(rec))
	}
	return out, nil
}

// SetMemoryEmbeddings implements store.MemoryStore.
func (s *Store) SetMemoryEmbeddings(ctx context.Context, model string, vecs map[string][]float32) error {
	if err := memory.CheckVectors(vecs); err != nil || len(vecs) == 0 {
		return err
	}
	rows := make([]map[string]any, 0, len(vecs))
	dims := 0
	for id, v := range vecs {
		f := make([]float64, len(v))
		for i, x := range v {
			f[i] = float64(x)
		}
		dims = len(v)
		rows = append(rows, map[string]any{"id": id, "vec": f})
	}
	if err := s.ensureVectorIndex(ctx, dims); err != nil {
		return fmt.Errorf("vector index: %w", err)
	}
	_, err := s.write(ctx, `UNWIND $rows AS r MATCH (e:MemoryEntity {id: r.id})
SET e.embedding = r.vec, e.embedding_model = $model`, map[string]any{"rows": rows, "model": model})
	return err
}

// MemoryNearest implements store.MemoryStore. The index is approximate and
// spans all repositories, so it is asked for more neighbours than needed
// and filtered to repo and model.
func (s *Store) MemoryNearest(ctx context.Context, repo, model string, vec []float32, k int) ([]memory.Scored, error) {
	k = vector.ClampK(k)
	out := []memory.Scored{}
	dims, err := s.vectorDims(ctx)
	if err != nil || dims != len(vec) {
		return out, err // no index yet, or a query from another model
	}
	q := make([]float64, len(vec))
	for i, x := range vec {
		q[i] = float64(x)
	}
	res, err := s.read(ctx, `CALL db.index.vector.queryNodes($index, $over, $vec) YIELD node AS e, score
WHERE e.repo = $repo AND e.embedding_model = $model
RETURN `+entityProj+`, score ORDER BY score DESC, e.id LIMIT $k`,
		map[string]any{"index": vectorIndex, "over": min(max(k*20, 100), 1000), "vec": q, "repo": repo, "model": model, "k": k})
	if err != nil {
		if strings.Contains(err.Error(), "no such vector schema index") {
			return out, nil
		}
		return nil, err
	}
	for _, rec := range res.Records {
		score, _ := rec.AsMap()["score"].(float64)
		out = append(out, memory.Scored{Entity: toEntity(rec), Score: score})
	}
	telemetry.VectorMatches.AddContext(ctx, float64(len(out)), telemetry.Tenant(ctx), "neo4j")
	return out, nil
}

// MemoryLexical implements store.MemoryStore: the namespace's entities (at
// most memory.MaxEntitiesPerRepo short documents) ranked in-process.
func (s *Store) MemoryLexical(ctx context.Context, repo, query string, k int) ([]memory.Scored, error) {
	if len(memory.Words(query)) == 0 {
		return []memory.Scored{}, nil
	}
	res, err := s.read(ctx, `MATCH (e:MemoryEntity {repo: $repo}) RETURN `+entityProj+` LIMIT $max`,
		map[string]any{"repo": repo, "max": memory.MaxEntitiesPerRepo})
	if err != nil {
		return nil, err
	}
	ents := make([]memory.Entity, 0, len(res.Records))
	for _, rec := range res.Records {
		ents = append(ents, toEntity(rec))
	}
	return memory.RankBM25(ents, query, vector.ClampK(k)), nil
}
