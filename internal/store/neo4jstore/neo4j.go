// Package neo4jstore implements store.Store on Neo4j 5.
//
// Query safety: every value goes through Cypher parameters. Labels,
// relationship types, traversal depth and ORDER BY expressions cannot be
// parameterized in Cypher, so they are only ever taken from the allowlists in
// package model and the metric table below, never from caller strings.
package neo4jstore

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/neo4j/neo4j-go-driver/v5/neo4j"
	"github.com/neo4j/neo4j-go-driver/v5/neo4j/dbtype"

	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/sessionmemory"
	"github.com/astrona-io/codesynapse/internal/store"
	"github.com/astrona-io/codesynapse/internal/store/nodeprops"
	"github.com/astrona-io/codesynapse/internal/telemetry"
)

const (
	writeBatch  = 1000
	deleteBatch = 5000
)

// Config holds connection settings. Password is never logged.
type Config struct {
	URI      string
	User     string
	Password string
	Database string
	// QueryTimeout bounds each read query; 0 means 30s.
	QueryTimeout time.Duration
}

// Store is a Neo4j-backed store.Store.
type Store struct {
	sessionmemory.Engine
	driver  neo4j.DriverWithContext
	db      string
	timeout time.Duration
}

var _ store.Store = (*Store)(nil)

// New connects to Neo4j and verifies connectivity.
func New(ctx context.Context, cfg Config) (*Store, error) {
	if cfg.URI == "" {
		return nil, errors.New("neo4j uri is required")
	}
	driver, err := neo4j.NewDriverWithContext(cfg.URI, neo4j.BasicAuth(cfg.User, cfg.Password, ""))
	if err != nil {
		return nil, fmt.Errorf("creating neo4j driver: %w", err)
	}
	if err := driver.VerifyConnectivity(ctx); err != nil {
		_ = driver.Close(ctx) //nolint:errcheck // connectivity error is the one worth returning
		return nil, fmt.Errorf("connecting to neo4j at %s: %w", cfg.URI, err)
	}
	timeout := cfg.QueryTimeout
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	s := &Store{driver: driver, db: cfg.Database, timeout: timeout}
	s.Engine = sessionmemory.Engine{Backend: s}
	return s, nil
}

// Close implements store.Store.
func (s *Store) Close(ctx context.Context) error { return s.driver.Close(ctx) }

// System names the database for metrics.
func (s *Store) System() string { return "neo4j" }

func (s *Store) write(ctx context.Context, query string, params map[string]any) (*neo4j.EagerResult, error) {
	defer observe(ctx, "write", time.Now())
	res, err := neo4j.ExecuteQuery(ctx, s.driver, query, params, neo4j.EagerResultTransformer,
		neo4j.ExecuteQueryWithDatabase(s.db))
	failed(ctx, "write", err)
	return res, err
}

func (s *Store) read(ctx context.Context, query string, params map[string]any) (*neo4j.EagerResult, error) {
	defer observe(ctx, "read", time.Now())
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	res, err := neo4j.ExecuteQuery(ctx, s.driver, query, params, neo4j.EagerResultTransformer,
		neo4j.ExecuteQueryWithDatabase(s.db), neo4j.ExecuteQueryWithReadersRouting())
	failed(ctx, "read", err)
	if err == nil {
		telemetry.GraphTraversed.AddContext(ctx, float64(len(res.Records)), telemetry.Tenant(ctx), "neo4j")
		telemetry.GraphRowsReturned.AddContext(ctx, float64(len(res.Records)), telemetry.Tenant(ctx), "neo4j")
	}
	return res, err
}

func observe(ctx context.Context, op string, start time.Time) {
	telemetry.DBDuration.ObserveContext(ctx, time.Since(start).Seconds(), telemetry.Tenant(ctx), "neo4j", op)
}

func failed(ctx context.Context, op string, err error) {
	if err != nil {
		telemetry.DBErrors.IncContext(ctx, telemetry.Tenant(ctx), "neo4j", op)
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			telemetry.DBTimeouts.IncContext(ctx, telemetry.Tenant(ctx), "neo4j", op)
		}
	}
}

