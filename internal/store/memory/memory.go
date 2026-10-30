// Package memory is an in-process store.Store. It serves tests, offline
// review in CI, and the MCP server when no graph database is available.
package memory

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/astrona-io/codesynapse/internal/curation"
	"github.com/astrona-io/codesynapse/internal/history"
	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/reviews"
	"github.com/astrona-io/codesynapse/internal/runtimefacts"
	"github.com/astrona-io/codesynapse/internal/sessionmemory"
	"github.com/astrona-io/codesynapse/internal/store"
	"github.com/astrona-io/codesynapse/internal/usage"
	"github.com/astrona-io/codesynapse/internal/vector"
)

// Store keeps graphs per repository. It is safe for concurrent use.
type Store struct {
	sessionmemory.Engine
	sessions       map[string]sessionmemory.Document
	sessionRevoked map[string]bool
	sessionVectors *vector.Flat
	codeVectors    *vector.Flat
	access         map[string]model.AccessSnapshot
	mu             sync.RWMutex
	repos          map[string]*repoGraph
	reviews        map[string]reviews.Review
	history        map[string][]history.Snapshot
	curated        map[string]curation.Item
	mem            map[string]*memRepo
	usage          map[string]usage.Bucket
	vecs           map[string]embedding // memory entity ID -> vector
	// runtime holds measured facts per repository; a re-index keeps them.
	runtime map[string]runtimefacts.Facts
}

type repoGraph struct {
	nodes map[string]model.Node
	out   map[string][]model.Edge
	in    map[string][]model.Edge
	// search holds every node in Search's tie-break order (qualified name
	// length, then ID) with names folded once at ingest, so a query neither
	// folds every name again nor sorts all matches. byName holds positions in
	// search ordered by folded name, which finds exact matches by binary
	// search instead of a full scan.
	search []searchKey
	byName []int
}

type searchKey struct {
	id              string
	name, qualified string // strings.ToLower, as model.ContainsFold compares
	kind            model.NodeKind
	qualifiedLen    int // of the original name: folding can change byte length
}

// searchCancelEvery bounds how many candidates Search scans between context
// checks.
const searchCancelEvery = 1024

func indexSearch(rg *repoGraph) {
	rg.search = make([]searchKey, 0, len(rg.nodes))
	for _, n := range rg.nodes {
		rg.search = append(rg.search, searchKey{
			id: n.ID, name: strings.ToLower(n.Name), qualified: strings.ToLower(n.QualifiedName),
			kind: n.Kind, qualifiedLen: len(n.QualifiedName),
		})
	}
	slices.SortFunc(rg.search, func(a, b searchKey) int {
		return cmp.Or(cmp.Compare(a.qualifiedLen, b.qualifiedLen), cmp.Compare(a.id, b.id))
	})
	rg.byName = make([]int, len(rg.search))
	for i := range rg.byName {
		rg.byName[i] = i
	}
	slices.SortFunc(rg.byName, func(a, b int) int {
		return cmp.Or(strings.Compare(rg.search[a].name, rg.search[b].name), cmp.Compare(a, b))
	})
}

// exactMatches returns the positions in search whose folded name is lq, in
// search order.
func (rg *repoGraph) exactMatches(lq string) []int {
	first, _ := slices.BinarySearchFunc(rg.byName, lq, func(i int, q string) int { return strings.Compare(rg.search[i].name, q) })
	last := first
	for last < len(rg.byName) && rg.search[rg.byName[last]].name == lq {
		last++
	}
	return rg.byName[first:last]
}

var _ store.Store = (*Store)(nil)

// New returns an empty store.
func New() *Store {
	s := &Store{repos: map[string]*repoGraph{}, reviews: map[string]reviews.Review{}, history: map[string][]history.Snapshot{}, curated: map[string]curation.Item{}, mem: map[string]*memRepo{}, usage: map[string]usage.Bucket{}, vecs: map[string]embedding{}}
	s.Engine = sessionmemory.Engine{Backend: s}
	s.sessions = map[string]sessionmemory.Document{}
	s.sessionRevoked = map[string]bool{}
	s.sessionVectors = vector.NewFlat()
	s.codeVectors = vector.NewFlat()
	return s
}

