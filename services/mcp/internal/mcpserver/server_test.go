package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/astrona-io/codesynapse/internal/execution"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
	"github.com/astrona-io/codesynapse/internal/testutil/require"

	"github.com/astrona-io/codesynapse/internal/license"
	agentmem "github.com/astrona-io/codesynapse/internal/memory"
	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/store/memory"
	"github.com/astrona-io/codesynapse/internal/store/storetest"
	"github.com/astrona-io/codesynapse/internal/usage"
)

func connect(t *testing.T) *mcp.ClientSession {
	t.Helper()
	return connectGraph(t, storetest.Fixture())
}

func connectGraph(t *testing.T, g *model.Graph) *mcp.ClientSession {
	t.Helper()
	ctx := context.Background()
	s := memory.New()
	require.NoError(t, s.ReplaceRepo(ctx, storetest.Repo, g))

	ct, st := mcp.NewInMemoryTransports()
	_, err := newTestServer(t, s, "test", Options{Reviews: s}).Connect(ctx, st, nil)
	require.NoError(t, err)
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test"}, nil).Connect(ctx, ct, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

func call(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any, out any) *mcp.CallToolResult {
	t.Helper()
	return callContext(context.Background(), t, cs, name, args, out)
}

func callContext(ctx context.Context, t *testing.T, cs *mcp.ClientSession, name string, args map[string]any, out any) *mcp.CallToolResult {
	t.Helper()
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	require.NoError(t, err)
	if out != nil && !res.IsError {
		b, err := json.Marshal(res.StructuredContent)
		require.NoError(t, err)
		require.NoError(t, json.Unmarshal(b, out))
	}
	return res
}

func TestToolsAreReadOnly(t *testing.T) {
	cs := connect(t)
	tools, err := cs.ListTools(context.Background(), nil)
	require.NoError(t, err)
	require.Greater(t, len(tools.Tools), 8)
	writers := map[string]bool{"submit_review": true, "update_review_comment": true}
	for _, tool := range tools.Tools {
		require.NotNil(t, tool.Annotations)
		assert.Equal(t, tool.Annotations.ReadOnlyHint, !writers[tool.Name], tool.Name)
	}
}

func TestSymbolContext(t *testing.T) {
	cs := connect(t)
	var out struct {
		Symbol struct {
			QualifiedID string `json:"qualified_id"`
		} `json:"symbol"`
		Risk    struct{ Score int } `json:"risk"`
		Callers []struct {
			QualifiedID string `json:"qualified_id"`
			EntryPoint  bool   `json:"entry_point"`
		}
		Tests []struct {
			QualifiedID string `json:"qualified_id"`
		}
		Docs []struct {
			QualifiedID string `json:"qualified_id"`
		}
		Hint string
	}
	res := call(t, cs, "get_symbol_context", map[string]any{"symbol": "app.Handler"}, &out)
	require.False(t, res.IsError)
	assert.Equal(t, out.Symbol.QualifiedID, "app.Handler")
	assert.Len(t, out.Callers, 1, "outline: direct non-test callers")
	assert.Len(t, out.Tests, 1)
	assert.Greater(t, out.Risk.Score, 0)
	assert.Len(t, out.Docs, 0, "docs only with verbosity=full")
	assert.NotEqual(t, out.Hint, "")

	res = call(t, cs, "get_symbol_context", map[string]any{"symbol": "app.Handler", "verbosity": "full"}, &out)
	require.False(t, res.IsError)
	assert.Len(t, out.Docs, 1)
}

func TestSuggestionsOnAmbiguousName(t *testing.T) {
	cs := connect(t)
	res := call(t, cs, "get_symbol_context", map[string]any{"symbol": "Do"}, nil)
	require.True(t, res.IsError)
	text := res.Content[0].(*mcp.TextContent).Text
	assert.Contains(t, text, "(*app.impl).Do")
	assert.Contains(t, text, "(app.I).Do")

	// The suggested qualified_id resolves.
	res = call(t, cs, "get_symbol_context", map[string]any{"symbol": "(*app.impl).Do"}, nil)
	assert.False(t, res.IsError)
}

func TestImplementations(t *testing.T) {
	cs := connect(t)
	var out struct {
		Results []struct {
			QualifiedID string `json:"qualified_id"`
		}
	}
	res := call(t, cs, "get_implementations", map[string]any{"symbol": "(app.I).Do"}, &out)
	require.False(t, res.IsError)
	require.Len(t, out.Results, 1)
	assert.Equal(t, out.Results[0].QualifiedID, "(*app.impl).Do")
}

func TestCallGraph(t *testing.T) {
	cs := connect(t)
	var out struct {
		Nodes []struct {
			QualifiedID string `json:"qualified_id"`
		}
		Edges       []struct{ From, To string }
		EntryPoints []string `json:"entry_points"`
	}
	res := call(t, cs, "get_call_graph", map[string]any{"symbol": "(*app.impl).Do", "direction": "callers", "depth": 3}, &out)
	require.False(t, res.IsError)
	assert.Greater(t, len(out.Nodes), 2)
	assert.NotEmpty(t, out.Edges)
	assert.Contains(t, out.EntryPoints, "app.main")
}

func TestPRContext(t *testing.T) {
	cs := connect(t)
	var rep struct {
		Summary struct {
			ChangedSymbols int `json:"changed_symbols"`
		}
		SensitiveTouched []string `json:"sensitive_touched"`
	}
	diff := "diff --git a/app/app.go b/app/app.go\n--- a/app/app.go\n+++ b/app/app.go\n@@ -26 +26 @@\n"
	res := call(t, cs, "get_pr_context", map[string]any{"diff": diff}, &rep)
	require.False(t, res.IsError)
	assert.Equal(t, rep.Summary.ChangedSymbols, 1)
	assert.Len(t, rep.SensitiveTouched, 1)

	res = call(t, cs, "get_pr_context", map[string]any{"files": []string{"app/app.go"}}, &rep)
	require.False(t, res.IsError)
	assert.Equal(t, rep.Summary.ChangedSymbols, 4, "files mode: every symbol in the file")
}

func TestMinimalContext(t *testing.T) {
	cs := connect(t)
	var out struct {
		Items []struct {
			QualifiedID string `json:"qualified_id"`
			Read        string
		}
		UsedTokens int `json:"used_tokens"`
		Budget     int `json:"max_tokens"`
	}
	res := call(t, cs, "minimal_context", map[string]any{"task": "change how Handler calls Do", "max_tokens": 300}, &out)
	require.False(t, res.IsError)
	require.NotEmpty(t, out.Items)
	assert.Equal(t, out.Items[0].QualifiedID, "app.Handler")
	assert.LessOrEqual(t, out.UsedTokens, out.Budget)
}

func TestPrompts(t *testing.T) {
	cs := connect(t)
	list, err := cs.ListPrompts(context.Background(), nil)
	require.NoError(t, err)
	names := map[string]bool{}
	for _, p := range list.Prompts {
		names[p.Name] = true
	}
	for _, n := range []string{"refactor-analysis", "pr-impact-review", "package-onboarding", "bug-investigation"} {
		assert.True(t, names[n], n)
	}
	got, err := cs.GetPrompt(context.Background(), &mcp.GetPromptParams{Name: "pr-impact-review", Arguments: map[string]string{"files": "a.go, b.go"}})
	require.NoError(t, err)
	text := got.Messages[0].Content.(*mcp.TextContent).Text
	assert.Contains(t, text, `"a.go", "b.go"`)
	assert.Contains(t, text, "get_pr_context")
	_, err = cs.GetPrompt(context.Background(), &mcp.GetPromptParams{Name: "refactor-analysis", Arguments: map[string]string{}})
	assert.Error(t, err)
}

func TestReuseFirstGuidance(t *testing.T) {
	ctx := context.Background()
	cs := connect(t)
	assert.Contains(t, cs.InitializeResult().Instructions, "Reuse before you write")

	got, err := cs.GetPrompt(ctx, &mcp.GetPromptParams{Name: "reuse-first", Arguments: map[string]string{"task": "add retry to the HTTP client"}})
	require.NoError(t, err)
	text := got.Messages[0].Content.(*mcp.TextContent).Text
	assert.Contains(t, text, "add retry to the HTTP client")
	assert.Contains(t, text, "Search and reuse existing code")
	_, err = cs.GetPrompt(ctx, &mcp.GetPromptParams{Name: "reuse-first", Arguments: map[string]string{}})
	assert.Error(t, err, "the task is required")

	// Every tool the guidance names must exist, or agents are told to call
	// something that is not there.
	tools, err := cs.ListTools(ctx, nil)
	require.NoError(t, err)
	have := map[string]bool{}
	for _, tl := range tools.Tools {
		have[tl.Name] = true
	}
	for _, name := range []string{"search", "search_all_repos", "minimal_context", "get_symbol_context", "get_implementations", "get_call_graph", "duplicate_code", "shared_code"} {
		assert.True(t, have[name], name)
		assert.Contains(t, text, name)
	}
}

func TestDependencyReviewPrompt(t *testing.T) {
	ctx := context.Background()
	cs := connect(t)
	got, err := cs.GetPrompt(ctx, &mcp.GetPromptParams{Name: "dependency-review", Arguments: map[string]string{"target": "replace left-pad"}})
	require.NoError(t, err)
	text := got.Messages[0].Content.(*mcp.TextContent).Text
	assert.Contains(t, text, "replace left-pad")
	assert.Contains(t, text, "| Dependency and version | Introduced by | Usage | License | Advisory | Proposed action | Verified outcome |")
	for _, eco := range []string{"Go:", "JavaScript and TypeScript:", "Python:", "Java, Kotlin and Scala:", "C# and .NET:", "Rust:", "Ruby:", "PHP:", "Swift and Objective-C:", "Dart and Flutter:", "Elixir and Erlang:", "C and C++:", "Haskell:", "R:", "Containers, CI and infrastructure:"} {
		assert.Contains(t, text, "- "+eco)
	}
	_, err = cs.GetPrompt(ctx, &mcp.GetPromptParams{Name: "dependency-review", Arguments: map[string]string{}})
	assert.Error(t, err, "the target is required")

	// The tools it points at must exist.
	tools, err := cs.ListTools(ctx, nil)
	require.NoError(t, err)
	have := map[string]bool{}
	for _, tl := range tools.Tools {
		have[tl.Name] = true
	}
	for _, name := range []string{"security_report", "license_report", "unused_code", "search", "get_symbol_context", "get_call_graph", "get_config_entries", "repo_status"} {
		assert.True(t, have[name], name)
		assert.Contains(t, text, name)
	}
}

func TestErrorsAreToolErrors(t *testing.T) {
	cs := connect(t)
	res := call(t, cs, "search", map[string]any{"query": "x", "kinds": []string{"Bogus"}}, nil)
	assert.True(t, res.IsError)
	res = call(t, cs, "get_pr_context", map[string]any{"base": "--upload-pack=evil"}, nil)
	assert.True(t, res.IsError)
	res = call(t, cs, "get_symbol_context", map[string]any{"symbol": "app.Handler", "verbosity": "huge"}, nil)
	assert.True(t, res.IsError)
}

func TestReviewWorkflow(t *testing.T) {
	cs := connect(t)
	var sub struct {
		ID       string
		Page     string
		Warnings []string
		Counts   struct{ Open int }
	}
	res := call(t, cs, "submit_review", map[string]any{
		"title": "Review of handler", "verdict": "request_changes", "summary": "Shell exec needs hardening.",
		"comments": []map[string]any{
			{"file": "app/app.go", "line": 27, "severity": "high", "category": "security", "message": "exec with a variable", "suggestion": "use exec.CommandContext with fixed argv", "rule": "exec-variable"},
			{"file": "new/file.go", "line": 1, "message": "nit"},
		},
	}, &sub)
	require.False(t, res.IsError, "%v", res.Content)
	assert.Equal(t, sub.Counts.Open, 2)
	assert.Contains(t, sub.Page, sub.ID)
	assert.Len(t, sub.Warnings, 1, "unknown file is reported, not rejected")

	var rv struct {
		Comments []struct{ ID, Symbol, Status string }
	}
	require.False(t, call(t, cs, "get_review", map[string]any{"id": sub.ID}, &rv).IsError)
	require.Len(t, rv.Comments, 2)
	assert.Equal(t, rv.Comments[0].Symbol, "(*app.impl).Do", "anchored to the symbol at that line")

	res = call(t, cs, "update_review_comment", map[string]any{"review_id": sub.ID, "comment_id": rv.Comments[0].ID, "status": "resolved", "note": "fixed"}, nil)
	require.False(t, res.IsError)
	require.False(t, call(t, cs, "get_review", map[string]any{"id": sub.ID, "status": "open"}, &rv).IsError)
	assert.Len(t, rv.Comments, 1)

	var list struct{ Reviews []struct{ ID string } }
	require.False(t, call(t, cs, "list_reviews", map[string]any{}, &list).IsError)
	assert.Len(t, list.Reviews, 1)

	res = call(t, cs, "submit_review", map[string]any{"title": "x", "comments": []map[string]any{{"file": "../etc/passwd", "message": "m"}}}, nil)
	assert.True(t, res.IsError, "path escapes rejected")
}

func TestAgentTaskPromptsAndReport(t *testing.T) {
	cs := connect(t)
	got, err := cs.GetPrompt(context.Background(), &mcp.GetPromptParams{Name: "security-triage", Arguments: map[string]string{"min_severity": "bogus"}})
	require.NoError(t, err)
	assert.Contains(t, got.Messages[0].Content.(*mcp.TextContent).Text, `min_severity: "medium"`, "unknown severity falls back")
	got, err = cs.GetPrompt(context.Background(), &mcp.GetPromptParams{Name: "briefing"})
	require.NoError(t, err)
	assert.Contains(t, got.Messages[0].Content.(*mcp.TextContent).Text, `kind: "report"`)

	var sub struct{ ID string }
	res := call(t, cs, "submit_review", map[string]any{"kind": "report", "title": "Briefing: app", "summary": "## What this is\nAn app."}, &sub)
	require.False(t, res.IsError, "%v", res.Content)
	var rv struct{ Kind string }
	require.False(t, call(t, cs, "get_review", map[string]any{"id": sub.ID}, &rv).IsError)
	assert.Equal(t, rv.Kind, "report")
}

func TestSecurityReportPathFilter(t *testing.T) {
	g := storetest.Fixture()
	for i, f := range []struct{ file, owner string }{{"app/app.go", "app.Handler"}, {"web/app.ts", "app.main"}} {
		id := storetest.ID(model.KindFinding, fmt.Sprintf("%s:%d:exec-variable", f.file, 10+i))
		g.AddNode(model.Node{ID: id, Repo: storetest.Repo, Kind: model.KindFinding, Name: "Process from a variable", File: f.file, StartLine: 10 + i, Rule: "exec-variable", Severity: "medium"})
		g.AddEdge(model.Edge{From: storetest.ID(model.KindFunction, f.owner), To: id, Kind: model.EdgeHasFinding})
	}
	cs := connectGraph(t, g)
	var all, some, none struct {
		Findings      []struct{ Location string }
		FindingCounts map[string]int `json:"finding_counts"`
		Score         int
	}
	require.False(t, call(t, cs, "security_report", map[string]any{}, &all).IsError)
	require.NotEmpty(t, all.Findings)
	require.Len(t, all.Findings, 2)
	assert.Equal(t, all.FindingCounts["medium"], 2)
	file := "app/app.go"
	require.False(t, call(t, cs, "security_report", map[string]any{"path": file}, &some).IsError)
	assert.NotEmpty(t, some.Findings)
	for _, f := range some.Findings {
		assert.True(t, strings.HasPrefix(f.Location, file+":"), f.Location)
	}
	assert.Len(t, some.Findings, 1)
	assert.Equal(t, some.FindingCounts["medium"], 1, "counts follow the filter")
	assert.Equal(t, some.Score, all.Score, "score stays repository-wide")
	require.False(t, call(t, cs, "security_report", map[string]any{"path": "nowhere/"}, &none).IsError)
	assert.Len(t, none.Findings, 0)
	assert.Len(t, none.FindingCounts, 0)
}

func TestDiscoveryTools(t *testing.T) {
	cs := connect(t)

	var st struct {
		Repo, Head string
		Stale      bool
		Advice     string
	}
	require.False(t, call(t, cs, "repo_status", map[string]any{}, &st).IsError)
	assert.Equal(t, st.Head, "abc")
	assert.True(t, st.Stale, "the fixture was indexed years ago")
	assert.NotEqual(t, st.Advice, "")

	var all struct {
		Results       []struct{ Repo, Name string }
		ReposSearched int `json:"repos_searched"`
	}
	require.False(t, call(t, cs, "search_all_repos", map[string]any{"query": "handler"}, &all).IsError)
	require.NotEmpty(t, all.Results)
	assert.Equal(t, all.Results[0].Repo, storetest.Repo)
	assert.Equal(t, all.ReposSearched, 1)
	require.False(t, call(t, cs, "search_all_repos", map[string]any{"query": "handler", "repo_prefix": "other/"}, &all).IsError)
	assert.Len(t, all.Results, 0)
	assert.True(t, call(t, cs, "search_all_repos", map[string]any{"query": " "}, nil).IsError)

	var cfg struct {
		Entries []struct{ Name, Location string }
	}
	require.False(t, call(t, cs, "get_config_entries", map[string]any{"file": "deploy/"}, &cfg).IsError)
	assert.True(t, len(cfg.Entries) >= 2, "Deployment and spec entries")
	require.False(t, call(t, cs, "get_config_entries", map[string]any{"query": "Deployment"}, &cfg).IsError)
	require.Len(t, cfg.Entries, 1)
	assert.Equal(t, cfg.Entries[0].Name, "Deployment/api")
	assert.True(t, call(t, cs, "get_config_entries", map[string]any{}, nil).IsError, "an unfiltered dump is refused")

	var docs struct {
		Sections []struct {
			Heading  string
			Location string
			Mentions []struct{ Name string }
		}
	}
	require.False(t, call(t, cs, "get_doc_section", map[string]any{"query": "usage"}, &docs).IsError)
	require.Len(t, docs.Sections, 1)
	assert.Equal(t, docs.Sections[0].Heading, "Usage")
	require.NotEmpty(t, docs.Sections[0].Mentions)
	assert.Equal(t, docs.Sections[0].Mentions[0].Name, "app.Handler")
}

func TestLicenseReport(t *testing.T) {
	// The fixture has no stored report: a clear tool error, not a crash.
	res := call(t, connect(t), "license_report", nil, nil)
	assert.True(t, res.IsError)

	rep := license.Build(license.Found{License: "Apache-2.0", Source: "LICENSE"}, []license.Dependency{
		{Ecosystem: license.Go, Name: "example.com/gpl", Version: "v1.0.0", License: "GPL-3.0"},
		{Ecosystem: license.NPM, Name: "lightningcss", Version: "1.0.0", License: "MPL-2.0"},
		{Ecosystem: license.NPM, Name: "lodash", Version: "4.17.21", License: "MIT"},
	}, false)
	enc, err := license.Encode(rep)
	require.NoError(t, err)
	g := storetest.Fixture()
	repoID := model.NodeID(storetest.Repo, model.KindRepository, storetest.Repo)
	repoNode, ok := g.Node(repoID)
	require.True(t, ok)
	repoNode.Licenses = enc
	var got license.Report
	res = call(t, connectGraph(t, g), "license_report", map[string]any{"verdict": "conflict"}, &got)
	require.False(t, res.IsError)
	assert.Equal(t, got.Conflict, 1)
	assert.Equal(t, got.Review, 1)
	require.Len(t, got.Issues, 1)
	assert.Equal(t, got.Issues[0].Name, "example.com/gpl")
	assert.True(t, strings.Contains(got.Issues[0].Verdict.Reason, "strong copyleft"))
}

func connectMemory(t *testing.T) *mcp.ClientSession {
	t.Helper()
	ctx := context.Background()
	s := memory.New()
	require.NoError(t, s.ReplaceRepo(ctx, storetest.Repo, storetest.Fixture()))
	ct, st := mcp.NewInMemoryTransports()
	_, err := newTestServer(t, s, "test", Options{Memory: s}).Connect(ctx, st, nil)
	require.NoError(t, err)
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test-client"}, nil).Connect(ctx, ct, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

func TestMemoryToolsOptIn(t *testing.T) {
	names := func(cs *mcp.ClientSession) map[string]*mcp.Tool {
		tools, err := cs.ListTools(context.Background(), nil)
		require.NoError(t, err)
		out := map[string]*mcp.Tool{}
		for _, tool := range tools.Tools {
			out[tool.Name] = tool
		}
		return out
	}
	_, on := names(connect(t))["upsert_graph_triples"]
	assert.False(t, on, "without Options.Memory (--allow-memory=false) the tools are absent")

	tools := names(connectMemory(t))
	require.NotNil(t, tools["upsert_graph_triples"])
	assert.False(t, tools["upsert_graph_triples"].Annotations.ReadOnlyHint)
	assert.True(t, tools["search_graph_nodes"].Annotations.ReadOnlyHint)
	assert.True(t, tools["query_subgraph"].Annotations.ReadOnlyHint)
}

func TestMemoryRoundTrip(t *testing.T) {
	cs := connectMemory(t)
	var up struct {
		Facts int `json:"facts"`
	}
	res := call(t, cs, "upsert_graph_triples", map[string]any{"triples": []map[string]any{
		{"subject": "Payments service", "subject_type": "Service", "predicate": "owned by", "object": "Team Blue", "object_type": "Team"},
		{"subject": "Team Blue", "predicate": "PREFERS", "object": "Small PRs"},
	}}, &up)
	require.False(t, res.IsError)
	assert.Equal(t, up.Facts, 2)

	var g agentmem.Graph
	res = call(t, cs, "search_graph_nodes", map[string]any{"query": "payments"}, &g)
	require.False(t, res.IsError)
	assert.Equal(t, g.Notice, agentmem.Notice)
	require.Len(t, g.Facts, 1)
	assert.Equal(t, g.Facts[0].Predicate, "OWNED_BY")
	assert.Equal(t, g.Facts[0].Source, "test-client", "provenance is the MCP client")

	res = call(t, cs, "query_subgraph", map[string]any{"entities": []string{"payments service"}, "depth": 2}, &g)
	require.False(t, res.IsError)
	assert.Len(t, g.Facts, 2, "depth 2 reaches Team Blue's preference")

	res = call(t, cs, "upsert_graph_triples", map[string]any{"repo": "not-indexed", "triples": []map[string]any{{"subject": "a", "predicate": "P", "object": "b"}}}, nil)
	assert.True(t, res.IsError, "memory is only written for indexed repositories")
	res = call(t, cs, "upsert_graph_triples", map[string]any{"triples": []map[string]any{{"subject": "a", "predicate": "MATCH (n) DETACH DELETE n", "object": "b"}}}, nil)
	assert.True(t, res.IsError, "predicates are validated")
}

func TestUsageMiddleware(t *testing.T) {
	ctx := context.Background()
	s := memory.New()
	require.NoError(t, s.ReplaceRepo(ctx, storetest.Repo, storetest.Fixture()))
	rec := usage.NewRecorder(s)
	ct, st := mcp.NewInMemoryTransports()
	_, err := newTestServer(t, s, "test", Options{Reviews: s, Usage: rec}).Connect(ctx, st, nil)
	require.NoError(t, err)
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "usage-client"}, nil).Connect(ctx, ct, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = cs.Close() })

	call(t, cs, "search", map[string]any{"repo": storetest.Repo, "query": "Handler"}, nil)
	call(t, cs, "search", map[string]any{"repo": storetest.Repo, "query": "x", "kinds": []string{"Bogus"}}, nil)
	_, err = cs.GetPrompt(ctx, &mcp.GetPromptParams{Name: "code-review", Arguments: map[string]string{"target": "HEAD~1..HEAD"}})
	require.NoError(t, err)
	require.NoError(t, rec.Flush(ctx))

	got, err := s.Usage(ctx, "2000-01-01")
	require.NoError(t, err)
	by := map[string]usage.Bucket{}
	for _, b := range got {
		by[b.Tool] = b
	}
	search := by["search"]
	assert.Equal(t, search.Repo, storetest.Repo)
	assert.Equal(t, search.Client, "usage-client")
	assert.Equal(t, search.Calls, 2)
	assert.Equal(t, search.Errors, 1, "tool errors count")
	assert.True(t, search.ResponseTokens > 0)
	assert.Equal(t, by[usage.Session].Calls, 1)
	assert.Equal(t, by[usage.Session].Client, "usage-client")
	assert.Equal(t, by["prompt:code-review"].Calls, 1)
}

