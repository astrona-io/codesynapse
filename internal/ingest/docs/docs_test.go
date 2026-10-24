package docs

import (
	"context"
	"os"
	"path/filepath"
	"strings"
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

func extractMarkdown(t *testing.T, readme string, code *model.Graph) *model.Graph {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "README.md"), []byte(readme), 0o600))
	g, err := Extract(context.Background(), "r", dir, code)
	require.NoError(t, err)
	return g
}

func nodesOf(g *model.Graph, kind model.NodeKind) []model.Node {
	var out []model.Node
	for _, n := range g.Nodes() {
		if n.Kind == kind {
			out = append(out, n)
		}
	}
	return out
}

func TestFencedCodeStaysOutOfSummaries(t *testing.T) {
	secret := "func leakedSecretBody()"
	runID := model.NodeID("r", model.KindFunction, "example.com/app.Run")
	code := model.NewGraph()
	code.AddNode(model.Node{ID: runID, Kind: model.KindFunction, Name: "Run", QualifiedName: "example.com/app.Run", Exported: true})

	for name, readme := range map[string]string{
		"tilde fence":          "# T\n\nProse.\n\n~~~go\n" + secret + " { `Run` }\n# not a heading\n~~~\n\nAfter.\n",
		"longer fence":         "# T\n\nProse.\n\n````md\n```go\n" + secret + " { `Run` }\n```\n# not a heading\n````\n\nAfter.\n",
		"indented fence":       "# T\n\nProse.\n\n   ```go\n   " + secret + " { `Run` }\n   ```\n\nAfter.\n",
		"mixed characters":     "# T\n\nProse.\n\n~~~\n```\n" + secret + " { `Run` }\n~~~\n\nAfter.\n",
		"unclosed fence":       "# T\n\nProse.\n\n```go\n" + secret + " { `Run` }\n# not a heading\n",
		"closing fence longer": "# T\n\nProse.\n\n```go\n" + secret + " { `Run` }\n`````\n\nAfter.\n",
	} {
		g := extractMarkdown(t, readme, code)
		for _, n := range g.Nodes() {
			assert.NotContains(t, n.Summary, "leakedSecretBody", name)
			assert.NotContains(t, n.Name, "not a heading", name)
		}
		for _, e := range g.Edges() {
			assert.True(t, e.Kind != model.EdgeMentions, name+": code in a fence mentions nothing")
		}
		assert.Len(t, nodesOf(g, model.KindSection), 1, name)
	}

	// Prose after a closed fence is still read, and a fence that is only
	// inline text does not start a block.
	g := extractMarkdown(t, "# T\n\n~~~\ncode\n~~~\n\nUse `Run` after.\n\nInline ```not a fence``` here.\n", code)
	mentions := 0
	for _, e := range g.Edges() {
		if e.Kind == model.EdgeMentions {
			mentions++
		}
	}
	assert.Equal(t, mentions, 1)
	assert.Contains(t, nodesOf(g, model.KindSection)[0].Summary, "Inline")
}

func TestDocumentTitleIsRedactedAndBounded(t *testing.T) {
	token := "ghp_" + strings.Repeat("a1B2", 9)
	g := extractMarkdown(t, "# Release notes "+token+" "+strings.Repeat("long ", 80)+"\n\nBody.\n", nil)
	docs := nodesOf(g, model.KindDocument)
	require.Len(t, docs, 1)
	assert.NotContains(t, docs[0].Name, "ghp_")
	assert.True(t, len([]rune(docs[0].Name)) <= 121, "bounded")
	sections := nodesOf(g, model.KindSection)
	require.Len(t, sections, 1)
	assert.NotContains(t, sections[0].Name, "ghp_")
	for _, n := range g.Nodes() {
		assert.NotContains(t, n.Name+n.Summary, token)
	}
}
