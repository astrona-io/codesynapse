package hugegraphstore

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/store"
	"github.com/astrona-io/codesynapse/internal/store/nodeprops"
)

const (
	writeBatch  = 500
	deleteBatch = 2000
)

// props turns a vertex into a flat map: lists stay lists, single values
// are unwrapped. Prepended to every script that returns nodes.
const props = `def P = { v -> def m = [:]; v.properties().each { m[it.key()] = it.value() }; m }
`

// Node writes and stale removal. Code-graph scripts always start at
// hasLabel('node'): reviews also carry a "repo" property.
const (
	dropRepo = `def vs = g.V().hasLabel('node').has('repo', repo).dedup().limit(batch).toList()
vs.each { it.remove() }
vs.size()`
)

func newRunID() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// ReplaceRepo stages data outside code labels, then replaces the graph in one transaction.
func (s *Store) ReplaceRepo(ctx context.Context, repo string, g *model.Graph) error {
	nodes, edges, err := store.Replacement(ctx, repo, g)
	if err != nil {
		return err
	}
	return s.replaceAtomic(ctx, repo, nodes, edges)
}

// batched writes rows in requests of at most writeBatch rows that stay
// under the request size limit.
func (s *Store) batched(ctx context.Context, script string, rows []map[string]any, extra map[string]any) error {
	eb, err := json.Marshal(extra)
	if err != nil {
		return err
	}
	budget := s.maxRequest - len(script) - len(eb) - 1024 // envelope and aliases
	var batch []map[string]any
	size := 0
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		b := map[string]any{"rows": batch}
		for k, v := range extra {
			b[k] = v
		}
		err := s.write(ctx, script, b, nil)
		batch, size = batch[:0], 0
		return err
	}
	for _, r := range rows {
		b, err := json.Marshal(r)
		if err != nil {
			return err
		}
		if len(batch) > 0 && (size+len(b)+1 > budget || len(batch) == writeBatch) {
			if err := flush(); err != nil {
				return err
			}
		}
		batch = append(batch, r)
		size += len(b) + 1
	}
	return flush()
}

// drain repeats a bounded delete until it removes nothing.
func (s *Store) drain(ctx context.Context, script string, b map[string]any) error {
	for {
		var n []int
		if err := s.write(ctx, script, b, &n); err != nil {
			return err
		}
		if len(n) == 0 || n[0] == 0 {
			return nil
		}
	}
}

// DeleteRepo implements store.Writer.
func (s *Store) DeleteRepo(ctx context.Context, repo string) error {
	s.published.forget(repo)
	if err := s.write(ctx, clearGeneration, map[string]any{"headID": vid("ih", repo)}, nil); err != nil {
		return err
	}
	if err := s.drain(ctx, dropRepo, map[string]any{"repo": repo, "batch": deleteBatch}); err != nil {
		return err
	}
	return s.deleteRuntimeOf(ctx, repo)
}

// toNodes decodes P maps.
func toNodes(maps []map[string]any) []model.Node {
	out := make([]model.Node, 0, len(maps))
	for _, m := range maps {
		out = append(out, toNode(m))
	}
	return out
}

func toNode(m map[string]any) model.Node {
	m["id"] = m["nid"]
	return nodeprops.From(m)
}

// Repos implements store.Reader.
func (s *Store) Repos(ctx context.Context) ([]model.Node, error) {
	var maps []map[string]any
	if err := s.read(ctx, props+`g.V().hasLabel('node').has('kind', 'Repository').toList().collect { P(it) }`, nil, &maps); err != nil {
		return nil, err
	}
	out := toNodes(maps)
	slices.SortFunc(out, func(a, b model.Node) int { return cmp.Compare(a.Name, b.Name) })
	return out[:min(len(out), store.MaxLimit)], nil
}

