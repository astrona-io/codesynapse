package nebulastore

import (
	"fmt"
	"slices"
	"testing"

	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
)

func deltaGraph(n int) ([]model.Node, []model.Edge) {
	nodes := make([]model.Node, n)
	var edges []model.Edge
	for i := range n {
		nodes[i] = model.Node{ID: fmt.Sprintf("f%d", i), Repo: "r", Kind: model.KindFunction, Name: fmt.Sprintf("f%d", i), StartLine: i + 1}
		if i > 0 {
			edges = append(edges, model.Edge{From: nodes[i-1].ID, To: nodes[i].ID, Kind: model.EdgeCalls, Count: 1})
		}
	}
	return nodes, edges
}

// storedAs is what the store holds after publishing nodes and edges.
func storedAs(nodes []model.Node, edges []model.Edge) map[string]nodeHash {
	out := map[string]nodeHash{}
	for id, h := range hashNodes(nodes, edges) {
		out[nodeVID(id)] = h
	}
	return out
}

func ids(nodes []model.Node) []string {
	out := make([]string, len(nodes))
	for i, n := range nodes {
		out[i] = n.ID
	}
	return out
}

func TestHashNodes(t *testing.T) {
	nodes, edges := deltaGraph(3)
	a := hashNodes(nodes, edges)
	reversed := slices.Clone(edges)
	slices.Reverse(reversed)
	assert.Equal(t, a, hashNodes(nodes, reversed), "edge order does not change hashes")
	assert.Equal(t, a["f0"].content, hashNodes(nodes, edges)["f0"].content, "deterministic")

	nodes[1].Summary = "changed"
	b := hashNodes(nodes, edges)
	assert.NotEqual(t, a["f1"].content, b["f1"].content, "a property change changes content_hash")
	assert.Equal(t, a["f1"].edges, b["f1"].edges, "but not edges_hash")

	edges[0].Evidence = model.EvidenceSyntactic
	c := hashNodes(nodes, edges)
	assert.NotEqual(t, b["f0"].edges, c["f0"].edges, "edge evidence changes the source's edges_hash")
	assert.Equal(t, b["f1"].edges, c["f1"].edges, "and only the source's")
}

func TestPlanDelta(t *testing.T) {
	base, baseEdges := deltaGraph(20)
	stored := storedAs(base, baseEdges)

	t.Run("nothing stored is a full write", func(t *testing.T) {
		d := planDelta(base, baseEdges, hashNodes(base, baseEdges), nil)
		assert.True(t, d.full)
	})
	t.Run("unchanged writes nothing", func(t *testing.T) {
		d := planDelta(base, baseEdges, hashNodes(base, baseEdges), stored)
		assert.False(t, d.full)
		assert.Equal(t, 0, len(d.nodes)+len(d.edges)+len(d.removed)+len(d.edgeFrom))
	})
	t.Run("property change writes the node only", func(t *testing.T) {
		nodes := slices.Clone(base)
		nodes[5].Summary = "new"
		d := planDelta(nodes, baseEdges, hashNodes(nodes, baseEdges), stored)
		assert.False(t, d.full)
		assert.Equal(t, []string{"f5"}, ids(d.nodes))
		assert.Equal(t, 0, len(d.edges)+len(d.edgeFrom))
	})
	t.Run("edge change rewrites the source's edges", func(t *testing.T) {
		edges := slices.Clone(baseEdges)
		edges[3].Count = 4 // f3 -> f4
		d := planDelta(base, edges, hashNodes(base, edges), stored)
		assert.Equal(t, []string{"f3"}, ids(d.nodes))
		assert.Equal(t, []string{nodeVID("f3")}, d.edgeFrom)
		assert.Equal(t, 1, len(d.edges))
	})
	t.Run("added and removed nodes", func(t *testing.T) {
		nodes := slices.Clone(base[:19]) // f19 removed
		edges := slices.Clone(baseEdges[:18])
		nodes = append(nodes, model.Node{ID: "g", Repo: "r", Kind: model.KindFunction, Name: "g"})
		edges = append(edges, model.Edge{From: "g", To: "f0", Kind: model.EdgeCalls, Count: 1})
		d := planDelta(nodes, edges, hashNodes(nodes, edges), stored)
		assert.False(t, d.full)
		assert.Equal(t, []string{nodeVID("f19")}, d.removed)
		// f18 lost its edge to f19; g is new with one edge.
		assert.Equal(t, []string{"f18", "g"}, ids(d.nodes))
		assert.Equal(t, []string{nodeVID("f18"), nodeVID("g")}, d.edgeFrom)
		assert.Equal(t, 1, len(d.edges))
	})
	t.Run("hashes from before this version count as changed", func(t *testing.T) {
		legacy := map[string]nodeHash{}
		for v := range stored {
			legacy[v] = nodeHash{}
		}
		assert.True(t, planDelta(base, baseEdges, hashNodes(base, baseEdges), legacy).full)
	})
	t.Run("more than half changed is a full write", func(t *testing.T) {
		nodes := slices.Clone(base)
		for i := range 11 {
			nodes[i].Summary = "new"
		}
		assert.True(t, planDelta(nodes, baseEdges, hashNodes(nodes, baseEdges), stored).full)
	})
}
