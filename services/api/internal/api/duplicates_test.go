package api

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"testing"

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
