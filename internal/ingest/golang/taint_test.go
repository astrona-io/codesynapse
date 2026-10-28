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
	assert.Contains(t, byFunc["Fetch"]["path-from-request"], "via r.PathValue in FetchThroughInterface, passed to Fetch")
	assert.Len(t, byFunc["readStatic"], 0, "only called with constants")
	assert.Len(t, byFunc["CleanCaller"], 0)
	assert.Len(t, byFunc["CleanLookupByID"], 0, "a record found by the input is not the input")
}

// TestTaintThroughFunctionValues: with data-flow analysis, request input
// passed through closures, func-typed parameters and fields and method
// values is reported where it reaches a sink; clean values and targets
// without sinks add nothing, and without the analysis nothing changes.
func TestTaintThroughFunctionValues(t *testing.T) {
	root, err := filepath.Abs("testdata/taintfunc")
	require.NoError(t, err)
	findings := func(t *testing.T, opts Options) map[string]map[string]string {
		t.Helper()
		opts.Repo, opts.Root = "taintfunc", root
		res, err := Extract(context.Background(), opts)
		require.NoError(t, err)
		g := res.Graph
		out := map[string]map[string]string{}
		for _, e := range g.Edges() {
			if e.Kind != model.EdgeHasFinding {
				continue
			}
			owner, _ := g.Node(e.From)
			f, _ := g.Node(e.To)
			if !strings.HasSuffix(f.Rule, "-from-request") {
				continue
			}
			if out[owner.Name] == nil {
				out[owner.Name] = map[string]string{}
			}
			out[owner.Name][f.Rule] = f.Summary
		}
		return out
	}
	on := findings(t, Options{Dataflow: true})
	tests := []struct {
		name, owner, rule, detail string
	}{
		{"closure", "ThroughClosure", "exec-from-request", "via r.FormValue in ThroughClosure, passed to run"},
		{"func-typed parameter", "runCommand", "exec-from-request", "via r.FormValue in ThroughParameter, passed to apply"},
		{"func-valued field", "findUser", "sql-from-request", "via r.FormValue in ThroughField, passed to h.onName"},
		{"method value", "Delete", "sql-from-request", "via r.PathValue in ThroughMethodValue, passed to del"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := on[tt.owner][tt.rule]
			require.True(t, ok, tt.owner+" reports "+tt.rule)
			assert.Contains(t, got, tt.detail)
		})
	}
	t.Run("clean", func(t *testing.T) {
		for _, owner := range []string{"CleanThroughClosure", "CleanThroughParameter", "CleanTarget", "logName"} {
			assert.Len(t, on[owner], 0, owner)
		}
		assert.Len(t, on, len(tests), "only the expected functions report")
	})
	t.Run("without dataflow", func(t *testing.T) {
		assert.Len(t, findings(t, Options{}), 0, "calls through function values are not followed")
	})
}
