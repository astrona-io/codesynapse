package nebulastore

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	nebula "github.com/vesoft-inc/nebula-go/v3"

	"github.com/astrona-io/codesynapse/internal/model"
)

// edgeKinds are the model edge kinds stored as edge types. Session facts
// live in their own tag, not in the code graph.
func edgeKinds() []string {
	var out []string
	for _, k := range model.AllEdgeKinds {
		if k != model.EdgeSessionFact {
			out = append(out, string(k))
		}
	}
	return out
}

// Node property types. TestSchemaCoversNodeProps keeps them in step with
// package nodeprops; "id" is stored as "nid". Lists are JSON text.
var (
	nodeTexts = []string{"nid", "kind", "repo", "name", "qualified_name", "package", "file", "signature", "summary", "body_hash", "clone_hash", "clone_sig", "via", "doc_type", "status", "meta", "duplicates",
		"time", "head", "root", "branch", "metrics", "severity", "rule", "ignore", "version", "fixed_version", "reachability", "url",
		"liveness", "cleanup", "licenses", "coverage", "language", "ecosystem", "license", "license_source", "symbol_id", "revision", "identities", "analysis", "latest", "deprecated", "retracted", "scorecard_date",
		// The publication run that last wrote the node (replace.go).
		"run"}
	nodeLongs   = []string{"bytes", "start_line", "end_line", "complexity", "loc", "level", "churn", "roots", "behind"}
	nodeBools   = []string{"exported", "is_test", "indirect", "dev", "code_absent"}
	nodeDoubles = []string{"scorecard"}
	nodeLists   = []string{"cwe", "aliases", "security_tags", "scorecard_checks"}
)

// indexedNodeColumns are the node columns the indexes below are made of.
var indexedNodeColumns = []string{"repo", "kind", "file"}

// column is a property of a tag or an edge type. A column with a default is
// NOT NULL, which lets UPSERT add to it on a vertex that does not exist yet.
type column struct{ name, typ, def string }

func texts(names ...string) []column   { return typed("string", names) }
func longs(names ...string) []column   { return typed("int64", names) }
func bools(names ...string) []column   { return typed("bool", names) }
func doubles(names ...string) []column { return typed("double", names) }

// defaulted are columns that are never NULL, so a read needs no nil case.
func defaulted(typ, def string, names ...string) []column {
	out := typed(typ, names)
	for i := range out {
		out[i].def = def
	}
	return out
}

// counters are int64 columns that start at 0.
func counters(names ...string) []column {
	out := longs(names...)
	for i := range out {
		out[i].def = "0"
	}
	return out
}

func typed(typ string, names []string) []column {
	out := make([]column, len(names))
	for i, n := range names {
		out[i] = column{name: n, typ: typ}
	}
	return out
}

// schemaItem is a tag or an edge type with its indexes. Property and index
// names are constants of this package.
type schemaItem struct {
	name    string
	edge    bool
	cols    []column
	indexes []index
}

// index is a native index; prefix lengths are for string properties.
type index struct {
	name string
	cols []indexCol
}

type indexCol struct {
	name   string
	prefix int // string length; 0 for a number
}

func (s schemaItem) kind() string {
	if s.edge {
		return "EDGE"
	}
	return "TAG"
}