func (s *Store) repoExists(ctx context.Context, repo string) error {
	var ok []bool
	if err := s.read(ctx, `g.V(v).hasNext()`, map[string]any{"v": nodeVID(model.NodeID(repo, model.KindRepository, repo))}, &ok); err != nil {
		return err
	}
	if len(ok) == 0 || !ok[0] {
		return fmt.Errorf("repo %q: %w", repo, store.ErrNotFound)
	}
	return nil
}

// Node implements store.Reader.
func (s *Store) Node(ctx context.Context, id string) (model.Node, error) {
	var maps []map[string]any
	if err := s.read(ctx, props+`g.V(v).toList().collect { P(it) }`, map[string]any{"v": nodeVID(id)}, &maps); err != nil {
		return model.Node{}, err
	}
	if len(maps) == 0 {
		return model.Node{}, fmt.Errorf("node %q: %w", id, store.ErrNotFound)
	}
	return toNode(maps[0]), nil
}

// FindingCounts implements store.Reader.
func (s *Store) FindingCounts(ctx context.Context, repo string) (map[string]int, error) {
	var out []map[string]int
	err := s.read(ctx, `[g.V().hasLabel('node').has('repo', repo).has('kind', 'Finding').dedup().toList().countBy { it.property('severity').orElse('') }]`,
		map[string]any{"repo": repo}, &out)
	if err != nil || len(out) == 0 {
		return map[string]int{}, err
	}
	return out[0], nil
}

const searchScript = props + `def t = g.V().hasLabel('node').has('repo', repo)
if (kinds) { t = t.has('kind', within(kinds)) }
def out = []
// dedup: a lazily cleaned index can return a re-written vertex twice.
t.dedup().toList().each { v ->
  def name = v.property('name').orElse('').toLowerCase()
  def qn = v.property('qualified_name').orElse('')
  if (name.contains(q) || qn.toLowerCase().contains(q)) { out << [v, name == q ? 0 : 1, qn.length(), v.value('nid')] }
}
out.sort { a, b -> a[1] <=> b[1] ?: a[2] <=> b[2] ?: a[3] <=> b[3] }
out.take(limit as int).collect { P(it[0]) }`

// Search implements store.Reader.
func (s *Store) Search(ctx context.Context, repo, q string, kinds []model.NodeKind, limit int) ([]model.Node, error) {
	if err := s.repoExists(ctx, repo); err != nil {
		return nil, err
	}
	var maps []map[string]any
	err := s.read(ctx, searchScript, map[string]any{"repo": repo, "q": lower(q), "kinds": kindStrings(kinds), "limit": store.ClampLimit(limit, 20)}, &maps)
	return toNodes(maps), err
}

const nodesInFileScript = props + `def out = g.V().hasLabel('node').has('repo', repo).has('file', file).dedup().toList().findAll { v ->
  v.property('start_line').orElse(0L) > 0 && !(v.value('kind') in ['File', 'Document', 'Config', 'Template'])
}
out.sort { a, b -> a.property('start_line').orElse(0L) <=> b.property('start_line').orElse(0L) ?: a.value('nid') <=> b.value('nid') }
out.take(limit as int).collect { P(it) }`

// NodesInFile implements store.Reader.
func (s *Store) NodesInFile(ctx context.Context, repo, file string) ([]model.Node, error) {
	if err := s.repoExists(ctx, repo); err != nil {
		return nil, err
	}
	var maps []map[string]any
	err := s.read(ctx, nodesInFileScript, map[string]any{"repo": repo, "file": file, "limit": store.MaxLimit}, &maps)
	return toNodes(maps), err
}

// traverseScript is a breadth-first search, which yields shortest distances
// directly, as the memory store does.
const traverseScript = props + `if (!g.V(start).hasNext()) { return [[found: false]] }
def seen = [start] as Set
def frontier = [start]
def hits = []
for (d in 1..depth) {
  if (frontier.isEmpty()) { break }
  def t = g.V(frontier as Object[])
  t = inward ? t.in(labels as String[]) : t.out(labels as String[])
  def next = []
  t.toList().each { v -> def id = v.id().toString(); if (seen.add(id)) { hits << [v, d]; next << id } }
  frontier = next
}
hits.sort { a, b -> a[1] <=> b[1] ?: a[0].value('nid') <=> b[0].value('nid') }
[[found: true, hits: hits.take(limit as int).collect { [P(it[0]), it[1]] }]]`

