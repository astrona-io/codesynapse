package review

import (
	"context"
	"testing"

	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/store/memory"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

// graphWith builds a repository whose functions own findings at lines.
func graphWith(t *testing.T, findings map[string][][2]any) *memory.Store {
	t.Helper()
	g := model.NewGraph()
	g.AddNode(model.Node{ID: model.NodeID("r", model.KindRepository, "r"), Kind: model.KindRepository, Repo: "r", Name: "r"})
	for fn, list := range findings {
		id := model.NodeID("r", model.KindFunction, "app."+fn)
		g.AddNode(model.Node{ID: id, Kind: model.KindFunction, Repo: "r", Name: fn, QualifiedName: "app." + fn, File: "app/app.go", StartLine: 1, EndLine: 100})
		for i, f := range list {
			fid := model.NodeID("r", model.KindFinding, fn+string(rune('a'+i)))
			g.AddNode(model.Node{ID: fid, Kind: model.KindFinding, Repo: "r", Name: "title", Rule: f[0].(string), Severity: "high", File: "app/app.go", StartLine: f[1].(int), EndLine: f[1].(int)})
			g.AddEdge(model.Edge{From: id, To: fid, Kind: model.EdgeHasFinding})
		}
	}
	s := memory.New()
	require.NoError(t, s.ReplaceRepo(context.Background(), "r", g))
	return s
}

func TestCompareFindings(t *testing.T) {
	base := graphWith(t, map[string][][2]any{
		"Keep":  {{"weak-hash", 10}},
		"Fixed": {{"sql-concat", 40}},
		"Twice": {{"weak-hash", 60}},
	})
	head := graphWith(t, map[string][][2]any{
		"Keep":  {{"weak-hash", 14}},                    // moved down by an edit above: unchanged
		"Twice": {{"weak-hash", 60}, {"weak-hash", 70}}, // a second copy: new
		"Added": {{"exec-variable", 80}},                // new
	})
	d, err := CompareFindings(context.Background(), base, head, "r", []string{"app/app.go"})
	require.NoError(t, err)
	assert.Equal(t, d.Unchanged, 2)
	assert.Equal(t, d.Introduced, map[string]int{"high": 2})
	assert.Equal(t, d.Fixed, map[string]int{"high": 1})
	require.Len(t, d.Removed, 1)
	assert.Equal(t, d.Removed[0].Owner, "app.Fixed")
	assert.Equal(t, d.Headline(), "introduces 2 high; fixes 1 high")

	same, err := CompareFindings(context.Background(), base, base, "r", []string{"app/app.go", "other.go"})
	require.NoError(t, err)
	assert.Equal(t, same.Headline(), "introduces and fixes no security findings")
}

func TestIntroducesAtLeast(t *testing.T) {
	d := SecurityDelta{Introduced: map[string]int{"medium": 1, "low": 0}, Fixed: map[string]int{"critical": 2}}
	assert.True(t, d.IntroducesAtLeast("medium"))
	assert.True(t, d.IntroducesAtLeast("low"))
	assert.False(t, d.IntroducesAtLeast("high"), "fixed findings never fail a change")
	assert.False(t, SecurityDelta{}.IntroducesAtLeast("info"))
}
