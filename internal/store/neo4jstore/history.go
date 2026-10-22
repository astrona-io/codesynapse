package neo4jstore

import (
	"context"
	"encoding/json"
	"slices"

	"github.com/astrona-io/codesynapse/internal/history"
	"github.com/astrona-io/codesynapse/internal/store"
)

// Snapshots are :Snapshot nodes (not :Node), so repository replacement and
// deletion never touch them. Each is one JSON document; repo and at are
// indexed.

// AddSnapshot implements store.HistoryStore.
func (s *Store) AddSnapshot(ctx context.Context, repo string, snap history.Snapshot) error {
	doc, err := json.Marshal(snap)
	if err != nil {
		return err
	}
	if _, err := s.write(ctx, `MERGE (s:Snapshot {repo: $repo, at: $at}) SET s.doc = $doc`,
		map[string]any{"repo": repo, "at": snap.At, "doc": string(doc)}); err != nil {
		return err
	}
	_, err = s.write(ctx, `MATCH (s:Snapshot {repo: $repo}) WITH s ORDER BY s.at DESC SKIP $keep DETACH DELETE s`,
		map[string]any{"repo": repo, "keep": history.MaxSnapshots})
	return err
}

// Snapshots implements store.HistoryStore.
func (s *Store) Snapshots(ctx context.Context, repo string, limit int) ([]history.Snapshot, error) {
	res, err := s.read(ctx, `MATCH (s:Snapshot {repo: $repo}) RETURN s.doc AS doc ORDER BY s.at DESC LIMIT $limit`,
		map[string]any{"repo": repo, "limit": store.ClampLimit(limit, 100)})
	if err != nil {
		return nil, err
	}
	out := make([]history.Snapshot, 0, len(res.Records))
	for _, rec := range res.Records {
		var snap history.Snapshot
		if err := json.Unmarshal([]byte(str(rec, "doc")), &snap); err != nil {
			return nil, err
		}
		out = append(out, snap)
	}
	slices.Reverse(out)
	return out, nil
}

// DeleteSnapshots implements store.HistoryStore.
func (s *Store) DeleteSnapshots(ctx context.Context, repo string) error {
	_, err := s.write(ctx, "MATCH (s:Snapshot {repo: $repo}) DETACH DELETE s", map[string]any{"repo": repo})
	return err
}
