package ingest

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	repoconfig "github.com/astrona-io/codesynapse/internal/config"
	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

func TestMarkInlineIgnores(t *testing.T) {
	root := policyFiles(t, map[string]string{
		"run.go": "package run\n" +
			"// codesynapse:ignore exec-variable fixed build input\n" +
			"exec.Command(name)\n" +
			"exec.Command(other) // codesynapse:ignore exec-variable\n" +
			"exec.Command(third) // codesynapse:ignore sql-from-request wrong rule\n" +
			"s := \"codesynapse:ignore exec-variable in a string\"; exec.Command(s)\n" +
			"exec.Command(fifth) /* codesynapse:ignore exec-variable token=ghp_0123456789abcdefghijklmnopqrstuvwxyzAB */\n",
		"values.yaml":  "# codesynapse:ignore sec-yaml-plain-secret test fixture only\npassword: x\n",
		"skipped/a.go": "// codesynapse:ignore exec-variable excluded path\nexec.Command(x)\n",
	})
	policy, err := repoconfig.Parse([]byte("version: 1\nignore:\n  paths: ['skipped/**']\n"))
	require.NoError(t, err)

	g := model.NewGraph()
	finding := func(id, rule, file string, line int) {
		g.AddNode(model.Node{ID: id, Kind: model.KindFinding, Repo: "r", Name: id, Rule: rule, File: file, StartLine: line})
	}
	finding("above", "exec-variable", "run.go", 3)
	finding("no-reason", "exec-variable", "run.go", 4)
	finding("other-rule", "exec-variable", "run.go", 5)
	finding("in-string", "exec-variable", "run.go", 6)
	finding("redacted", "exec-variable", "run.go", 7)
	finding("alias", "yaml-hardcoded-secret", "values.yaml", 2)
	finding("excluded", "exec-variable", "skipped/a.go", 2)
	finding("missing", "exec-variable", "gone.go", 1)
	markInlineIgnores(context.Background(), root, g, policy)

	ignore := func(id string) string { n, _ := g.Node(id); return n.Ignore }
	assert.Equal(t, ignore("above"), "fixed build input")
	assert.Equal(t, ignore("no-reason"), "")
	assert.Equal(t, ignore("other-rule"), "")
	assert.Equal(t, ignore("in-string"), "")
	assert.NotContains(t, ignore("redacted"), "ghp_0123456789")
	assert.Equal(t, ignore("alias"), "test fixture only")
	assert.Equal(t, ignore("excluded"), "")
	assert.Equal(t, ignore("missing"), "")
}

func TestMarkInlineIgnoresSkipsSymlinks(t *testing.T) {
	outside := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(outside, "x.go"), []byte("// codesynapse:ignore exec-variable outside\nexec.Command(x)\n"), 0o600))
	root := t.TempDir()
	require.NoError(t, os.Symlink(filepath.Join(outside, "x.go"), filepath.Join(root, "x.go")))
	g := model.NewGraph()
	g.AddNode(model.Node{ID: "f", Kind: model.KindFinding, Repo: "r", Rule: "exec-variable", File: "x.go", StartLine: 2})
	markInlineIgnores(context.Background(), root, g, nil)
	n, _ := g.Node("f")
	assert.Equal(t, n.Ignore, "")
}

func TestRunStoresInlineIgnoreOnFinding(t *testing.T) {
	root := policyFiles(t, map[string]string{
		"app.yaml": "# codesynapse:ignore yaml-hardcoded-secret local test fixture\npassword: actual-secret-value\n",
	})
	g, err := Run(context.Background(), Options{Repo: "r", Root: root})
	require.NoError(t, err)
	found := false
	for _, n := range g.Nodes() {
		if n.Kind == model.KindFinding && n.Rule == "yaml-hardcoded-secret" {
			found = true
			assert.Equal(t, n.Ignore, "local test fixture")
		}
	}
	assert.True(t, found, "the finding stays in the graph")
}
