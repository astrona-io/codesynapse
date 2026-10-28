package mcpserver

import (
	"context"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/astrona-io/codesynapse/internal/store/memory"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"
)

// listed connects to server and returns its tools, prompts and instructions.
func listed(t *testing.T, server *mcp.Server) (tools, prompts []string, instructions string) {
	t.Helper()
	ctx := context.Background()
	ct, st := mcp.NewInMemoryTransports()
	ss, err := server.Connect(ctx, st, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = ss.Close() })
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "toolsets-test"}, nil).Connect(ctx, ct, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = cs.Close() })
	lt, err := cs.ListTools(ctx, nil)
	require.NoError(t, err)
	for _, tool := range lt.Tools {
		tools = append(tools, tool.Name)
	}
	lp, err := cs.ListPrompts(ctx, nil)
	require.NoError(t, err)
	for _, p := range lp.Prompts {
		prompts = append(prompts, p.Name)
	}
	slices.Sort(tools)
	slices.Sort(prompts)
	return tools, prompts, cs.InitializeResult().Instructions
}

func fullOptions(s *memory.Store) Options {
	return Options{Memory: s, Reviews: s, History: s, Curated: s, CatalogWrites: true, Offload: &APIClient{}}
}

func TestEveryToolInOneToolset(t *testing.T) {
	s := memory.New()
	tools, _, _ := listed(t, newTestServer(t, s, "test", fullOptions(s)))
	seen := map[string]string{}
	for set, names := range Toolsets {
		for _, name := range names {
			_, dup := seen[name]
			assert.False(t, dup, "%s is in %s and %s", name, set, seen[name])
			seen[name] = set
			_, ok := toolPolicies[name]
			assert.True(t, ok, "%s in toolset %s has a policy", name, set)
		}
	}
	for _, name := range tools {
		assert.True(t, toolsetOf(name) != "", "%s belongs to a toolset", name)
	}
	assert.Equal(t, len(seen), len(tools), "the sets list exactly the tools a full server registers")
	assert.Len(t, Toolsets[ToolsetCore], 13)
}

func TestToolsetSelection(t *testing.T) {
	s := memory.New()
	all := fullOptions(s)
	allTools, allPrompts, allText := listed(t, newTestServer(t, s, "test", all))

	core := all
	core.Toolsets = []string{ToolsetCore}
	tools, prompts, text := listed(t, newTestServer(t, s, "test", core))
	want := slices.Clone(Toolsets[ToolsetCore])
	slices.Sort(want)
	assert.Equal(t, tools, want)
	for _, p := range []string{"code-review", "briefing", "security-triage", "context-offload"} {
		assert.True(t, slices.Contains(allPrompts, p), p)
		assert.False(t, slices.Contains(prompts, p), "%s needs a toolset core lacks", p)
	}
	for _, tool := range allTools {
		if !slices.Contains(tools, tool) {
			assert.False(t, strings.Contains(text, tool), "core instructions name %s, which core does not serve", tool)
		}
	}
	assert.True(t, len(text) < len(allText), "instructions shrink with the toolset")
	assert.Contains(t, text, "Reuse before you write")
	assert.Contains(t, text, "10. Freshness and breadth: repo_status says whether the graph is current.")

	some := all
	some.Toolsets = []string{ToolsetCore, "memory", "reviews"}
	tools, _, text = listed(t, newTestServer(t, s, "test", some))
	assert.Len(t, tools, len(Toolsets[ToolsetCore])+len(Toolsets["memory"])+len(Toolsets["reviews"]))
	assert.Contains(t, text, "submit_review")
	assert.False(t, strings.Contains(text, "security_report"), "reviews without security leave it out")
	assert.Contains(t, text, "recall_context")
	assert.Contains(t, text, "\n14. Memory", "memory follows the rules shown: eleven core, reviews, reuse")

	for _, name := range []string{"submit_review", "search_all_repos", "security_report", "architecture_context", "recall_context", "duplicate_code", "get_doc_section"} {
		assert.Contains(t, allText, name, "every tool's guidance with all sets")
	}
	assert.Contains(t, allText, "never accepted", "catalog sentences stay with the architecture rule")
}

func TestParseToolsets(t *testing.T) {
	for _, tc := range []struct {
		spec string
		want []string
		err  bool
	}{
		{"", nil, false},
		{"all", nil, false},
		{"core", []string{"core"}, false},
		{"reviews, memory,reviews", []string{"core", "memory", "reviews"}, false},
		{"memory,all", nil, false},
		{"memory,nope", nil, true},
	} {
		got, err := ParseToolsets(tc.spec)
		assert.Equal(t, err != nil, tc.err, tc.spec)
		assert.Equal(t, got, tc.want, tc.spec)
	}
}

func TestServersForRequest(t *testing.T) {
	s := memory.New()
	opts := fullOptions(s)
	opts.Toolsets = []string{ToolsetCore, "docs"}
	x := NewServers(s, "test", opts)
	def := x.ForRequest(httptest.NewRequest("POST", "/mcp", nil))
	assert.True(t, def == x.For([]string{"core", "docs"}), "no parameter: the server default, cached")
	all := x.ForRequest(httptest.NewRequest("POST", "/mcp?toolsets=all", nil))
	tools, _, _ := listed(t, all)
	assert.Len(t, tools, len(toolPolicies))
	mem := x.ForRequest(httptest.NewRequest("POST", "/mcp?toolsets=memory", nil))
	tools, _, _ = listed(t, mem)
	assert.Len(t, tools, len(Toolsets[ToolsetCore])+len(Toolsets["memory"]))
	assert.True(t, x.ForRequest(httptest.NewRequest("POST", "/mcp?toolsets=nope", nil)) == nil, "an unknown set is refused")
}

// The git tools read the developer's checkout; the server lists them so every
// client sees the same tools, and answers a direct call with where to go
// instead of pretending.
func TestGitToolsAreListedButAnsweredByTheBridge(t *testing.T) {
	s := memory.New()
	cs := connectGraphOptions(t, s, fullOptions(s))
	for name, args := range map[string]map[string]any{"git_status": {}, "git_diff_summary": {}, "git_blame": {"file": "a.go", "start": 1}, "git_log": {}} {
		res := call(t, cs, name, args, nil)
		assert.True(t, res.IsError, name)
		text := res.Content[0].(*mcp.TextContent).Text
		assert.Contains(t, text, "bridge", name)
	}

	core := fullOptions(s)
	core.Toolsets = []string{ToolsetCore}
	tools, _, instructions := listed(t, newTestServer(t, s, "test", core))
	assert.NotContains(t, strings.Join(tools, ","), "git_status", "not in the core set: agents that do not use it do not pay for its definitions")
	assert.NotContains(t, instructions, "git_status")

	withGit := fullOptions(s)
	withGit.Toolsets = []string{ToolsetCore, "git"}
	tools, _, instructions = listed(t, newTestServer(t, s, "test", withGit))
	assert.Contains(t, strings.Join(tools, ","), "git_blame")
	assert.Contains(t, instructions, "git_diff_summary")
}

func connectGraphOptions(t *testing.T, s *memory.Store, o Options) *mcp.ClientSession {
	t.Helper()
	ctx := context.Background()
	ct, st := mcp.NewInMemoryTransports()
	_, err := newTestServer(t, s, "test", o).Connect(ctx, st, nil)
	require.NoError(t, err)
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test"}, nil).Connect(ctx, ct, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}
