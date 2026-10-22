package neo4jstore

import (
	"context"

	"github.com/parisnakitakejser/codesynapse/internal/store"
)

// MemoryFactCounts implements store.MemoryStore. This operator-only aggregate
// reads no entity names, predicates, sources or other stored content.
func (s *Store) MemoryFactCounts(ctx context.Context, afterNamespace string, limit int) ([]store.MemoryFactCount, error) {
	res, err := s.read(ctx, `MATCH (e:MemoryEntity)
WHERE e.repo > $after
WITH DISTINCT e.repo AS namespace ORDER BY namespace LIMIT $limit
OPTIONAL MATCH (:MemoryEntity {repo: namespace})-[f:FACT {repo: namespace}]->(:MemoryEntity {repo: namespace})
WHERE coalesce(f.valid_to, '') = ''
RETURN namespace, count(DISTINCT f.id) AS facts ORDER BY namespace`, map[string]any{"after": afterNamespace, "limit": store.ClampLimit(limit, 100)})
	if err != nil {
		return nil, err
	}
	out := make([]store.MemoryFactCount, 0, len(res.Records))
	for _, r := range res.Records {
		n, _ := r.AsMap()["facts"].(int64)
		out = append(out, store.MemoryFactCount{Namespace: str(r, "namespace"), Facts: n})
	}
	return out, nil
}
