package golang

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	repoconfig "github.com/astrona-io/codesynapse/internal/config"
	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

func TestPolicyFiltersImportedGoAndNestedModules(t *testing.T) {
	root := t.TempDir()
	for rel, src := range map[string]string{
		"go.mod":             "module example.com/root\n\ngo 1.23\n",
		"main.go":            "package root\nimport \"example.com/root/hidden\"\nfunc Visible() string { return hidden.Secret() }\n",
		"ignored.go":         "package root\nimport \"os/exec\"\nfunc Ignored() { exec.Command(\"echo\") }\n",
		"hidden/hidden.go":   "package hidden\nfunc Secret() string { return \"confidential\" }\n",
		"nested/go.mod":      "module example.com/nested\n\ngo 1.23\n",
		"nested/nested.go":   "package nested\nfunc Included() {}\n",
		"excluded/go.mod":    "malformed excluded module\n",
		"excluded/broken.go": "invalid Go syntax\n",
	} {
		p := filepath.Join(root, rel)
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o700))
		require.NoError(t, os.WriteFile(p, []byte(src), 0o600))
	}
	policy, err := repoconfig.Parse([]byte("version: 1\nignore:\n  paths: [hidden/**, excluded/**, ignored.go]\n"))
	require.NoError(t, err)
	res, err := Extract(repoconfig.WithPolicy(context.Background(), policy), Options{Repo: "r", Root: root})
	require.NoError(t, err)
	found := map[string]bool{}
	for _, n := range res.Graph.Nodes() {
		assert.False(t, strings.HasPrefix(n.File, "hidden/") || strings.HasPrefix(n.File, "excluded/"))
		if n.Kind == model.KindFunction {
			found[n.Name] = true
		}
	}
	assert.True(t, found["Visible"])
	assert.True(t, found["Included"])
	assert.False(t, found["Secret"])
	assert.False(t, found["Ignored"])
	assert.Equal(t, len(res.ExternalImports["os/exec"]), 0)
	for _, e := range res.Graph.Edges() {
		_, from := res.Graph.Node(e.From)
		_, to := res.Graph.Node(e.To)
		assert.True(t, from && to)
	}
}

func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for rel, src := range files {
		p := filepath.Join(root, rel)
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o700))
		require.NoError(t, os.WriteFile(p, []byte(src), 0o600))
	}
	return root
}

func functionNames(res *Result) []string {
	var out []string
	for _, n := range res.Graph.Nodes() {
		if n.Kind == model.KindFunction {
			out = append(out, n.Name)
		}
	}
	slices.Sort(out)
	return out
}

// Nested modules are indexed whether or not the repository has a policy file,
// and a neutral policy changes nothing about what is found.
func TestNestedModulesAreIndexedWithoutAPolicy(t *testing.T) {
	root := writeTree(t, map[string]string{
		"go.mod":                      "module example.com/root\n\ngo 1.23\n",
		"root.go":                     "package root\nfunc RootFunction() {}\n",
		"tools/go.mod":                "module example.com/tools\n\ngo 1.23\n",
		"tools/tools.go":              "package tools\nfunc NestedFunction() {}\n",
		"tools/deep/deeper/deeper.go": "package deeper\nfunc DeepFunction() {}\n",
		"vendor/v/v.go":               "package v\nfunc Vendored() {}\n",
		"_scratch/s.go":               "package s\nfunc Scratch() {}\n",
		"sub/inner/go.mod":            "module example.com/inner\n\ngo 1.23\n",
		"sub/inner/.git/HEAD":         "ref: refs/heads/main\n", // a nested repository keeps its own boundary
		"sub/inner/inner.go":          "package inner\nfunc InnerRepo() {}\n",
	})
	want := []string{"DeepFunction", "NestedFunction", "RootFunction"}

	none, err := Extract(context.Background(), Options{Repo: "r", Root: root})
	require.NoError(t, err)
	assert.Equal(t, strings.Join(functionNames(none), ","), strings.Join(want, ","))

	neutral, err := repoconfig.Parse([]byte("version: 1\n"))
	require.NoError(t, err)
	withPolicy, err := Extract(repoconfig.WithPolicy(context.Background(), neutral), Options{Repo: "r", Root: root})
	require.NoError(t, err)
	assert.Equal(t, strings.Join(functionNames(withPolicy), ","), strings.Join(want, ","))
	assert.Equal(t, len(none.Graph.Nodes()), len(withPolicy.Graph.Nodes()), "identical coverage")

	// A policy exclusion still removes a nested module.
	skip, err := repoconfig.Parse([]byte("version: 1\nignore:\n  paths: [tools/**]\n"))
	require.NoError(t, err)
	res, err := Extract(repoconfig.WithPolicy(context.Background(), skip), Options{Repo: "r", Root: root})
	require.NoError(t, err)
	assert.Equal(t, strings.Join(functionNames(res), ","), "RootFunction")
}