// EnsureSchema implements store.Writer.
func (s *Store) EnsureSchema(ctx context.Context) error {
	stmts := []string{
		"CREATE CONSTRAINT ingest_head_repo IF NOT EXISTS FOR (h:IngestHead) REQUIRE h.repo IS UNIQUE",
		"CREATE CONSTRAINT session_scope IF NOT EXISTS FOR (s:SessionMemoryScope) REQUIRE (s.tenant_id,s.session_id,s.ns) IS UNIQUE",
		"CREATE CONSTRAINT session_tenant IF NOT EXISTS FOR (s:SessionMemoryTenant) REQUIRE s.tenant_id IS UNIQUE",
		"CREATE CONSTRAINT session_revoked IF NOT EXISTS FOR (s:SessionMemoryRevocation) REQUIRE (s.tenant_id,s.session_id) IS UNIQUE",
		"CREATE CONSTRAINT session_entity IF NOT EXISTS FOR (s:SessionMemoryEntity) REQUIRE s.id IS UNIQUE",
		"CREATE INDEX session_entity_scope IF NOT EXISTS FOR (s:SessionMemoryEntity) ON (s.tenant_id,s.session_id,s.ns)",
		"CREATE INDEX session_fact_scope IF NOT EXISTS FOR ()-[s:SESSION_FACT]-() ON (s.tenant_id,s.session_id,s.ns)",
		"CREATE CONSTRAINT access_vertex_id IF NOT EXISTS FOR (n:AccessVertex) REQUIRE n.aid IS UNIQUE",
		"CREATE INDEX access_vertex_tenant IF NOT EXISTS FOR (n:AccessVertex) ON (n.tenant_id)",
		"DROP CONSTRAINT access_head_tenant IF EXISTS",
		"CREATE CONSTRAINT access_head_id IF NOT EXISTS FOR (n:AccessHead) REQUIRE n.aid IS UNIQUE",
		"CREATE INDEX access_head_tenant_revision IF NOT EXISTS FOR (n:AccessHead) ON (n.tenant_id, n.policy_revision)",
		"CREATE CONSTRAINT node_id IF NOT EXISTS FOR (n:Node) REQUIRE n.id IS UNIQUE",
		"CREATE INDEX node_repo IF NOT EXISTS FOR (n:Node) ON (n.repo)",
		"CREATE INDEX node_repo_file IF NOT EXISTS FOR (n:Node) ON (n.repo, n.file)",
		"CREATE INDEX node_repo_kind IF NOT EXISTS FOR (n:Node) ON (n.repo, n.kind)",
		"CREATE TEXT INDEX node_name IF NOT EXISTS FOR (n:Node) ON (n.name)",
		"CREATE CONSTRAINT review_id IF NOT EXISTS FOR (r:Review) REQUIRE r.id IS UNIQUE",
		"CREATE INDEX review_repo IF NOT EXISTS FOR (r:Review) ON (r.repo, r.created_at)",
		"CREATE INDEX curated_repo_kind IF NOT EXISTS FOR (c:Curated) ON (c.repo, c.kind, c.created_at)",
		"CREATE INDEX curated_id IF NOT EXISTS FOR (c:Curated) ON (c.id)",
		"CREATE INDEX snapshot_repo_at IF NOT EXISTS FOR (s:Snapshot) ON (s.repo, s.at)",
		"CREATE CONSTRAINT memory_entity_id IF NOT EXISTS FOR (e:MemoryEntity) REQUIRE e.id IS UNIQUE",
		"CREATE INDEX memory_entity_repo IF NOT EXISTS FOR (e:MemoryEntity) ON (e.repo)",
		"CREATE INDEX memory_fact_id IF NOT EXISTS FOR ()-[f:FACT]-() ON (f.id)",
		"CREATE INDEX memory_fact_repo IF NOT EXISTS FOR ()-[f:FACT]-() ON (f.repo)",
		"CREATE CONSTRAINT mcp_usage_key IF NOT EXISTS FOR (u:McpUsage) REQUIRE u.key IS UNIQUE",
		"CREATE INDEX mcp_usage_day IF NOT EXISTS FOR (u:McpUsage) ON (u.day)",
	}
	for _, q := range stmts {
		if _, err := s.write(ctx, q, nil); err != nil {
			return fmt.Errorf("schema %q: %w", q, err)
		}
	}
	return nil
}

