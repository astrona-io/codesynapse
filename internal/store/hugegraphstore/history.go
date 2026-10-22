package hugegraphstore

import (
	"context"
	"encoding/json"

	"github.com/astrona-io/codesynapse/internal/history"
	"github.com/astrona-io/codesynapse/internal/store"
)

// Snapshots are their own vertex label, so repository replacement and
// deletion (label "node") never touch them. A snapshot is one JSON document.

const (
	addSnapshot = `def old = g.V(v).tryNext()
if (old.isPresent()) { old.get().remove() }
graph.addVertex(T.label, 'snapshot', T.id, v, 'sid', sid, 'repo', repo, 'at', at, 'doc', doc)
def all = g.V().hasLabel('snapshot').has('repo', repo).toList()
all.sort { a, b -> b.value('at') <=> a.value('at') }
all.drop(keep as int).each { it.remove() }
1`
	listSnapshots = `def all = g.V().hasLabel('snapshot').has('repo', repo).toList()
all.sort { a, b -> b.value('at') <=> a.value('at') }
all.take(limit as int).collect { it.value('doc') }`
	deleteSnapshots = `g.V().hasLabel('snapshot').has('repo', repo).drop().iterate()
1`
)

func snapshotVID(repo, at string) string { return vid("s", repo+"|"+at) }

// AddSnapshot implements store.HistoryStore.
func (s *Store) AddSnapshot(ctx context.Context, repo string, snap history.Snapshot) error {
	doc, err := json.Marshal(snap)
	if err != nil {
		return err
	}
	return s.write(ctx, addSnapshot, map[string]any{"v": snapshotVID(repo, snap.At), "sid": snapshotVID(repo, snap.At), "repo": repo,
		"at": snap.At, "doc": string(doc), "keep": history.MaxSnapshots}, nil)
}

// Snapshots implements store.HistoryStore.
func (s *Store) Snapshots(ctx context.Context, repo string, limit int) ([]history.Snapshot, error) {
	var docs []string
	if err := s.read(ctx, listSnapshots, map[string]any{"repo": repo, "limit": store.ClampLimit(limit, 100)}, &docs); err != nil {
		return nil, err
	}
	out := make([]history.Snapshot, 0, len(docs))
	for i := len(docs) - 1; i >= 0; i-- { // newest first in the store, oldest first here
		var snap history.Snapshot
		if err := json.Unmarshal([]byte(docs[i]), &snap); err != nil {
			return nil, err
		}
		out = append(out, snap)
	}
	return out, nil
}

// DeleteSnapshots implements store.HistoryStore.
func (s *Store) DeleteSnapshots(ctx context.Context, repo string) error {
	return s.write(ctx, deleteSnapshots, map[string]any{"repo": repo}, nil)
}
