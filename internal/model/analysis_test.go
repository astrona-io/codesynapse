package model

import (
	"strings"
	"testing"

	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

func TestEdgeEvidenceMerge(t *testing.T) {
	for _, tt := range []struct {
		name  string
		first Evidence
		then  Evidence
		want  Evidence
	}{
		{"typed survives a guess", EvidenceTyped, EvidenceInferred, EvidenceTyped},
		{"syntactic replaces a guess", EvidenceInferred, EvidenceSyntactic, EvidenceSyntactic},
		{"unknown never replaces evidence", EvidenceSyntactic, "", EvidenceSyntactic},
		{"evidence replaces unknown", "", EvidenceInferred, EvidenceInferred},
		{"unknown stays unknown", "", "", ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			g := NewGraph()
			g.AddEdge(Edge{From: "a", To: "b", Kind: EdgeCalls, Evidence: tt.first})
			g.AddEdge(Edge{From: "a", To: "b", Kind: EdgeCalls, Evidence: tt.then})
			edges := g.Edges()
			require.Len(t, edges, 1)
			assert.Equal(t, edges[0].Evidence, tt.want)
			assert.Equal(t, edges[0].Count, 2)
		})
	}
}

func TestEvidenceValues(t *testing.T) {
	for _, e := range []Evidence{"", EvidenceTyped, EvidenceSyntactic, EvidenceInferred} {
		assert.True(t, e.Valid(), string(e))
	}
	for _, e := range []Evidence{EvidenceUnknown, "TYPED", "ssa"} {
		assert.False(t, e.Valid(), string(e))
	}
	assert.Equal(t, Evidence("").OrUnknown(), EvidenceUnknown)
	assert.Equal(t, EvidenceInferred.OrUnknown(), EvidenceInferred)
	assert.True(t, ResolutionEdge(EdgeDispatchesTo))
	assert.False(t, ResolutionEdge(EdgeContains))
}

func TestAnalysisVersions(t *testing.T) {
	a, err := DecodeAnalysis("")
	require.NoError(t, err)
	assert.Equal(t, a.Version, 0, "legacy nodes have no record")

	encoded := Analysis{Level: LevelTyped, Status: StatusPartial, Errors: 2, Edges: map[Evidence]int{EvidenceTyped: 3}}.Encode()
	a, err = DecodeAnalysis(encoded)
	require.NoError(t, err)
	assert.Equal(t, a.Version, AnalysisVersion)
	assert.Equal(t, a.Errors, 2)
	assert.Equal(t, a.Edges[EvidenceTyped], 3)

	// A newer writer may add fields and bump the version; readers keep what
	// they know.
	a, err = DecodeAnalysis(`{"version":7,"level":"typed","status":"complete","ssa_edges":9}`)
	require.NoError(t, err)
	assert.Equal(t, a.Version, 7)
	assert.Equal(t, a.Status, StatusComplete)

	_, err = DecodeAnalysis("{" + strings.Repeat(" ", maxAnalysisBytes) + "}")
	assert.Error(t, err)
	_, err = DecodeAnalysis("not json")
	assert.Error(t, err)
}