// Traverse implements store.Reader.
func (s *Store) Traverse(ctx context.Context, id string, edges []model.EdgeKind, dir store.Direction, depth, limit int) ([]model.Hit, error) {
	if len(edges) == 0 {
		return nil, errors.New("traverse needs at least one edge kind")
	}
	labels := make([]string, 0, len(edges))
	for _, e := range edges {
		if !e.Valid() {
			return nil, fmt.Errorf("invalid edge kind %q", e)
		}
		labels = append(labels, string(e))
	}
	var res []struct {
		Found bool    `json:"found"`
		Hits  [][]any `json:"hits"`
	}
	err := s.read(ctx, traverseScript, map[string]any{"start": nodeVID(id), "labels": labels, "inward": dir == store.In,
		"depth": store.ClampDepth(depth), "limit": store.ClampLimit(limit, 50)}, &res)
	if err != nil {
		return nil, err
	}
	if len(res) == 0 || !res[0].Found {
		return nil, fmt.Errorf("node %q: %w", id, store.ErrNotFound)
	}
	var hits []model.Hit
	for _, h := range res[0].Hits {
		m, _ := h[0].(map[string]any)
		hits = append(hits, model.Hit{Node: toNode(m), Depth: nodeprops.Int(h[1])})
	}
	return hits, nil
}

// listScript mirrors memory.Matches and the stores' metric rules.
const listScript = props + `def t = g.V().hasLabel('node').has('repo', repo)
if (kinds) { t = t.has('kind', within(kinds)) }
def out = []
t.dedup().toList().each { v ->
  if (tag && !v.property('security_tags').orElse([]).contains(tag)) { return }
  if (prefix && !v.property('file').orElse('').startsWith(prefix)) { return }
  if (exported && !v.property('exported').orElse(false)) { return }
  if (undocumented && v.property('summary').orElse('') != '') { return }
  if (notests && v.property('is_test').orElse(false)) { return }
  def score = 0L
  switch (metric) {
    case 'complexity': score = v.property('complexity').orElse(0L); break
    case 'churn': score = v.property('churn').orElse(0L); break
    case 'loc': score = v.property('loc').orElse(0L); break
    case 'fan_in': score = v.edges(Direction.IN, 'CALLS').size(); break
    case 'authored': score = v.edges(Direction.OUT, 'AUTHORED').size(); break
  }
  out << [v, score]
}
def nid = { it[0].value('nid') }
switch (metric) {
  case 'name':
    def dn = { def v = it[0]; def q = v.property('qualified_name').orElse(''); q ?: v.property('name').orElse('') }
    out.sort { a, b -> dn(a) <=> dn(b) ?: nid(a) <=> nid(b) }; break
  case 'time':
    out.sort { a, b -> b[0].property('time').orElse('') <=> a[0].property('time').orElse('') ?: nid(a) <=> nid(b) }; break
  default:
    out.sort { a, b -> b[1] <=> a[1] ?: nid(a) <=> nid(b) }
}
out.take(limit as int).collect { [P(it[0]), it[1]] }`

