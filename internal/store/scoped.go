package store

import (
	"cmp"
	"context"
	"slices"
	"strings"

	"github.com/astrona-io/codesynapse/internal/model"
)

// ForRepository confines code queries to an already-authorized graph. Callers
// must resolve permission before constructing it; selectors cannot grant access.
func ForRepository(r Reader, repo string) Reader {
	if scoped, ok := r.(repositoryReader); ok && scoped.repo == repo {
		return r
	}
	return repositoryReader{reader: r, repo: repo}
}

// RepositoryGraphReader applies the repository boundary during backend traversal.
// Implementations must check every hop and both ends of every returned edge.
type RepositoryGraphReader interface {
	TraverseRepository(ctx context.Context, repo, id string, edges []model.EdgeKind, dir Direction, depth, limit int) ([]model.Hit, error)
	SubgraphRepository(ctx context.Context, repo string, ids []string, kinds []model.EdgeKind) ([]model.Edge, error)
}

type repositoryReader struct {
	reader Reader
	repo   string
}

func (s repositoryReader) allows(repo string) bool { return s.repo != "" && repo == s.repo }
func (s repositoryReader) allowsNode(n model.Node) bool {
	return s.allows(n.Repo) && strings.HasPrefix(n.ID, s.repo+"|")
}
func (s repositoryReader) Repos(ctx context.Context) ([]model.Node, error) {
	n, err := s.Node(ctx, model.NodeID(s.repo, model.KindRepository, s.repo))
	if err != nil {
		return nil, err
	}
	return []model.Node{n}, nil
}
func (s repositoryReader) Stats(ctx context.Context, repo string) (model.Stats, error) {
	if !s.allows(repo) {
		return model.Stats{}, ErrNotFound
	}
	return s.reader.Stats(ctx, repo)
}
func (s repositoryReader) DependencyGraph(ctx context.Context, repo string) (DependencyGraph, error) {
	if !s.allows(repo) {
		return DependencyGraph{}, ErrNotFound
	}
	return s.reader.DependencyGraph(ctx, repo)
}
func (s repositoryReader) FindingCounts(ctx context.Context, repo string) (map[string]int, error) {
	if !s.allows(repo) {
		return nil, ErrNotFound
	}
	return s.reader.FindingCounts(ctx, repo)
}
func (s repositoryReader) Node(ctx context.Context, id string) (model.Node, error) {
	if s.repo == "" || !strings.HasPrefix(id, s.repo+"|") {
		return model.Node{}, ErrNotFound
	}
	n, err := s.reader.Node(ctx, id)
	if err != nil {
		return model.Node{}, err
	}
	if !s.allowsNode(n) || n.ID != id {
		return model.Node{}, ErrNotFound
	}
	return n, nil
}
func (s repositoryReader) filter(ns []model.Node) []model.Node {
	out := make([]model.Node, 0, len(ns))
	for _, n := range ns {
		if s.allowsNode(n) {
			out = append(out, n)
		}
	}
	return out
}
func (s repositoryReader) Search(ctx context.Context, repo, q string, kinds []model.NodeKind, limit int) ([]model.Node, error) {
	if !s.allows(repo) {
		return nil, ErrNotFound
	}
	ns, err := s.reader.Search(ctx, repo, q, kinds, limit)
	return s.filter(ns), err
}
func (s repositoryReader) NodesInFile(ctx context.Context, repo, file string) ([]model.Node, error) {
	if !s.allows(repo) {
		return nil, ErrNotFound
	}
	ns, err := s.reader.NodesInFile(ctx, repo, file)
	return s.filter(ns), err
}
func (s repositoryReader) List(ctx context.Context, repo string, f Filter, m Metric, limit int) ([]Ranked, error) {
	if !s.allows(repo) {
		return nil, ErrNotFound
	}
	ns, err := s.reader.List(ctx, repo, f, m, limit)
	out := make([]Ranked, 0, len(ns))
	for _, n := range ns {
		if s.allowsNode(n.Node) {
			out = append(out, n)
		}
	}
	return out, err
}

