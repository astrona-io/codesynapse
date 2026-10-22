package catalog

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/astrona-io/codesynapse/internal/arch"
	"github.com/astrona-io/codesynapse/internal/ingest/docs"
	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/store"
	"github.com/astrona-io/codesynapse/internal/store/memory"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

func write(t *testing.T, root, rel, text string) {
	t.Helper()
	p := filepath.Join(root, rel)
	require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o750))
	require.NoError(t, os.WriteFile(p, []byte(text), 0o600))
}

func code(repo string) *model.Graph {
	g := model.NewGraph()
	add := func(n model.Node) { n.Repo = repo; g.AddNode(n) }
	pkg := "ex.com/app/internal/store"
	add(model.Node{ID: model.NodeID(repo, model.KindRepository, repo), Kind: model.KindRepository, Name: repo, Head: "abc123", Time: "2026-10-22T10:00:00Z"})
	add(model.Node{ID: model.NodeID(repo, model.KindPackage, pkg), Kind: model.KindPackage, Name: "store", QualifiedName: pkg})
	add(model.Node{ID: model.NodeID(repo, model.KindFile, "internal/store/store.go"), Kind: model.KindFile, Name: "store.go", File: "internal/store/store.go", Package: pkg})
	add(model.Node{ID: model.NodeID(repo, model.KindType, pkg+".Reader"), Kind: model.KindType, Name: "Reader", QualifiedName: pkg + ".Reader", Package: pkg, File: "internal/store/store.go", Exported: true})
	add(model.Node{ID: model.NodeID(repo, model.KindFunction, pkg+".Open"), Kind: model.KindFunction, Name: "Open", QualifiedName: pkg + ".Open", Package: pkg, File: "internal/store/store.go", StartLine: 5, EndLine: 20, Exported: true})
	add(model.Node{ID: model.NodeID(repo, model.KindFile, "web/render.go"), Kind: model.KindFile, Name: "render.go", File: "web/render.go", Package: "ex.com/app/web"})
	add(model.Node{ID: model.NodeID(repo, model.KindFunction, "ex.com/app/web.Render"), Kind: model.KindFunction, Name: "Render", QualifiedName: "ex.com/app/web.Render", Package: "ex.com/app/web", File: "web/render.go", StartLine: 3, EndLine: 9, Exported: true})
	return g
}

// catalogOf indexes a tree of records on top of a small code graph, the way
// ingest does, and returns a service fenced to the repository.
func catalogOf(t *testing.T, repo string, files map[string]string, st *memory.Store) Service {
	t.Helper()
	return indexWith(t, repo, files, st)
}

// indexWith is catalogOf for a code graph without the nodes in drop, as after
// a rename or a removal.
func indexWith(t *testing.T, repo string, files map[string]string, st *memory.Store, drop ...string) Service {
	t.Helper()
	root := t.TempDir()
	for rel, text := range files {
		write(t, root, rel, text)
	}
	g := code(repo)
	if len(drop) > 0 {
		kept := model.NewGraph()
		for _, n := range g.Nodes() {
			if !slices.Contains(drop, n.ID) {
				kept.AddNode(n)
			}
		}
		g = kept
	}
	d, err := docs.Extract(context.Background(), repo, root, g)
	require.NoError(t, err)
	g.Merge(d)
	require.NoError(t, arch.Extract(context.Background(), repo, root, g))
	require.NoError(t, st.ReplaceRepo(context.Background(), repo, g))
	return Service{R: store.ForRepository(st, repo)}
}