func newRunID() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// ReplaceRepo commits every batch and stale-row deletion in one transaction.
func (s *Store) ReplaceRepo(ctx context.Context, repo string, g *model.Graph) error {
	nodes, edges, err := store.Replacement(ctx, repo, g)
	if err != nil {
		return err
	}
	if err := store.CommitCheck(ctx); err != nil {
		return err
	}
	run, err := newRunID()
	if err != nil {
		return fmt.Errorf("generating run id: %w", err)
	}
	byKind := map[model.NodeKind][]map[string]any{}
	for _, n := range nodes {
		byKind[n.Kind] = append(byKind[n.Kind], map[string]any{"id": n.ID, "props": nodeprops.To(n)})
	}
	byEdge := map[model.EdgeKind][]map[string]any{}
	for _, e := range edges {
		byEdge[e.Kind] = append(byEdge[e.Kind], map[string]any{"from": e.From, "to": e.To, "count": int64(e.Count)})
	}
	sess := s.driver.NewSession(ctx, neo4j.SessionConfig{DatabaseName: s.db})
	defer func() { _ = sess.Close(ctx) }()
	defer observe(ctx, "write", time.Now())
	_, err = sess.ExecuteWrite(ctx, func(tx neo4j.ManagedTransaction) (any, error) {
		// The head serializes competing replacements, including empty graphs.
		if _, err := tx.Run(ctx, `MERGE (h:IngestHead {repo:$repo}) SET h.run=$run`, map[string]any{"repo": repo, "run": run}); err != nil {
			return nil, err
		}
		batch := func(query string, rows []map[string]any) error {
			for start := 0; start < len(rows); start += writeBatch {
				res, err := tx.Run(ctx, query, map[string]any{"rows": rows[start:min(start+writeBatch, len(rows))], "run": run})
				if err != nil {
					return err
				}
				if _, err = res.Consume(ctx); err != nil {
					return err
				}
			}
			return nil
		}
		for _, kind := range model.AllNodeKinds {
			q := fmt.Sprintf(`UNWIND $rows AS r MERGE (n:Node {id:r.id}) SET n=r.props,n.ingest_run=$run SET n:%s`, kind)
			if err := batch(q, byKind[kind]); err != nil {
				return nil, fmt.Errorf("write replacement nodes: %w", err)
			}
		}
		for _, kind := range model.AllEdgeKinds {
			q := fmt.Sprintf(`UNWIND $rows AS r MATCH (a:Node {id:r.from}) MATCH (b:Node {id:r.to}) MERGE (a)-[e:%s]->(b) SET e.count=r.count,e.ingest_run=$run`, kind)
			if err := batch(q, byEdge[kind]); err != nil {
				return nil, fmt.Errorf("write replacement edges: %w", err)
			}
		}
		for _, q := range []string{
			`MATCH (a:Node {repo:$repo})-[e]->() WHERE e.ingest_run IS NULL OR e.ingest_run<>$run DELETE e`,
			`MATCH (n:Node {repo:$repo}) WHERE n.ingest_run IS NULL OR n.ingest_run<>$run DETACH DELETE n`,
		} {
			res, err := tx.Run(ctx, q, map[string]any{"repo": repo, "run": run})
			if err != nil {
				return nil, err
			}
			if _, err = res.Consume(ctx); err != nil {
				return nil, err
			}
		}
		return nil, store.CommitCheck(ctx)
	})
	failed(ctx, "write", err)
	return err
}

func (s *Store) batched(ctx context.Context, q string, rows []map[string]any, extra map[string]any) error {
	for start := 0; start < len(rows); start += writeBatch {
		params := map[string]any{"rows": rows[start:min(start+writeBatch, len(rows))]}
		for k, v := range extra {
			params[k] = v
		}
		if _, err := s.write(ctx, q, params); err != nil {
			return err
		}
	}
	return nil
}

// drain repeats a bounded delete until it affects nothing, keeping each
// transaction small on large repositories.
func (s *Store) drain(ctx context.Context, q string, params map[string]any) error {
	for {
		res, err := s.write(ctx, q, params)
		if err != nil {
			return err
		}
		if len(res.Records) == 0 {
			return nil
		}
		c, _ := res.Records[0].Get("c")
		if n, ok := c.(int64); !ok || n == 0 {
			return nil
		}
	}
}

