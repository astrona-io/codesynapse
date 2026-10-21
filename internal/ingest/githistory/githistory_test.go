package githistory

import (
	"testing"

	"github.com/parisnakitakejser/codesynapse/internal/testutil/assert"
	"github.com/parisnakitakejser/codesynapse/internal/testutil/require"

	"github.com/parisnakitakejser/codesynapse/internal/gitx"
	"github.com/parisnakitakejser/codesynapse/internal/model"
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
