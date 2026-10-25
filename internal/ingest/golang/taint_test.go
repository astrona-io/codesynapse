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
		"SSRFFromHeader":      {"http-client-default", "ssrf-from-request"},
		"PathFromQuery":       {"path-from-request"},
		"CleanConfiguredURL":  {"http-client-default"},
	}
	for name, rules := range want {
		assert.Equal(t, byFunc[name], rules, name)
	}
	for _, clean := range []string{"CleanSQLParameter", "CleanPathBase", "CleanExec"} {
		assert.Len(t, byFunc[clean], 0, clean)
	}
}

// TestTaintThroughCalls: request input passed into the repository's own
// functions is reported where it reaches a sink, naming the caller; a
// helper's result carries what it was given unless it parses it.
func TestTaintThroughCalls(t *testing.T) {
	root, err := filepath.Abs("testdata/taint")
	require.NoError(t, err)
	res, err := Extract(context.Background(), Options{Repo: "taint", Root: root})
	require.NoError(t, err)
	g := res.Graph
	byFunc := map[string]map[string]string{}
	for _, e := range g.Edges() {
		if e.Kind != model.EdgeHasFinding {
			continue
		}
		owner, _ := g.Node(e.From)
		f, _ := g.Node(e.To)
		if byFunc[owner.Name] == nil {
			byFunc[owner.Name] = map[string]string{}
		}
		byFunc[owner.Name][f.Rule] = f.Summary
	}
	assert.Contains(t, byFunc["readUpload"]["path-from-request"], "via r.FormValue in DownloadThroughHelper, passed to readUpload")
	assert.Contains(t, byFunc["query"]["sql-from-request"], "in SearchTwoLevels, passed to lookup")
	assert.Contains(t, byFunc["FetchNormalised"]["ssrf-from-request"], "via r.FormValue")
	assert.Contains(t, byFunc["walkTree"]["path-from-request"], "in RecursiveHelper")
	_, fromRequest := byFunc["ConcatOfParsedID"]["sql-from-request"]
	assert.False(t, fromRequest, "parseID returns a number")
	assert.Contains(t, byFunc["ConcatOfParsedID"]["sql-concat"], "dynamically built")
	assert.Len(t, byFunc["readStatic"], 0, "only called with constants")
	assert.Len(t, byFunc["CleanCaller"], 0)
}