func TestOffloadTurns(t *testing.T) {
	var got struct {
		Turns  []map[string]string `json:"turns"`
		Source string              `json:"source"`
	}
	var auth, path, primary, parent string
	fail := false
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth, path = r.Header.Get("Authorization"), r.URL.Path
		primary, parent = r.Header.Get(execution.SessionHeader), r.Header.Get(execution.ParentSessionHeader)
		_ = json.NewDecoder(r.Body).Decode(&got)
		if fail {
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"detail":"memory extraction runs only on a local model"}`))
			return
		}
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"id":"j1","repo":"fixture","state":"queued","turns":1}`))
	}))
	t.Cleanup(api.Close)

	ctx := context.Background()
	s := memory.New()
	require.NoError(t, s.ReplaceRepo(ctx, storetest.Repo, storetest.Fixture()))
	ct, st := mcp.NewInMemoryTransports()
	_, err := newTestServer(t, s, "test", Options{Memory: s, Offload: &APIClient{BaseURL: api.URL, Token: "secret"}}).Connect(ctx, st, nil)
	require.NoError(t, err)
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "claude-code"}, nil).Connect(ctx, ct, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = cs.Close() })

	var job struct{ ID, State string }
	res := call(t, cs, "offload_turns", map[string]any{"turns": []map[string]string{{"role": "user", "text": "We decided to drop Redis."}}}, &job)
	require.False(t, res.IsError)
	assert.Equal(t, job.ID, "j1")
	assert.Equal(t, path, "/api/v1/repos/fixture/session-memory/offload")
	assert.Equal(t, auth, "Bearer secret")
	assert.Equal(t, primary, "fixture-chat")
	assert.Equal(t, parent, primary)
	assert.Equal(t, got.Source, "claude-code")
	assert.Equal(t, got.Turns[0]["text"], "We decided to drop Redis.")

	fail = true
	res = call(t, cs, "offload_turns", map[string]any{"turns": []map[string]string{{"role": "user", "text": "x"}}}, nil)
	require.True(t, res.IsError)
	assert.True(t, strings.Contains(res.Content[0].(*mcp.TextContent).Text, "local model"), "the API's reason reaches the agent")
	res = call(t, cs, "offload_turns", map[string]any{"turns": []map[string]string{}}, nil)
	assert.True(t, res.IsError)

	_, absent := map[string]bool{}["x"]
	tools, err := connectMemory(t).ListTools(ctx, nil)
	require.NoError(t, err)
	for _, tool := range tools.Tools {
		absent = absent || tool.Name == "offload_turns"
	}
	assert.False(t, absent, "offload_turns needs --api-url")
}

