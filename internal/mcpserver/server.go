// Package mcpserver exposes the code graph to AI agents over the Model
// Context Protocol. Every tool is read-only and returns compact JSON with
// file:line locations, so an agent can answer "where is X, who calls it,
// what breaks if I change it" without reading whole files.
package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/parisnakitakejser/codesynapse/internal/metrics"
	"github.com/parisnakitakejser/codesynapse/internal/model"
	"github.com/parisnakitakejser/codesynapse/internal/query"
	"github.com/parisnakitakejser/codesynapse/internal/security"
	"github.com/parisnakitakejser/codesynapse/internal/store"
)

// Instructions are sent to clients on initialize and tell the model how to
// use the tools economically.
const Instructions = `CodeSynapse is a deterministic graph layer for this repository's Go code, docs, YAML, templates, history and dependencies. Always use CodeSynapse graph tools to inspect symbols, callers, implementations and test coverage BEFORE reading full source files or running text greps.

Rules for context efficiency:
1. Start small: use verbosity "outline" (the default) and depth 1 first. Expand only when needed.
2. Interface resolution: do not grep for interface calls. Use get_implementations (IMPLEMENTS / DISPATCHES_TO edges) to find concrete implementations.
3. Use composite context: prefer get_symbol_context, get_pr_context and minimal_context over many sequential calls.
4. Exact matches: when a lookup returns suggestions, retry with the exact qualified_id it lists (e.g. store.(*Store).Get).
5. Test boundaries: before editing a symbol, check its tests (is_test: true callers) to confirm safety and find fixtures.
6. Read narrowly: file_outline and the "read"/"location" fields give file:start-end ranges; read only those lines.
7. Reviews: when asked to review code, analyse with get_pr_context, security_report and rule_guide, then record the result with submit_review (comments anchored to file:line, with concrete suggestions). Mark comments you have addressed with update_review_comment.
Locations are repo-relative. The graph reflects the last ingest; files edited since may differ.`

// Options configure the MCP server.
type Options struct {
	// Reviews enables the review tools (submit_review and friends), which
	// write to this store. Nil leaves the server strictly read-only.
	Reviews store.ReviewStore
}

// New builds an MCP server backed by r.
func New(r store.Reader, version string, opts ...Options) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "codesynapse", Version: version}, &mcp.ServerOptions{
		Instructions: Instructions,
	})
	h := &handlers{svc: query.Service{R: r}}
	readOnly := &mcp.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true}
	add := func(name, desc string) *mcp.Tool {
		return &mcp.Tool{Name: name, Description: desc, Annotations: readOnly}
	}

	mcp.AddTool(s, add("repo_list", "List indexed repositories with head commit and ingest time."), h.repoList)
	mcp.AddTool(s, add("repo_overview", "Stats, packages (with summaries) and documents of a repository. Start here."), h.repoOverview)
	mcp.AddTool(s, add("search", "Find symbols, files or doc sections by name substring. Returns ids for the other tools."), h.search)
	registerAgentTools(s, h, add)
	registerPrompts(s)
	if len(opts) > 0 && opts[0].Reviews != nil {
		registerReviewTools(s, h, opts[0].Reviews)
		registerReviewPrompt(s)
	}
	mcp.AddTool(s, add("file_outline", "Symbols and doc sections in a file with line ranges and signatures. Use before reading the file."), h.fileOutline)
	mcp.AddTool(s, add("hotspots", "Functions ranked by complexity x file churn: where defects and review effort concentrate."), h.hotspots)
	mcp.AddTool(s, add("security_symbols", "Functions that call security-sensitive APIs (exec, sql, crypto, http, filesystem, ...), optionally filtered by tag."), h.securitySymbols)
	mcp.AddTool(s, add("file_history", "Recent commits that modified a file."), h.fileHistory)
	mcp.AddTool(s, add("security_report", "Security review: graded score, code findings (rule, severity, CWE, location, exposed from an entry point, tested) and vulnerable dependencies (CVE, severity, fixed version, called/imported/required). Use min_severity to trim."), h.securityReport)
	mcp.AddTool(s, add("repo_metrics", "Repository metrics from the last ingest: size, complexity distributions, test reachability, doc coverage, package coupling, dead-code candidates and the token benchmark."), h.repoMetrics)
	return s
}

type handlers struct {
	svc     query.Service
	reviews store.ReviewStore
}

// repo resolves an optional repo argument: empty means "the only repo".
func (h *handlers) repo(ctx context.Context, repo string) (string, error) {
	if repo != "" {
		return repo, nil
	}
	repos, err := h.svc.R.Repos(ctx)
	if err != nil {
		return "", err
	}
	switch len(repos) {
	case 0:
		return "", errors.New("no repositories are indexed; run `codesynapse ingest` first")
	case 1:
		return repos[0].Name, nil
	}
	names := make([]string, 0, len(repos))
	for _, r := range repos {
		names = append(names, r.Name)
	}
	return "", fmt.Errorf("several repositories are indexed; pass repo as one of: %s", strings.Join(names, ", "))
}