// CodeVectors is the index of symbol embeddings (internal/semantic).
func (s *Store) CodeVectors() vector.Index { return s.codeVectors }

// EnsureSchema is a no-op.
func (s *Store) EnsureSchema(context.Context) error { return nil }

// CheckSchema is a no-op: the memory store has no schema.
func (s *Store) CheckSchema(context.Context) error { return nil }

// System names the store for metrics.
func (s *Store) System() string { return "memory" }

// Close is a no-op.
func (s *Store) Close(context.Context) error { return nil }

// ReplaceRepo implements store.Writer.
func (s *Store) ReplaceRepo(ctx context.Context, repo string, g *model.Graph) error {
	nodes, edges, err := store.Replacement(ctx, repo, g)
	if err != nil {
		return err
	}
	rg := &repoGraph{nodes: map[string]model.Node{}, out: map[string][]model.Edge{}, in: map[string][]model.Edge{}}
	for _, n := range nodes {
		if n.Repo != repo {
			return fmt.Errorf("node %s belongs to repo %q, not %q", n.ID, n.Repo, repo)
		}
		rg.nodes[n.ID] = n
	}
	for _, e := range edges {
		if _, ok := rg.nodes[e.From]; !ok {
			continue
		}
		if _, ok := rg.nodes[e.To]; !ok {
			continue
		}
		rg.out[e.From] = append(rg.out[e.From], e)
		rg.in[e.To] = append(rg.in[e.To], e)
	}
	indexSearch(rg)
	if err := store.CommitCheck(ctx); err != nil {
		return err
	}
	s.mu.Lock()
	s.repos[repo] = rg
	s.mu.Unlock()
	return nil
}

// DeleteRepo implements store.Writer.
func (s *Store) DeleteRepo(_ context.Context, repo string) error {
	s.mu.Lock()
	delete(s.repos, repo)
	s.deleteRuntimeOf(repo)
	s.mu.Unlock()
	return nil
}

// Repos implements store.Reader.
func (s *Store) Repos(context.Context) ([]model.Node, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []model.Node
	for name, rg := range s.repos {
		if n, ok := rg.nodes[model.NodeID(name, model.KindRepository, name)]; ok {
			out = append(out, n)
		}
	}
	slices.SortFunc(out, func(a, b model.Node) int { return cmp.Compare(a.Name, b.Name) })
	return out, nil
}

func (s *Store) repo(name string) (*repoGraph, error) {
	rg, ok := s.repos[name]
	if !ok {
		return nil, fmt.Errorf("repo %q: %w", name, store.ErrNotFound)
	}
	return rg, nil
}

// DependencyGraph implements store.Reader.
func (s *Store) DependencyGraph(_ context.Context, repo string) (store.DependencyGraph, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rg, err := s.repo(repo)
	if err != nil {
		return store.DependencyGraph{}, err
	}
	var g store.DependencyGraph
	isModule := map[string]bool{}
	for id, n := range rg.nodes {
		switch n.Kind {
		case model.KindModule:
			g.Modules = append(g.Modules, n)
			isModule[id] = true
		case model.KindVulnerability:
			g.Vulnerabilities = append(g.Vulnerabilities, n)
		}
	}
	for id := range isModule {
		for _, e := range rg.out[id] {
			if e.Kind == model.EdgeDependsOn && isModule[e.To] {
				g.Edges = append(g.Edges, e)
			}
		}
	}
	byID := func(a, b model.Node) int { return cmp.Compare(a.ID, b.ID) }
	slices.SortFunc(g.Modules, byID)
	slices.SortFunc(g.Vulnerabilities, byID)
	slices.SortFunc(g.Edges, func(a, b model.Edge) int { return cmp.Or(cmp.Compare(a.From, b.From), cmp.Compare(a.To, b.To)) })
	if len(g.Modules) > store.MaxDependencies {
		g.Modules, g.Truncated = g.Modules[:store.MaxDependencies], true
	}
	if len(g.Vulnerabilities) > store.MaxDependencies {
		g.Vulnerabilities, g.Truncated = g.Vulnerabilities[:store.MaxDependencies], true
	}
	if len(g.Edges) > store.MaxDependencyEdges {
		g.Edges, g.Truncated = g.Edges[:store.MaxDependencyEdges], true
	}
	return g, nil
}

