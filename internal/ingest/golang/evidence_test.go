package golang

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

type labelledEdge struct {
	from, to string
	kind     model.EdgeKind
}

// evidenceTruth lists, by hand, every in-repository resolution edge of
// testdata/evidence. Calls through f, fn, the closure and undefined() have
// no knowable target and are absent on purpose.
var evidenceTruth = func() map[labelledEdge]bool {
	id := func(k model.NodeKind, local string) string { return model.NodeID("evidence", k, local) }
	run := id(model.KindFunction, "example.com/evidence/app.Run")
	return map[labelledEdge]bool{
		{run, id(model.KindInterfaceMethod, "(example.com/evidence/app.Store).Save"), model.EdgeCalls}:                                                                 true,
		{run, id(model.KindFunction, "example.com/evidence/app.Map"), model.EdgeCalls}:                                                                                 true,
		{run, id(model.KindFunction, "example.com/evidence/app.helper"), model.EdgeCalls}:                                                                              true,
		{run, id(model.KindFunction, "example.com/evidence/app.double"), model.EdgeReferences}:                                                                         true,
		{id(model.KindType, "example.com/evidence/app.disk"), id(model.KindType, "example.com/evidence/app.Store"), model.EdgeImplements}:                              true,
		{id(model.KindInterfaceMethod, "(example.com/evidence/app.Store).Save"), id(model.KindMethod, "(example.com/evidence/app.disk).Save"), model.EdgeDispatchesTo}: true,
		{id(model.KindFunction, "example.com/evidence/broken.Broken"), id(model.KindFunction, "example.com/evidence/broken.ok"), model.EdgeCalls}:                      true,
	}
}()

func extractEvidence(t *testing.T, opts Options) *model.Graph {
	t.Helper()
	root, err := filepath.Abs("testdata/evidence")
	require.NoError(t, err)
	opts.Repo, opts.Root = "evidence", root
	res, err := Extract(context.Background(), opts)
	require.NoError(t, err)
	return res.Graph
}

func analysisOf(t *testing.T, g *model.Graph, id string) model.Analysis {
	t.Helper()
	n, ok := g.Node(id)
	require.True(t, ok, "missing node "+id)
	a, err := model.DecodeAnalysis(n.Analysis)
	require.NoError(t, err)
	return a
}

// TestEvidencePrecisionRecall compares typed edges with the labels. The
// numbers are reported in docs/EDGE_EVIDENCE.md.
func TestEvidencePrecisionRecall(t *testing.T) {
	g := extractEvidence(t, Options{})
	found, correct := 0, 0
	for _, e := range g.Edges() {
		if !model.ResolutionEdge(e.Kind) {
			continue
		}
		assert.Equal(t, e.Evidence, model.EvidenceTyped, "go/types resolves every Go edge")
		found++
		if evidenceTruth[labelledEdge{e.From, e.To, e.Kind}] {
			correct++
		} else {
			t.Errorf("unlabelled edge %s -[%s]-> %s", e.From, e.Kind, e.To)
		}
	}
	assert.Equal(t, found, len(evidenceTruth), "precision")
	assert.Equal(t, correct, len(evidenceTruth), "recall")
}

func TestAnalysisRecords(t *testing.T) {
	g := extractEvidence(t, Options{})
	id := func(k model.NodeKind, local string) string { return model.NodeID("evidence", k, local) }

	app := analysisOf(t, g, id(model.KindPackage, "example.com/evidence/app"))
	assert.Equal(t, app.Version, model.AnalysisVersion)
	assert.Equal(t, app.Level, model.LevelTyped)
	assert.Equal(t, app.Status, model.StatusComplete)
	assert.Equal(t, app.IgnoredFiles, 1, "tagged.go is excluded by its build tag")
	assert.Equal(t, app.UnresolvedCalls, 3, "f(x), fn(2) and the closure; len and int64 are not calls to find")
	_, tagged := g.Node(id(model.KindFunction, "example.com/evidence/app.Tagged"))
	assert.False(t, tagged)

	run := analysisOf(t, g, id(model.KindFunction, "example.com/evidence/app.Run"))
	assert.Equal(t, run.UnresolvedCalls, 2)
	mapFn := analysisOf(t, g, id(model.KindFunction, "example.com/evidence/app.Map"))
	assert.Equal(t, mapFn.UnresolvedCalls, 1)
	helper := analysisOf(t, g, id(model.KindFunction, "example.com/evidence/app.helper"))
	assert.Equal(t, helper.Version, 0, "nothing to say about a fully resolved body")

	broken := analysisOf(t, g, id(model.KindPackage, "example.com/evidence/broken"))
	assert.Equal(t, broken.Status, model.StatusPartial)
	assert.True(t, broken.Errors >= 1)
	assert.Equal(t, broken.UnresolvedCalls, 1, "undefined() has no target")
	file := analysisOf(t, g, id(model.KindFile, "broken/broken.go"))
	assert.Equal(t, file.Level, model.LevelTyped)
	assert.Equal(t, file.Status, model.StatusPartial)
}

func TestCallSiteBudget(t *testing.T) {
	g := extractEvidence(t, Options{MaxCallSites: 1})
	id := func(k model.NodeKind, local string) string { return model.NodeID("evidence", k, local) }
	app := analysisOf(t, g, id(model.KindPackage, "example.com/evidence/app"))
	assert.Equal(t, app.Status, model.StatusTruncated)
	assert.True(t, app.OmittedBodies > 0)
	file := analysisOf(t, g, id(model.KindFile, "app/app.go"))
	assert.Equal(t, file.Status, model.StatusTruncated)
	truncated := 0
	for _, n := range g.Nodes() {
		if a, err := model.DecodeAnalysis(n.Analysis); err == nil && n.Kind.IsCallable() && a.OmittedBodies > 0 {
			assert.Equal(t, a.Status, model.StatusTruncated)
			truncated++
		}
	}
	assert.True(t, truncated > 0, "skipped bodies are marked on their functions")
}
