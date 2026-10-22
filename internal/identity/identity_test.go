package identity

import (
	"fmt"
	"testing"

	"github.com/parisnakitakejser/codesynapse/internal/model"
	"github.com/parisnakitakejser/codesynapse/internal/testutil/assert"
	"github.com/parisnakitakejser/codesynapse/internal/testutil/require"
)

func declaration(name, file string) model.Node {
	return model.Node{ID: model.NodeID("r", model.KindFunction, "pkg."+name), Repo: "r", Kind: model.KindFunction, Name: name, Language: "go", File: file, Signature: "func " + name + "(x int) int", BodyHash: name + "-body", CloneHash: "same-structure", StartLine: 3, EndLine: 12}
}

func graph(revision string, nodes ...model.Node) *model.Graph {
	g := model.NewGraph()
	g.AddNode(model.Node{ID: model.NodeID("r", model.KindRepository, "r"), Kind: model.KindRepository, Repo: "r", Head: revision})
	for _, n := range nodes {
		g.AddNode(n)
	}
	return g
}

func TestReconcile(t *testing.T) {
	old := declaration("Before", "a.go")
	base, err := Reconcile(graph("rev1", old), "r", Report{Version: 1, Repo: "r"})
	require.NoError(t, err)
	id := base.Symbols[0].ID
	for _, tt := range []struct {
		name   string
		nodes  []model.Node
		stable bool
		review bool
	}{
		{"ordinary edit", []model.Node{func() model.Node { n := old; n.BodyHash = "edited"; n.StartLine = 40; return n }()}, true, false},
		{"rename", []model.Node{declaration("After", "a.go")}, true, false},
		{"move", []model.Node{declaration("Before", "b.go")}, true, false},
		{"copy keeps separate identity", []model.Node{old, declaration("Copy", "a.go")}, false, true},
		{"split is ambiguous", []model.Node{declaration("First", "a.go"), declaration("Second", "a.go")}, false, true},
		{"no evidence", []model.Node{func() model.Node { n := declaration("Other", "z.go"); n.CloneHash = "different"; return n }()}, false, false},
		{"signature mismatch", []model.Node{func() model.Node {
			n := declaration("Other", "a.go")
			n.Signature = "func Other(x string) bool"
			return n
		}()}, false, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			g := graph("rev2", tt.nodes...)
			r, err := Reconcile(g, "r", base)
			require.NoError(t, err)
			n, _ := g.Node(tt.nodes[len(tt.nodes)-1].ID)
			assert.Equal(t, n.SymbolID == id, tt.stable)
			assert.Equal(t, n.Revision, "rev2")
			var latest Symbol
			for _, s := range r.Symbols {
				if s.ID == n.SymbolID {
					latest = s
				}
			}
			assert.Equal(t, len(latest.Candidates) > 0, tt.review)
			if tt.review {
				assert.Equal(t, latest.Candidates[0], id)
				// Review evidence must survive the next unchanged ingest.
				next, err := Reconcile(graph("rev2", tt.nodes...), "r", r)
				require.NoError(t, err)
				for _, s := range next.Symbols {
					if s.ID == latest.ID {
						assert.Equal(t, s.Candidates, latest.Candidates)
					}
				}
			}
			assert.Equal(t, base.Symbols[0].Current.Revision, "rev1", "input remains immutable")
			assert.Equal(t, len(base.Symbols[0].History), 0)
		})
	}
}

func TestAmbiguousMerge(t *testing.T) {
	a, b := declaration("A", "a.go"), declaration("B", "a.go")
	base, err := Reconcile(graph("one", a, b), "r", Report{Version: 1, Repo: "r"})
	require.NoError(t, err)
	r, err := Reconcile(graph("two", declaration("Merged", "a.go")), "r", base)
	require.NoError(t, err)
	assert.Equal(t, len(r.Symbols), 3)
	assert.Equal(t, len(r.Symbols[2].Candidates), 2)
	assert.False(t, r.Symbols[0].Active)
	assert.False(t, r.Symbols[1].Active)
}

func TestRemovedNameAndHistory(t *testing.T) {
	n := declaration("A", "a.go")
	base, err := Reconcile(graph("one", n), "r", Report{Version: 1, Repo: "r"})
	require.NoError(t, err)
	removed, err := Reconcile(graph("two"), "r", base)
	require.NoError(t, err)
	returned, err := Reconcile(graph("three", n), "r", removed)
	require.NoError(t, err)
	assert.Equal(t, len(returned.Symbols), 2)
	assert.True(t, returned.Symbols[0].ID != returned.Symbols[1].ID)
	for i := 0; i < MaxHistory+3; i++ {
		returned, err = Reconcile(graph(fmt.Sprint(i), n), "r", returned)
		require.NoError(t, err)
	}
	assert.Equal(t, len(returned.Symbols[1].History), MaxHistory)
	assert.Equal(t, returned.Symbols[1].HistoryOmitted, 3)
	before := len(returned.Symbols[1].History)
	unchanged, err := Reconcile(graph(fmt.Sprint(MaxHistory+2), n), "r", returned)
	require.NoError(t, err)
	assert.Equal(t, len(unchanged.Symbols[1].History), before)
}

func TestDecodeAndScope(t *testing.T) {
	r, err := Reconcile(graph("one", declaration("A", "a.go")), "r", Report{Version: 1, Repo: "r"})
	require.NoError(t, err)
	raw, err := Encode(r)
	require.NoError(t, err)
	decoded, err := Decode(raw, "r")
	require.NoError(t, err)
	assert.Equal(t, decoded, r)
	_, err = Decode(raw, "other")
	assert.True(t, err != nil)
	_, err = Decode("broken", "r")
	assert.True(t, err != nil)
	g := graph("two")
	g.AddNode(model.Node{ID: "other|Function|secret", Repo: "other", Kind: model.KindFunction})
	_, err = Reconcile(g, "r", r)
	assert.True(t, err != nil)
}

func TestBoundedAmbiguity(t *testing.T) {
	nodes := []model.Node{}
	for i := 0; i < MaxCandidates+10; i++ {
		nodes = append(nodes, declaration(fmt.Sprintf("Original%d", i), "a.go"))
	}
	base, err := Reconcile(graph("one", nodes...), "r", Report{Version: 1, Repo: "r"})
	require.NoError(t, err)
	r, err := Reconcile(graph("two", declaration("New", "a.go")), "r", base)
	require.NoError(t, err)
	last := r.Symbols[len(r.Symbols)-1]
	assert.Len(t, last.Candidates, MaxCandidates)
	assert.True(t, last.CandidatesOmitted > 0)
	for _, old := range base.Symbols {
		assert.True(t, last.ID != old.ID)
	}
}