// DeleteRepo implements store.Writer.
func (s *Store) DeleteRepo(ctx context.Context, repo string) error {
	return s.drain(ctx, `MATCH (n:Node {repo: $repo})
WITH n LIMIT $batch DETACH DELETE n RETURN count(*) AS c`, map[string]any{"repo": repo, "batch": deleteBatch})
}

// Repos implements store.Reader.
func (s *Store) Repos(ctx context.Context) ([]model.Node, error) {
	res, err := s.read(ctx, "MATCH (n:Repository) RETURN n ORDER BY n.name LIMIT $limit", map[string]any{"limit": store.MaxLimit})
	if err != nil {
		return nil, err
	}
	return nodes(res)
}

func (s *Store) repoExists(ctx context.Context, repo string) error {
	res, err := s.read(ctx, "MATCH (n:Repository {repo: $repo}) RETURN n.id AS id LIMIT 1", map[string]any{"repo": repo})
	if err != nil {
		return err
	}
	if len(res.Records) == 0 {
		return fmt.Errorf("repo %q: %w", repo, store.ErrNotFound)
	}
	return nil
}

// DependencyGraph implements store.Reader.
func (s *Store) DependencyGraph(ctx context.Context, repo string) (store.DependencyGraph, error) {
	if err := s.repoExists(ctx, repo); err != nil {
		return store.DependencyGraph{}, err
	}
	var g store.DependencyGraph
	for _, q := range []struct {
		kind string
		into *[]model.Node
	}{{"Module", &g.Modules}, {"Vulnerability", &g.Vulnerabilities}} {
		res, err := s.read(ctx, "MATCH (n:Node {repo: $repo, kind: $kind}) RETURN n ORDER BY n.id LIMIT $limit",
			map[string]any{"repo": repo, "kind": q.kind, "limit": store.MaxDependencies + 1})
		if err != nil {
			return g, err
		}
		if *q.into, err = nodes(res); err != nil {
			return g, err
		}
		if len(*q.into) > store.MaxDependencies {
			*q.into, g.Truncated = (*q.into)[:store.MaxDependencies], true
		}
	}
	res, err := s.read(ctx, `MATCH (a:Node {repo: $repo, kind: 'Module'})-[:DEPENDS_ON]->(b:Node {repo: $repo, kind: 'Module'})
RETURN a.id AS from, b.id AS to ORDER BY from, to LIMIT $limit`, map[string]any{"repo": repo, "limit": store.MaxDependencyEdges + 1})
	if err != nil {
		return g, err
	}
	for _, r := range res.Records {
		g.Edges = append(g.Edges, model.Edge{From: str(r, "from"), To: str(r, "to"), Kind: model.EdgeDependsOn, Count: 1})
	}
	if len(g.Edges) > store.MaxDependencyEdges {
		g.Edges, g.Truncated = g.Edges[:store.MaxDependencyEdges], true
	}
	return g, nil
}

// FindingCounts implements store.Reader.
func (s *Store) FindingCounts(ctx context.Context, repo string) (map[string]int, error) {
	res, err := s.read(ctx, "MATCH (n:Finding {repo: $repo}) RETURN n.severity AS severity, count(n) AS n", map[string]any{"repo": repo})
	if err != nil {
		return nil, err
	}
	out := map[string]int{}
	for _, rec := range res.Records {
		n, _ := rec.AsMap()["n"].(int64)
		out[str(rec, "severity")] = int(n)
	}
	return out, nil
}