// FindingCounts implements store.Reader.
func (s *Store) FindingCounts(_ context.Context, repo string) (map[string]int, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rg, err := s.repo(repo)
	if err != nil {
		return nil, err
	}
	out := map[string]int{}
	for _, n := range rg.nodes {
		if n.Kind == model.KindFinding {
			out[n.Severity]++
		}
	}
	return out, nil
}

// Stats implements store.Reader.
func (s *Store) Stats(_ context.Context, repo string) (model.Stats, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rg, err := s.repo(repo)
	if err != nil {
		return model.Stats{}, err
	}
	b := store.NewStatsBuilder(repo)
	for _, n := range rg.nodes {
		b.Add(n, 1)
	}
	for _, edges := range rg.out {
		for _, e := range edges {
			b.AddEdges(e.Kind, 1)
		}
	}
	return b.Stats(), nil
}

// Node implements store.Reader.
func (s *Store) Node(_ context.Context, id string) (model.Node, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, rg := range s.repos {
		if n, ok := rg.nodes[id]; ok {
			return n, nil
		}
	}
	return model.Node{}, fmt.Errorf("node %q: %w", id, store.ErrNotFound)
}

// Search implements store.Reader. Matches are exact names first, then by
// qualified name length and ID, the same order as the other stores.
func (s *Store) Search(ctx context.Context, repo, q string, kinds []model.NodeKind, limit int) ([]model.Node, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	limit = store.ClampLimit(limit, 20)
	s.mu.RLock()
	defer s.mu.RUnlock()
	rg, err := s.repo(repo)
	if err != nil {
		return nil, err
	}
	lq := strings.ToLower(q)
	wanted := func(k searchKey) bool { return len(kinds) == 0 || slices.Contains(kinds, k.kind) }
	var out []model.Node
	add := func(k searchKey) {
		if out == nil { // a query that matches nothing allocates nothing
			out = make([]model.Node, 0, min(limit, len(rg.search)))
		}
		out = append(out, rg.nodes[k.id])
	}
	for _, i := range rg.exactMatches(lq) {
		if len(out) == limit {
			return out, nil
		}
		if wanted(rg.search[i]) {
			add(rg.search[i])
		}
	}
	for i, k := range rg.search {
		if len(out) == limit {
			break
		}
		if i > 0 && i%searchCancelEvery == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		if k.name != lq && wanted(k) && (strings.Contains(k.name, lq) || strings.Contains(k.qualified, lq)) {
			add(k)
		}
	}
	return out, nil
}

// NodesInFile implements store.Reader.
func (s *Store) NodesInFile(_ context.Context, repo, file string) ([]model.Node, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rg, err := s.repo(repo)
	if err != nil {
		return nil, err
	}
	var out []model.Node
	for _, n := range rg.nodes {
		if n.File == file && n.StartLine > 0 && !isFileLevel(n.Kind) {
			out = append(out, n)
		}
	}
	slices.SortFunc(out, func(a, b model.Node) int {
		return cmp.Or(cmp.Compare(a.StartLine, b.StartLine), cmp.Compare(a.ID, b.ID))
	})
	return out[:min(len(out), store.MaxLimit)], nil
}

// Traverse implements store.Reader with a breadth-first search, which yields
// shortest distances directly.
func (s *Store) Traverse(_ context.Context, id string, edges []model.EdgeKind, dir store.Direction, depth, limit int) ([]model.Hit, error) {
	depth = store.ClampDepth(depth)
	limit = store.ClampLimit(limit, 50)
	s.mu.RLock()
	defer s.mu.RUnlock()
	var rg *repoGraph
	for _, candidate := range s.repos {
		if _, ok := candidate.nodes[id]; ok {
			rg = candidate
			break
		}
	}
	if rg == nil {
		return nil, fmt.Errorf("node %q: %w", id, store.ErrNotFound)
	}
	adj := rg.out
	if dir == store.In {
		adj = rg.in
	}
	seen := map[string]bool{id: true}
	frontier := []string{id}
	var hits []model.Hit
	for d := 1; d <= depth && len(frontier) > 0; d++ {
		var next []string
		for _, cur := range frontier {
			for _, e := range adj[cur] {
				if !slices.Contains(edges, e.Kind) {
					continue
				}
				other := e.To
				if dir == store.In {
					other = e.From
				}
				if seen[other] {
					continue
				}
				seen[other] = true
				hits = append(hits, model.Hit{Node: rg.nodes[other], Depth: d})
				next = append(next, other)
			}
		}
		frontier = next
	}
	slices.SortStableFunc(hits, func(a, b model.Hit) int {
		return cmp.Or(cmp.Compare(a.Depth, b.Depth), cmp.Compare(a.Node.ID, b.Node.ID))
	})
	return hits[:min(len(hits), limit)], nil
}

