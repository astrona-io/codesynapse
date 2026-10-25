package nebulastore

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/store"
	"github.com/astrona-io/codesynapse/internal/store/nodeprops"
)

var _ store.Reader = (*Store)(nil)

// toNode restores a node from the properties of its vertex: NULL columns
// are skipped, lists are JSON text, "nid" is the model ID.
func toNode(p map[string]any) model.Node {
	m := make(map[string]any, len(p))
	for k, v := range p {
		if v == nil {
			continue
		}
		m[k] = v
	}
	m["id"] = m["nid"]
	for _, k := range nodeprops.Lists {
		if text, ok := m[k].(string); ok {
			var l []string
			_ = json.Unmarshal([]byte(text), &l) //nolint:errcheck // an unreadable list is an empty one
			m[k] = l
		}
	}
	return nodeprops.From(m)
}

func nodesOf(rows []Row) []model.Node {
	out := make([]model.Node, 0, len(rows))
	for _, r := range rows {
		if p, ok := r["p"].(map[string]any); ok {
			out = append(out, toNode(p))
		}
	}
	return out
}

// Repos implements store.Reader.
func (s *Store) Repos(ctx context.Context) ([]model.Node, error) {
	rows, err := s.read(ctx, "MATCH (v:`node`) WHERE v.`node`.`kind` == \"Repository\" RETURN properties(v) AS p, v.`node`.`name` AS name ORDER BY name LIMIT $limit",
		map[string]any{"limit": int64(store.MaxLimit)})
	if err != nil {
		return nil, err
	}
	return nodesOf(rows), nil
}

func (s *Store) repoExists(ctx context.Context, repo string) error {
	_, err := s.Node(ctx, model.NodeID(repo, model.KindRepository, repo))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("repo %q: %w", repo, store.ErrNotFound)
		}
		return err
	}
	return nil
}

// Node implements store.Reader.
func (s *Store) Node(ctx context.Context, id string) (model.Node, error) {
	rows, err := s.read(ctx, "FETCH PROP ON `node` "+literal(nodeVID(id))+" YIELD properties(vertex) AS p", nil)
	if err != nil {
		return model.Node{}, err
	}
	if ns := nodesOf(rows); len(ns) > 0 {
		return ns[0], nil
	}
	return model.Node{}, fmt.Errorf("node %q: %w", id, store.ErrNotFound)
}

// FindingCounts implements store.Reader.
func (s *Store) FindingCounts(ctx context.Context, repo string) (map[string]int, error) {
	rows, err := s.read(ctx, "MATCH (v:`node`) WHERE v.`node`.`repo` == $repo AND v.`node`.`kind` == \"Finding\" RETURN v.`node`.`severity` AS s, count(v) AS n",
		map[string]any{"repo": repo})
	if err != nil {
		return nil, err
	}
	out := map[string]int{}
	for _, r := range rows {
		sev, _ := r["s"].(string)
		out[sev] += nodeprops.Int(r["n"])
	}
	return out, nil
}

// Search implements store.Reader.
func (s *Store) Search(ctx context.Context, repo, q string, kinds []model.NodeKind, limit int) ([]model.Node, error) {
	if err := s.repoExists(ctx, repo); err != nil {
		return nil, err
	}
	stmt := "MATCH (v:`node`) WHERE v.`node`.`repo` == $repo" +
		" AND (size($kinds) == 0 OR v.`node`.`kind` IN $kinds)" +
		" AND (toLower(v.`node`.`name`) CONTAINS $q OR toLower(coalesce(v.`node`.`qualified_name`, \"\")) CONTAINS $q)" +
		" RETURN properties(v) AS p, CASE WHEN toLower(v.`node`.`name`) == $q THEN 0 ELSE 1 END AS rank," +
		" size(coalesce(v.`node`.`qualified_name`, \"\")) AS len, v.`node`.`nid` AS nid" +
		" ORDER BY rank, len, nid LIMIT $limit"
	rows, err := s.read(ctx, stmt, map[string]any{"repo": repo, "q": lower(q), "kinds": anys(kindStrings(kinds)), "limit": int64(store.ClampLimit(limit, 20))})
	if err != nil {
		return nil, err
	}
	return nodesOf(rows), nil
}