// Each expansion starts at a node in this graph, so a malformed cross-graph edge
// cannot make a later hop traverse an unauthorized repository.
func (s repositoryReader) Traverse(ctx context.Context, id string, edges []model.EdgeKind, dir Direction, depth, limit int) ([]model.Hit, error) {
	if s.repo == "" || !strings.HasPrefix(id, s.repo+"|") {
		return nil, ErrNotFound
	}
	if native, ok := s.reader.(RepositoryGraphReader); ok {
		depth, limit = ClampDepth(depth), ClampLimit(limit, 50)
		hits, err := native.TraverseRepository(ctx, s.repo, id, edges, dir, depth, limit)
		if err != nil {
			return nil, err
		}
		out := make([]model.Hit, 0, min(len(hits), limit))
		seen := map[string]bool{id: true}
		slices.SortFunc(hits, func(a, b model.Hit) int {
			return cmp.Or(cmp.Compare(a.Depth, b.Depth), cmp.Compare(a.Node.ID, b.Node.ID))
		})
		for _, hit := range hits {
			if s.allowsNode(hit.Node) && hit.Depth > 0 && hit.Depth <= depth && !seen[hit.Node.ID] {
				out = append(out, hit)
				seen[hit.Node.ID] = true
				if len(out) == limit {
					break
				}
			}
		}
		return out, nil
	}
	if _, err := s.Node(ctx, id); err != nil {
		return nil, err
	}
	depth, limit = ClampDepth(depth), ClampLimit(limit, 50)
	frontier := []string{id}
	seen := map[string]bool{id: true}
	out := []model.Hit{}
	for d := 1; d <= depth && len(frontier) > 0; d++ {
		level := []model.Node{}
		for _, root := range frontier {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			hits, err := s.reader.Traverse(ctx, root, edges, dir, 1, MaxLimit)
			if err != nil {
				return nil, err
			}
			for _, hit := range hits {
				if !s.allowsNode(hit.Node) || seen[hit.Node.ID] {
					continue
				}
				seen[hit.Node.ID] = true
				level = append(level, hit.Node)
			}
		}
		slices.SortFunc(level, func(a, b model.Node) int { return cmp.Compare(a.ID, b.ID) })
		level = level[:min(len(level), limit-len(out))]
		frontier = make([]string, 0, len(level))
		for _, node := range level {
			out = append(out, model.Hit{Node: node, Depth: d})
			frontier = append(frontier, node.ID)
		}
		if len(out) >= limit {
			break
		}
	}
	slices.SortFunc(out, func(a, b model.Hit) int {
		return cmp.Or(cmp.Compare(a.Depth, b.Depth), cmp.Compare(a.Node.ID, b.Node.ID))
	})
	return out, nil
}
func (s repositoryReader) Subgraph(ctx context.Context, ids []string, kinds []model.EdgeKind) ([]model.Edge, error) {
	if len(ids) > MaxLimit*4 {
		return nil, ErrNotFound
	}
	ids = slices.Clone(ids)
	slices.Sort(ids)
	ids = slices.Compact(ids)
	if s.repo == "" {
		return nil, ErrNotFound
	}
	for _, id := range ids {
		if !strings.HasPrefix(id, s.repo+"|") {
			return nil, ErrNotFound
		}
	}
	want := map[string]bool{}
	native, isNative := s.reader.(RepositoryGraphReader)
	for _, id := range ids {
		if !isNative {
			if _, err := s.Node(ctx, id); err != nil {
				return nil, err
			}
		}
		want[id] = true
	}
	var es []model.Edge
	var err error
	if isNative {
		es, err = native.SubgraphRepository(ctx, s.repo, ids, kinds)
	} else {
		es, err = s.reader.Subgraph(ctx, ids, kinds)
	}
	if err != nil {
		return nil, err
	}
	out := make([]model.Edge, 0, len(es))
	for _, e := range es {
		if want[e.From] && want[e.To] && (len(kinds) == 0 || slices.Contains(kinds, e.Kind)) {
			out = append(out, e)
		}
	}
	return out, err
}