// List implements store.Reader.
func (s *Store) List(_ context.Context, repo string, f store.Filter, m store.Metric, limit int) ([]store.Ranked, error) {
	if !m.Valid() {
		return nil, fmt.Errorf("unknown metric %q", m)
	}
	limit = store.ClampLimit(limit, 20)
	s.mu.RLock()
	defer s.mu.RUnlock()
	rg, err := s.repo(repo)
	if err != nil {
		return nil, err
	}
	var out []store.Ranked
	for _, n := range rg.nodes {
		if !Matches(n, f) {
			continue
		}
		out = append(out, store.Ranked{Node: n, Score: score(rg, n, m)})
	}
	slices.SortFunc(out, func(a, b store.Ranked) int {
		switch m {
		case store.ByName:
			return cmp.Compare(a.Node.DisplayName(), b.Node.DisplayName())
		case store.ByTime:
			return cmp.Or(cmp.Compare(b.Node.Time, a.Node.Time), cmp.Compare(a.Node.ID, b.Node.ID))
		default:
			return cmp.Or(cmp.Compare(b.Score, a.Score), cmp.Compare(a.Node.ID, b.Node.ID))
		}
	})
	return out[:min(len(out), limit)], nil
}

// Matches reports whether n passes f. Exported for reuse by other stores'
// tests; the NebulaGraph store expresses the same rules in nGQL.
func Matches(n model.Node, f store.Filter) bool {
	if len(f.Kinds) > 0 && !slices.Contains(f.Kinds, n.Kind) {
		return false
	}
	if f.Tag != "" && !slices.Contains(n.SecurityTags, f.Tag) {
		return false
	}
	if f.FilePrefix != "" && !strings.HasPrefix(n.File, f.FilePrefix) {
		return false
	}
	if f.ExportedOnly && !n.Exported {
		return false
	}
	if f.Undocumented && n.Summary != "" {
		return false
	}
	if f.ExcludeTests && n.IsTest {
		return false
	}
	return true
}

func score(rg *repoGraph, n model.Node, m store.Metric) int {
	switch m {
	case store.ByComplexity:
		return n.Complexity
	case store.ByChurn:
		return n.Churn
	case store.ByLOC:
		return n.LOC
	case store.ByFanIn:
		return countEdges(rg.in[n.ID], model.EdgeCalls)
	case store.ByAuthored:
		return countEdges(rg.out[n.ID], model.EdgeAuthored)
	}
	return 0
}

func countEdges(edges []model.Edge, kind model.EdgeKind) int {
	c := 0
	for _, e := range edges {
		if e.Kind == kind {
			c++
		}
	}
	return c
}

// Subgraph implements store.Reader.
func (s *Store) Subgraph(_ context.Context, ids []string, kinds []model.EdgeKind) ([]model.Edge, error) {
	want := make(map[string]bool, len(ids))
	for _, id := range ids {
		want[id] = true
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []model.Edge
	for _, rg := range s.repos {
		for _, id := range ids {
			for _, e := range rg.out[id] {
				if want[e.To] && (len(kinds) == 0 || slices.Contains(kinds, e.Kind)) {
					out = append(out, e)
				}
			}
		}
	}
	slices.SortFunc(out, func(a, b model.Edge) int {
		return cmp.Or(cmp.Compare(a.From, b.From), cmp.Compare(a.To, b.To), cmp.Compare(a.Kind, b.Kind))
	})
	return out[:min(len(out), store.MaxSubgraphEdges)], nil
}

// isFileLevel reports kinds that represent a whole file, which outlines omit.
func isFileLevel(k model.NodeKind) bool {
	switch k {
	case model.KindFile, model.KindDocument, model.KindConfig, model.KindTemplate:
		return true
	}
	return false
}

// SaveReview implements store.ReviewStore (insert or replace).
func (s *Store) SaveReview(_ context.Context, r reviews.Review) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r.Comments = slices.Clone(r.Comments)
	s.reviews[r.ID] = r
	return nil
}

