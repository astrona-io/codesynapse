package refactorplan

import (
	"context"
	"strings"
	"testing"

	"github.com/parisnakitakejser/codesynapse/internal/duplicates"
	"github.com/parisnakitakejser/codesynapse/internal/model"
	"github.com/parisnakitakejser/codesynapse/internal/store"
	"github.com/parisnakitakejser/codesynapse/internal/store/memory"
	"github.com/parisnakitakejser/codesynapse/internal/testutil/assert"
	"github.com/parisnakitakejser/codesynapse/internal/testutil/require"
)

type fixture struct {
	g    *model.Graph
	hash string
}

// reader stores the graph and returns the reader the plan is built from.
func (f fixture) reader(t *testing.T) store.Reader {
	t.Helper()
	s := memory.New()
	require.NoError(t, s.ReplaceRepo(context.Background(), "r", f.g))
	return store.ForRepository(s, "r")
}

// repoWith builds a repository whose functions are the given nodes, with a
// duplicate report that groups them.
func repoWith(t *testing.T, kind string, nodes ...model.Node) fixture {
	t.Helper()
	g := model.NewGraph()
	rid := model.NodeID("r", model.KindRepository, "r")
	rn := g.AddNode(model.Node{ID: rid, Kind: model.KindRepository, Repo: "r", Name: "r"})
	rep := duplicates.Report{}
	grp := duplicates.Group{Hash: "h1", Kind: kind, Similarity: 1, Scope: "repository"}
	if kind == "near" {
		grp.Similarity = 0.82
	}
	for _, n := range nodes {
		n.Repo = "r"
		n.ID = model.NodeID("r", n.Kind, n.QualifiedName)
		g.AddNode(n)
		grp.Members = append(grp.Members, duplicates.Member{ID: n.ID, Name: n.QualifiedName, File: n.File, StartLine: n.StartLine, EndLine: n.EndLine, LOC: n.LOC})
		grp.Copies++
	}
	rep.Groups = []duplicates.Group{grp}
	enc, err := duplicates.Encode(rep)
	require.NoError(t, err)
	rn.Duplicates = enc
	return fixture{g: g, hash: "h1"}
}

func fn(kind model.NodeKind, pkg, name, file, sig string, exported bool) model.Node {
	return model.Node{Kind: kind, Name: name, QualifiedName: pkg + "." + name, Package: pkg, File: file, StartLine: 10, EndLine: 30, LOC: 21, Signature: sig, Exported: exported}
}

func TestForGroupFunctionsAcrossPackages(t *testing.T) {
	ctx := context.Background()
	a := fn(model.KindFunction, "example.com/x/accounts", "newID", "internal/accounts/secrets.go", "func newID() string", false)
	b := fn(model.KindFunction, "example.com/x/offload", "newID", "services/api/internal/offload/offload.go", "func newID() string", false)
	c := fn(model.KindFunction, "example.com/x/store", "NewRunID", "pkg/store/neo4j.go", "func NewRunID(prefix string) string", true)
	c.SecurityTags = []string{"crypto"}
	fx := repoWith(t, "exact", a, b, c)

	p, err := ForGroup(ctx, fx.reader(t), "r", fx.hash, "exact")
	require.NoError(t, err)
	assert.Equal(t, p.Name, "NewID", "newID twice and NewRunID once: the most common name, public because one copy is")
	assert.Contains(t, p.Home, "share no directory", "internal/, services/ and internal/ share nothing")
	assert.Equal(t, len(p.Copies), 3)
	risks := strings.Join(p.Risks, "\n")
	assert.Contains(t, risks, "may be public API")
	assert.Contains(t, risks, "different packages")
	assert.Contains(t, risks, "signatures differ", "the third takes a parameter")
	assert.Contains(t, risks, "No test reaches")
	assert.Contains(t, risks, "crypto")
	assert.True(t, len(p.Steps) >= 5 && strings.HasPrefix(p.Steps[0], "1. "))
	assert.Contains(t, p.Approach, "Extract one function")
	assert.Contains(t, p.Approach, "different names (NewRunID, newID)", "the names are listed so one can be chosen for the shared part")
}

func TestExportedInsideInternalIsNotPublic(t *testing.T) {
	ctx := context.Background()
	a := fn(model.KindFunction, "example.com/x/internal/a", "Do", "internal/a/a.go", "func Do() error", true)
	b := fn(model.KindFunction, "example.com/x/internal/b", "Do", "internal/b/b.go", "func Do() error", true)
	fx := repoWith(t, "exact", a, b)
	p, err := ForGroup(ctx, fx.reader(t), "r", fx.hash, "exact")
	require.NoError(t, err)
	risks := strings.Join(p.Risks, "\n")
	assert.Contains(t, risks, "in internal/")
	assert.False(t, strings.Contains(risks, "may be public API"))
}