// schemaItems is everything the store keeps. A tag or edge type an older
// version created gets the columns it lacks appended.
var schemaItems = func() []schemaItem {
	node := slices.Concat(texts(nodeTexts...), longs(nodeLongs...), bools(nodeBools...), doubles(nodeDoubles...), texts(nodeLists...))
	// A composite index misses rows whose later columns are NULL, and a
	// lookup by one column may be served by any index that starts with it, so
	// every indexed column is never NULL.
	for i := range node {
		if slices.Contains(indexedNodeColumns, node[i].name) {
			node[i].def = `""`
		}
	}
	items := []schemaItem{
		// The lease a publisher holds on a repository (replace.go).
		// Every lease write sets stage_expires; the index lets the size
		// sampler see whether any lease is held (sizes.go).
		{name: "ingest_head", cols: slices.Concat(texts("repo", "stage_run"), counters("stage_expires")),
			indexes: []index{{"ingest_head_by_expires", []indexCol{{"stage_expires", 0}}}}},
		{name: "node", cols: node, indexes: []index{
			{"node_by_repo", []indexCol{{"repo", 64}}},
			{"node_by_kind", []indexCol{{"kind", 24}}},
			{"node_by_repo_kind", []indexCol{{"repo", 64}, {"kind", 24}}},
			{"node_by_repo_file", []indexCol{{"repo", 64}, {"file", 128}}},
		}},
	}
	items = append(items,
		schemaItem{name: "review", cols: texts("rid", "repo", "created_at", "updated_at", "doc"), indexes: []index{{"review_by_repo", []indexCol{{"repo", 64}}}}},
		schemaItem{name: "snapshot", cols: texts("sid", "repo", "at", "doc"), indexes: []index{{"snapshot_by_repo", []indexCol{{"repo", 64}}}}},
		schemaItem{name: "curated", cols: texts("cid", "repo", "kind", "created_at", "doc"), indexes: []index{{"curated_by_repo", []indexCol{{"repo", 64}}}}},
		schemaItem{name: "usage", cols: slices.Concat(texts("ukey", "day", "org", "repo", "tool", "client"), longs("daynum"),
			counters("calls", "errors", "response_tokens", "duration_ms", "max_duration_ms", "ranges_returned", "ranges_read")),
			indexes: []index{{"usage_by_daynum", []indexCol{{"daynum", 0}}}}},
	)
	// Agent memory (memory.go): entities and facts per namespace. Every
	// column is set on write, so none is NULL.
	items = append(items,
		schemaItem{name: "mem_entity", cols: slices.Concat(defaulted("string", `""`, "eid", "tenant_id", "ns", "name", "type", "aliases", "created_at", "updated_at", "emb_model")),
			indexes: []index{{"mem_entity_by_ns", []indexCol{{"tenant_id", 64}, {"ns", 128}}}, {"mem_entity_all", nil}}},
		schemaItem{name: "mem_fact", cols: slices.Concat(defaulted("string", `""`, "fid", "tenant_id", "ns", "subj", "obj", "predicate", "source", "origin_session_id", "valid_from", "valid_to", "created_at", "updated_at"),
			counters("confirmations"), defaulted("double", "1.0", "confidence")),
			indexes: []index{{"mem_fact_by_ns", []indexCol{{"tenant_id", 64}, {"ns", 128}}}, {"mem_fact_all", nil}}},
	)
	// Access projections (access.go): one tag for every kind, and their own
	// edge types, apart from the code graph.
	items = append(items, schemaItem{name: "access_vertex", cols: slices.Concat(texts("aid", "kind", "tenant_id", "user_id", "team_id", "repo_id", "graph_id", "default_repo_access", "digest"),
		longs("policy_revision"), bools("active", "inherits_default", "usable", "personal")),
		indexes: []index{{"access_by_tenant_revision", []indexCol{{"tenant_id", 64}, {"policy_revision", 0}}}, {"access_by_tenant_kind", []indexCol{{"tenant_id", 64}, {"kind", 24}}}}})
	for _, k := range accessEdges {
		items = append(items, schemaItem{name: k, edge: true, cols: slices.Concat(texts("tenant_id", "role"), longs("rank"))})
	}
	// Transient chat memory (session.go): one document per tenant, session
	// and namespace, and a tombstone per revoked execution.
	items = append(items,
		schemaItem{name: "session_scope", cols: slices.Concat(defaulted("string", `""`, "tenant_id", "session_id", "ns", "doc"), counters("revision", "expires", "facts"), defaulted("bool", "false", "deleted")),
			indexes: []index{{"session_by_tenant", []indexCol{{"tenant_id", 64}, {"session_id", 128}}}}},
		schemaItem{name: "session_revoked", cols: defaulted("string", `""`, "tenant_id", "session_id")},
	)
	// Runtime facts (runtime.go): a head per namespace and the chunks of its
	// gzipped document.
	items = append(items,
		schemaItem{name: "runtime_head", cols: slices.Concat(defaulted("string", `""`, "ns", "graph_name", "gen"), counters("chunks")),
			indexes: []index{{"runtime_head_by_graph", []indexCol{{"graph_name", 128}}}}},
		schemaItem{name: "runtime_chunk", cols: slices.Concat(defaulted("string", `""`, "ns", "graph_name", "gen", "data"), counters("idx")),
			indexes: []index{{"runtime_chunk_by_ns", []indexCol{{"ns", 128}, {"gen", 24}}}, {"runtime_chunk_by_graph", []indexCol{{"graph_name", 128}}}}},
	)
	// One edge type per model edge kind (schemaEdges) carries these.
	edge := slices.Concat(longs("count"), texts("run", "evidence"))
	for _, k := range edgeKinds() {
		items = append(items, schemaItem{name: k, edge: true, cols: edge})
	}
	return items
}()