// Stats implements store.Reader. The aggregation mirrors memory.accumulate.
func (s *Store) Stats(ctx context.Context, repo string) (model.Stats, error) {
	if err := s.repoExists(ctx, repo); err != nil {
		return model.Stats{}, err
	}
	st := model.Stats{Repo: repo, Nodes: map[model.NodeKind]int{}, Edges: map[model.EdgeKind]int{}, SecurityTags: map[string]int{}}
	p := map[string]any{"repo": repo}

	res, err := s.read(ctx, "MATCH (n:Node {repo: $repo}) RETURN n.kind AS k, count(*) AS c", p)
	if err != nil {
		return st, err
	}
	for _, r := range res.Records {
		st.Nodes[model.NodeKind(str(r, "k"))] = num(r, "c")
	}

	res, err = s.read(ctx, "MATCH (n:Node {repo: $repo})-[e]->() RETURN type(e) AS k, count(*) AS c", p)
	if err != nil {
		return st, err
	}
	for _, r := range res.Records {
		st.Edges[model.EdgeKind(str(r, "k"))] = num(r, "c")
	}

	res, err = s.read(ctx, `MATCH (n:Node {repo: $repo})
WITH n, n.kind IN ['Function','Method'] AS callable, coalesce(n.is_test, false) AS test
RETURN
  sum(CASE WHEN n.kind = 'File' THEN coalesce(n.loc, 0) ELSE 0 END) AS loc,
  sum(CASE WHEN callable AND test THEN 1 ELSE 0 END) AS tests,
  sum(CASE WHEN callable AND NOT test THEN coalesce(n.complexity, 0) ELSE 0 END) AS csum,
  sum(CASE WHEN callable AND NOT test THEN 1 ELSE 0 END) AS callables,
  sum(CASE WHEN n.kind IN ['Function','Method','Type'] AND coalesce(n.exported, false) AND NOT test THEN 1 ELSE 0 END) AS exported,
  sum(CASE WHEN n.kind IN ['Function','Method','Type'] AND coalesce(n.exported, false) AND NOT test AND coalesce(n.summary, '') <> '' THEN 1 ELSE 0 END) AS documented`, p)
	if err != nil {
		return st, err
	}
	if len(res.Records) == 1 {
		r := res.Records[0]
		st.TotalLOC, st.Tests, st.Exported, st.Documented = num(r, "loc"), num(r, "tests"), num(r, "exported"), num(r, "documented")
		if c := num(r, "callables"); c > 0 {
			st.AvgComplexity = float64(num(r, "csum")) / float64(c)
		}
	}

	res, err = s.read(ctx, `MATCH (n:Node {repo: $repo})
WHERE n.kind IN ['Function','Method'] AND NOT coalesce(n.is_test, false)
UNWIND coalesce(n.security_tags, []) AS t
RETURN t, count(*) AS c`, p)
	if err != nil {
		return st, err
	}
	for _, r := range res.Records {
		st.SecurityTags[str(r, "t")] = num(r, "c")
	}

	st.Formats = map[string]int{}
	res, err = s.read(ctx, `MATCH (n:Node {repo: $repo})
WHERE n.kind IN ['File','Config','Template','Document']
RETURN n.kind AS k, coalesce(n.language, '') AS l, coalesce(n.package, '') <> '' AS chart, count(*) AS c`, p)
	if err != nil {
		return st, err
	}
	for _, r := range res.Records {
		chart, _ := r.Get("chart")
		isChart, _ := chart.(bool)
		st.Formats[model.FileFormat(model.NodeKind(str(r, "k")), str(r, "l"), isChart)] += num(r, "c")
	}
	res, err = s.read(ctx, `MATCH (n:Node {repo: $repo})
RETURN
  count(DISTINCT CASE WHEN n.kind = 'Template' AND coalesce(n.package, '') <> '' THEN n.package END) AS charts,
  sum(CASE WHEN n.kind = 'ConfigEntry' AND NOT coalesce(n.signature, '') IN ['', 'map', 'list', 'scalar', 'alias'] THEN 1 ELSE 0 END) AS k8s`, p)
	if err != nil {
		return st, err
	}
	if len(res.Records) == 1 {
		st.HelmCharts, st.K8sResources = num(res.Records[0], "charts"), num(res.Records[0], "k8s")
	}
	return st, nil
}

// Node implements store.Reader.
func (s *Store) Node(ctx context.Context, id string) (model.Node, error) {
	res, err := s.read(ctx, "MATCH (n:Node {id: $id}) RETURN n", map[string]any{"id": id})
	if err != nil {
		return model.Node{}, err
	}
	ns, err := nodes(res)
	if err != nil {
		return model.Node{}, err
	}
	if len(ns) == 0 {
		return model.Node{}, fmt.Errorf("node %q: %w", id, store.ErrNotFound)
	}
	return ns[0], nil
}