// RepoArg is embedded in every input.
type RepoArg struct {
	Repo string `json:"repo,omitempty" jsonschema:"repository name; optional when only one is indexed"`
}

type empty struct{}

// RepoInfo describes an indexed repository.
type RepoInfo struct {
	Name       string `json:"name"`
	Head       string `json:"head,omitempty"`
	IngestedAt string `json:"ingested_at,omitempty"`
}

type repoListOut struct {
	Repos []RepoInfo `json:"repos"`
}

func (h *handlers) repoList(ctx context.Context, _ *mcp.CallToolRequest, _ empty) (*mcp.CallToolResult, repoListOut, error) {
	repos, err := h.svc.R.Repos(ctx)
	if err != nil {
		return nil, repoListOut{}, err
	}
	out := repoListOut{Repos: []RepoInfo{}}
	for _, r := range repos {
		out.Repos = append(out.Repos, RepoInfo{Name: r.Name, Head: r.Head, IngestedAt: r.Time})
	}
	return nil, out, nil
}

func (h *handlers) repoOverview(ctx context.Context, _ *mcp.CallToolRequest, in RepoArg) (*mcp.CallToolResult, query.Overview, error) {
	repo, err := h.repo(ctx, in.Repo)
	if err != nil {
		return nil, query.Overview{}, err
	}
	ov, err := h.svc.Overview(ctx, repo)
	ov.Repo.Root = "" // host paths are not useful to the agent
	return nil, ov, err
}

type searchIn struct {
	RepoArg
	Query string   `json:"query" jsonschema:"case-insensitive name substring"`
	Kinds []string `json:"kinds,omitempty" jsonschema:"filter by node kind: Function, Method, InterfaceMethod, Type, Package, File, Document, Section, Config, ConfigEntry, Template, TemplateDef, Finding, Vulnerability, Module"`
	Limit int      `json:"limit,omitempty" jsonschema:"max results, default 20"`
}

type refsOut struct {
	Results []query.Ref `json:"results"`
}

func (h *handlers) search(ctx context.Context, _ *mcp.CallToolRequest, in searchIn) (*mcp.CallToolResult, refsOut, error) {
	repo, err := h.repo(ctx, in.Repo)
	if err != nil {
		return nil, refsOut{}, err
	}
	if strings.TrimSpace(in.Query) == "" {
		return nil, refsOut{}, errors.New("query is required")
	}
	kinds := make([]model.NodeKind, 0, len(in.Kinds))
	for _, k := range in.Kinds {
		nk := model.NodeKind(k)
		if !nk.Valid() {
			return nil, refsOut{}, fmt.Errorf("unknown kind %q", k)
		}
		kinds = append(kinds, nk)
	}
	ns, err := h.svc.R.Search(ctx, repo, in.Query, kinds, in.Limit)
	if err != nil {
		return nil, refsOut{}, err
	}
	out := refsOut{Results: make([]query.Ref, 0, len(ns))}
	for _, n := range ns {
		out.Results = append(out.Results, query.RefOf(n))
	}
	return nil, out, nil
}

type fileIn struct {
	RepoArg
	File string `json:"file" jsonschema:"repo-relative path, e.g. internal/store/store.go"`
}

type outlineOut struct {
	File    string               `json:"file"`
	Entries []query.OutlineEntry `json:"entries"`
}

func (h *handlers) fileOutline(ctx context.Context, _ *mcp.CallToolRequest, in fileIn) (*mcp.CallToolResult, outlineOut, error) {
	repo, err := h.repo(ctx, in.Repo)
	if err != nil {
		return nil, outlineOut{}, err
	}
	entries, err := h.svc.Outline(ctx, repo, in.File)
	if err != nil {
		return nil, outlineOut{}, err
	}
	if entries == nil {
		entries = []query.OutlineEntry{}
	}
	return nil, outlineOut{File: in.File, Entries: entries}, nil
}

type limitIn struct {
	RepoArg
	Limit int `json:"limit,omitempty" jsonschema:"max results, default 20"`
}

type hotspotsOut struct {
	Hotspots []query.Hotspot `json:"hotspots"`
}

func (h *handlers) hotspots(ctx context.Context, _ *mcp.CallToolRequest, in limitIn) (*mcp.CallToolResult, hotspotsOut, error) {
	repo, err := h.repo(ctx, in.Repo)
	if err != nil {
		return nil, hotspotsOut{}, err
	}
	hs, err := h.svc.Hotspots(ctx, repo, in.Limit)
	if hs == nil {
		hs = []query.Hotspot{}
	}
	return nil, hotspotsOut{Hotspots: hs}, err
}