// List implements store.Reader.
func (s *Store) List(ctx context.Context, repo string, f store.Filter, m store.Metric, limit int) ([]store.Ranked, error) {
	if !m.Valid() {
		return nil, fmt.Errorf("unknown metric %q", m)
	}
	if err := s.repoExists(ctx, repo); err != nil {
		return nil, err
	}
	var rows [][]any
	err := s.read(ctx, listScript, map[string]any{
		"repo": repo, "kinds": kindStrings(f.Kinds), "tag": f.Tag, "prefix": f.FilePrefix, "exported": f.ExportedOnly,
		"undocumented": f.Undocumented, "notests": f.ExcludeTests, "metric": string(m), "limit": store.ClampLimit(limit, 20),
	}, &rows)
	if err != nil {
		return nil, err
	}
	out := make([]store.Ranked, 0, len(rows))
	for _, r := range rows {
		mp, _ := r[0].(map[string]any)
		out = append(out, store.Ranked{Node: toNode(mp), Score: nodeprops.Int(r[1])})
	}
	return out, nil
}

const subgraphScript = `def want = ids as Set
def out = []
g.V(ids as Object[]).toList().each { a ->
  a.edges(Direction.OUT, labels as String[]).each { e ->
    def b = e.inVertex()
    if (want.contains(b.id().toString())) { out << [a.value('nid'), b.value('nid'), e.label(), e.property('count').orElse(1L), e.property('evidence').orElse('')] }
  }
}
out`

const dependencyScript = props + `def mods = g.V().hasLabel('node').has('repo', repo).has('kind', 'Module').dedup().toList()
mods.sort { it.value('nid') }
def vulns = g.V().hasLabel('node').has('repo', repo).has('kind', 'Vulnerability').dedup().toList()
vulns.sort { it.value('nid') }
def ids = mods.collect { it.id().toString() } as Set
def edges = []
mods.each { a ->
  a.edges(Direction.OUT, 'DEPENDS_ON').each { e ->
    def b = e.inVertex()
    if (ids.contains(b.id().toString())) { edges << [a.value('nid'), b.value('nid')] }
  }
}
edges.sort { x, y -> x[0] <=> y[0] ?: x[1] <=> y[1] }
[[modules: mods.take(limit as int).collect { P(it) }, vulns: vulns.take(limit as int).collect { P(it) }, edges: edges.take(elimit as int),
  more: mods.size() > limit || vulns.size() > limit || edges.size() > elimit]]`

// DependencyGraph implements store.Reader.
func (s *Store) DependencyGraph(ctx context.Context, repo string) (store.DependencyGraph, error) {
	if err := s.repoExists(ctx, repo); err != nil {
		return store.DependencyGraph{}, err
	}
	var res []struct {
		Modules []map[string]any `json:"modules"`
		Vulns   []map[string]any `json:"vulns"`
		Edges   [][]string       `json:"edges"`
		More    bool             `json:"more"`
	}
	if err := s.read(ctx, dependencyScript, map[string]any{"repo": repo, "limit": store.MaxDependencies, "elimit": store.MaxDependencyEdges}, &res); err != nil {
		return store.DependencyGraph{}, err
	}
	g := store.DependencyGraph{}
	if len(res) == 0 {
		return g, nil
	}
	g.Modules, g.Vulnerabilities, g.Truncated = toNodes(res[0].Modules), toNodes(res[0].Vulns), res[0].More
	for _, e := range res[0].Edges {
		if len(e) == 2 {
			g.Edges = append(g.Edges, model.Edge{From: e[0], To: e[1], Kind: model.EdgeDependsOn, Count: 1})
		}
	}
	return g, nil
}

// Subgraph implements store.Reader.
func (s *Store) Subgraph(ctx context.Context, ids []string, kinds []model.EdgeKind) ([]model.Edge, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	if len(ids) > store.MaxLimit*4 {
		return nil, fmt.Errorf("subgraph accepts at most %d ids", store.MaxLimit*4)
	}
	vids := make([]string, len(ids))
	for i, id := range ids {
		vids[i] = nodeVID(id)
	}
	labels := make([]string, 0, len(kinds))
	for _, k := range kinds {
		if !k.Valid() {
			return nil, fmt.Errorf("invalid edge kind %q", k)
		}
		labels = append(labels, string(k))
	}
	var rows [][]any
	if err := s.read(ctx, subgraphScript, map[string]any{"ids": vids, "labels": labels}, &rows); err != nil {
		return nil, err
	}
	out := make([]model.Edge, 0, len(rows))
	for _, r := range rows {
		from, _ := r[0].(string)
		to, _ := r[1].(string)
		kind, _ := r[2].(string)
		out = append(out, model.Edge{From: from, To: to, Kind: model.EdgeKind(kind), Count: nodeprops.Int(r[3]), Evidence: rowEvidence(r)})
	}
	slices.SortFunc(out, func(a, b model.Edge) int {
		return cmp.Or(cmp.Compare(a.From, b.From), cmp.Compare(a.To, b.To), cmp.Compare(a.Kind, b.Kind))
	})
	return out[:min(len(out), store.MaxSubgraphEdges)], nil
}