func kindStrings(kinds []model.NodeKind) []string {
	out := make([]string, 0, len(kinds))
	for _, k := range kinds {
		out = append(out, string(k))
	}
	return out
}

// Search implements store.Reader.
func (s *Store) Search(ctx context.Context, repo, q string, kinds []model.NodeKind, limit int) ([]model.Node, error) {
	if err := s.repoExists(ctx, repo); err != nil {
		return nil, err
	}
	res, err := s.read(ctx, `MATCH (n:Node {repo: $repo})
WHERE (size($kinds) = 0 OR n.kind IN $kinds)
  AND (toLower(n.name) CONTAINS $q OR toLower(coalesce(n.qualified_name, '')) CONTAINS $q)
RETURN n
ORDER BY CASE WHEN toLower(n.name) = $q THEN 0 ELSE 1 END, size(coalesce(n.qualified_name, '')), n.id
LIMIT $limit`, map[string]any{
		"repo": repo, "q": strings.ToLower(q), "kinds": kindStrings(kinds), "limit": store.ClampLimit(limit, 20),
	})
	if err != nil {
		return nil, err
	}
	return nodes(res)
}

// NodesInFile implements store.Reader.
func (s *Store) NodesInFile(ctx context.Context, repo, file string) ([]model.Node, error) {
	if err := s.repoExists(ctx, repo); err != nil {
		return nil, err
	}
	res, err := s.read(ctx, `MATCH (n:Node {repo: $repo, file: $file})
WHERE coalesce(n.start_line, 0) > 0 AND NOT n.kind IN ['File', 'Document', 'Config', 'Template']
RETURN n ORDER BY n.start_line, n.id LIMIT $limit`,
		map[string]any{"repo": repo, "file": file, "limit": store.MaxLimit})
	if err != nil {
		return nil, err
	}
	return nodes(res)
}

// Traverse implements store.Reader.
func (s *Store) Traverse(ctx context.Context, id string, edges []model.EdgeKind, dir store.Direction, depth, limit int) ([]model.Hit, error) {
	if len(edges) == 0 {
		return nil, errors.New("traverse needs at least one edge kind")
	}
	types := make([]string, 0, len(edges))
	for _, e := range edges {
		if !e.Valid() {
			return nil, fmt.Errorf("invalid edge kind %q", e)
		}
		types = append(types, string(e))
	}
	left, right := "-", "->"
	if dir == store.In {
		left, right = "<-", "-"
	}
	// Relationship types and depth are allowlisted/clamped above.
	q := fmt.Sprintf(`MATCH (s:Node {id: $id})
MATCH p = (s)%s[:%s*1..%d]%s(m:Node)
WHERE m <> s
WITH m, min(length(p)) AS depth
RETURN m, depth ORDER BY depth, m.id LIMIT $limit`,
		left, strings.Join(types, "|"), store.ClampDepth(depth), right)
	res, err := s.read(ctx, q, map[string]any{"id": id, "limit": store.ClampLimit(limit, 50)})
	if err != nil {
		return nil, err
	}
	if len(res.Records) == 0 {
		if _, err := s.Node(ctx, id); err != nil {
			return nil, err
		}
		return nil, nil
	}
	hits := make([]model.Hit, 0, len(res.Records))
	for _, r := range res.Records {
		n, err := nodeFrom(r, "m")
		if err != nil {
			return nil, err
		}
		hits = append(hits, model.Hit{Node: n, Depth: num(r, "depth")})
	}
	return hits, nil
}

// metricCypher maps each metric to its score expression and ORDER BY clause.
var metricCypher = map[store.Metric]struct{ score, order string }{
	store.ByName:       {"0", "coalesce(n.qualified_name, n.name), n.id"},
	store.ByComplexity: {"coalesce(n.complexity, 0)", "score DESC, n.id"},
	store.ByChurn:      {"coalesce(n.churn, 0)", "score DESC, n.id"},
	store.ByLOC:        {"coalesce(n.loc, 0)", "score DESC, n.id"},
	store.ByFanIn:      {"size([(n)<-[:CALLS]-() | 1])", "score DESC, n.id"},
	store.ByTime:       {"0", "n.time DESC, n.id"},
	store.ByAuthored:   {"size([(n)-[:AUTHORED]->() | 1])", "score DESC, n.id"},
}