// DDL is built from the constants above; names are quoted with backticks so
// a model name that is also an nGQL keyword (time, level, version) works.
func quote(name string) string { return "`" + ident(name) + "`" }

func (c column) ddl() string {
	if c.def != "" {
		return quote(c.name) + " " + c.typ + " NOT NULL DEFAULT " + c.def
	}
	return quote(c.name) + " " + c.typ + " NULL"
}

func (s schemaItem) create() string {
	cols := make([]string, len(s.cols))
	for i, c := range s.cols {
		cols[i] = c.ddl()
	}
	return fmt.Sprintf("CREATE %s IF NOT EXISTS %s (%s)", s.kind(), quote(s.name), strings.Join(cols, ", "))
}

func (s schemaItem) alter(missing []column) string {
	cols := make([]string, len(missing))
	for i, c := range missing {
		cols[i] = c.ddl()
	}
	return fmt.Sprintf("ALTER %s %s ADD (%s)", s.kind(), quote(s.name), strings.Join(cols, ", "))
}

func (s schemaItem) createIndex(ix index) string {
	cols := make([]string, len(ix.cols))
	for i, c := range ix.cols {
		cols[i] = quote(c.name)
		if c.prefix > 0 {
			cols[i] += fmt.Sprintf("(%d)", c.prefix)
		}
	}
	return fmt.Sprintf("CREATE %s INDEX IF NOT EXISTS %s ON %s (%s)", s.kind(), quote(ix.name), quote(s.name), strings.Join(cols, ", "))
}

// EnsureSchema implements store.Writer. The space, tags, edge types and
// indexes are created when missing and columns appended; nothing is dropped.
// It needs a user that may create spaces (GOD on a fresh install).
func (s *Store) EnsureSchema(ctx context.Context) error {
	create := fmt.Sprintf("CREATE SPACE IF NOT EXISTS %s (partition_num = %d, replica_factor = %d, vid_type = FIXED_STRING(40))",
		quote(s.cfg.Space), s.cfg.Partitions, s.cfg.Replicas)
	if err := s.admin(ctx, func(sess *nebula.Session) error { rs, err := sess.Execute(create); return check(rs, err) }); err != nil {
		return fmt.Errorf("nebula schema: create space: %w", err)
	}
	deadline := time.Now().Add(s.cfg.SchemaWait)
	if err := s.until(ctx, deadline, "the space", func() error { return s.bind() }); err != nil {
		return err
	}
	for _, item := range schemaItems {
		if err := s.ensureItem(ctx, item, deadline); err != nil {
			return fmt.Errorf("nebula schema: %s %s: %w", strings.ToLower(item.kind()), item.name, err)
		}
	}
	return nil
}

