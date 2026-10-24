package githistory

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"

	"github.com/astrona-io/codesynapse/internal/gitx"
	"github.com/astrona-io/codesynapse/internal/model"
)

func TestApply(t *testing.T) {
	g := model.NewGraph()
	fileID := model.NodeID("r", model.KindFile, "a.go")
	g.AddNode(model.Node{ID: fileID, Kind: model.KindFile, File: "a.go"})

	Apply("r", []gitx.Commit{
		{Hash: "aaaaaaaaaaaaaaaa", AuthorName: "Ada", AuthorEmail: "Ada@Example.com", Subject: "fix", Files: []gitx.FileStat{{Path: "a.go", Added: 2}, {Path: "gone.go"}}},
		{Hash: "bbbbbbbbbbbbbbbb", AuthorName: "Ada", AuthorEmail: "ada@example.com", Subject: "feat", Files: []gitx.FileStat{{Path: "a.go"}}},
	}, g)

	f, ok := g.Node(fileID)
	require.True(t, ok)
	assert.Equal(t, f.Churn, 2)

	authors := 0
	for _, n := range g.Nodes() {
		if n.Kind == model.KindAuthor {
			authors++
			assert.NotContains(t, n.ID, "example.com")
		}
	}
	assert.Equal(t, authors, 1, "e-mail matching is case-insensitive")

	for _, e := range g.Edges() {
		assert.NotEqual(t, e.To, model.NodeID("r", model.KindFile, "gone.go"))
	}
}

// History links and churn attach to graph files whose names git quotes in
// its log output: non-ASCII, tabs, quotes and backslashes.
func TestExtractAttachesHistoryToUnicodeAndEscapedNames(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		_, err := gitx.Run(context.Background(), dir, args...)
		require.NoError(t, err)
	}
	run("init", "-q")
	run("config", "user.email", "a@example.com")
	run("config", "user.name", "A")
	run("config", "commit.gpgsign", "false")
	g := model.NewGraph()
	names := []string{"café.go", "tab\there.go", "quo\"te.go", "back\\slash.go"}
	for _, n := range names {
		require.NoError(t, os.WriteFile(filepath.Join(dir, n), []byte("package x\n"), 0o600))
		g.AddNode(model.Node{ID: model.NodeID("r", model.KindFile, n), Kind: model.KindFile, File: n})
	}
	run("add", "-A")
	run("commit", "-q", "-m", "add")
	require.NoError(t, Extract(context.Background(), "r", dir, 10, g))

	for _, n := range names {
		f, ok := g.Node(model.NodeID("r", model.KindFile, n))
		require.True(t, ok, n)
		assert.Equal(t, f.Churn, 1, n)
	}
	modifies := 0
	for _, e := range g.Edges() {
		if e.Kind == model.EdgeModifies {
			modifies++
		}
	}
	assert.Equal(t, modifies, len(names))
}
