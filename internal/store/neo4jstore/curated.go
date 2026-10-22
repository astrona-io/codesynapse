package neo4jstore

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/parisnakitakejser/codesynapse/internal/curation"
	"github.com/parisnakitakejser/codesynapse/internal/store"
)

// Curated items are :Curated nodes (not :Node), so repository replacement and
// deletion never touch them. Each is one JSON document.

// SaveCurated implements store.CuratedStore.
func (s *Store) SaveCurated(ctx context.Context, it curation.Item) error {
	doc, err := json.Marshal(it)
	if err != nil {
		return err
	}
	_, err = s.write(ctx, `MERGE (c:Curated {repo: $repo, id: $id}) SET c.kind = $kind, c.created_at = $created, c.doc = $doc`,
		map[string]any{"repo": it.Repo, "id": it.ID, "kind": it.Kind, "created": it.CreatedAt, "doc": string(doc)})
	return err
}

// GetCurated implements store.CuratedStore.
func (s *Store) GetCurated(ctx context.Context, repo, id string) (curation.Item, error) {
	res, err := s.read(ctx, "MATCH (c:Curated {repo: $repo, id: $id}) RETURN c.doc AS doc", map[string]any{"repo": repo, "id": id})
	if err != nil {
		return curation.Item{}, err
	}
	if len(res.Records) == 0 {
		return curation.Item{}, fmt.Errorf("item %q: %w", id, store.ErrNotFound)
	}
	var it curation.Item
	err = json.Unmarshal([]byte(str(res.Records[0], "doc")), &it)
	return it, err
}

// ListCurated implements store.CuratedStore.
func (s *Store) ListCurated(ctx context.Context, repo, kind string, limit int) ([]curation.Item, error) {
	res, err := s.read(ctx, `MATCH (c:Curated {repo: $repo}) WHERE $kind = '' OR c.kind = $kind
RETURN c.doc AS doc ORDER BY c.created_at DESC, c.id LIMIT $limit`, map[string]any{"repo": repo, "kind": kind, "limit": store.ClampLimit(limit, 100)})
	if err != nil {
		return nil, err
	}
	out := make([]curation.Item, 0, len(res.Records))
	for _, rec := range res.Records {
		var it curation.Item
		if err := json.Unmarshal([]byte(str(rec, "doc")), &it); err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, nil
}

// DeleteCurated implements store.CuratedStore.
func (s *Store) DeleteCurated(ctx context.Context, repo, id string) error {
	_, err := s.write(ctx, "MATCH (c:Curated {repo: $repo, id: $id}) DELETE c", map[string]any{"repo": repo, "id": id})
	return err
}

// DeleteAllCurated implements store.CuratedStore.
func (s *Store) DeleteAllCurated(ctx context.Context, repo string) error {
	_, err := s.write(ctx, "MATCH (c:Curated {repo: $repo}) DELETE c", map[string]any{"repo": repo})
	return err
}