var records = map[string]string{
	"docs/adr/0001-use-bolt.md":     "# ADR 0001: Use Bolt\n\n- Status: accepted\n- Topic: storage\n- Components: store\n\n## Decision\n\nWe keep graphs in an embedded store.\n",
	"docs/adr/0002-use-neo4j.md":    "---\nstatus: accepted\ntopic: storage\ncomponents: [store, api]\nsupersedes: ADR-0001\nimplements: [internal/store, store.Reader, store.Open]\nrelates: [ADR-0004]\n---\n# Use Neo4j\n\n## Decision\n\nGraphs live in Neo4j. `Open` connects to it.\n\n## Consequences\n\nThe store package wraps the driver.\n",
	"docs/adr/0003-cache.md":        "# Cache reads\n\n- Status: proposed\n- Components: api\n\n## Decision\n\nWe could cache reads for speed in the API layer.\n",
	"docs/adr/0004-logging.md":      "# Log as JSON\n\n- Status: accepted\n- Topic: logging\n- Components: api\n- Conflicts-with: ADR-0005\n",
	"docs/adr/0005-log-text.md":     "# Log as text\n\n- Status: accepted\n- Topic: logging\n- Components: api\n",
	"docs/adr/0006-rejected.md":     "# Use SQLite\n\n- Status: rejected\n- Topic: storage\n- Components: store\n",
	"docs/standards/errors.md":      "---\nstatus: active\ncomponents: [api]\nimplements: [web.Render]\n---\n# Error handling\n\nWrap every error with context before returning it to callers.\n",
	"docs/architecture/overview.md": "# System overview\n\nThree services and a graph store, described for newcomers.\n",
	"docs/rfcs/0007-sharding.md":    "# RFC 7: Sharding\n\nStatus: in review\n\nHow to split graphs across stores.\n",
}

func TestListFiltersFacetsAndBinding(t *testing.T) {
	ctx := context.Background()
	svc := catalogOf(t, "r", records, memory.New())
	all, err := svc.List(ctx, "r", Filter{})
	require.NoError(t, err)
	assert.Equal(t, all.Total, 9)
	assert.Equal(t, all.Source.Head, "abc123", "an answer says which revision it describes")
	assert.Equal(t, all.Facets.Types["adr"], 6)
	assert.Equal(t, all.Facets.Statuses["accepted"], 4)
	assert.Equal(t, all.Facets.Components["api"], 5)
	assert.Equal(t, all.Entries[0].Type, "adr", "decisions come first")
	assert.True(t, all.Entries[0].Binding, "and the rules in force come before drafts and history")

	byID := map[string]Entry{}
	for _, e := range all.Entries {
		byID[e.ID] = e
	}
	assert.False(t, byID["ADR-0001"].Binding, "accepted but superseded by an accepted record: not a rule any more")
	assert.True(t, byID["ADR-0002"].Binding)
	assert.False(t, byID["ADR-0003"].Binding, "a proposal is not a requirement")
	assert.False(t, byID["ADR-0006"].Binding)
	assert.False(t, byID["RFC-0007"].Binding)
	assert.False(t, byID["overview:docs-architecture-overview"].Binding, "an overview informs")
	assert.True(t, byID["standard:docs-standards-errors"].Binding)
	assert.Equal(t, byID["ADR-0001"].SupersededBy, []string{"ADR-0002"})

	bind, err := svc.List(ctx, "r", Filter{BindingOnly: true})
	require.NoError(t, err)
	assert.Equal(t, bind.Total, 4, "ADR-0002, ADR-0004, ADR-0005 and the standard; not the superseded, proposed or rejected ones")
	for _, e := range bind.Entries {
		assert.True(t, e.Binding)
	}
	adrs, _ := svc.List(ctx, "r", Filter{Type: "adr", Status: "proposed"})
	assert.Equal(t, adrs.Total, 1)
	comp, _ := svc.List(ctx, "r", Filter{Component: "STORE"})
	assert.Equal(t, comp.Total, 3, "component names are case-insensitive")
	q, _ := svc.List(ctx, "r", Filter{Q: "neo4j graphs"})
	assert.Equal(t, q.Total, 1)
	page, _ := svc.List(ctx, "r", Filter{Limit: 2, Offset: 1})
	assert.Equal(t, len(page.Entries), 2)
	assert.Equal(t, page.Total, 9, "the total ignores the page")
	beyond, _ := svc.List(ctx, "r", Filter{Offset: 100})
	assert.Equal(t, len(beyond.Entries), 0)
}