func TestForGroupMethodsSameFile(t *testing.T) {
	ctx := context.Background()
	mk := func(recv string) model.Node {
		n := fn(model.KindMethod, "example.com/x/cmd", "(*"+recv+").Complete", "cmd/main.go", "func (o *"+recv+") Complete() error", true)
		n.Name = "Complete"
		return n
	}
	a, b := mk("statsOptions"), mk("auditOptions")
	b.StartLine, b.EndLine = 100, 120
	fx := repoWith(t, "exact", a, b)
	p, err := ForGroup(ctx, fx.reader(t), "r", fx.hash, "")
	require.NoError(t, err)
	assert.Equal(t, p.Name, "Complete")
	assert.Contains(t, p.Approach, "methods", "methods of different receivers need what they use passed in")
	assert.Equal(t, strings.Contains(strings.Join(p.Risks, " "), "different packages"), false, "one package")
}

func TestForGroupNearAndMissing(t *testing.T) {
	ctx := context.Background()
	a := fn(model.KindFunction, "p", "Do", "a/a.go", "func Do(x int) error", false)
	b := fn(model.KindFunction, "p", "Do2", "a/b.go", "func Do2(x int) error", false)
	b.Liveness = "unreachable"
	fx := repoWith(t, "near", a, b)
	p, err := ForGroup(ctx, fx.reader(t), "r", fx.hash, "near")
	require.NoError(t, err)
	assert.Contains(t, p.Approach, "edited copies (~82% alike)")
	assert.Contains(t, p.Steps[0], "side by side")
	assert.Contains(t, strings.Join(p.Risks, "\n"), "delete such a copy instead of merging it")
	assert.Contains(t, strings.Join(p.Risks, "\n"), "fix applied to only one")

	_, err = ForGroup(ctx, fx.reader(t), "r", "nope", "exact")
	assert.ErrorIs(t, err, ErrNotFound)
	_, err = ForGroup(ctx, fx.reader(t), "r", fx.hash, "exact")
	assert.ErrorIs(t, err, ErrNotFound, "the hash exists only as a near group")
}

func TestNormalizeSignature(t *testing.T) {
	m := func(sig string) model.Node { return model.Node{Signature: sig} }
	assert.Equal(t, normalizeSignature(m("func (o *statsOptions) Complete(ctx context.Context) error")), normalizeSignature(m("func (a *auditOptions) Complete(ctx context.Context) error")))
	assert.Equal(t, normalizeSignature(m("func newID() string")), normalizeSignature(m("func newRunID() string")))
	assert.True(t, normalizeSignature(m("func f(a int) string")) != normalizeSignature(m("func f(a int, b int) string")))
}

func TestCommonDir(t *testing.T) {
	assert.Equal(t, commonDir([]string{"internal/store/a", "internal/store/b"}), "internal/store")
	assert.Equal(t, commonDir([]string{"cmd", "internal"}), "")
	assert.Equal(t, commonDir([]string{"x/y", "x/y"}), "x/y")
}

func TestForGroupCallersAndTests(t *testing.T) {
	ctx := context.Background()
	a := fn(model.KindFunction, "example.com/x/p", "dedupe", "p/a.go", "func dedupe(in []string) []string", false)
	b := fn(model.KindFunction, "example.com/x/q", "dedupeStrings", "q/b.go", "func dedupeStrings(in []string) []string", false)
	fx := repoWith(t, "exact", a, b)
	add := func(kind model.NodeKind, name string, test bool) string {
		n := model.Node{ID: model.NodeID("r", kind, name), Kind: kind, Repo: "r", Name: name, QualifiedName: name, File: "x.go", StartLine: 1, EndLine: 5, IsTest: test}
		fx.g.AddNode(n)
		return n.ID
	}
	caller, test := add(model.KindFunction, "p.useIt", false), add(model.KindFunction, "p.TestDedupe", true)
	idA := model.NodeID("r", model.KindFunction, "example.com/x/p.dedupe")
	fx.g.AddEdge(model.Edge{From: caller, To: idA, Kind: model.EdgeCalls})
	fx.g.AddEdge(model.Edge{From: test, To: idA, Kind: model.EdgeCalls})

	p, err := ForGroup(ctx, fx.reader(t), "r", fx.hash, "exact")
	require.NoError(t, err)
	require.Len(t, p.Copies, 2)
	first := p.Copies[0]
	require.Len(t, first.Callers, 1, "tests are not callers to update")
	assert.Equal(t, first.Callers[0].Name, "p.useIt")
	require.Len(t, first.Tests, 1)
	assert.Equal(t, first.Tests[0].Name, "p.TestDedupe")
	risks := strings.Join(p.Risks, "\n")
	assert.Contains(t, risks, "No test reaches q.dedupeStrings", "only the second copy is untested")
	assert.False(t, strings.Contains(risks, "No test reaches p.dedupe"))
	assert.Equal(t, p.Name, "dedupe", "ties go to the first name alphabetically")
}
