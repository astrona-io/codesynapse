package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/parisnakitakejser/codesynapse/internal/agentctx"
	"github.com/parisnakitakejser/codesynapse/internal/cleanup"
	"github.com/parisnakitakejser/codesynapse/internal/license"
	"github.com/parisnakitakejser/codesynapse/internal/model"
	"github.com/parisnakitakejser/codesynapse/internal/security"
)

// maxDiffBytes bounds diffs submitted to get_pr_context.
const maxDiffBytes = 4 << 20

func registerAgentTools(s *mcp.Server, h *handlers, add func(name, desc string) *mcp.Tool) {
	mcp.AddTool(s, add("get_symbol_context",
		"Composite snapshot of one symbol or package: signature, doc summary, location, risk score, callers, callees, tests, implementations and findings. verbosity=outline (default) is cheap; full adds transitive callers, untested public callers, docs and file churn. Packages return exported API, interfaces, hub symbols and imports."),
		h.symbolContext)
	mcp.AddTool(s, add("get_call_graph",
		"Bounded call graph around a symbol: direction callers, callees or both; depth default 2 (max 4). Marks entry points (main, exported API, handlers) and includes the edges."),
		h.callGraph)
	mcp.AddTool(s, add("get_implementations",
		"Resolve interface dispatch: interface type -> implementing types, concrete type -> its interfaces, interface method -> concrete methods, concrete method -> interface methods it satisfies."),
		h.implementations)
	mcp.AddTool(s, add("get_pr_context",
		"Change analysis in one call: pass a unified diff, a list of modified files, or base/head git refs. Returns changed symbols with risk, blast radius, covering tests, findings, docs to re-check, exported symbols without tests and security-sensitive symbols touched."),
		h.prContext)
	mcp.AddTool(s, add("unused_code",
		"Unused code to remove: unreachable and test-only functions (with confidence and security relevance), files with nothing live, unused go.mod modules (and CVEs removing them eliminates), unused Helm values and template defines."),
		h.unusedCode)
	mcp.AddTool(s, add("duplicate_code",
		"Code that exists more than once in a repository: groups of functions with the same structure (names and literal values ignored) and of edited copies (kind near), the redundant lines, how far apart the copies are (file, package, repository) and where each copy is. Use it before writing a helper, and to find what to merge."),
		h.duplicateCode)
	mcp.AddTool(s, add("shared_code",
		"Code that several repositories contain: copies found across every repository you can see, and where it is worth it a recommendation to move them into a shared library or framework, with candidates and the lines saved. Use it before copying code between repositories."),
		h.sharedCode)
	mcp.AddTool(s, add("license_report",
		"Dependency licences checked against the repository's own licence: conflicts (e.g. GPL or AGPL in an Apache/MIT project, non-open-source licences), items to review (weak copyleft, unknown licences) and counts per licence. Each verdict says why and where the licence was read. Guidance, not legal advice."),
		h.licenseReport)
	mcp.AddTool(s, add("rule_guide",
		"How to fix a security finding: why the rule matters, the fix, CWE, and vulnerable -> fixed code examples (language-specific first). Call it before fixing a finding from security_report or get_pr_context."),
		h.ruleGuide)
	mcp.AddTool(s, add("minimal_context",
		"Given a task description (and optionally files), return the most relevant symbols, doc sections and config entries with line ranges, packed under max_tokens (default 1500). Use it first on a new task."),
		h.minimalContext)
}

func (h *handlers) agent() agentctx.Service { return agentctx.Service{R: h.svc.R} }

type symbolContextIn struct {
	RepoArg
	Symbol    string `json:"symbol" jsonschema:"qualified_id (e.g. store.(*Store).Get), node id, name, or package path"`
	Verbosity string `json:"verbosity,omitempty" jsonschema:"outline (default) or full"`
	Depth     int    `json:"depth,omitempty" jsonschema:"caller depth for verbosity=full (1-4, default 1)"`
	Limit     int    `json:"limit,omitempty" jsonschema:"max entries per list (default 5 outline, 20 full)"`
}

func (h *handlers) symbolContext(ctx context.Context, _ *mcp.CallToolRequest, in symbolContextIn) (*mcp.CallToolResult, agentctx.SymbolContext, error) {
	repo, err := h.repo(ctx, in.Repo)
	if err != nil {
		return nil, agentctx.SymbolContext{}, err
	}
	switch in.Verbosity {
	case "", agentctx.Outline, agentctx.Full:
	default:
		return nil, agentctx.SymbolContext{}, fmt.Errorf("verbosity must be outline or full, not %q", in.Verbosity)
	}
	out, err := h.agent().SymbolContext(ctx, repo, in.Symbol, agentctx.ContextOptions{Verbosity: in.Verbosity, Depth: in.Depth, Limit: in.Limit})
	return nil, out, err
}

