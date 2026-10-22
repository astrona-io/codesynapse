package neo4jstore

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/neo4j/neo4j-go-driver/v5/neo4j/dbtype"

	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/store"
	"github.com/astrona-io/codesynapse/internal/store/nodeprops"
)

// TraverseRepository restricts each quantified hop before it can expand again.
func (s *Store) TraverseRepository(ctx context.Context, repo, id string, edges []model.EdgeKind, dir store.Direction, depth, limit int) ([]model.Hit, error) {
	if repo == "" {
		return nil, store.ErrNotFound
	}
	if len(edges) == 0 {
		return nil, fmt.Errorf("traverse needs at least one edge kind")
	}
	types := make([]string, len(edges))
	for i, e := range edges {
		if !e.Valid() {
			return nil, fmt.Errorf("invalid edge kind %q", e)
		}
		types[i] = string(e)
	}
	left, right := "-", "->"
	if dir == store.In {
		left, right = "<-", "-"
	}
	q := fmt.Sprintf(`MATCH (root:Node {id:$id,repo:$repo})
CALL {
 WITH root
 MATCH p=(root)((a:Node {repo:$repo})%s[:%s]%s(b:Node {repo:$repo})){1,%d}(m:Node)
 WHERE m <> root
 WITH m,min(length(p)) AS depth ORDER BY depth,m.id LIMIT $limit
 RETURN collect({node:m,depth:depth}) AS hits
}
RETURN hits`, left, strings.Join(types, "|"), right, store.ClampDepth(depth))
	res, err := s.read(ctx, q, map[string]any{"repo": repo, "id": id, "limit": store.ClampLimit(limit, 50)})
	if err != nil {
		return nil, err
	}
	if len(res.Records) != 1 {
		return nil, store.ErrNotFound
	}
	raw, _ := res.Records[0].Get("hits")
	rows, _ := raw.([]any)
	out := make([]model.Hit, 0, len(rows))
	for _, row := range rows {
		m, ok := row.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("invalid scoped traversal result")
		}
		n, err := scopedNode(m["node"])
		if err != nil {
			return nil, err
		}
		d, _ := m["depth"].(int64)
		out = append(out, model.Hit{Node: n, Depth: int(d)})
	}
	return out, nil
}

// SubgraphRepository validates all IDs and extracts only repository edges in one query.
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
	for _, kind := range kinds {
		if !kind.Valid() {
			return nil, fmt.Errorf("invalid edge kind %q", kind)
		}
	}
	res, err := s.read(ctx, `UNWIND $ids AS id
OPTIONAL MATCH (n:Node {id:id,repo:$repo})
WITH collect(n) AS nodes
CALL {
 WITH nodes
 WITH nodes WHERE size(nodes)=size($ids)
 UNWIND nodes AS a
 MATCH (a)-[e]->(b:Node {repo:$repo})
 WHERE b IN nodes AND (size($kinds)=0 OR type(e) IN $kinds)
 WITH a,e,b ORDER BY a.id,b.id,type(e) LIMIT $limit
 RETURN collect({from:a.id,to:b.id,kind:type(e),count:coalesce(e.count,1)}) AS edges
}
RETURN size(nodes)=size($ids) AS found,edges`, map[string]any{"repo": repo, "ids": ids, "kinds": edgeStrings(kinds), "limit": store.MaxSubgraphEdges})
	if err != nil {
		return nil, err
	}
	if len(res.Records) != 1 {
		return nil, store.ErrNotFound
	}
	found, _ := res.Records[0].Get("found")
	if found != true {
		return nil, store.ErrNotFound
	}
	raw, _ := res.Records[0].Get("edges")
	rows, _ := raw.([]any)
	out := make([]model.Edge, 0, len(rows))
	for _, row := range rows {
		m, ok := row.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("invalid scoped subgraph result")
		}
		from, _ := m["from"].(string)
		to, _ := m["to"].(string)
		kind, _ := m["kind"].(string)
		count, _ := m["count"].(int64)
		out = append(out, model.Edge{From: from, To: to, Kind: model.EdgeKind(kind), Count: int(count)})
	}
	return out, nil
}

func scopedNode(value any) (model.Node, error) {
	n, ok := value.(dbtype.Node)
	if !ok {
		return model.Node{}, fmt.Errorf("invalid scoped traversal node")
	}
	return nodeprops.From(n.Props), nil
}
