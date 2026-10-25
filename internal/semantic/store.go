package semantic

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/vector"
)

// Tenant is the vector tenant of symbol descriptions. They describe a graph,
// not an organisation: who may see a graph is decided by repository access,
// which fences every node a ranking returns (store.ForRepository).
const Tenant = "code-graph"

// maxItemID is the longest vector ID the Milvus index accepts.
const maxItemID = 128

// Store keeps symbol embeddings in a vector.Index, one namespace per graph.
// An item's ID is the node ID and a hash of the embedded text, so a symbol
// whose description changed gets a new vector and an unchanged one is not
// embedded again.
type Store struct {
	Idx   vector.Index
	Model string
}

// Stats says what a Sync did.
type Stats struct {
	Symbols  int // embeddable symbols in the graph
	Embedded int // vectors made now
	Skipped  int // symbols whose ID is too long to index
}

func itemID(nodeID, text string) (string, bool) {
	sum := sha256.Sum256([]byte(text))
	id := nodeID + "#" + hex.EncodeToString(sum[:8])
	return id, len(id) <= maxItemID
}

func nodeOf(itemID string) string {
	if i := strings.LastIndexByte(itemID, '#'); i >= 0 {
		return itemID[:i]
	}
	return itemID
}

// Embeddable reports whether a symbol gets a vector: functions, methods,
// types and doc sections that are not tests.
func Embeddable(n model.Node) bool {
	switch n.Kind {
	case model.KindFunction, model.KindMethod, model.KindInterfaceMethod, model.KindType, model.KindSection:
		return !n.IsTest
	}
	return false
}

// Sync embeds the symbols of graph that have no vector for their current
// description. Vectors of earlier descriptions stay (the ranking resolves
// every hit to a live node and keeps the best one per node).
func (s Store) Sync(ctx context.Context, emb Embedder, graph string, nodes []model.Node) (Stats, error) {
	var st Stats
	type pending struct{ id, text string }
	var all []pending
	for _, n := range nodes {
		if !Embeddable(n) {
			continue
		}
		st.Symbols++
		text := Text(n)
		id, ok := itemID(n.ID, text)
		if !ok {
			st.Skipped++
			continue
		}
		all = append(all, pending{id, text})
	}
	have := map[string]bool{}
	if p, ok := s.Idx.(vector.Prober); ok {
		ids := make([]string, len(all))
		for i, a := range all {
			ids[i] = a.id
		}
		var err error
		if have, err = p.Has(ctx, s.Model, Tenant, graph, ids); err != nil {
			return st, err
		}
	}
	var todo []pending
	for _, a := range all {
		if !have[a.id] {
			todo = append(todo, a)
		}
	}
	for off := 0; off < len(todo); off += batch {
		part := todo[off:min(off+batch, len(todo))]
		texts := make([]string, len(part))
		for i, p := range part {
			texts[i] = p.text
		}
		vecs, err := emb.Embed(ctx, texts)
		if err != nil {
			return st, err
		}
		if len(vecs) != len(part) {
			return st, fmt.Errorf("semantic: %d vectors for %d texts", len(vecs), len(part))
		}
		items := make([]vector.Item, len(part))
		for i, p := range part {
			items[i] = vector.Item{ID: p.id, Tenant: Tenant, Namespace: graph, Vector: normalise(vecs[i])}
		}
		if err := s.Idx.Upsert(ctx, s.Model, items); err != nil {
			return st, err
		}
		st.Embedded += len(part)
	}
	return st, nil
}

// Rank returns up to k symbols of graph nearest to the task, best first, one
// per node. k is bounded by vector.MaxK.
func (s Store) Rank(ctx context.Context, emb Embedder, graph, task string, k int) ([]Hit, error) {
	vecs, err := emb.Embed(ctx, []string{"search_query: " + task})
	if err != nil {
		return nil, err
	}
	if len(vecs) != 1 {
		return nil, errors.New("semantic: no query vector")
	}
	hits, err := s.Idx.Search(ctx, s.Model, Tenant, graph, normalise(vecs[0]), k)
	if err != nil {
		return nil, err
	}
	var out []Hit
	seen := map[string]bool{}
	for _, h := range hits {
		id := nodeOf(h.ID)
		if !seen[id] {
			seen[id] = true
			out = append(out, Hit{ID: id, Score: h.Score})
		}
	}
	return out, nil
}
