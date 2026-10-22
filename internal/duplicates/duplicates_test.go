package duplicates

import (
	"fmt"
	"testing"

	"github.com/parisnakitakejser/codesynapse/internal/model"
	"github.com/parisnakitakejser/codesynapse/internal/testutil/assert"
	"github.com/parisnakitakejser/codesynapse/internal/testutil/require"
)

func fn(repo, pkg, file, name, hash string, loc int, opts ...func(*model.Node)) model.Node {
	n := model.Node{ID: model.NodeID(repo, model.KindFunction, pkg+"."+name), Kind: model.KindFunction, Repo: repo, Name: name,
		QualifiedName: pkg + "." + name, Package: pkg, File: file, StartLine: 1, EndLine: loc, LOC: loc, CloneHash: hash}
	for _, o := range opts {
		o(&n)
	}
	return n
}

func TestAnalyze(t *testing.T) {
	g := model.NewGraph()
	for _, n := range []model.Node{
		fn("r", "x/a", "a/f.go", "A", "h1", 20),
		fn("r", "x/a", "a/f.go", "B", "h1", 20),  // same file
		fn("r", "x/a", "a/g.go", "C", "h2", 10),  // different file, same package
		fn("r", "x/b", "b/g.go", "C2", "h2", 12), // different package
		fn("r", "x/a", "a/g.go", "Solo", "h3", 30),
		fn("r", "x/a", "a/g.go", "Plain", "", 8),
		fn("r", "x/a", "a/f_test.go", "TestA", "h1", 20, func(n *model.Node) { n.IsTest = true }),
	} {
		g.AddNode(n)
	}
	rep := Analyze(g)
	require.Len(t, rep.Groups, 2)
	assert.Equal(t, rep.Groups[0].Hash, "h1", "20 redundant lines beat 10")
	assert.Equal(t, rep.Groups[0].Scope, "file")
	assert.Equal(t, rep.Groups[0].Redundant, 20)
	assert.Equal(t, rep.Groups[1].Scope, "repository")
	assert.Equal(t, rep.Groups[1].Redundant, 10)
	assert.Equal(t, rep.Summary.Groups, 2)
	assert.Equal(t, rep.Summary.DuplicatedFuncs, 4)
	assert.Equal(t, rep.Summary.RedundantLOC, 30)
	assert.Equal(t, rep.Summary.FunctionLOC, 20+20+10+12+30+8, "tests are not counted")
	assert.Len(t, rep.Fingerprints, 5, "candidates only")
	assert.Equal(t, rep.Fingerprints[0].Hash, "h3", "largest first")

	enc, err := Encode(rep)
	require.NoError(t, err)
	back, err := Decode(enc)
	require.NoError(t, err)
	assert.Equal(t, back.Summary, rep.Summary)
	empty, err := Decode("")
	require.NoError(t, err)
	assert.Equal(t, empty.Summary.Groups, 0)
	_, err = Decode("{")
	assert.Error(t, err)
}

func TestAnalyzeCaps(t *testing.T) {
	g := model.NewGraph()
	for i := range MaxGroups + 5 {
		h := fmt.Sprintf("h%04d", i)
		g.AddNode(fn("r", "p", "f.go", "A"+h, h, 10))
		g.AddNode(fn("r", "p", "f.go", "B"+h, h, 10))
	}
	rep := Analyze(g)
	assert.Len(t, rep.Groups, MaxGroups)
	assert.True(t, rep.Truncated)
}

func report(t *testing.T, repo string, nodes ...model.Node) Report {
	t.Helper()
	g := model.NewGraph()
	for _, n := range nodes {
		n.Repo = repo
		g.AddNode(n)
	}
	return Analyze(g)
}

func TestAcross(t *testing.T) {
	a := report(t, "org/a",
		fn("org/a", "x/auth", "auth/t.go", "Verify", "h1", 40),
		fn("org/a", "x/auth", "auth/u.go", "Parse", "h2", 30),
		fn("org/a", "x/log", "log/l.go", "Fmt", "h3", 25),
		fn("org/a", "x/only", "only.go", "Only", "h9", 99),
	)
	b := report(t, "org/b",
		fn("org/b", "y/auth", "auth/t.go", "Check", "h1", 40),
		fn("org/b", "y/auth", "auth/u.go", "Read", "h2", 30),
		fn("org/b", "y/log", "log/l.go", "Format", "h3", 25),
	)
	c := report(t, "org/c", fn("org/c", "z", "z.go", "Z", "h2", 30))

	got := Across(map[string]Report{"org/a": a, "org/b": b, "org/c": c})
	require.Len(t, got.Groups, 3, "h9 is in one repository only")
	assert.Equal(t, got.Groups[0].Hash, "h2", "three copies, 60 redundant lines")
	assert.Equal(t, got.Groups[0].Copies, 3)
	assert.Equal(t, got.Groups[0].Redundant, 60)
	assert.Equal(t, got.Groups[1].Hash, "h1")
	assert.Equal(t, got.RedundantLOC, 40+60+25)

	require.Len(t, got.Suggestions, 2, "one per set of repositories that copy enough")
	s := got.Suggestions[0]
	assert.Equal(t, s.Kind, "library")
	assert.Equal(t, s.Repos, []string{"org/a", "org/b"})
	assert.Equal(t, s.Groups, 2, "h1 and h3 are shared by exactly a and b")
	assert.Equal(t, s.RedundantLOC, 65)
	assert.Equal(t, got.Suggestions[1].Repos, []string{"org/a", "org/b", "org/c"})
}

func TestAcrossFramework(t *testing.T) {
	var as, bs []model.Node
	for i, pkg := range []string{"auth", "log", "cfg", "http"} {
		h := fmt.Sprintf("h%d", i)
		as = append(as, fn("a", "x/"+pkg, pkg+".go", "F"+h, h, 20))
		bs = append(bs, fn("b", "y/"+pkg, pkg+".go", "G"+h, h, 20))
	}
	got := Across(map[string]Report{"a": report(t, "a", as...), "b": report(t, "b", bs...)})
	require.Len(t, got.Suggestions, 1)
	assert.Equal(t, got.Suggestions[0].Kind, "framework", "four areas copied")
	assert.Equal(t, got.Suggestions[0].RedundantLOC, 80)
}

func TestAcrossPartial(t *testing.T) {
	r := Report{Truncated: true}
	got := Across(map[string]Report{"a": r, "b": {}})
	assert.Equal(t, got.Partial, []string{"a"})
	assert.Len(t, got.Groups, 0)
}
