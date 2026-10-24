package ingest

import (
	"context"
	"strings"
	"testing"

	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/sbom"
	"github.com/astrona-io/codesynapse/internal/store/memory"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

// A lockfile can install several versions of one package. Each is a module of
// the graph and a component of the SBOM, and a requirement points at the copy
// it really gets.
func TestEveryInstalledDependencyVersionReachesTheGraphAndTheSBOM(t *testing.T) {
	root := policyFiles(t, map[string]string{
		"package.json": `{"name":"app","dependencies":{"top":"1","shared":"1"}}`,
		"package-lock.json": `{"lockfileVersion":3,"packages":{
			"":{"name":"app","dependencies":{"top":"1","shared":"1"}},
			"node_modules/top":{"version":"1.0.0","dependencies":{"shared":"2"}},
			"node_modules/top/node_modules/shared":{"version":"2.0.0"},
			"node_modules/shared":{"version":"1.0.0"}}}`,
	})
	g, err := Run(context.Background(), Options{Repo: "r", Root: root})
	require.NoError(t, err)

	byVersion := map[string]model.Node{}
	for _, n := range g.Nodes() {
		if n.Kind == model.KindModule && n.Name == "shared" {
			byVersion[n.Version] = n
		}
	}
	require.Len(t, byVersion, 2, "both installed versions are modules of the graph")
	assert.Equal(t, byVersion["1.0.0"].ID, model.NodeID("r", model.KindModule, "npm:shared"), "the preferred version keeps the plain ID")
	assert.NotEqual(t, byVersion["2.0.0"].ID, byVersion["1.0.0"].ID)

	ctx := context.Background()
	st := memory.New()
	require.NoError(t, st.ReplaceRepo(ctx, "r", g))
	dg, err := st.DependencyGraph(ctx, "r")
	require.NoError(t, err)
	repoNode, ok := g.Node(model.NodeID("r", model.KindRepository, "r"))
	require.True(t, ok)
	doc := sbom.Build(*repoNode, dg, sbom.Options{Tool: "test"})

	refs := map[string]string{} // component version -> bom-ref
	for _, c := range doc.Components {
		if c.Name == "shared" {
			refs[c.Version] = c.BOMRef
		}
	}
	require.Len(t, refs, 2, "the SBOM lists both versions")
	assert.True(t, strings.Contains(refs["2.0.0"], "2.0.0") && strings.Contains(refs["1.0.0"], "1.0.0"))

	// top asks for shared 2 and gets the copy nested under it; the project
	// asks for shared 1 and gets the top-level one.
	depends := map[string][]string{}
	var topRef string
	for _, c := range doc.Components {
		if c.Name == "top" {
			topRef = c.BOMRef
		}
	}
	for _, d := range doc.Dependencies {
		depends[d.Ref] = d.DependsOn
	}
	assert.Equal(t, strings.Join(depends[topRef], ","), refs["2.0.0"], "top depends on shared 2.0.0 only")
	assert.True(t, slicesContains(depends["root"], refs["1.0.0"]), "the project depends on shared 1.0.0")
	assert.False(t, slicesContains(depends["root"], refs["2.0.0"]))
}

func slicesContains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
