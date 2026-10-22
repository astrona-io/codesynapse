package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/parisnakitakejser/codesynapse/internal/cleanup"
	"github.com/parisnakitakejser/codesynapse/internal/cleanupplan"
	"github.com/parisnakitakejser/codesynapse/internal/duplicates"
	"github.com/parisnakitakejser/codesynapse/internal/model"
	"github.com/parisnakitakejser/codesynapse/internal/store/memory"
	"github.com/parisnakitakejser/codesynapse/internal/testutil/assert"
	"github.com/parisnakitakejser/codesynapse/internal/testutil/require"
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
