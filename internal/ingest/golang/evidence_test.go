package golang

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/tools/go/packages"

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

// dataflowTruth lists the targets of the fixture's calls through function
// values that a correct analysis must find: fn(2) and f(x) both call double.
var dataflowTruth = func() map[labelledEdge]bool {
	id := func(k model.NodeKind, local string) string { return model.NodeID("evidence", k, local) }
	double := id(model.KindFunction, "example.com/evidence/app.double")
	return map[labelledEdge]bool{
		{id(model.KindFunction, "example.com/evidence/app.Run"), double, model.EdgeCalls}: true,
		{id(model.KindFunction, "example.com/evidence/app.Map"), double, model.EdgeCalls}: true,
	}
}()

// TestDataflowPrecisionRecall checks the optional analysis against the
// labels; the numbers are reported in docs/EDGE_EVIDENCE.md.
func TestDataflowPrecisionRecall(t *testing.T) {
	g := extractEvidence(t, Options{Dataflow: true})
	typed, found, correct := 0, 0, 0
	for _, e := range g.Edges() {
		switch {
		case !model.ResolutionEdge(e.Kind):
		case e.Evidence == model.EvidenceTyped:
			typed++
			assert.True(t, evidenceTruth[labelledEdge{e.From, e.To, e.Kind}], "typed edges are unchanged")
		case e.Evidence == model.EvidenceDataflow:
			found++
			if dataflowTruth[labelledEdge{e.From, e.To, e.Kind}] {
				correct++
			} else {
				t.Errorf("unlabelled dataflow edge %s -> %s", e.From, e.To)
			}
		default:
			t.Errorf("unexpected evidence %q", e.Evidence)
		}
	}
	assert.Equal(t, typed, len(evidenceTruth))
	assert.Equal(t, found, len(dataflowTruth), "precision")
	assert.Equal(t, correct, len(dataflowTruth), "recall")

	id := func(k model.NodeKind, local string) string { return model.NodeID("evidence", k, local) }
	app := analysisOf(t, g, id(model.KindPackage, "example.com/evidence/app"))
	assert.Equal(t, app.Dataflow, model.DataflowVTA)
	assert.Equal(t, app.UnresolvedCalls, 3, "the typed count does not change")
	assert.Equal(t, app.DataflowCalls, 2, "fn(2) and f(x); the immediately called closure is static in SSA")
	run := analysisOf(t, g, id(model.KindFunction, "example.com/evidence/app.Run"))
	assert.Equal(t, run.UnresolvedCalls, 2)
	assert.Equal(t, run.DataflowCalls, 1)
	broken := analysisOf(t, g, id(model.KindPackage, "example.com/evidence/broken"))
	assert.Equal(t, broken.Dataflow, model.DataflowSkipped, "SSA needs well-typed code")

	off := extractEvidence(t, Options{})
	for _, e := range off.Edges() {
		assert.False(t, e.Evidence == model.EvidenceDataflow, "the analysis is opt-in")
	}
	assert.Equal(t, analysisOf(t, off, id(model.KindPackage, "example.com/evidence/app")).Dataflow, "")
}

func TestDataflowBounds(t *testing.T) {
	id := func(k model.NodeKind, local string) string { return model.NodeID("evidence", k, local) }
	g := extractEvidence(t, Options{Dataflow: true, MaxDataflowFunctions: 1})
	app := analysisOf(t, g, id(model.KindPackage, "example.com/evidence/app"))
	assert.Equal(t, app.Dataflow, model.DataflowSkipped, "too large: skipped, not failed")
	for _, e := range g.Edges() {
		assert.False(t, e.Evidence == model.EvidenceDataflow)
	}
	assert.Equal(t, len(g.Edges()), len(extractEvidence(t, Options{}).Edges()), "the typed graph is kept")
}

func TestDataflowCancellation(t *testing.T) {
	root, err := filepath.EvalSymlinks("testdata/evidence")
	require.NoError(t, err)
	root, err = filepath.Abs(root)
	require.NoError(t, err)
	pkgs, err := packages.Load(&packages.Config{
		Dir: root, Env: append(os.Environ(), "GOPROXY=off", "GOWORK=off", "CGO_ENABLED=0"),
		Mode: packages.NeedName | packages.NeedFiles | packages.NeedCompiledGoFiles | packages.NeedSyntax |
			packages.NeedTypes | packages.NeedTypesInfo | packages.NeedImports | packages.NeedModule,
	}, "./...")
	require.NoError(t, err)
	x := &extractor{opts: Options{Repo: "evidence", Root: root}, g: model.NewGraph(), modules: map[string]bool{"example.com/evidence": true}, analysis: map[string]*packageAnalysis{}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	assert.True(t, errors.Is(x.dataflow(ctx, pkgs), context.Canceled), "a cancelled ingest stops instead of skipping")
}