type securityIn struct {
	RepoArg
	Tag   string `json:"tag,omitempty" jsonschema:"security tag: exec, sql, cypher, http, network, filesystem, crypto, weak-crypto, insecure-rand, tls, unsafe, reflect, deserialize, template, env, syscall, auth"`
	Limit int    `json:"limit,omitempty" jsonschema:"max results, default 50"`
}

// SecuritySymbol is a function with the sensitive API categories it touches.
type SecuritySymbol struct {
	query.Ref
	Tags []string `json:"security_tags"`
}

type securityOut struct {
	Symbols []SecuritySymbol `json:"symbols"`
}

func (h *handlers) securitySymbols(ctx context.Context, _ *mcp.CallToolRequest, in securityIn) (*mcp.CallToolResult, securityOut, error) {
	repo, err := h.repo(ctx, in.Repo)
	if err != nil {
		return nil, securityOut{}, err
	}
	out := securityOut{Symbols: []SecuritySymbol{}}
	ranked, err := h.svc.R.List(ctx, repo, store.Filter{
		Kinds: []model.NodeKind{model.KindFunction, model.KindMethod}, Tag: in.Tag, ExcludeTests: true,
	}, store.ByFanIn, defaultInt(in.Limit, 50)*4)
	if err != nil {
		return nil, out, err
	}
	for _, r := range ranked {
		if len(r.Node.SecurityTags) == 0 {
			continue
		}
		out.Symbols = append(out.Symbols, SecuritySymbol{Ref: query.RefOf(r.Node), Tags: r.Node.SecurityTags})
		if len(out.Symbols) >= defaultInt(in.Limit, 50) {
			break
		}
	}
	return nil, out, nil
}

// CommitRef is a commit in a file's history.
type CommitRef struct {
	Hash    string `json:"hash"`
	Time    string `json:"time"`
	Subject string `json:"subject"`
}

type historyOut struct {
	File    string      `json:"file"`
	Commits []CommitRef `json:"commits"`
}

func (h *handlers) fileHistory(ctx context.Context, _ *mcp.CallToolRequest, in fileIn) (*mcp.CallToolResult, historyOut, error) {
	repo, err := h.repo(ctx, in.Repo)
	if err != nil {
		return nil, historyOut{}, err
	}
	out := historyOut{File: in.File, Commits: []CommitRef{}}
	hits, err := h.svc.R.Traverse(ctx, model.NodeID(repo, model.KindFile, strings.TrimPrefix(in.File, "./")),
		[]model.EdgeKind{model.EdgeModifies}, store.In, 1, 50)
	if err != nil {
		return nil, out, err
	}
	for _, hit := range hits {
		out.Commits = append(out.Commits, CommitRef{Hash: hit.Node.Name, Time: hit.Node.Time, Subject: hit.Node.Summary})
	}
	return nil, out, nil
}

func defaultInt(v, def int) int {
	if v <= 0 {
		return def
	}
	return v
}

func (h *handlers) repoMetrics(ctx context.Context, _ *mcp.CallToolRequest, in RepoArg) (*mcp.CallToolResult, metrics.Report, error) {
	repo, err := h.repo(ctx, in.Repo)
	if err != nil {
		return nil, metrics.Report{}, err
	}
	n, err := h.svc.R.Node(ctx, model.NodeID(repo, model.KindRepository, repo))
	if err != nil {
		return nil, metrics.Report{}, err
	}
	if n.Metrics == "" {
		return nil, metrics.Report{}, errors.New("no metrics stored; re-ingest the repository")
	}
	var rep metrics.Report
	if err := json.Unmarshal([]byte(n.Metrics), &rep); err != nil {
		return nil, rep, fmt.Errorf("decoding metrics: %w", err)
	}
	return nil, rep, nil
}

type securityReportIn struct {
	RepoArg
	MinSeverity string `json:"min_severity,omitempty" jsonschema:"info, low, medium, high or critical; default low"`
	Limit       int    `json:"limit,omitempty" jsonschema:"max findings and vulnerabilities each"`
}

func (h *handlers) securityReport(ctx context.Context, _ *mcp.CallToolRequest, in securityReportIn) (*mcp.CallToolResult, security.Report, error) {
	repo, err := h.repo(ctx, in.Repo)
	if err != nil {
		return nil, security.Report{}, err
	}
	minSev := in.MinSeverity
	if minSev == "" {
		minSev = "low"
	}
	if model.SeverityRank(minSev) < 0 {
		return nil, security.Report{}, fmt.Errorf("unknown severity %q", minSev)
	}
	rep, err := security.Build(ctx, h.svc.R, repo, security.Options{MinSeverity: minSev})
	if err != nil {
		return nil, rep, err
	}
	rep.Sensitive = nil // keep the answer small; security_symbols has these
	if in.Limit > 0 {
		rep.Findings = rep.Findings[:min(len(rep.Findings), in.Limit)]
		rep.Vulnerabilities = rep.Vulnerabilities[:min(len(rep.Vulnerabilities), in.Limit)]
	}
	return nil, rep, nil
}
