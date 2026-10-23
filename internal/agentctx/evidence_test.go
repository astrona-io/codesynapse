package agentctx

import (
	"context"
	"testing"

	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/store/storetest"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

// The fixture's TestHandler -> Handler edge has no evidence, as in a graph
// indexed before 4j.4; the others are typed and app/app.go is partial.
func TestSymbolContextEvidence(t *testing.T) {
	s := svc(t)
	out, err := s.SymbolContext(context.Background(), storetest.Repo, "app.Handler", ContextOptions{})
	require.NoError(t, err)
	require.Len(t, out.Callers, 1)
	assert.Equal(t, out.Callers[0].Evidence, model.EvidenceTyped)
	require.Len(t, out.Tests, 1)
	assert.Equal(t, out.Tests[0].Evidence, model.EvidenceUnknown, "legacy edges are unknown, never typed")
	require.Len(t, out.Callees, 1)
	assert.Equal(t, out.Callees[0].Evidence, model.EvidenceTyped)
	require.NotNil(t, out.Analysis)
	assert.Equal(t, *out.Analysis, AnalysisNote{Level: model.LevelTyped, Status: model.StatusPartial})
}

func TestCallGraphEvidence(t *testing.T) {
	s := svc(t)
	g, err := s.CallGraph(context.Background(), storetest.Repo, "app.Handler", "callers", 1, 10)
	require.NoError(t, err)
	got := map[string]model.Evidence{}
	for _, e := range g.Edges {
		got[e.From] = e.Evidence
	}
	assert.Equal(t, got["app.main"], model.EvidenceTyped)
	assert.Equal(t, got["app.TestHandler"], model.EvidenceUnknown)
}
