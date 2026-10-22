package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/astrona-io/codesynapse/internal/cleanup"
	"github.com/astrona-io/codesynapse/internal/cleanupplan"
	"github.com/astrona-io/codesynapse/internal/duplicates"
	"github.com/astrona-io/codesynapse/internal/history"
	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/refactorplan"
	"github.com/astrona-io/codesynapse/internal/store/memory"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

func dupGraph(t *testing.T, repo string, names ...string) *model.Graph {
	t.Helper()
	g := model.NewGraph()
	repoNode := g.AddNode(model.Node{ID: model.NodeID(repo, model.KindRepository, repo), Kind: model.KindRepository, Repo: repo, Name: repo})
	for _, name := range names {
		g.AddNode(model.Node{ID: model.NodeID(repo, model.KindFunction, name), Kind: model.KindFunction, Repo: repo, Name: name,
			QualifiedName: "pkg." + name, Package: "x/pkg", File: name + ".go", StartLine: 1, EndLine: 40, LOC: 40, CloneHash: "same"})
	}
	enc, err := duplicates.Encode(duplicates.Analyze(g))
	require.NoError(t, err)
	repoNode.Duplicates = enc
	return g
}

func TestDuplicateEndpoints(t *testing.T) {
	ctx := context.Background()
	st := memory.New()
	require.NoError(t, st.ReplaceRepo(ctx, "org/a", dupGraph(t, "org/a", "One", "Two")))
	require.NoError(t, st.ReplaceRepo(ctx, "org/b", dupGraph(t, "org/b", "Three")))
	ts := httptest.NewServer(New(ctx, Config{SourcesRoot: t.TempDir(), Version: "test"}, st, slog.New(slog.NewTextHandler(io.Discard, nil))))
	t.Cleanup(ts.Close)

	var one duplicates.Report
	require.Equal(t, get(t, ts, "/api/v1/repos/org%2Fa/duplicates", &one), 200)
	require.Len(t, one.Groups, 1)
	assert.Equal(t, one.Groups[0].Redundant, 40)
	assert.Len(t, one.Fingerprints, 0, "fingerprints stay server side")

	var cross duplicates.CrossReport
	require.Equal(t, get(t, ts, "/api/v1/shared-code", &cross), 200)
	assert.Equal(t, cross.Repos, 2)
	require.Len(t, cross.Groups, 1)
	assert.Equal(t, cross.Groups[0].Copies, 3)
	assert.Equal(t, cross.Groups[0].Repos, []string{"org/a", "org/b"})
}

func TestSBOMEndpoint(t *testing.T) {
	ctx := context.Background()
	st := memory.New()
	g := model.NewGraph()
	g.AddNode(model.Node{ID: model.NodeID("org/a", model.KindRepository, "org/a"), Kind: model.KindRepository, Repo: "org/a", Name: "org/a", Head: "abc", Time: "2026-10-22T10:00:00Z"})
	for _, name := range []string{"github.com/x/x", "github.com/y/y"} {
		g.AddNode(model.Node{ID: model.NodeID("org/a", model.KindModule, name), Kind: model.KindModule, Repo: "org/a", Name: name, Version: "v1.0.0", Ecosystem: "Go", License: "MIT"})
	}
	g.AddEdge(model.Edge{From: model.NodeID("org/a", model.KindModule, "github.com/x/x"), To: model.NodeID("org/a", model.KindModule, "github.com/y/y"), Kind: model.EdgeDependsOn})
	require.NoError(t, st.ReplaceRepo(ctx, "org/a", g))
	ts := httptest.NewServer(New(ctx, Config{SourcesRoot: t.TempDir(), Version: "test"}, st, slog.New(slog.NewTextHandler(io.Discard, nil))))
	t.Cleanup(ts.Close)

	res, err := http.Get(ts.URL + "/api/v1/repos/org%2Fa/sbom") //nolint:noctx // test
	require.NoError(t, err)
	defer res.Body.Close() //nolint:errcheck // test
	require.Equal(t, res.StatusCode, 200)
	assert.Equal(t, res.Header.Get("Content-Type"), "application/vnd.cyclonedx+json")
	assert.Equal(t, res.Header.Get("Content-Disposition"), `attachment; filename="org-a.cdx.json"`)
	var doc map[string]any
	require.NoError(t, json.NewDecoder(res.Body).Decode(&doc))
	assert.Equal(t, doc["specVersion"], "1.5")
	assert.Len(t, doc["components"], 2)
	assert.Len(t, doc["dependencies"], 2, "the project's and x's")

	assert.Equal(t, get(t, ts, "/api/v1/repos/nope/sbom", nil), 404)
	assert.Equal(t, sbomFileName("host.example/team/../x y"), "host.example-team-..-x-y.cdx.json")
}

