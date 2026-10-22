package hugegraphstore

import (
	"context"
	"fmt"
	"slices"

	"github.com/parisnakitakejser/codesynapse/internal/model"
	"github.com/parisnakitakejser/codesynapse/internal/store"
	"github.com/parisnakitakejser/codesynapse/internal/store/nodeprops"
)

const scopedTraverse = props + `if (!g.V(start).hasLabel('node').has('repo',repo).hasNext()) { return [[found:false]] }
def seen=[start] as Set
def frontier=[start]
def hits=[]
for(d in 1..depth) {
 if(frontier.isEmpty() || hits.size()>=limit) break
 def t=g.V(frontier as Object[]).hasLabel('node').has('repo',repo)
 t=inward ? t.in(labels as String[]) : t.out(labels as String[])
 def next=t.hasLabel('node').has('repo',repo).filter{!seen.contains(it.get().id().toString())}
   .dedup().order().by('nid').limit(limit-hits.size()).toList()
 frontier=[]
 next.each { v -> def id=v.id().toString();seen.add(id);frontier<<id;hits<<[P(v),d] }
}
[[found:true,hits:hits]]`

// TraverseRepository executes one bounded search with repository checks at every hop.
func (s *Store) TraverseRepository(ctx context.Context, repo, id string, edges []model.EdgeKind, dir store.Direction, depth, limit int) ([]model.Hit, error) {
	if repo == "" {
		return nil, store.ErrNotFound
	}
	if len(edges) == 0 {
		return nil, fmt.Errorf("traverse needs at least one edge kind")
	}
	labels := make([]string, len(edges))
	for i, e := range edges {
		if !e.Valid() {
			return nil, fmt.Errorf("invalid edge kind %q", e)
		}
		labels[i] = string(e)
	}
	var res []struct {
		Found bool    `json:"found"`
		Hits  [][]any `json:"hits"`
	}
	err := s.read(ctx, scopedTraverse, map[string]any{"repo": repo, "start": nodeVID(id), "labels": labels, "inward": dir == store.In, "depth": store.ClampDepth(depth), "limit": store.ClampLimit(limit, 50)}, &res)
	if err != nil {
		return nil, err
	}
	if len(res) != 1 || !res[0].Found {
		return nil, store.ErrNotFound
	}
	out := make([]model.Hit, 0, len(res[0].Hits))
	for _, h := range res[0].Hits {
		if len(h) != 2 {
			return nil, fmt.Errorf("invalid scoped traversal result")
		}
		m, ok := h[0].(map[string]any)
		if !ok {
			return nil, fmt.Errorf("invalid scoped traversal node")
		}
		out = append(out, model.Hit{Node: toNode(m), Depth: nodeprops.Int(h[1])})
	}
	return out, nil
}

const scopedSubgraph = `def roots=g.V(ids as Object[]).hasLabel('node').has('repo',repo).toList()
if(roots.size()!=ids.size()) return [[found:false]]
def es=g.V(ids as Object[]).hasLabel('node').has('repo',repo).outE(labels as String[])
 .where(__.inV().hasLabel('node').has('repo',repo).hasId(within(ids)))
 .order().by(__.outV().values('nid')).by(__.inV().values('nid')).by(T.label).limit(limit).toList()
[[found:true,edges:es.collect{[it.outVertex().value('nid'),it.inVertex().value('nid'),it.label(),it.property('count').orElse(1L)]}]]`

// SubgraphRepository checks all requested vertices and both edge endpoints in one query.
func (s *Store) SubgraphRepository(ctx context.Context, repo string, ids []string, kinds []model.EdgeKind) ([]model.Edge, error) {
	if repo == "" {
		return nil, store.ErrNotFound
	}
	if len(ids) == 0 {
		return nil, nil
	}
	if len(ids) > store.MaxLimit*4 {
		return nil, store.ErrNotFound
	}
	ids = slices.Clone(ids)
	slices.Sort(ids)
	ids = slices.Compact(ids)
	vids := make([]string, len(ids))
	for i, id := range ids {
		vids[i] = nodeVID(id)
	}
	labels := make([]string, len(kinds))
	for i, k := range kinds {
		if !k.Valid() {
			return nil, fmt.Errorf("invalid edge kind %q", k)
		}
		labels[i] = string(k)
	}
	var res []struct {
		Found bool    `json:"found"`
		Edges [][]any `json:"edges"`
	}
	if err := s.read(ctx, scopedSubgraph, map[string]any{"repo": repo, "ids": vids, "labels": labels, "limit": store.MaxSubgraphEdges}, &res); err != nil {
		return nil, err
	}
	if len(res) != 1 || !res[0].Found {
		return nil, store.ErrNotFound
	}
	out := make([]model.Edge, 0, len(res[0].Edges))
	for _, r := range res[0].Edges {
		if len(r) != 4 {
			return nil, fmt.Errorf("invalid scoped subgraph result")
		}
		from, _ := r[0].(string)
		to, _ := r[1].(string)
		kind, _ := r[2].(string)
		out = append(out, model.Edge{From: from, To: to, Kind: model.EdgeKind(kind), Count: nodeprops.Int(r[3])})
	}
	return out, nil
}
