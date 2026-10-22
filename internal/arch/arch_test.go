package arch

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

func TestCanonicalStatus(t *testing.T) {
	tests := []struct {
		typ, raw, want string
		ok             bool
	}{
		{ADR, "Accepted", "accepted", true},
		{ADR, "approved", "accepted", true},
		{ADR, "**Accepted** (2026-10-22)", "accepted", true},
		{ADR, "Superseded by ADR-0005", "superseded", true},
		{ADR, "draft", Unknown, false}, // an ADR is proposed, not draft
		{ADR, "current", Unknown, false},
		{RFC, "in review", "in-review", true},
		{RFC, "Proposed", "draft", true},
		{Standard, "Approved", "active", true},
		{Policy, "enforced", "active", true},
		{Overview, "Accepted", "current", true},
		{Runbook, "stale", "outdated", true},
		{Diagram, "deprecated", "retired", true},
		{ADR, "banana", Unknown, false},
	}
	for _, tt := range tests {
		t.Run(tt.typ+" "+tt.raw, func(t *testing.T) {
			got, ok := Canonical(tt.typ, tt.raw)
			assert.Equal(t, got, tt.want)
			assert.Equal(t, ok, tt.ok)
		})
	}
}

func TestBindingOnlyForRulesInForce(t *testing.T) {
	assert.True(t, Binding(ADR, "accepted"))
	assert.True(t, Binding(Standard, "active"))
	assert.True(t, Binding(Policy, "active"))
	for _, c := range [][2]string{{ADR, "proposed"}, {ADR, "rejected"}, {ADR, "superseded"}, {ADR, "deprecated"}, {ADR, Unknown},
		{RFC, "accepted"}, {RFC, "implemented"}, {Overview, "current"}, {Runbook, "current"}, {Standard, "draft"}, {Policy, "retired"}} {
		assert.False(t, Binding(c[0], c[1]), "%s %s informs but does not bind", c[0], c[1])
	}
}

func TestParseDocFormats(t *testing.T) {
	fm := parseDoc([]byte("---\nid: ADR-0007\ntitle: Use Postgres\nstatus: Accepted\nowner: [Ana, Bo]\ncomponents: [api, web]\ntags: store, db\nsupersedes: ADR-0002\ntopic: Storage\nimplements:\n  - internal/store\n  - store.Reader\n---\n# Ignored\n\n## Decision\n\nWe use Postgres for accounts because it is transactional.\n"))
	assert.Equal(t, fm.Format, "front-matter")
	assert.Equal(t, fm.Title, "Use Postgres")
	assert.Equal(t, fm.Fields["status"], []string{"Accepted"})
	assert.Equal(t, fm.Fields["components"], []string{"api", "web"})
	assert.Equal(t, fm.Fields["labels"], []string{"store", "db"})
	assert.Equal(t, fm.Fields["implements"], []string{"internal/store", "store.Reader"})
	assert.Contains(t, fm.Summary, "We use Postgres for accounts")

	bullets := parseDoc([]byte("# ADR 0001: Search inside the stores\n\n- Status: accepted\n- Date: 2026-10-22\n- Phase: SaaS 4c\n\n## Context\n\nCodeSynapse answers agents with small, precise results.\n"))
	assert.Equal(t, bullets.Format, "bullets")
	assert.Equal(t, bullets.Title, "Search inside the stores", "the ADR number prefix is not part of the title")
	assert.Equal(t, bullets.Fields["status"], []string{"accepted"})
	assert.Equal(t, bullets.Fields["date"], []string{"2026-10-22"})

	madr := parseDoc([]byte("# Use Redis\n\n## Status\n\nSuperseded by [ADR-0009](0009-use-valkey.md)\n\n## Context\n\nCaching needs a store that is fast and widely run.\n"))
	assert.Equal(t, madr.Format, "section")
	assert.Equal(t, madr.Fields["superseded-by"], []string{"0009-use-valkey.md"})
	assert.Equal(t, madr.Fields["status"][0], "Superseded by [ADR-0009](0009-use-valkey.md)")
}

func write(t *testing.T, root, rel, text string) {
	t.Helper()
	p := filepath.Join(root, rel)
	require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o750))
	require.NoError(t, os.WriteFile(p, []byte(text), 0o600))
}