func TestIssuesNameWhatToLookAt(t *testing.T) {
	ctx := context.Background()
	files := map[string]string{}
	for k, v := range records {
		files[k] = v
	}
	// ADR-0001 still says accepted although ADR-0002 replaces it.
	files["docs/adr/0008-loop-a.md"] = "# Loop A\n\n- Status: superseded\n- Supersedes: ADR-0009\n"
	files["docs/adr/0009-loop-b.md"] = "# Loop B\n\n- Status: superseded\n- Supersedes: ADR-0008\n"
	files["docs/adr/0010-orphan.md"] = "# Orphan\n\n- Status: superseded\n"
	files["docs/adr/0011-weak.md"] = "# Weak successor\n\n- Status: proposed\n- Supersedes: ADR-0003\n"
	files["docs/adr/0012-odd-status.md"] = "# Odd\n\n- Status: banana\n"
	svc := catalogOf(t, "r", files, memory.New())
	issues, _, err := svc.Issues(ctx, "r")
	require.NoError(t, err)
	by := map[string][]Issue{}
	for _, is := range issues {
		by[is.Kind] = append(by[is.Kind], is)
	}
	require.Len(t, by["supersession-mismatch"], 2, "ADR-0001 and ADR-0003 are replaced but not marked")
	assert.Contains(t, by["supersession-mismatch"][0].Message, "ADR-0001 is replaced by ADR-0002 but its status is accepted")
	require.Len(t, by["potential-conflict"], 1, "two rules are in force for logging in the api; storage has only one since ADR-0001 is superseded")
	conflict := by["potential-conflict"][0]
	assert.Equal(t, conflict.Records, []string{"ADR-0004", "ADR-0005"})
	assert.Equal(t, conflict.Severity, "warning")
	require.Len(t, by["explicit-conflict"], 1)
	assert.Equal(t, by["explicit-conflict"][0].Records, []string{"ADR-0004", "ADR-0005"})
	require.Len(t, by["supersession-cycle"], 1)
	assert.Equal(t, by["supersession-cycle"][0].Records, []string{"ADR-0008", "ADR-0009"})
	require.Len(t, by["superseded-without-successor"], 1)
	assert.Equal(t, by["superseded-without-successor"][0].Records, []string{"ADR-0010"})
	require.Len(t, by["successor-not-in-force"], 1)
	assert.Equal(t, by["successor-not-in-force"][0].Records, []string{"ADR-0011", "ADR-0003"})
	assert.True(t, len(by["status-unknown"]) >= 1)
	assert.Equal(t, len(by["unresolved-reference"]), 0, "every reference in these records resolves")
}

func TestGetShowsChainCodeAndSource(t *testing.T) {
	ctx := context.Background()
	svc := catalogOf(t, "r", records, memory.New())
	d, err := svc.Get(ctx, "r", "adr-0002")
	require.NoError(t, err)
	assert.Equal(t, d.ID, "ADR-0002", "IDs are case-insensitive")
	assert.Equal(t, d.Status, "accepted")
	assert.True(t, d.Binding)
	assert.Equal(t, d.Lifecycle, []string{"proposed", "accepted", "rejected", "deprecated", "superseded"})
	assert.Equal(t, d.Source.Head, "abc123")
	assert.Equal(t, d.Source.RecordPath, "docs/adr/0002-use-neo4j.md")
	assert.Equal(t, d.Fingerprint != "", true)

	require.Len(t, d.Chain, 1)
	assert.Equal(t, d.Chain[0], ChainLink{ID: "ADR-0001", Title: "Use Bolt", Status: "accepted", Relation: "supersedes", Depth: 1})
	old, err := svc.Get(ctx, "r", "ADR-0001")
	require.NoError(t, err)
	require.Len(t, old.Chain, 1)
	assert.Equal(t, old.Chain[0].Relation, "superseded-by")
	assert.False(t, old.Binding)
	assert.Equal(t, len(old.Problems), 1, "it still says accepted although a newer record replaces it")

	names := map[string]string{}
	for _, c := range d.ImplementedBy {
		names[c.Name] = c.Declared
	}
	assert.Equal(t, names["ex.com/app/internal/store"], "internal/store", "a directory names its package")
	assert.Equal(t, names["store.Reader"], "store.Reader")
	assert.Equal(t, names["store.Open"], "store.Open")
	mentioned := false
	for _, m := range d.Mentions {
		mentioned = mentioned || m.Name == "store.Open"
	}
	assert.True(t, mentioned, "code named in the text is a weaker link than a declared one")
	require.Len(t, d.Outline, 4)
	sections := []string{}
	for _, o := range d.Outline {
		sections = append(sections, o.Name)
	}
	assert.Equal(t, sections, []string{"(preamble)", "Use Neo4j", "Decision", "Consequences"})
	require.Len(t, d.Related, 1)
	assert.Equal(t, d.Related[0].ID, "ADR-0004")

	_, err = svc.Get(ctx, "r", "ADR-9999")
	assert.ErrorIs(t, err, ErrNotFound)
}

