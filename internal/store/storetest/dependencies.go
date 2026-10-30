package storetest

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/store"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

// dependencyContract checks DependencyGraph: every module and vulnerability
// of one repository, the requirement edges between modules, nothing from
// another repository and no other kind of edge.
func dependencyContract(t *testing.T, db CodeGraph) {
	ctx := context.Background()
	repo := fmt.Sprintf("deps-fixture-%d", time.Now().UnixNano())
	other := repo + "-other"
	t.Cleanup(func() { assert.NoError(t, db.DeleteRepo(ctx, repo)); assert.NoError(t, db.DeleteRepo(ctx, other)) })

	mod := func(r, name string) string { return model.NodeID(r, model.KindModule, name) }
	g := model.NewGraph()
	repoID := model.NodeID(repo, model.KindRepository, repo)
	g.AddNode(model.Node{ID: repoID, Repo: repo, Kind: model.KindRepository, Name: repo})
	for _, name := range []string{"b", "a", "npm:c"} {
		n := model.Node{ID: mod(repo, name), Repo: repo, Kind: model.KindModule, Name: name, Version: "1.0.0", Ecosystem: "Go", Via: "x > y", Roots: 2}
		if name == "a" {
			n.Scorecard, n.ScorecardDate, n.ScorecardChecks = 3.7, "2026-10-19", []string{"Code-Review:0", "Maintained:2"}
		}
		g.AddNode(n)
		g.AddEdge(model.Edge{From: repoID, To: mod(repo, name), Kind: model.EdgeDependsOn})
	}
	g.AddEdge(model.Edge{From: mod(repo, "a"), To: mod(repo, "b"), Kind: model.EdgeDependsOn})
	g.AddEdge(model.Edge{From: mod(repo, "b"), To: mod(repo, "npm:c"), Kind: model.EdgeDependsOn})
	vuln := model.NodeID(repo, model.KindVulnerability, "b@GO-1")
	g.AddNode(model.Node{ID: vuln, Repo: repo, Kind: model.KindVulnerability, Name: "GO-1", Severity: "high", Package: "b", Version: "1.0.0"})
	g.AddEdge(model.Edge{From: vuln, To: mod(repo, "b"), Kind: model.EdgeAffects})
	require.NoError(t, db.ReplaceRepo(ctx, repo, g))

	o := model.NewGraph()
	o.AddNode(model.Node{ID: model.NodeID(other, model.KindRepository, other), Repo: other, Kind: model.KindRepository, Name: other})
	o.AddNode(model.Node{ID: mod(other, "z"), Repo: other, Kind: model.KindModule, Name: "z", Version: "9"})
	require.NoError(t, db.ReplaceRepo(ctx, other, o))

	got, err := db.DependencyGraph(ctx, repo)
	require.NoError(t, err)
	assert.False(t, got.Truncated)
	require.Len(t, got.Modules, 3)
	assert.Equal(t, got.Modules[0].Name, "a", "ordered by ID")
	assert.Equal(t, got.Modules[0].Via, "x > y", "paths survive storage")
	assert.Equal(t, got.Modules[0].Roots, 2)
	assert.Equal(t, got.Modules[0].Scorecard, 3.7, "a float survives storage")
	assert.Equal(t, got.Modules[0].ScorecardDate, "2026-10-19")
	assert.Equal(t, got.Modules[0].ScorecardChecks, []string{"Code-Review:0", "Maintained:2"})
	assert.Equal(t, got.Modules[1].ScorecardDate, "", "unset stays unset")
	require.Len(t, got.Vulnerabilities, 1)
	assert.Equal(t, got.Vulnerabilities[0].Name, "GO-1")
	require.Len(t, got.Edges, 2, "module to module only: not from the repository, not AFFECTS")
	assert.Equal(t, got.Edges[0], model.Edge{From: mod(repo, "a"), To: mod(repo, "b"), Kind: model.EdgeDependsOn, Count: 1})
	assert.Equal(t, got.Edges[1].To, mod(repo, "npm:c"))

	_, err = db.DependencyGraph(ctx, repo+"-missing")
	assert.ErrorIs(t, err, store.ErrNotFound)
}