// statsScript groups the repo's nodes by what the Stats rules look at and
// returns sums per group; store.StatsBuilder applies the rules.
const statsScript = `def groups = [:]
def tags = [:]
g.V().hasLabel('node').has('repo', repo).dedup().toList().each { v ->
  def kind = v.value('kind')
  def test = v.property('is_test').orElse(false)
  def callable = kind == 'Function' || kind == 'Method'
  def pkg = v.property('package').orElse('')
  def key = [kind, test, v.property('exported').orElse(false), v.property('summary').orElse('') != '',
    v.property('language').orElse(''), kind == 'Template' ? pkg : (pkg ? 'x' : ''),
    kind == 'ConfigEntry' ? v.property('signature').orElse('') : '']
  def gr = groups[key]
  if (gr == null) { gr = [0L, 0L, 0L]; groups[key] = gr }
  gr[0] += 1L; gr[1] += v.property('loc').orElse(0L); gr[2] += v.property('complexity').orElse(0L)
  if (callable && !test) { v.property('security_tags').orElse([]).each { tags[it] = (tags[it] ?: 0L) + 1L } }
}
def edges = g.V().hasLabel('node').has('repo', repo).dedup().outE().groupCount().by(label).next()
[[groups: groups.collect { k, c -> k + c }, tags: tags, edges: edges]]`

// Stats implements store.Reader.
func (s *Store) Stats(ctx context.Context, repo string) (model.Stats, error) {
	if err := s.repoExists(ctx, repo); err != nil {
		return model.Stats{}, err
	}
	var res []struct {
		Groups [][]any        `json:"groups"`
		Tags   map[string]any `json:"tags"`
		Edges  map[string]any `json:"edges"`
	}
	if err := s.read(ctx, statsScript, map[string]any{"repo": repo}, &res); err != nil {
		return model.Stats{}, err
	}
	b := store.NewStatsBuilder(repo)
	if len(res) == 0 {
		return b.Stats(), nil
	}
	for _, gr := range res[0].Groups {
		if len(gr) != 10 {
			return model.Stats{}, errors.New("hugegraph: unexpected stats row")
		}
		str := func(i int) string { v, _ := gr[i].(string); return v }
		flag := func(i int) bool { v, _ := gr[i].(bool); return v }
		n := model.Node{Kind: model.NodeKind(str(0)), IsTest: flag(1), Exported: flag(2), Language: str(4), Package: str(5),
			Signature: str(6), LOC: nodeprops.Int(gr[8]), Complexity: nodeprops.Int(gr[9])}
		if flag(3) {
			n.Summary = "x"
		}
		b.Add(n, nodeprops.Int(gr[7]))
	}
	tags := map[string]int{}
	for t, c := range res[0].Tags {
		tags[t] = nodeprops.Int(c)
	}
	b.AddTags(tags)
	for k, c := range res[0].Edges {
		b.AddEdges(model.EdgeKind(k), nodeprops.Int(c))
	}
	return b.Stats(), nil
}

func kindStrings(kinds []model.NodeKind) []string {
	out := make([]string, 0, len(kinds))
	for _, k := range kinds {
		out = append(out, string(k))
	}
	return out
}
