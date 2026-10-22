package model

import (
	"testing"

	"github.com/astrona-io/codesynapse/internal/testutil/assert"
)

func TestShortName(t *testing.T) {
	tests := []struct {
		kind NodeKind
		in   string
		want string
	}{
		{KindMethod, "(*github.com/org/repo/internal/store.Store).Get", "(*store.Store).Get"},
		{KindFunction, "github.com/org/repo/cmd/codesynapse.main", "codesynapse.main"},
		{KindFunction, "app.Handler", "app.Handler"},
		{KindExternal, "os/exec.Command", "exec.Command"},
		{KindPackage, "github.com/org/repo/internal/store", "github.com/org/repo/internal/store"},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			assert.Equal(t, Node{Kind: tt.kind, QualifiedName: tt.in}.ShortName(), tt.want)
		})
	}
}

func TestGraphDedupAndPrune(t *testing.T) {
	g := NewGraph()
	g.AddNode(Node{ID: "a"})
	g.AddNode(Node{ID: "a", Name: "dup"})
	g.AddNode(Node{ID: "b"})
	g.AddEdge(Edge{From: "a", To: "b", Kind: EdgeCalls})
	g.AddEdge(Edge{From: "a", To: "b", Kind: EdgeCalls})
	g.AddEdge(Edge{From: "a", To: "missing", Kind: EdgeCalls})
	assert.Len(t, g.Nodes(), 2)
	assert.Equal(t, g.PruneDanglingEdges(), 1)
	assert.Len(t, g.Edges(), 1)
	assert.Equal(t, g.Edges()[0].Count, 2)
}
