package docs

import (
	"context"
	"testing"

	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"

	"github.com/astrona-io/codesynapse/internal/model"
)

func TestExtract(t *testing.T) {
	code := model.NewGraph()
	runID := model.NodeID("r", model.KindFunction, "example.com/app.Run")
	saveID := model.NodeID("r", model.KindMethod, "(*example.com/store.Disk).Save")
	code.AddNode(model.Node{ID: runID, Kind: model.KindFunction, Name: "Run", QualifiedName: "example.com/app.Run", Exported: true})
	code.AddNode(model.Node{ID: saveID, Kind: model.KindMethod, Name: "Save", QualifiedName: "(*example.com/store.Disk).Save", Exported: true})

	g, err := Extract(context.Background(), "r", "testdata/repo", code)
	require.NoError(t, err)

	var sections []model.Node
	for _, n := range g.Nodes() {
		if n.Kind == model.KindSection {
			sections = append(sections, n)
		}
	}
	require.Len(t, sections, 3)
	assert.Equal(t, sections[0].Name, "Sample")
	assert.Equal(t, sections[0].StartLine, 1)
	assert.Equal(t, sections[0].EndLine, 4)
	assert.NotContains(t, sections[0].Summary, "ghp_")
	assert.Equal(t, sections[1].Name, "Usage")
	assert.Equal(t, sections[2].Name, "Details")
	assert.Equal(t, sections[2].Level, 3)

	doc, ok := g.Node(model.NodeID("r", model.KindDocument, "README.md"))
	require.True(t, ok)
	assert.Equal(t, doc.Name, "Sample")

	mentions := map[string]bool{}
	for _, e := range g.Edges() {
		if e.Kind == model.EdgeMentions {
			mentions[e.To] = true
		}
	}
	assert.True(t, mentions[runID])
	assert.True(t, mentions[saveID])
}

func TestReceiverShort(t *testing.T) {
	assert.Equal(t, receiverShort("(*example.com/p.T).M"), "T")
	assert.Equal(t, receiverShort("(example.com/p.I).M"), "I")
	assert.Equal(t, receiverShort("pkg.F"), "")
}
