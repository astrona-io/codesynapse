package hugegraphstore

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/parisnakitakejser/codesynapse/internal/curation"
	"github.com/parisnakitakejser/codesynapse/internal/store"
)

// Curated items are their own vertex label, so repository replacement and
// deletion (label "node") never touch them. Each is one JSON document.

const (
	saveCurated = `def old = g.V(v).tryNext()
if (old.isPresent()) { old.get().remove() }
graph.addVertex(T.label, 'curated', T.id, v, 'cid', cid, 'repo', repo, 'kind', kind, 'created_at', created, 'doc', doc)
1`
	getCurated  = `g.V(v).hasLabel('curated').values('doc').toList()`
	listCurated = `def all = g.V().hasLabel('curated').has('repo', repo).toList()
if (kind) { all = all.findAll { it.value('kind') == kind } }
all.sort { a, b -> b.value('created_at') <=> a.value('created_at') ?: a.value('cid') <=> b.value('cid') }
all.take(limit as int).collect { it.value('doc') }`
	deleteCurated = `g.V(v).hasLabel('curated').drop().iterate()
1`
	deleteAllCurated = `g.V().hasLabel('curated').has('repo', repo).drop().iterate()
1`
)

func curatedVID(repo, id string) string { return vid("c", repo+"|"+id) }

// SaveCurated implements store.CuratedStore.
func (s *Store) SaveCurated(ctx context.Context, it curation.Item) error {
	doc, err := json.Marshal(it)
	if err != nil {
		return err
	}
	return s.write(ctx, saveCurated, map[string]any{"v": curatedVID(it.Repo, it.ID), "cid": it.ID, "repo": it.Repo, "kind": it.Kind,
		"created": it.CreatedAt, "doc": string(doc)}, nil)
}

// GetCurated implements store.CuratedStore.
func (s *Store) GetCurated(ctx context.Context, repo, id string) (curation.Item, error) {
	var docs []string
	if err := s.read(ctx, getCurated, map[string]any{"v": curatedVID(repo, id)}, &docs); err != nil {
		return curation.Item{}, err
	}
	if len(docs) == 0 {
		return curation.Item{}, fmt.Errorf("item %q: %w", id, store.ErrNotFound)
	}
	var it curation.Item
	err := json.Unmarshal([]byte(docs[0]), &it)
	return it, err
}

// ListCurated implements store.CuratedStore.
func (s *Store) ListCurated(ctx context.Context, repo, kind string, limit int) ([]curation.Item, error) {
	var docs []string
	if err := s.read(ctx, listCurated, map[string]any{"repo": repo, "kind": kind, "limit": store.ClampLimit(limit, 100)}, &docs); err != nil {
		return nil, err
	}
	out := make([]curation.Item, 0, len(docs))
	for _, d := range docs {
		var it curation.Item
		if err := json.Unmarshal([]byte(d), &it); err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, nil
}

// DeleteCurated implements store.CuratedStore.
func (s *Store) DeleteCurated(ctx context.Context, repo, id string) error {
	return s.write(ctx, deleteCurated, map[string]any{"v": curatedVID(repo, id)}, nil)
}

// DeleteAllCurated implements store.CuratedStore.
func (s *Store) DeleteAllCurated(ctx context.Context, repo string) error {
	return s.write(ctx, deleteAllCurated, map[string]any{"repo": repo}, nil)
}