func TestContextSeparatesRulesFromBackground(t *testing.T) {
	ctx := context.Background()
	svc := catalogOf(t, "r", records, memory.New())

	res, err := svc.Context(ctx, "r", Task{Symbols: []string{"store.Open"}, Query: "add a cache for reads in the api"})
	require.NoError(t, err)
	ids := func(as []Applicable) []string {
		out := []string{}
		for _, a := range as {
			out = append(out, a.ID)
		}
		return out
	}
	assert.Contains(t, ids(res.Binding), "ADR-0002", "carried out by the package it touches")
	assert.NotContains(t, ids(res.Binding), "ADR-0003", "a proposal is not a rule")
	assert.NotContains(t, ids(res.Binding), "ADR-0001", "a superseded decision is not a rule")
	assert.Contains(t, ids(res.Informational), "ADR-0003", "but it is background for a cache task")
	assert.Equal(t, res.Binding[0].ID, "ADR-0002")
	assert.Contains(t, res.Binding[0].Why[0], "implemented by")
	assert.Equal(t, res.Source.Head, "abc123")

	byFile, err := svc.Context(ctx, "r", Task{Files: []string{"web/render.go"}})
	require.NoError(t, err)
	assert.Equal(t, ids(byFile.Binding), []string{"standard:docs-standards-errors"}, "an implemented symbol: the standard names web.Render")
	assert.Equal(t, len(byFile.Informational), 0)

	byComponent, err := svc.Context(ctx, "r", Task{Components: []string{"api"}})
	require.NoError(t, err)
	assert.Equal(t, len(byComponent.Binding), 4, "the standard, the two logging decisions and the storage decision apply to the api")
	conflicting := false
	for _, is := range byComponent.Issues {
		conflicting = conflicting || is.Kind == "potential-conflict"
	}
	assert.True(t, conflicting, "records that may disagree are shown with the rules, not resolved")

	limited, err := svc.Context(ctx, "r", Task{Components: []string{"api"}, Limit: 1})
	require.NoError(t, err)
	assert.Equal(t, len(limited.Binding)+len(limited.Informational), 1)
	assert.True(t, limited.Truncated)
	assert.True(t, limited.Omitted >= 2, "what was left out is counted, not hidden")

	none, err := svc.Context(ctx, "r", Task{Files: []string{"nowhere.go"}, Symbols: []string{"missing.Thing"}})
	require.NoError(t, err)
	assert.Equal(t, len(none.Binding), 0)
	joined := ""
	for _, n := range none.Notes {
		joined += n + "\n"
	}
	assert.Contains(t, joined, `file "nowhere.go" is not in the index`)
	assert.Contains(t, joined, "not the same as no constraint")
}

func TestRepositoriesAreFenced(t *testing.T) {
	ctx := context.Background()
	st := memory.New()
	mine := catalogOf(t, "mine", records, st)
	other := catalogOf(t, "other", map[string]string{"docs/adr/0001-secret.md": "# Secret plan\n\n- Status: accepted\n- Components: vault\n"}, st)

	got, err := mine.List(ctx, "mine", Filter{})
	require.NoError(t, err)
	for _, e := range got.Entries {
		assert.True(t, e.NodeID[:len("mine|")] == "mine|", "only this repository's records: %s", e.NodeID)
	}
	_, err = mine.List(ctx, "other", Filter{})
	assert.Error(t, err, "a reader fenced to one repository cannot list another's")
	_, err = mine.Get(ctx, "other", "ADR-0001")
	assert.Error(t, err)
	_, _, err = mine.Issues(ctx, "other")
	assert.Error(t, err)
	_, err = mine.Context(ctx, "other", Task{Query: "secret plan vault"})
	assert.Error(t, err)

	// The same ID in two repositories means two different records.
	a, err := mine.Get(ctx, "mine", "ADR-0001")
	require.NoError(t, err)
	b, err := other.Get(ctx, "other", "ADR-0001")
	require.NoError(t, err)
	assert.Equal(t, a.Title, "Use Bolt")
	assert.Equal(t, b.Title, "Secret plan")
	// Traversal from one repository's code never reaches the other's records.
	res, err := mine.Context(ctx, "mine", Task{Query: "secret plan vault"})
	require.NoError(t, err)
	for _, x := range append(res.Binding, res.Informational...) {
		assert.True(t, x.Title != "Secret plan")
	}
	_ = store.ErrNotFound
}
