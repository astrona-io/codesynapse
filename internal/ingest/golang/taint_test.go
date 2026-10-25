package golang

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

// TestTaint: request input is followed through variables, string building,
// parsing and decoding inside a function, and stops at sanitizers.
func TestTaint(t *testing.T) {
	root, err := filepath.Abs("testdata/taint")
	require.NoError(t, err)
	res, err := Extract(context.Background(), Options{Repo: "taint", Root: root})
	require.NoError(t, err)
	g := res.Graph
	byFunc := map[string][]string{}
	for _, e := range g.Edges() {
		if e.Kind != model.EdgeHasFinding {
			continue
		}
		owner, _ := g.Node(e.From)
		f, _ := g.Node(e.To)
		byFunc[owner.Name] = append(byFunc[owner.Name], f.Rule)
		if strings.HasSuffix(f.Rule, "-from-request") {
			assert.Contains(t, f.Summary, "(via ", f.Rule+" names its source")
		}
	}
	for _, rules := range byFunc {
		slices.Sort(rules)
	}
	want := map[string][]string{
		"SQLThroughVariable":  {"sql-from-request"},
		"ConcatOfANumber":     {"sql-concat"},
		"ConcatAfterReassign": {"sql-concat"},
		"PathThroughJoin":     {"path-from-request"},
		"SSRFThroughParse":    {"http-client-default", "ssrf-from-request"},
		"SSRFThroughJSONBody": {"ssrf-from-request"},
		"ExecArgument":        {"exec-from-request"},
		"ExecThroughRange":    {"exec-from-request"},
	}
	for name, rules := range want {
		assert.Equal(t, byFunc[name], rules, name)
	}
	for _, clean := range []string{"CleanSQLParameter", "CleanPathBase", "CleanExec"} {
		assert.Len(t, byFunc[clean], 0, clean)
	}
}