type callGraphIn struct {
	RepoArg
	Symbol    string `json:"symbol" jsonschema:"qualified_id, node id or name"`
	Direction string `json:"direction,omitempty" jsonschema:"callers, callees or both (default both)"`
	Depth     int    `json:"depth,omitempty" jsonschema:"hops, default 2, max 4"`
	Limit     int    `json:"limit,omitempty" jsonschema:"max nodes per direction, default 60"`
}

func (h *handlers) callGraph(ctx context.Context, _ *mcp.CallToolRequest, in callGraphIn) (*mcp.CallToolResult, agentctx.CallGraph, error) {
	repo, err := h.repo(ctx, in.Repo)
	if err != nil {
		return nil, agentctx.CallGraph{}, err
	}
	switch in.Direction {
	case "", "both", "callers", "callees":
	default:
		return nil, agentctx.CallGraph{}, fmt.Errorf("direction must be callers, callees or both")
	}
	g, err := h.agent().CallGraph(ctx, repo, in.Symbol, in.Direction, in.Depth, in.Limit)
	return nil, g, err
}

type implementationsIn struct {
	RepoArg
	Symbol string `json:"symbol" jsonschema:"interface or concrete type, or a method"`
	Limit  int    `json:"limit,omitempty" jsonschema:"max results, default 50"`
}

func (h *handlers) implementations(ctx context.Context, _ *mcp.CallToolRequest, in implementationsIn) (*mcp.CallToolResult, agentctx.RefList, error) {
	repo, err := h.repo(ctx, in.Repo)
	if err != nil {
		return nil, agentctx.RefList{}, err
	}
	out, err := h.agent().Implementations(ctx, repo, in.Symbol, in.Limit)
	return nil, out, err
}

type prContextIn struct {
	RepoArg
	Diff  string   `json:"diff,omitempty" jsonschema:"unified diff text (git diff output)"`
	Files []string `json:"files,omitempty" jsonschema:"modified file paths; every symbol in them counts as changed"`
	Base  string   `json:"base,omitempty" jsonschema:"base git ref (with head) when neither diff nor files is given"`
	Head  string   `json:"head,omitempty" jsonschema:"head git ref, default HEAD"`
	Depth int      `json:"depth,omitempty" jsonschema:"caller depth for blast radius, default 3"`
}

func (h *handlers) prContext(ctx context.Context, _ *mcp.CallToolRequest, in prContextIn) (*mcp.CallToolResult, agentctx.PRContext, error) {
	repo, err := h.repo(ctx, in.Repo)
	if err != nil {
		return nil, agentctx.PRContext{}, err
	}
	if len(in.Diff) > maxDiffBytes {
		return nil, agentctx.PRContext{}, fmt.Errorf("diff exceeds %d bytes", maxDiffBytes)
	}
	opts := agentctx.PROptions{Diff: []byte(in.Diff), Files: in.Files, Base: in.Base, Head: in.Head, Depth: in.Depth}
	if len(opts.Diff) == 0 && len(opts.Files) == 0 && in.Base != "" {
		n, err := h.svc.R.Node(ctx, model.NodeID(repo, model.KindRepository, repo))
		if err != nil {
			return nil, agentctx.PRContext{}, err
		}
		if n.Root == "" {
			return nil, agentctx.PRContext{}, errors.New("repository root unknown; pass a diff or files instead")
		}
		if _, err := os.Stat(n.Root); err != nil {
			return nil, agentctx.PRContext{}, errors.New("repository checkout is not reachable from this server; pass a diff or files instead")
		}
		opts.Root = n.Root
	}
	out, err := h.agent().PRContext(ctx, repo, opts)
	return nil, out, err
}

type minimalContextIn struct {
	RepoArg
	Task      string   `json:"task" jsonschema:"what you are trying to do, in plain words; include symbol names you know"`
	Files     []string `json:"files,omitempty" jsonschema:"files already known to be relevant (boosted)"`
	MaxTokens int      `json:"max_tokens,omitempty" jsonschema:"token budget for the answer, default 1500"`
}

func (h *handlers) minimalContext(ctx context.Context, _ *mcp.CallToolRequest, in minimalContextIn) (*mcp.CallToolResult, agentctx.MinimalContext, error) {
	repo, err := h.repo(ctx, in.Repo)
	if err != nil {
		return nil, agentctx.MinimalContext{}, err
	}
	if strings.TrimSpace(in.Task) == "" {
		return nil, agentctx.MinimalContext{}, errors.New("task is required")
	}
	out, err := h.agent().MinimalContext(ctx, repo, in.Task, in.Files, in.MaxTokens)
	return nil, out, err
}

