package ingest

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	repoconfig "github.com/astrona-io/codesynapse/internal/config"
	"github.com/astrona-io/codesynapse/internal/metrics"
	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/security"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

func policyFiles(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, text := range files {
		path := filepath.Join(root, filepath.FromSlash(name))
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
		require.NoError(t, os.WriteFile(path, []byte(text), 0o600))
	}
	return root
}

func TestPolicyApprovalAndFilteredGraph(t *testing.T) {
	root := policyFiles(t, map[string]string{
		".codesynapse.yaml":          "version: 1\nignore:\n  paths: [excluded/**, docs/old.md]\n",
		"app.js":                     "export function visible(x) { return x + 1; }\n",
		"excluded/app.js":            "export function hidden(x) { return x + 2; }\n",
		"excluded/go.mod":            "malformed ignored go module\n",
		"excluded/app.yaml":          "password: actual-secret-value\n",
		"excluded/package-lock.json": `{"lockfileVersion":3,"packages":{"node_modules/hidden-dependency":{"version":"1.0.0"}}}`,
		"docs/old.md":                "# Removed architecture\n",
		"docs/current.md":            "# Current architecture\n",
	})
	ctx := context.Background()
	_, err := Run(ctx, Options{Repo: "r", Root: root})
	assert.True(t, errors.Is(err, ErrPolicyApproval))
	policy, err := repoconfig.Load(ctx, root)
	require.NoError(t, err)
	g, err := Run(ctx, Options{Repo: "r", Root: root, ApprovedPolicyDigest: policy.Digest()})
	require.NoError(t, err)
	found := false
	for _, n := range g.Nodes() {
		assert.False(t, strings.HasPrefix(n.File, "excluded/"))
		assert.NotEqual(t, n.File, "docs/old.md")
		assert.NotEqual(t, n.File, repoconfig.Filename)
		assert.NotEqual(t, n.Name, "hidden-dependency")
		if n.Kind == model.KindFunction && n.Name == "visible" {
			found = true
		}
	}
	assert.True(t, found)
	node, ok := g.Node(model.NodeID("r", model.KindRepository, "r"))
	require.True(t, ok)
	meta, err := RepositoryPolicy(*node)
	require.NoError(t, err)
	assert.Equal(t, meta.Digest, policy.Digest())
	assert.Equal(t, meta.ExcludedFiles, 5)
	var metricReport metrics.Report
	require.NoError(t, json.Unmarshal([]byte(node.Metrics), &metricReport))
	assert.Equal(t, metricReport.Benchmark.PolicyDigest, policy.Digest())
	var coverage security.Coverage
	require.NoError(t, json.Unmarshal([]byte(node.Coverage), &coverage))
	assert.Equal(t, coverage.ExcludedFiles, 5)
	_, notChecked := coverage.Lines()
	assert.True(t, len(notChecked) > 0)
	// A revised request cannot inherit approval for the previous digest.
	require.NoError(t, os.WriteFile(filepath.Join(root, repoconfig.Filename), []byte("version: 1\nignore:\n  paths: [app.js]\n"), 0o600))
	_, err = Run(ctx, Options{Repo: "r", Root: root, ApprovedPolicyDigest: policy.Digest()})
	assert.True(t, errors.Is(err, ErrPolicyApproval))
}

func TestPolicyReplacementDropsRenamedAndDeletedFiles(t *testing.T) {
	root := policyFiles(t, map[string]string{"old.js": "export function task(x) { return x; }\n"})
	first, err := Run(context.Background(), Options{Repo: "r", Root: root})
	require.NoError(t, err)
	require.NoError(t, os.Rename(filepath.Join(root, "old.js"), filepath.Join(root, "new.js")))
	require.NoError(t, os.WriteFile(filepath.Join(root, repoconfig.Filename), []byte("version: 1\nignore:\n  paths: [new.js]\n"), 0o600))
	policy, err := repoconfig.Load(context.Background(), root)
	require.NoError(t, err)
	repo, _ := first.Node(model.NodeID("r", model.KindRepository, "r"))
	next, err := Run(context.Background(), Options{Repo: "r", Root: root, ApprovedPolicyDigest: policy.Digest(), PreviousIdentities: repo.Identities})
	require.NoError(t, err)
	for _, n := range next.Nodes() {
		assert.NotEqual(t, n.File, "old.js")
		assert.NotEqual(t, n.File, "new.js")
	}
}

func TestUnapprovedSuppressionsLeaveFindingsActive(t *testing.T) {
	root := policyFiles(t, map[string]string{
		".codesynapse.yaml": "version: 1\nscanners:\n  security:\n    ignore_rules:\n      - rule_id: sec-yaml-plain-secret\n        paths: [app.yaml]\n",
		"app.yaml":          "password: actual-secret-value\n",
	})
	g, err := Run(context.Background(), Options{Repo: "r", Root: root})
	require.NoError(t, err)
	found := false
	for _, n := range g.Nodes() {
		if n.Kind == model.KindFinding && n.Rule == "yaml-hardcoded-secret" {
			found = true
		}
	}
	assert.True(t, found)
}

func TestRootModulePath(t *testing.T) {
	dir := t.TempDir()
	assert.Equal(t, rootModulePath(dir), "", "no go.mod")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/app\n\ngo 1.22\n"), 0o600))
	assert.Equal(t, rootModulePath(dir), "example.com/app")
	n := model.Node{Kind: model.KindRepository, Meta: `{"repository_policy":{},"go_module":"example.com/app"}`}
	assert.Equal(t, model.GoModuleOf(n), "example.com/app")

	outside := filepath.Join(t.TempDir(), "go.mod")
	require.NoError(t, os.WriteFile(outside, []byte("module elsewhere.com/x\n"), 0o600))
	linked := t.TempDir()
	require.NoError(t, os.Symlink(outside, filepath.Join(linked, "go.mod")))
	assert.Equal(t, rootModulePath(linked), "", "a symlinked go.mod is not read")
}