func codeGraph() *model.Graph {
	g := model.NewGraph()
	add := func(n model.Node) { n.Repo = "r"; g.AddNode(n) }
	add(model.Node{ID: "r|Package|ex.com/app/internal/store", Kind: model.KindPackage, Name: "store", QualifiedName: "ex.com/app/internal/store"})
	add(model.Node{ID: "r|File|internal/store/store.go", Kind: model.KindFile, Name: "store.go", File: "internal/store/store.go", Package: "ex.com/app/internal/store"})
	add(model.Node{ID: "r|Type|ex.com/app/internal/store.Reader", Kind: model.KindType, Name: "Reader", QualifiedName: "ex.com/app/internal/store.Reader", Exported: true})
	add(model.Node{ID: "r|Function|ex.com/app/internal/store.New", Kind: model.KindFunction, Name: "New", QualifiedName: "ex.com/app/internal/store.New", Exported: true})
	add(model.Node{ID: "r|Function|ex.com/app/other.New", Kind: model.KindFunction, Name: "New", QualifiedName: "ex.com/app/other.New", Exported: true})
	return g
}

func TestExtractBuildsRecordsAndRelations(t *testing.T) {
	root := t.TempDir()
	write(t, root, "docs/adr/0001-use-bolt.md", "# ADR 0001: Use Bolt\n\n- Status: superseded\n- Date: 2026-01-01\n\n## Context\n\nAn embedded store was enough at first.\n")
	write(t, root, "docs/adr/0002-use-postgres.md", "---\nstatus: accepted\nowner: Ana <ana@example.com>\ncomponents: [API, Store]\ntopic: storage\nsupersedes: ADR-0001\nimplements: [internal/store, store.Reader, New, missing.Thing, internal/store/store.go]\nrelates: [0003-use-neo4j.md, ADR-0042]\n---\n# Use Postgres\n\n## Decision\n\nAccounts live in PostgreSQL.\n")
	write(t, root, "docs/adr/0003-use-neo4j.md", "# Use Neo4j\n\n- Status: Approved\n- Components: store\n- Topic: Storage\n")
	write(t, root, "docs/adr/README.md", "# Index\n")
	write(t, root, "docs/adr/0000-template.md", "# Template\n\n- Status: proposed\n")
	write(t, root, "docs/rfcs/0004-cache.md", "# RFC 4: Cache\n\nStatus: in review\n")
	write(t, root, "docs/standards/errors.md", "---\nstatus: active\n---\n# Error handling\n\nWrap errors with context.\n")
	write(t, root, "docs/architecture/overview.md", "# Overview\n\nThe system has three services and a graph store.\n")
	write(t, root, "docs/architecture/flow.mmd", "graph TD; A-->B\n")
	write(t, root, "docs/notes/random.md", "# Not a record\n\nJust notes.\n")
	write(t, root, "docs/notes/decision.md", "---\ntype: adr\nstatus: proposed\nid: ADR-0100\n---\n# Out of place\n")
	write(t, root, "docs/adr/0009-no-status.md", "# No status\n\nSome text that is long enough to be a summary.\n")

	g := codeGraph()
	require.NoError(t, Extract(context.Background(), "r", root, g))

	recs := map[string]model.Node{}
	for _, n := range g.Nodes() {
		if n.Kind == model.KindRecord {
			recs[n.QualifiedName] = n
		}
	}
	for _, id := range []string{"ADR-0001", "ADR-0002", "ADR-0003", "RFC-0004", "ADR-0100", "ADR-0009", "standard:docs-standards-errors", "overview:docs-architecture-overview", "diagram:docs-architecture-flow"} {
		_, ok := recs[id]
		assert.True(t, ok, id)
	}
	assert.Equal(t, len(recs), 9, "indexes, templates and unrelated notes are not records")

	a2 := recs["ADR-0002"]
	assert.Equal(t, a2.DocType, "adr")
	assert.Equal(t, a2.Status, "accepted")
	assert.Equal(t, a2.Name, "Use Postgres")
	assert.Equal(t, a2.File, "docs/adr/0002-use-postgres.md")
	assert.Contains(t, a2.Summary, "Accounts live in PostgreSQL")
	var m Meta
	require.NoError(t, json.Unmarshal([]byte(a2.Meta), &m))
	assert.Equal(t, m.Components, []string{"api", "store"})
	assert.Equal(t, m.Supersedes, []string{"ADR-0001"})
	assert.Equal(t, m.Relates, []string{"ADR-0003"})
	assert.Equal(t, m.Topic, "storage")
	assert.True(t, m.Owner != "" && !contains(m.Owner, "@"), "personal data is masked: %q", m.Owner)
	assert.Contains(t, joinAll(m.Unresolved), `relates: no record matches "ADR-0042"`)
	assert.Contains(t, joinAll(m.Unresolved), `nothing in the code is called "missing.Thing"`)
	assert.Contains(t, joinAll(m.Unresolved), `"New" matches 2 symbols`)

	// The older record learns who replaced it.
	var m1 Meta
	require.NoError(t, json.Unmarshal([]byte(recs["ADR-0001"].Meta), &m1))
	assert.Equal(t, m1.SupersededBy, []string{"ADR-0002"})
	assert.Equal(t, recs["ADR-0001"].Status, "superseded")

	assert.Equal(t, recs["ADR-0003"].Status, "accepted", "approved is accepted")
	assert.Equal(t, recs["RFC-0004"].Status, "in-review")
	assert.Equal(t, recs["ADR-0009"].Status, Unknown)
	assert.Contains(t, joinAll(metaOf(t, recs["ADR-0009"]).Unresolved), "no status found")
	assert.Equal(t, recs["overview:docs-architecture-overview"].Status, "current", "informational documents are current by default")

	has := func(from, to string, k model.EdgeKind) bool {
		for _, e := range g.Edges() {
			if e.From == from && e.To == to && e.Kind == k {
				return true
			}
		}
		return false
	}
	assert.True(t, has("r|Record|ADR-0002", "r|Record|ADR-0001", model.EdgeSupersedes))
	assert.True(t, has("r|Record|ADR-0002", "r|Record|ADR-0003", model.EdgeRelatesTo))
	assert.True(t, has("r|Record|ADR-0002", "r|Component|api", model.EdgeAppliesTo))
	assert.True(t, has("r|Package|ex.com/app/internal/store", "r|Record|ADR-0002", model.EdgeImplementsDecision), "a directory names its package")
	assert.True(t, has("r|Type|ex.com/app/internal/store.Reader", "r|Record|ADR-0002", model.EdgeImplementsDecision))
	assert.True(t, has("r|File|internal/store/store.go", "r|Record|ADR-0002", model.EdgeImplementsDecision))
	assert.False(t, has("r|Function|ex.com/app/other.New", "r|Record|ADR-0002", model.EdgeImplementsDecision), "an ambiguous name links nothing")
}