type unusedCodeIn struct {
	RepoArg
	Category      string `json:"category,omitempty" jsonschema:"unreachable, test-only or all (default all)"`
	MinConfidence string `json:"min_confidence,omitempty" jsonschema:"low, medium or high (default medium)"`
	SecurityOnly  bool   `json:"security_only,omitempty" jsonschema:"only items with security relevance"`
	Limit         int    `json:"limit,omitempty" jsonschema:"max functions, default 30"`
}

func (h *handlers) unusedCode(ctx context.Context, _ *mcp.CallToolRequest, in unusedCodeIn) (*mcp.CallToolResult, cleanup.Report, error) {
	repo, err := h.repo(ctx, in.Repo)
	if err != nil {
		return nil, cleanup.Report{}, err
	}
	n, err := h.svc.R.Node(ctx, model.NodeID(repo, model.KindRepository, repo))
	if err != nil {
		return nil, cleanup.Report{}, err
	}
	if n.Cleanup == "" {
		return nil, cleanup.Report{}, errors.New("no cleanup report stored; re-ingest the repository")
	}
	var rep cleanup.Report
	if err := json.Unmarshal([]byte(n.Cleanup), &rep); err != nil {
		return nil, rep, fmt.Errorf("decoding cleanup report: %w", err)
	}
	rank := map[string]int{cleanup.Low: 0, cleanup.Medium: 1, cleanup.High: 2}
	minRank, ok := rank[in.MinConfidence]
	if !ok {
		minRank = 1
	}
	limit := in.Limit
	if limit <= 0 {
		limit = 30
	}
	items := rep.Items[:0:0]
	for _, it := range rep.Items {
		if (in.Category == "" || in.Category == "all" || in.Category == it.Category) &&
			rank[it.Confidence] >= minRank && (!in.SecurityOnly || len(it.Security) > 0) {
			items = append(items, it)
		}
	}
	rep.Items = items[:min(len(items), limit)]
	rep.Files = rep.Files[:min(len(rep.Files), 20)]
	rep.UnusedValues = rep.UnusedValues[:min(len(rep.UnusedValues), 20)]
	return nil, rep, nil
}

type ruleGuideIn struct {
	Rule     string `json:"rule" jsonschema:"rule id from a finding, e.g. sql-concat"`
	Language string `json:"language,omitempty" jsonschema:"go, python, typescript, yaml or github-actions; puts matching examples first"`
}

func (h *handlers) ruleGuide(_ context.Context, _ *mcp.CallToolRequest, in ruleGuideIn) (*mcp.CallToolResult, security.Guide, error) {
	g, ok := security.GuideFor(in.Rule, in.Language)
	if !ok {
		var ids []string
		for _, c := range security.Catalog() {
			ids = append(ids, c.ID)
		}
		return nil, security.Guide{}, fmt.Errorf("unknown rule %q; known rules: %s", in.Rule, strings.Join(ids, ", "))
	}
	if in.Language != "" && len(g.Examples) > 2 {
		g.Examples = g.Examples[:2] // keep the answer small
	}
	return nil, g, nil
}

type licenseReportIn struct {
	RepoArg
	Verdict string `json:"verdict,omitempty" jsonschema:"conflict, review or all (default all): which dependencies to list"`
	Limit   int    `json:"limit,omitempty" jsonschema:"max dependencies listed, default 30"`
}

func (h *handlers) licenseReport(ctx context.Context, _ *mcp.CallToolRequest, in licenseReportIn) (*mcp.CallToolResult, license.Report, error) {
	repo, err := h.repo(ctx, in.Repo)
	if err != nil {
		return nil, license.Report{}, err
	}
	n, err := h.svc.R.Node(ctx, model.NodeID(repo, model.KindRepository, repo))
	if err != nil {
		return nil, license.Report{}, err
	}
	if n.Licenses == "" {
		return nil, license.Report{}, errors.New("no licence report stored; re-ingest the repository")
	}
	var rep license.Report
	if err := json.Unmarshal([]byte(n.Licenses), &rep); err != nil {
		return nil, rep, fmt.Errorf("decoding licence report: %w", err)
	}
	limit := in.Limit
	if limit <= 0 {
		limit = 30
	}
	issues := rep.Issues[:0:0]
	for _, e := range rep.Issues {
		if (in.Verdict == "" || in.Verdict == "all" || in.Verdict == e.Verdict.Level) && len(issues) < limit {
			issues = append(issues, e)
		}
	}
	rep.Issues = issues
	return nil, rep, nil
}