// List implements store.Reader. Filter semantics mirror memory.Matches.
func (s *Store) List(ctx context.Context, repo string, f store.Filter, m store.Metric, limit int) ([]store.Ranked, error) {
	mc, ok := metricCypher[m]
	if !ok {
		return nil, fmt.Errorf("unknown metric %q", m)
	}
	if err := s.repoExists(ctx, repo); err != nil {
		return nil, err
	}
	q := fmt.Sprintf(`MATCH (n:Node {repo: $repo})
WHERE (size($kinds) = 0 OR n.kind IN $kinds)
  AND ($tag = '' OR $tag IN coalesce(n.security_tags, []))
  AND ($prefix = '' OR coalesce(n.file, '') STARTS WITH $prefix)
  AND (NOT $exported OR coalesce(n.exported, false))
  AND (NOT $undocumented OR coalesce(n.summary, '') = '')
  AND (NOT $notests OR NOT coalesce(n.is_test, false))
WITH n, %s AS score
RETURN n, score ORDER BY %s LIMIT $limit`, mc.score, mc.order)
	res, err := s.read(ctx, q, map[string]any{
		"repo": repo, "kinds": kindStrings(f.Kinds), "tag": f.Tag, "prefix": f.FilePrefix,
		"exported": f.ExportedOnly, "undocumented": f.Undocumented, "notests": f.ExcludeTests,
		"limit": store.ClampLimit(limit, 20),
	})
	if err != nil {
		return nil, err
	}
	out := make([]store.Ranked, 0, len(res.Records))
	for _, r := range res.Records {
		n, err := nodeFrom(r, "n")
		if err != nil {
			return nil, err
		}
		out = append(out, store.Ranked{Node: n, Score: num(r, "score")})
	}
	return out, nil
}

// Subgraph implements store.Reader.
func (s *Store) Subgraph(ctx context.Context, ids []string, kinds []model.EdgeKind) ([]model.Edge, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	if len(ids) > store.MaxLimit*4 {
		return nil, fmt.Errorf("subgraph accepts at most %d ids", store.MaxLimit*4)
	}
	res, err := s.read(ctx, `MATCH (a:Node)-[e]->(b:Node)
WHERE a.id IN $ids AND b.id IN $ids AND (size($kinds) = 0 OR type(e) IN $kinds)
RETURN a.id AS from, b.id AS to, type(e) AS kind, coalesce(e.count, 1) AS count
ORDER BY from, to, kind LIMIT $limit`, map[string]any{
		"ids": ids, "kinds": edgeStrings(kinds), "limit": store.MaxSubgraphEdges,
	})
	if err != nil {
		return nil, err
	}
	out := make([]model.Edge, 0, len(res.Records))
	for _, r := range res.Records {
		out = append(out, model.Edge{From: str(r, "from"), To: str(r, "to"), Kind: model.EdgeKind(str(r, "kind")), Count: num(r, "count")})
	}
	return out, nil
}

func edgeStrings(kinds []model.EdgeKind) []string {
	out := make([]string, 0, len(kinds))
	for _, k := range kinds {
		out = append(out, string(k))
	}
	return out
}

// nodes decodes column "n" of every record.
func nodes(res *neo4j.EagerResult) ([]model.Node, error) {
	out := make([]model.Node, 0, len(res.Records))
	for _, r := range res.Records {
		n, err := nodeFrom(r, "n")
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, nil
}

func nodeFrom(r *neo4j.Record, key string) (model.Node, error) {
	v, ok := r.Get(key)
	if !ok {
		return model.Node{}, fmt.Errorf("record has no %q", key)
	}
	dn, ok := v.(dbtype.Node)
	if !ok {
		return model.Node{}, fmt.Errorf("%q is %T, not a node", key, v)
	}
	return nodeprops.From(dn.Props), nil
}

func str(r *neo4j.Record, key string) string {
	v, _ := r.Get(key)
	s, _ := v.(string)
	return s
}

func num(r *neo4j.Record, key string) int {
	v, _ := r.Get(key)
	switch n := v.(type) {
	case int64:
		return int(n)
	case float64:
		return int(n)
	}
	return 0
}
