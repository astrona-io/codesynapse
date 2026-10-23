package ingest

import (
	"testing"

	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
)

func TestAnalysisSummary(t *testing.T) {
	g := model.NewGraph()
	pkg := model.Analysis{Level: model.LevelTyped, Status: model.StatusTruncated, Errors: 1, IgnoredFiles: 2, UnresolvedCalls: 3, OmittedBodies: 4}
	g.AddNode(model.Node{ID: "p", Kind: model.KindPackage, Analysis: pkg.Encode()})
	// Go files repeat their package's status; they count once each.
	g.AddNode(model.Node{ID: "f1", Kind: model.KindFile, Analysis: model.Analysis{Level: model.LevelTyped, Status: model.StatusTruncated}.Encode()})
	g.AddNode(model.Node{ID: "f2", Kind: model.KindFile, Analysis: model.Analysis{Level: model.LevelTyped, Status: model.StatusTruncated}.Encode()})
	g.AddNode(model.Node{ID: "py", Kind: model.KindFile, Analysis: model.Analysis{Level: model.LevelSyntactic, Status: model.StatusPartial, Errors: 1}.Encode()})
	g.AddNode(model.Node{ID: "legacy", Kind: model.KindFile})
	g.AddEdge(model.Edge{From: "f1", To: "f2", Kind: model.EdgeCalls, Evidence: model.EvidenceTyped})
	g.AddEdge(model.Edge{From: "py", To: "f2", Kind: model.EdgeCalls, Evidence: model.EvidenceInferred})
	g.AddEdge(model.Edge{From: "py", To: "f1", Kind: model.EdgeReferences})
	g.AddEdge(model.Edge{From: "p", To: "f1", Kind: model.EdgeContains})

	sum := analysisSummary(g)
	assert.Equal(t, sum.Truncated, 2)
	assert.Equal(t, sum.Partial, 1)
	assert.Equal(t, sum.Errors, 1, "file errors are not added to their package's")
	assert.Equal(t, sum.IgnoredFiles, 2)
	assert.Equal(t, sum.UnresolvedCalls, 3)
	assert.Equal(t, sum.OmittedBodies, 4)
	assert.Equal(t, sum.Edges, map[model.Evidence]int{model.EvidenceTyped: 1, model.EvidenceInferred: 1, model.EvidenceUnknown: 1})
}
