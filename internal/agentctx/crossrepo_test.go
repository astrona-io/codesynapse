package agentctx

import (
	"context"
	"testing"

	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/store/memory"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

func TestPRContextListsCallersInOtherRepositories(t *testing.T) {
	ctx := context.Background()
	s := memory.New()
	const libMod, parse = "github.com/acme/lib", "github.com/acme/lib.Parse"

	lib := model.NewGraph()
	lib.AddNode(model.Node{ID: model.NodeID("lib", model.KindRepository, "lib"), Kind: model.KindRepository, Repo: "lib", Name: "lib", Meta: `{"go_module":"` + libMod + `"}`})
	file := model.NodeID("lib", model.KindFile, "lib.go")
	lib.AddNode(model.Node{ID: file, Kind: model.KindFile, Repo: "lib", Name: "lib.go", File: "lib.go"})
	fn := model.NodeID("lib", model.KindFunction, parse)
	lib.AddNode(model.Node{ID: fn, Kind: model.KindFunction, Repo: "lib", Name: "Parse", QualifiedName: parse, Package: libMod, File: "lib.go", StartLine: 3, EndLine: 9, Exported: true})
	lib.AddEdge(model.Edge{From: file, To: fn, Kind: model.EdgeContains})
	require.NoError(t, s.ReplaceRepo(ctx, "lib", lib))

	consumer := func(repo string, calls bool) {
		g := model.NewGraph()
		g.AddNode(model.Node{ID: model.NodeID(repo, model.KindRepository, repo), Kind: model.KindRepository, Repo: repo, Name: repo})
		g.AddNode(model.Node{ID: model.NodeID(repo, model.KindModule, libMod), Kind: model.KindModule, Repo: repo, Name: libMod, Version: "v1.2.0"})
		main := model.NodeID(repo, model.KindFunction, "github.com/acme/"+repo+".main")
		g.AddNode(model.Node{ID: main, Kind: model.KindFunction, Repo: repo, Name: "main", QualifiedName: "github.com/acme/" + repo + ".main", File: "main.go", StartLine: 7})
		if calls {
			ext := model.NodeID(repo, model.KindExternal, parse)
			g.AddNode(model.Node{ID: ext, Kind: model.KindExternal, Repo: repo, Name: "Parse", QualifiedName: parse, Package: libMod})
			g.AddEdge(model.Edge{From: main, To: ext, Kind: model.EdgeCalls})
		}
		require.NoError(t, s.ReplaceRepo(ctx, repo, g))
	}
	consumer("app", true)
	consumer("quiet", false) // requires the library, never calls Parse
	consumer("hidden", true) // calls it, but the caller may not see it

	got, err := Service{R: s}.PRContext(ctx, "lib", PROptions{Files: []string{"lib.go"}, Consumers: []string{"app", "quiet", "lib", "missing"}})
	require.NoError(t, err)
	require.Len(t, got.UsedElsewhere, 1)
	u := got.UsedElsewhere[0]
	assert.Equal(t, u.Repo, "app")
	assert.Equal(t, u.Version, "v1.2.0")
	assert.Equal(t, u.Callers, []string{"app.main main.go:7"})

	got, err = Service{R: s}.PRContext(ctx, "lib", PROptions{Files: []string{"lib.go"}})
	require.NoError(t, err)
	assert.Len(t, got.UsedElsewhere, 0)
}