// NodesInFile implements store.Reader.
func (s *Store) NodesInFile(ctx context.Context, repo, file string) ([]model.Node, error) {
	if err := s.repoExists(ctx, repo); err != nil {
		return nil, err
	}
	stmt := "MATCH (v:`node`) WHERE v.`node`.`repo` == $repo AND v.`node`.`file` == $file" +
		" AND coalesce(v.`node`.`start_line`, 0) > 0 AND NOT v.`node`.`kind` IN [\"File\",\"Document\",\"Config\",\"Template\"]" +
		" RETURN properties(v) AS p, v.`node`.`start_line` AS line, v.`node`.`nid` AS nid ORDER BY line, nid LIMIT $limit"
	rows, err := s.read(ctx, stmt, map[string]any{"repo": repo, "file": file, "limit": int64(store.MaxLimit)})
	if err != nil {
		return nil, err
	}
	return nodesOf(rows), nil
}

// Traverse implements store.Reader. It is a breadth-first search, one GO
// step per level, which yields shortest distances directly, as the memory
// store does.
func (s *Store) Traverse(ctx context.Context, id string, edges []model.EdgeKind, dir store.Direction, depth, limit int) ([]model.Hit, error) {
	if len(edges) == 0 {
		return nil, errors.New("traverse needs at least one edge kind")
	}
	types := make([]string, 0, len(edges))
	for _, e := range edges {
		if !e.Valid() {
			return nil, fmt.Errorf("invalid edge kind %q", e)
		}
		types = append(types, quote(string(e)))
	}
	if _, err := s.Node(ctx, id); err != nil {
		return nil, err
	}
	reverse := ""
	if dir == store.In {
		reverse = " REVERSELY"
	}
	start := nodeVID(id)
	seen := map[string]bool{start: true}
	frontier := []string{start}
	type found struct {
		vid   string
		depth int
	}
	var hits []found
	for d := 1; d <= store.ClampDepth(depth) && len(frontier) > 0; d++ {
		rows, err := s.read(ctx, "GO FROM "+literals(frontier)+" OVER "+strings.Join(types, ",")+reverse+" YIELD DISTINCT id($$) AS id", nil)
		if err != nil {
			return nil, err
		}
		var next []string
		for _, r := range rows {
			v, _ := r["id"].(string)
			if v != "" && !seen[v] {
				seen[v] = true
				next = append(next, v)
				hits = append(hits, found{v, d})
			}
		}
		frontier = next
	}
	nodes, err := s.fetchNodes(ctx, func() []string {
		v := make([]string, len(hits))
		for i, h := range hits {
			v[i] = h.vid
		}
		return v
	}())
	if err != nil {
		return nil, err
	}
	out := make([]model.Hit, 0, len(hits))
	for _, h := range hits {
		if n, ok := nodes[h.vid]; ok {
			out = append(out, model.Hit{Node: n, Depth: h.depth})
		}
	}
	slices.SortFunc(out, func(a, b model.Hit) int {
		return cmp.Or(cmp.Compare(a.Depth, b.Depth), cmp.Compare(a.Node.ID, b.Node.ID))
	})
	return out[:min(len(out), store.ClampLimit(limit, 50))], nil
}

// fetchNodes reads vertices by ID in batches, keyed by vertex ID.
func (s *Store) fetchNodes(ctx context.Context, vids []string) (map[string]model.Node, error) {
	out := make(map[string]model.Node, len(vids))
	for off := 0; off < len(vids); off += 200 {
		part := vids[off:min(off+200, len(vids))]
		rows, err := s.read(ctx, "FETCH PROP ON `node` "+literals(part)+" YIELD id(vertex) AS id, properties(vertex) AS p", nil)
		if err != nil {
			return nil, err
		}
		for _, r := range rows {
			id, _ := r["id"].(string)
			if p, ok := r["p"].(map[string]any); ok {
				out[id] = toNode(p)
			}
		}
	}
	return out, nil
}

func kindStrings(kinds []model.NodeKind) []string {
	out := make([]string, 0, len(kinds))
	for _, k := range kinds {
		out = append(out, string(k))
	}
	return out
}