// GetReview implements store.ReviewStore.
func (s *Store) GetReview(_ context.Context, id string) (reviews.Review, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r, ok := s.reviews[id]
	if !ok {
		return reviews.Review{}, fmt.Errorf("review %q: %w", id, store.ErrNotFound)
	}
	r.Comments = slices.Clone(r.Comments)
	return r, nil
}

// ListReviews implements store.ReviewStore.
func (s *Store) ListReviews(_ context.Context, repo string, limit int) ([]reviews.Review, error) {
	limit = store.ClampLimit(limit, 50)
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []reviews.Review
	for _, r := range s.reviews {
		if r.Repo == repo {
			out = append(out, r)
		}
	}
	slices.SortFunc(out, func(a, b reviews.Review) int {
		return cmp.Or(cmp.Compare(b.CreatedAt, a.CreatedAt), cmp.Compare(a.ID, b.ID))
	})
	return out[:min(len(out), limit)], nil
}

// DeleteReview implements store.ReviewStore.
func (s *Store) DeleteReview(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.reviews, id)
	return nil
}

// AddSnapshot implements store.HistoryStore.
func (s *Store) AddSnapshot(_ context.Context, repo string, snap history.Snapshot) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	all := slices.DeleteFunc(slices.Clone(s.history[repo]), func(x history.Snapshot) bool { return x.At == snap.At })
	all = append(all, snap)
	slices.SortFunc(all, func(a, b history.Snapshot) int { return cmp.Compare(a.At, b.At) })
	if len(all) > history.MaxSnapshots {
		all = all[len(all)-history.MaxSnapshots:]
	}
	s.history[repo] = all
	return nil
}

// Snapshots implements store.HistoryStore.
func (s *Store) Snapshots(_ context.Context, repo string, limit int) ([]history.Snapshot, error) {
	limit = store.ClampLimit(limit, 100)
	s.mu.RLock()
	defer s.mu.RUnlock()
	all := s.history[repo]
	return slices.Clone(all[max(0, len(all)-limit):]), nil
}

// DeleteSnapshots implements store.HistoryStore.
func (s *Store) DeleteSnapshots(_ context.Context, repo string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.history, repo)
	return nil
}

// SaveCurated implements store.CuratedStore.
func (s *Store) SaveCurated(_ context.Context, it curation.Item) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.curated[it.Repo+"|"+it.ID] = it
	return nil
}

// GetCurated implements store.CuratedStore.
func (s *Store) GetCurated(_ context.Context, repo, id string) (curation.Item, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	it, ok := s.curated[repo+"|"+id]
	if !ok {
		return curation.Item{}, fmt.Errorf("item %q: %w", id, store.ErrNotFound)
	}
	return it, nil
}

// ListCurated implements store.CuratedStore.
func (s *Store) ListCurated(_ context.Context, repo, kind string, limit int) ([]curation.Item, error) {
	limit = store.ClampLimit(limit, 100)
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []curation.Item
	for _, it := range s.curated {
		if it.Repo == repo && (kind == "" || it.Kind == kind) {
			out = append(out, it)
		}
	}
	slices.SortFunc(out, func(a, b curation.Item) int {
		return cmp.Or(cmp.Compare(b.CreatedAt, a.CreatedAt), cmp.Compare(a.ID, b.ID))
	})
	return out[:min(len(out), limit)], nil
}

// DeleteCurated implements store.CuratedStore.
func (s *Store) DeleteCurated(_ context.Context, repo, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.curated, repo+"|"+id)
	return nil
}

// DeleteAllCurated implements store.CuratedStore.
func (s *Store) DeleteAllCurated(_ context.Context, repo string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, it := range s.curated {
		if it.Repo == repo {
			delete(s.curated, k)
		}
	}
	return nil
}
