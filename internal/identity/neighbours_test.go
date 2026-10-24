package identity

import (
	"fmt"
	"testing"

	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

func neighbourhoodGraph(name, file, revision string, degree int) *model.Graph {
	n := declaration(name, file)
	g := graph(revision, n)
	for i := 0; i < degree; i++ {
		id := model.NodeID("r", model.KindExternal, fmt.Sprintf("external.Call%d", i))
		g.AddNode(model.Node{ID: id, Repo: "r", Kind: model.KindExternal})
		g.AddEdge(model.Edge{From: n.ID, To: id, Kind: model.EdgeCalls})
	}
	if degree >= 2 {
		caller := declaration("Caller", "caller.go")
		g.AddNode(caller)
		g.AddEdge(model.Edge{From: caller.ID, To: n.ID, Kind: model.EdgeCalls})
	}
	return g
}

func TestNeighbourhoodCorroboratesRenameAndMove(t *testing.T) {
	for _, tt := range []struct {
		name   string
		degree int
		stable bool
	}{{"two independent neighbours", 2, true}, {"one neighbour is insufficient", 1, false}, {"incomplete neighbourhood", MaxNeighbours + 1, false}} {
		t.Run(tt.name, func(t *testing.T) {
			base, err := Reconcile(neighbourhoodGraph("Before", "old.go", "one", tt.degree), "r", Report{Repo: "r", Version: 1})
			require.NoError(t, err)
			g := neighbourhoodGraph("After", "new.go", "two", tt.degree)
			next, err := Reconcile(g, "r", base)
			require.NoError(t, err)
			n, _ := g.Node(declaration("After", "new.go").ID)
			assert.Equal(t, n.SymbolID == base.Symbols[0].ID, tt.stable)
			again, err := Reconcile(g, "r", next)
			require.NoError(t, err)
			for _, s := range again.Symbols {
				if s.ID == n.SymbolID {
					for _, prior := range next.Symbols {
						if prior.ID == s.ID {
							assert.Equal(t, len(s.History), len(prior.History), "unchanged ingest does not add history")
						}
					}
				}
			}
		})
	}
}

func TestNeighbourhoodDoesNotCollapseCopies(t *testing.T) {
	base, err := Reconcile(neighbourhoodGraph("Before", "old.go", "one", 2), "r", Report{Repo: "r", Version: 1})
	require.NoError(t, err)
	g := neighbourhoodGraph("Before", "old.go", "two", 2)
	copyGraph := neighbourhoodGraph("Copy", "new.go", "two", 2)
	for _, n := range copyGraph.Nodes() {
		if n.Kind != model.KindRepository {
			g.AddNode(n)
		}
	}
	for _, e := range copyGraph.Edges() {
		g.AddEdge(e)
	}
	_, err = Reconcile(g, "r", base)
	require.NoError(t, err)
	original, _ := g.Node(declaration("Before", "old.go").ID)
	copied, _ := g.Node(declaration("Copy", "new.go").ID)
	assert.Equal(t, original.SymbolID, base.Symbols[0].ID)
	assert.True(t, copied.SymbolID != original.SymbolID)
}

func TestNeighbourhoodEncodingSeparatesUntrustedNames(t *testing.T) {
	prefix := "out:CALLS:external:r|External|"
	fingerprint := func(names []string) string {
		fn := declaration("F", "f.go")
		g := graph("one", fn)
		for _, name := range names {
			id := model.NodeID("r", model.KindExternal, name)
			g.AddNode(model.Node{ID: id, Repo: "r", Kind: model.KindExternal})
			g.AddEdge(model.Edge{From: fn.ID, To: id, Kind: model.EdgeCalls})
		}
		return neighbours(g, nil)[fn.ID].hash
	}
	one := fingerprint([]string{"a", "b\n" + prefix + "c"})
	two := fingerprint([]string{"a\n" + prefix + "b", "c"})
	assert.True(t, one != "" && two != "" && one != two)
}