func (s *Store) ensureItem(ctx context.Context, item schemaItem, deadline time.Time) error {
	if _, err := s.write(ctx, item.create(), nil); err != nil {
		return err
	}
	// The Graph service learns the schema on its next heartbeat.
	probe := fmt.Sprintf("DESCRIBE %s %s", item.kind(), quote(item.name))
	var have []Row
	if err := s.until(ctx, deadline, item.name, func() (err error) { have, err = s.read(ctx, probe, nil); return }); err != nil {
		return err
	}
	known := map[string]bool{}
	for _, r := range have {
		if f, ok := r["Field"].(string); ok {
			known[f] = true
		}
	}
	var missing []column
	for _, c := range item.cols {
		if !known[c.name] {
			missing = append(missing, c)
		}
	}
	if len(missing) > 0 {
		if _, err := s.write(ctx, item.alter(missing), nil); err != nil {
			return err
		}
		if err := s.until(ctx, deadline, item.name, func() error {
			rows, err := s.read(ctx, probe, nil)
			if err == nil && len(rows) < len(known)+len(missing) {
				err = fmt.Errorf("columns not visible yet")
			}
			return err
		}); err != nil {
			return err
		}
	}
	// DESCRIBE is answered by the Graph service; a write needs the Storage
	// service to know the schema too, which it learns on its own heartbeat.
	probeVID := literal(vid("p", "schema-probe"))
	fetch := "FETCH PROP ON " + quote(item.name) + " " + probeVID + " YIELD vertex AS v"
	if item.edge {
		fetch = "FETCH PROP ON " + quote(item.name) + " " + probeVID + "->" + probeVID + "@0 YIELD edge AS e"
	}
	if err := s.until(ctx, deadline, item.name, func() error { _, err := s.read(ctx, fetch, nil); return err }); err != nil {
		return err
	}
	if len(item.indexes) == 0 {
		return nil
	}
	existing, err := s.indexNames(ctx, item)
	if err != nil {
		return err
	}
	for _, ix := range item.indexes {
		if _, err := s.write(ctx, item.createIndex(ix), nil); err != nil {
			return err
		}
		if err := s.until(ctx, deadline, ix.name, func() error { return s.indexListed(ctx, item, ix.name) }); err != nil {
			return err
		}
		if existing[ix.name] {
			continue
		}
		// A new index on a tag that already has rows (an upgrade) must be
		// built over them before a lookup can use it.
		if err := s.until(ctx, deadline, ix.name, func() error { return s.buildIndex(ctx, item, ix.name) }); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) indexNames(ctx context.Context, item schemaItem) (map[string]bool, error) {
	rows, err := s.read(ctx, fmt.Sprintf("SHOW %s INDEXES", item.kind()), nil)
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, r := range rows {
		if n, ok := r["Index Name"].(string); ok {
			out[n] = true
		}
	}
	return out, nil
}

func (s *Store) indexListed(ctx context.Context, item schemaItem, name string) error {
	names, err := s.indexNames(ctx, item)
	if err == nil && !names[name] {
		err = fmt.Errorf("index %s not listed yet", name)
	}
	return err
}

// buildIndex submits a rebuild until the index reports FINISHED. A job
// submitted before the storage service has the index fails at once, so a
// FAILED or missing status submits it again.
func (s *Store) buildIndex(ctx context.Context, item schemaItem, name string) error {
	rows, err := s.read(ctx, fmt.Sprintf("SHOW %s INDEX STATUS", item.kind()), nil)
	if err != nil {
		return err
	}
	status := ""
	for _, r := range rows {
		if n, _ := r["Name"].(string); n == name {
			status, _ = r["Index Status"].(string)
		}
	}
	if status == "FINISHED" {
		return nil
	}
	if status == "" || status == "FAILED" {
		if _, err := s.write(ctx, fmt.Sprintf("REBUILD %s INDEX %s", item.kind(), quote(name)), nil); err != nil {
			return err
		}
	}
	return fmt.Errorf("index %s is %q", name, status)
}

// until retries f until it succeeds or the deadline passes.
func (s *Store) until(ctx context.Context, deadline time.Time, what string, f func() error) error {
	for {
		err := f()
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("nebula schema: %s not ready: %w", what, err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}