func TestRecallContext(t *testing.T) {
	ctx := context.Background()
	// Without the API: the gate and name search run locally.
	cs := connectMemory(t)
	call(t, cs, "upsert_graph_triples", map[string]any{"triples": []map[string]any{{"subject": "Payments", "predicate": "OWNED_BY", "object": "Team Blue"}}}, nil)
	assert.True(t, call(t, cs, "recall_context", map[string]any{"query": "who owns payments?"}, nil).IsError, "session recall must not expose durable facts when the API is unavailable")
	var out struct {
		Skipped string
		Mode    string
		Facts   []agentmem.Fact
	}
	res := call(t, cs, "recall_context", map[string]any{"query": "who owns payments?", "memory_scope": "repository"}, &out)
	require.False(t, res.IsError)
	assert.Equal(t, out.Mode, "names")
	require.Len(t, out.Facts, 1)
	call(t, cs, "recall_context", map[string]any{"query": "thanks!", "memory_scope": "repository"}, &out)
	assert.Equal(t, out.Skipped, "small talk")

	// With the API: its hybrid answer is passed through.
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, r.URL.Path, "/api/v1/repos/fixture/session-memory/recall")
		assert.Equal(t, r.Header.Get(execution.SessionHeader), "fixture-chat")
		assert.Equal(t, r.Header.Get(execution.ParentSessionHeader), "fixture-chat")
		_, _ = w.Write([]byte(`{"mode":"hybrid","entries":[{"id":"x","name":"Payments","score":0.9,"via":"vector"}],"notice":"n","entities":[],"facts":[]}`))
	}))
	t.Cleanup(api.Close)
	s := memory.New()
	require.NoError(t, s.ReplaceRepo(ctx, storetest.Repo, storetest.Fixture()))
	ct, st := mcp.NewInMemoryTransports()
	_, err := newTestServer(t, s, "test", Options{Memory: s, Offload: &APIClient{BaseURL: api.URL}}).Connect(ctx, st, nil)
	require.NoError(t, err)
	cs2, err := mcp.NewClient(&mcp.Implementation{Name: "c"}, nil).Connect(ctx, ct, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = cs2.Close() })
	res = call(t, cs2, "recall_context", map[string]any{"query": "money flow"}, &out)
	require.False(t, res.IsError)
	assert.Equal(t, out.Mode, "hybrid")
}
