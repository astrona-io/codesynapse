package vuln

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/parisnakitakejser/codesynapse/internal/testutil/assert"
	"github.com/parisnakitakejser/codesynapse/internal/testutil/require"
)

func TestParseGoList(t *testing.T) {
	out := []byte(`{"ImportPath":"example.com/app","Imports":["github.com/a/a/pkg","fmt"],"Module":{"Path":"example.com/app","Main":true}}
{"ImportPath":"fmt","Standard":true}
{"ImportPath":"github.com/a/a/pkg","Imports":["github.com/c/c","github.com/a/a/internal","fmt"],"Module":{"Path":"github.com/a/a"}}
{"ImportPath":"github.com/a/a/internal","Imports":["github.com/c/c/sub"],"Module":{"Path":"github.com/a/a"}}
{"ImportPath":"github.com/c/c","Imports":["github.com/d/d [github.com/d/d.test]"],"Module":{"Path":"github.com/c/c"}}
{"ImportPath":"github.com/c/c/sub","Module":{"Path":"github.com/c/c"}}
{"ImportPath":"github.com/d/d","Module":{"Path":"github.com/d/d"}}
{"ImportPath":"github.com/d/d [github.com/d/d.test]","Module":{"Path":"github.com/d/d"}}
`)
	got := parseGoList(out)
	assert.Equal(t, got["github.com/a/a"], []string{"github.com/c/c"}, "imports inside a module are not edges, repeats are merged")
	assert.Equal(t, got["github.com/c/c"], []string{"github.com/d/d"}, "a test variant counts as its package")
	_, main := got["example.com/app"]
	assert.False(t, main, "the main module is not an importer")
	assert.Equal(t, len(parseGoList([]byte("garbage"))), 0)
	assert.Equal(t, len(parseGoList(nil)), 0)
}

func TestParseNPMLockRequires(t *testing.T) {
	lock := []byte(`{"lockfileVersion":3,"packages":{
	  "":{"name":"app"},
	  "node_modules/a":{"version":"1.0.0","dependencies":{"b":"^1","c":"^2"}},
	  "node_modules/b":{"version":"1.1.0","optionalDependencies":{"d":"*"}},
	  "node_modules/c":{"version":"2.0.0"},
	  "node_modules/d":{"version":"3.0.0"}}}`)
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{"dependencies":{"a":"^1"}}`), 0o600))
	mods := parseNPMLock(lock, dir)
	by := map[string]Module{}
	for _, m := range mods {
		by[m.Path] = m
	}
	assert.Equal(t, by["a"].Requires, []string{"npm:b", "npm:c"})
	assert.Equal(t, by["b"].Requires, []string{"npm:d"})

	AssignPaths(mods, nil)
	by = map[string]Module{}
	for _, m := range mods {
		by[m.Path] = m
	}
	assert.Equal(t, by["a"].Via, "", "direct")
	assert.Equal(t, by["b"].Via, "a")
	assert.Equal(t, by["d"].Via, "a > b")
	assert.Equal(t, by["d"].Roots, 1)
}

func TestParseTOMLLockRequires(t *testing.T) {
	uv := `[[package]]
name = "requests"
version = "2.31.0"
dependencies = [
    { name = "certifi" },
    { name = "urllib3", marker = "python_version >= '3.8'" },
]

[[package]]
name = "certifi"
version = "2024.1.1"
`
	mods := parseTOMLLock([]byte(uv), t.TempDir())
	require.Len(t, mods, 2)
	assert.Equal(t, mods[0].Requires, []string{"PyPI:certifi", "PyPI:urllib3"})
	assert.Equal(t, len(mods[1].Requires), 0)

	poetry := `[[package]]
name = "Flask"
version = "3.0.0"

[package.dependencies]
click = ">=8.1.3"
Werkzeug = {version = ">=3.0.0", markers = "python_version >= '3.8'"}
python = "^3.8"

[package.extras]
async = ["asgiref"]
`
	mods = parseTOMLLock([]byte(poetry), t.TempDir())
	require.Len(t, mods, 1)
	assert.Equal(t, mods[0].Requires, []string{"PyPI:click", "PyPI:werkzeug"})
}

func TestAssignPaths(t *testing.T) {
	mods := []Module{
		{Path: "github.com/a/a", Version: "v1"},
		{Path: "github.com/b/b", Version: "v1"},
		{Path: "github.com/c/c", Version: "v1", Indirect: true},
		{Path: "github.com/d/d", Version: "v1", Indirect: true},
		{Path: "github.com/e/e", Version: "v1", Indirect: true}, // no edge known
	}
	AssignPaths(mods, map[string][]string{
		"github.com/a/a": {"github.com/c/c"},
		"github.com/b/b": {"github.com/c/c", "github.com/d/d"},
		"github.com/c/c": {"github.com/d/d"},
	})
	by := map[string]Module{}
	for _, m := range mods {
		by[m.Path] = m
	}
	assert.Equal(t, by["github.com/c/c"].Via, "github.com/a/a", "alphabetically first direct dependency wins a tie")
	assert.Equal(t, by["github.com/c/c"].Roots, 2, "reached from a and b")
	assert.Equal(t, by["github.com/d/d"].Via, "github.com/b/b", "b reaches d in one step, a only through c")
	assert.Equal(t, by["github.com/d/d"].Roots, 2)
	assert.Equal(t, by["github.com/e/e"].Via, "")
	assert.Equal(t, by["github.com/e/e"].Roots, 0, "unknown path stays empty")
	assert.Equal(t, by["github.com/a/a"].Via, "", "direct dependencies have no chain")

	// Cycles and edges to modules that are not part of the project are harmless.
	mods = []Module{{Path: "x"}, {Path: "y", Indirect: true}}
	AssignPaths(mods, map[string][]string{"x": {"y", "gone"}, "y": {"x", "y"}})
	assert.Equal(t, mods[1].Via, "x")
}