func contains(s, sub string) bool { return len(sub) > 0 && len(s) >= len(sub) && indexOf(s, sub) >= 0 }

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func joinAll(ss []string) string {
	out := ""
	for _, s := range ss {
		out += s + "\n"
	}
	return out
}

func metaOf(t *testing.T, n model.Node) Meta {
	t.Helper()
	var m Meta
	require.NoError(t, json.Unmarshal([]byte(n.Meta), &m))
	return m
}

func TestDuplicateIDsGetASuffix(t *testing.T) {
	root := t.TempDir()
	write(t, root, "docs/adr/a/0001-x.md", "# One\n\n- Status: accepted\n")
	write(t, root, "docs/adr/b/0001-y.md", "# Two\n\n- Status: accepted\n")
	g := model.NewGraph()
	require.NoError(t, Extract(context.Background(), "r", root, g))
	ids := map[string]bool{}
	for _, n := range g.Nodes() {
		if n.Kind == model.KindRecord {
			ids[n.QualifiedName] = true
		}
	}
	assert.True(t, ids["ADR-0001"] && ids["ADR-0001-2"], "%v", ids)
}

func TestNoRecordsIsFine(t *testing.T) {
	g := model.NewGraph()
	require.NoError(t, Extract(context.Background(), "r", t.TempDir(), g))
	assert.Equal(t, len(g.Nodes()), 0)
}
