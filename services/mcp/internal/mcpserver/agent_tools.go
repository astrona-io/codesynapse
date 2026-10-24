package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/astrona-io/codesynapse/internal/agentctx"
	"github.com/astrona-io/codesynapse/internal/cleanup"
	"github.com/astrona-io/codesynapse/internal/coretools"
	"github.com/astrona-io/codesynapse/internal/license"
	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/security"
	"github.com/astrona-io/codesynapse/internal/verifyplan"
)

func registerAgentTools(s *mcp.Server, h *handlers, add func(name, desc string) *mcp.Tool) {
	mcp.AddTool(s, add("code_identities", "Permanent symbol IDs with current and historical revision-specific locations, evidence for renames and moves, and uncertain identity or possible-copy candidates for review. Candidates are evidence, not verified lineage. Removed symbols stay historical. Bounded pagination reports omissions; available=false means the index predates identity analysis. Pass a sym- ID to inspect it, or review_only for unresolved matches."), h.codeIdentities)
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
	mcp.AddTool(s, add("verify_plan",
		"What to run to check a change: pass a diff, files or base/head refs as for get_pr_context. Returns the tests that reach the changed code as exact commands (go test ./pkg -run '^(TestA|TestB)$', pytest file::test, npm test -- file), closest first, then build and lint for each language touched, and the changed functions no test reaches. Run the steps in order before you finish."),
		h.verifyPlan)
	mcp.AddTool(s, add("unused_code",
		"Unused code to remove: unreachable and test-only functions (with confidence and security relevance), files with nothing live, unused go.mod modules (and CVEs removing them eliminates), unused Helm values and template defines."),
		h.unusedCode)
	mcp.AddTool(s, add("cleanup_plan",
		"A ranked list of changes worth making in a repository: delete unreachable code (security-relevant first), remove unused dependencies, upgrade vulnerable ones (indirect ones through the package that pulls them in), resolve licence conflicts, merge duplicate code and cover untested hotspots. Each step has a value, effort and safety, why it matters, its targets with file:line and how to verify it. Use it to pick what to do next."),
		h.cleanupPlan)
	mcp.AddTool(s, add("refactor_duplicates",
		"How to merge one group of duplicate functions found by duplicate_code (pass its hash and kind): a suggested name and home for the shared function, the approach, the copies with their callers and tests, the risks (public API, import cycles, differing signatures, untested copies, unused copies) and the steps in order. Plans only; it never edits code."),
		h.refactorDuplicates)
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

// The core tools' inputs and answers live in internal/coretools, which the
// Claude Code bridge also answers from a working tree.
type (
	symbolContextIn   = coretools.SymbolContextIn
	callGraphIn       = coretools.CallGraphIn
	implementationsIn = coretools.ImplementationsIn
	prContextIn       = coretools.PRContextIn
	minimalContextIn  = coretools.MinimalContextIn
)

func (h *handlers) symbolContext(ctx context.Context, _ *mcp.CallToolRequest, in symbolContextIn) (*mcp.CallToolResult, agentctx.SymbolContext, error) {
	repo, err := h.repo(ctx, in.Repo)
	if err != nil {
		return nil, agentctx.SymbolContext{}, err
	}
	out, err := coretools.SymbolContext(ctx, h.svc.R, repo, in)
	return nil, out, err
}

func (h *handlers) callGraph(ctx context.Context, _ *mcp.CallToolRequest, in callGraphIn) (*mcp.CallToolResult, agentctx.CallGraph, error) {
	repo, err := h.repo(ctx, in.Repo)
	if err != nil {
		return nil, agentctx.CallGraph{}, err
	}
	out, err := coretools.CallGraph(ctx, h.svc.R, repo, in)
	return nil, out, err
}

func (h *handlers) implementations(ctx context.Context, _ *mcp.CallToolRequest, in implementationsIn) (*mcp.CallToolResult, agentctx.RefList, error) {
	repo, err := h.repo(ctx, in.Repo)
	if err != nil {
		return nil, agentctx.RefList{}, err
	}
	out, err := coretools.Implementations(ctx, h.svc.R, repo, in)
	return nil, out, err
}

func (h *handlers) prContext(ctx context.Context, _ *mcp.CallToolRequest, in prContextIn) (*mcp.CallToolResult, agentctx.PRContext, error) {
	repo, err := h.repo(ctx, in.Repo)
	if err != nil {
		return nil, agentctx.PRContext{}, err
	}
	out, err := coretools.PRContext(ctx, h.svc.R, repo, in)
	return nil, out, err
}

func (h *handlers) verifyPlan(ctx context.Context, _ *mcp.CallToolRequest, in coretools.VerifyPlanIn) (*mcp.CallToolResult, verifyplan.Plan, error) {
	repo, err := h.repo(ctx, in.Repo)
	if err != nil {
		return nil, verifyplan.Plan{}, err
	}
	out, err := coretools.VerifyPlan(ctx, h.svc.R, repo, in)
	return nil, out, err
}

func (h *handlers) minimalContext(ctx context.Context, _ *mcp.CallToolRequest, in minimalContextIn) (*mcp.CallToolResult, agentctx.MinimalContext, error) {
	repo, err := h.repo(ctx, in.Repo)
	if err != nil {
		return nil, agentctx.MinimalContext{}, err
	}
	out, err := coretools.MinimalContext(ctx, h.svc.R, repo, in)
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
