package golang

import (
	"testing"

	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

func TestFirstPartyPrefix(t *testing.T) {
	for module, want := range map[string]string{
		"github.com/acme/app":        "github.com/acme",
		"github.com/acme/app/v2":     "github.com/acme",
		"gitlab.com/group/sub/app":   "gitlab.com/group",
		"go.acme.dev/app":            "go.acme.dev",
		"git.acme.internal/team/app": "git.acme.internal",
		"github.com":                 "",
		"app":                        "",
		"":                           "",
	} {
		assert.Equal(t, FirstPartyPrefix(module), want, module)
	}
}

func TestLinkFirstParty(t *testing.T) {
	g := model.NewGraph()
	main := model.NodeID("app", model.KindFunction, "github.com/acme/app.main")
	run := model.NodeID("app", model.KindFunction, "github.com/acme/app.run")
	g.AddNode(model.Node{ID: main, Kind: model.KindFunction, Repo: "app"})
	g.AddNode(model.Node{ID: run, Kind: model.KindFunction, Repo: "app"})
	sensitive := model.NodeID("app", model.KindExternal, "github.com/acme/lib.Exec")
	g.AddNode(model.Node{ID: sensitive, Kind: model.KindExternal, Repo: "app", SecurityTags: []string{"exec"}})
	g.AddEdge(model.Edge{From: run, To: sensitive, Kind: model.EdgeCalls})

	LinkFirstParty("app", g, &Result{
		ExternalCalls: map[string][]string{
			"(*github.com/acme/lib.Client).Do": {main, run, main},
			"github.com/acme/lib.Exec":         {run},            // already linked during extraction
			"github.com/other/lib.F":           {main},           // another owner
			"github.com/acme/lib.Gone":         {"app|vanished"}, // a caller that is not a node
		},
		ExternalPackages: map[string]string{
			"(*github.com/acme/lib.Client).Do": "github.com/acme/lib",
			"github.com/acme/lib.Exec":         "github.com/acme/lib",
			"github.com/other/lib.F":           "github.com/other/lib",
			"github.com/acme/lib.Gone":         "github.com/acme/lib",
		},
	}, "github.com/acme/app")

	do := model.NodeID("app", model.KindExternal, "(*github.com/acme/lib.Client).Do")
	n, ok := g.Node(do)
	require.True(t, ok)
	assert.Equal(t, n.Name, "Do")
	assert.Equal(t, n.Package, "github.com/acme/lib")
	counts := map[string]int{}
	for _, e := range g.Edges() {
		if e.Kind == model.EdgeCalls {
			counts[e.From+" -> "+e.To] = e.Count
		}
	}
	assert.Equal(t, counts, map[string]int{main + " -> " + do: 2, run + " -> " + do: 1, run + " -> " + sensitive: 1})
	_, ok = g.Node(model.NodeID("app", model.KindExternal, "github.com/other/lib.F"))
	assert.False(t, ok, "another owner's module")

	before := len(g.Nodes())
	LinkFirstParty("app", g, &Result{ExternalCalls: map[string][]string{"fmt.Println": {main}}, ExternalPackages: map[string]string{"fmt.Println": "fmt"}}, "example")
	assert.Equal(t, len(g.Nodes()), before, "no host to go by")
}