func TestCleanupPlanEndpoint(t *testing.T) {
	ctx := context.Background()
	st := memory.New()
	g := dupGraph(t, "org/a", "One", "Two")
	rn, ok := g.Node(model.NodeID("org/a", model.KindRepository, "org/a"))
	require.True(t, ok)
	cl, err := cleanup.Encode(cleanup.Report{Modules: []cleanup.ModuleItem{{Path: "github.com/old/lib", Version: "v1"}}})
	require.NoError(t, err)
	rn.Cleanup = cl
	require.NoError(t, st.ReplaceRepo(ctx, "org/a", g))
	ts := httptest.NewServer(New(ctx, Config{SourcesRoot: t.TempDir(), Version: "test"}, st, slog.New(slog.NewTextHandler(io.Discard, nil))))
	t.Cleanup(ts.Close)

	var plan cleanupplan.Plan
	require.Equal(t, get(t, ts, "/api/v1/repos/org%2Fa/cleanup/plan", &plan), 200)
	kinds := map[string]bool{}
	for _, s := range plan.Steps {
		kinds[s.Kind] = true
	}
	assert.True(t, kinds[cleanupplan.RemoveDependency], "the unused module")
	assert.True(t, kinds[cleanupplan.MergeDuplicates], "the two identical functions")
	assert.Equal(t, plan.Summary.Steps, len(plan.Steps))
	assert.Equal(t, get(t, ts, "/api/v1/repos/nope/cleanup/plan", nil), 404)
}

func TestRefactorPlanEndpoint(t *testing.T) {
	ctx := context.Background()
	const hash = "0123456789abcdef"
	g := model.NewGraph()
	rn := g.AddNode(model.Node{ID: model.NodeID("org/a", model.KindRepository, "org/a"), Kind: model.KindRepository, Repo: "org/a", Name: "org/a"})
	for _, name := range []string{"One", "Two"} {
		g.AddNode(model.Node{ID: model.NodeID("org/a", model.KindFunction, "pkg."+name), Kind: model.KindFunction, Repo: "org/a", Name: name, QualifiedName: "pkg." + name,
			Package: "x/pkg", File: name + ".go", StartLine: 1, EndLine: 40, LOC: 40, CloneHash: hash, Signature: "func " + name + "() error"})
	}
	enc, err := duplicates.Encode(duplicates.Analyze(g))
	require.NoError(t, err)
	rn.Duplicates = enc
	st := memory.New()
	require.NoError(t, st.ReplaceRepo(ctx, "org/a", g))
	ts := httptest.NewServer(New(ctx, Config{SourcesRoot: t.TempDir(), Version: "test"}, st, slog.New(slog.NewTextHandler(io.Discard, nil))))
	t.Cleanup(ts.Close)

	var plan refactorplan.Plan
	require.Equal(t, get(t, ts, "/api/v1/repos/org%2Fa/duplicates/refactor?hash="+hash+"&kind=exact", &plan), 200)
	assert.Equal(t, plan.Scope, "package")
	assert.Equal(t, len(plan.Copies), 2)
	assert.True(t, plan.Name != "" && len(plan.Steps) > 3)

	assert.Equal(t, get(t, ts, "/api/v1/repos/org%2Fa/duplicates/refactor?hash=ffffffffffffffff", nil), 404)
	assert.Equal(t, get(t, ts, "/api/v1/repos/org%2Fa/duplicates/refactor?hash=zz", nil), 400)
	assert.Equal(t, get(t, ts, "/api/v1/repos/org%2Fa/duplicates/refactor?hash="+hash+"&kind=other", nil), 400)
	assert.Equal(t, get(t, ts, "/api/v1/repos/nope/duplicates/refactor?hash="+hash, nil), 404)
}

func TestHistoryEndpointAndRegressionItem(t *testing.T) {
	ctx := context.Background()
	st := memory.New()
	g := dupGraph(t, "org/a", "One", "Two")
	require.NoError(t, st.ReplaceRepo(ctx, "org/a", g))
	require.NoError(t, st.AddSnapshot(ctx, "org/a", history.Snapshot{At: "2026-10-01T00:00:00Z", Grade: "A", Score: 95}))
	ts := httptest.NewServer(New(ctx, Config{SourcesRoot: t.TempDir(), Version: "test"}, st, slog.New(slog.NewTextHandler(io.Discard, nil))))
	t.Cleanup(ts.Close)

	var tr history.Trend
	require.Equal(t, get(t, ts, "/api/v1/repos/org%2Fa/history", &tr), 200)
	assert.Len(t, tr.Snapshots, 1)
	assert.Len(t, tr.Regressions, 0, "one snapshot has nothing to compare with")
	var a Attention
	require.Equal(t, get(t, ts, "/api/v1/attention", &a), 200)
	for _, it := range a.Items {
		assert.True(t, it.Kind != "regression")
	}

	require.NoError(t, st.AddSnapshot(ctx, "org/a", history.Snapshot{At: "2026-10-02T00:00:00Z", Grade: "C", Score: 70, VulnsCalled: 2, DupPct: 1}))
	require.Equal(t, get(t, ts, "/api/v1/repos/org%2Fa/history?limit=1", &tr), 200)
	assert.Len(t, tr.Snapshots, 1, "limit keeps the latest")
	require.Equal(t, get(t, ts, "/api/v1/repos/org%2Fa/history", &tr), 200)
	assert.Equal(t, tr.Since, "2026-10-01T00:00:00Z")
	require.Equal(t, get(t, ts, "/api/v1/attention", &a), 200)
	var found *AttentionItem
	for i, it := range a.Items {
		if it.Kind == "regression" {
			found = &a.Items[i]
		}
	}
	require.NotNil(t, found)
	assert.Equal(t, found.Level, "high", "a worse grade and called vulnerabilities")
	assert.Contains(t, found.Title, "Security grade A to C")
	assert.Equal(t, found.Link, "/repos/org%2Fa/history")
	assert.Equal(t, get(t, ts, "/api/v1/repos/nope/history", nil), 404)
}
