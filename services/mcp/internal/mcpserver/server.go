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
	"slices"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/astrona-io/codesynapse/internal/coretools"
	"github.com/astrona-io/codesynapse/internal/metrics"
	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/query"
	"github.com/astrona-io/codesynapse/internal/security"
	"github.com/astrona-io/codesynapse/internal/store"
	"github.com/astrona-io/codesynapse/internal/usage"
)

// The server instructions are sent to clients on initialize and tell the
// model how to use the tools economically; instructionsFor (toolsets.go)
// renders them for the selected toolsets.

// CatalogInstructions are appended to the architecture rule when the
// catalog write tools are enabled.
const CatalogInstructions = `
   You may propose a record with architecture_propose (a draft, always proposed or draft, never accepted) and tie code to a record with architecture_link. Search architecture_proposals first so you do not duplicate one. A proposal is not a decision and you cannot approve, accept, reject or supersede anything: tell the user what you proposed and that a person turns it into a decision by committing the file with an accepted status.`

// Options configure the MCP server.
type Options struct {
	// Reviews enables the review tools (submit_review and friends), which
	// write to this store. Nil leaves the server strictly read-only.
	Reviews store.ReviewStore
	// Curated keeps the proposals and links of the architecture catalog; the
	// read tools show them. With CatalogWrites the propose and link tools are
	// added, which write only there, never to code or the code graph.
	Curated       store.CuratedStore
	CatalogWrites bool
	// History enables repo_trend, which reads the snapshots taken after each
	// ingest. Nil leaves the tool out.
	History store.HistoryStore
	// Memory enables the agent-memory tools (upsert_graph_triples and
	// friends), which write to this store. Nil leaves them out entirely.
	Memory store.MemoryStore
	// Usage, when set, counts sessions, tool calls and prompt requests.
	Usage *usage.Recorder
	// Access scopes calls made with personal API keys to the key's
	// organisation; without it such calls see no repositories.
	Access Access
	// StdioToken is a personal API key supplied by the process environment.
	StdioToken string
	// Runtime validates upstream primary identity and binds it to transports.
	Runtime *Runtime
	// Offload, with Memory, adds offload_turns: background extraction of
	// triples from conversation turns by the API's local model.
	Offload *APIClient
	// Toolsets limits the tools to these sets (ParseToolsets); nil serves
	// every tool.
	Toolsets []string
}

// New builds an MCP server backed by r.
func New(r store.Reader, version string, opts ...Options) *mcp.Server {
	var o Options
	if len(opts) > 0 {
		o = opts[0]
	}
	instructions := instructionsFor(o.Toolsets, o.CatalogWrites && o.Curated != nil, o.Memory != nil)
	s := mcp.NewServer(&mcp.Implementation{Name: "codesynapse", Version: version}, &mcp.ServerOptions{
		Instructions: instructions,
	})
	h := &handlers{svc: query.Service{R: r}}
	if len(opts) > 0 {
		h.access = opts[0].Access
	}
	s.AddReceivingMiddleware(h.authorizationMiddleware())
	if len(opts) > 0 && opts[0].Usage != nil {
		s.AddReceivingMiddleware(usageMiddleware(opts[0].Usage))
	}
	s.AddReceivingMiddleware(telemetryMiddleware())
	// Inside the caller and runtime checks (so it knows who and which chat),
	// outside authorization (so it records refusals too).
	s.AddReceivingMiddleware(h.auditMiddleware())
	s.AddReceivingMiddleware(traceMiddleware())
	stdioToken := ""
	if len(opts) > 0 {
		stdioToken = opts[0].StdioToken
	}
	runtime := NewRuntime(h.access, stdioToken, "", "")
	if len(opts) > 0 && opts[0].Runtime != nil {
		runtime = opts[0].Runtime
	}
	s.AddReceivingMiddleware(runtime.middleware())
	s.AddReceivingMiddleware(callerMiddleware(h.access, stdioToken))
	readOnly := &mcp.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true}
	add := func(name, desc string) *mcp.Tool {
		return &mcp.Tool{Name: name, Description: desc, Annotations: readOnly}
	}

	mcp.AddTool(s, add("repo_list", "List indexed repositories with head commit and ingest time."), h.repoList)
	mcp.AddTool(s, add("repo_overview", "Stats, packages (with summaries) and documents of a repository. Start here."), h.repoOverview)
	mcp.AddTool(s, add("search", "Find symbols, files or doc sections by name substring. Returns ids for the other tools."), h.search)
	registerAgentTools(s, h, add)
	registerGitTools(s, add)
	registerDiscoveryTools(s, h, add)
	if len(opts) > 0 {
		h.curated = opts[0].Curated
	}
	registerArchitectureTools(s, h, add, len(opts) > 0 && opts[0].CatalogWrites && opts[0].Curated != nil)
	registerPrompts(s)
	if len(opts) > 0 && opts[0].History != nil {
		h.history = opts[0].History
		mcp.AddTool(s, add("repo_trend",
			"How a repository is changing: the security grade and score, vulnerabilities your code calls, serious findings, licence conflicts, duplicate and unused code, test reach and documentation after each sync, and what got worse or better since the sync before. Use it to answer whether the codebase is improving, and to check your change did not make something worse."),
			h.repoTrend)
	}
	if len(opts) > 0 && opts[0].Reviews != nil {
		registerReviewTools(s, h, opts[0].Reviews)
		registerReviewPrompt(s)
		registerAgentTaskPrompts(s)
	}
	if len(opts) > 0 && opts[0].Memory != nil {
		registerMemoryTools(s, h, opts[0].Memory)
		registerRecallTool(s, h)
		registerMemoryPrompt(s)
		h.offload = opts[0].Offload
		if opts[0].Offload != nil {
			registerOffloadTool(s, h, opts[0].Offload)
		}
	}
	mcp.AddTool(s, add("file_outline", "Symbols and doc sections in a file with line ranges and signatures. Use before reading the file."), h.fileOutline)
	mcp.AddTool(s, add("hotspots", "Functions ranked by complexity x file churn: where defects and review effort concentrate."), h.hotspots)
	mcp.AddTool(s, add("attack_surface", "Entry points the code registers (HTTP routes, MCP tools, CLI commands) with their handler, the guards seen at registration, in route groups and router middleware, or on the handler (policy arguments, wrapping middleware, before_action, @login_required) and the sensitive operations the handler reaches (exec, sql, filesystem, network, ...) with the closest call. Unguarded and widely reaching ones first. Filter with kind, reaching, unguarded or query. Go router middleware counts only within the function that builds the router. get_symbol_context's reached_from answers the reverse: which entry points end up at a symbol."), h.attackSurface)
	mcp.AddTool(s, add("security_symbols", "Functions that call security-sensitive APIs (exec, sql, crypto, http, filesystem, ...), optionally filtered by tag."), h.securitySymbols)
	mcp.AddTool(s, add("file_history", "Recent commits that modified a file."), h.fileHistory)
	mcp.AddTool(s, add("security_report", "Security review: graded score, findings from code, Kubernetes/Helm, CI and Compose rules (rule, severity, CWE, location, exposed, tested) and vulnerable dependencies (CVE, severity, fixed version, called/imported/required). Also says what the rules checked and what they did not (review those files by hand), and the commit it describes: if your checkout is on another commit, call repo_status and ask for a sync. Grade N/A means nothing was covered. Use min_severity to trim."), h.securityReport)
	mcp.AddTool(s, add("repo_metrics", "Repository metrics from the last ingest: size, complexity distributions, test reachability, doc coverage, package coupling, dead-code candidates and the token benchmark."), h.repoMetrics)
	pruneToolsets(s, o.Toolsets)
	return s
}

type handlers struct {
	svc     query.Service
	reviews store.ReviewStore
	history store.HistoryStore
	curated store.CuratedStore
	memory  store.MemoryStore
	offload *APIClient
	access  Access
}

// repo resolves an optional repo argument: empty means "the only repo".
func (h *handlers) repo(ctx context.Context, repo string) (string, error) {
	allowed, restricted, err := h.allowed(ctx)
	if err != nil {
		return "", err
	}
	if repo != "" {
		if restricted && !slices.Contains(allowed, repo) {
			return "", fmt.Errorf("repository %q is not available to your organisation", repo)
		}
		noteUsageRepository(ctx, repo)
		return repo, nil
	}
	repos, err := h.visibleRepos(ctx)
	if err != nil {
		return "", err
	}
	switch len(repos) {
	case 0:
		if restricted {
			return "", errors.New("your organisation has no repositories yet; add one in CodeSynapse first")
		}
		return "", errors.New("no repositories are indexed; run `codesynapse ingest` first")
	case 1:
		noteUsageRepository(ctx, repos[0].Name)
		return repos[0].Name, nil
	}
	names := make([]string, 0, len(repos))
	for _, r := range repos {
		names = append(names, r.Name)
	}
	return "", fmt.Errorf("several repositories are indexed; pass repo as one of: %s", strings.Join(names, ", "))
}

// visibleRepos lists the repository nodes the caller may use.
func (h *handlers) visibleRepos(ctx context.Context) ([]model.Node, error) {
	repos, err := h.svc.R.Repos(ctx)
	if err != nil {
		return nil, err
	}
	allowed, restricted, err := h.allowed(ctx)
	if err != nil || !restricted {
		return repos, err
	}
	return slices.DeleteFunc(repos, func(n model.Node) bool { return !slices.Contains(allowed, n.Name) }), nil
}

// RepoArg is embedded in every input.
// RepoArg selects the repository (internal/coretools).
type RepoArg = coretools.RepoArg

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
	repos, err := h.visibleRepos(ctx)
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

type (
	searchIn   = coretools.SearchIn
	refsOut    = coretools.RefsOut
	fileIn     = coretools.FileIn
	outlineOut = coretools.OutlineOut
)

func (h *handlers) search(ctx context.Context, _ *mcp.CallToolRequest, in searchIn) (*mcp.CallToolResult, refsOut, error) {
	repo, err := h.repo(ctx, in.Repo)
	if err != nil {
		return nil, refsOut{}, err
	}
	out, err := coretools.Search(ctx, h.svc.R, repo, in)
	return nil, out, err
}

func (h *handlers) fileOutline(ctx context.Context, _ *mcp.CallToolRequest, in fileIn) (*mcp.CallToolResult, outlineOut, error) {
	repo, err := h.repo(ctx, in.Repo)
	if err != nil {
		return nil, outlineOut{}, err
	}
	out, err := coretools.FileOutline(ctx, h.svc.R, repo, in)
	return nil, out, err
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
	hits, err := store.ForRepository(h.svc.R, repo).Traverse(ctx, model.NodeID(repo, model.KindFile, strings.TrimPrefix(in.File, "./")),
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
	Path        string `json:"path,omitempty" jsonschema:"only findings in this file or directory (repo-relative); counts follow, score and grade stay repository-wide"`
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
	rep, err := security.Build(ctx, h.svc.R, repo, h.securityOptions(ctx, repo, minSev))
	if err != nil {
		return nil, rep, err
	}
	rep.Sensitive = nil // keep the answer small; security_symbols has these
	filterFindingsByPath(&rep, in.Path)
	if in.Limit > 0 {
		rep.Findings = rep.Findings[:min(len(rep.Findings), in.Limit)]
		rep.Suppressed = rep.Suppressed[:min(len(rep.Suppressed), in.Limit)]
		rep.Vulnerabilities = rep.Vulnerabilities[:min(len(rep.Vulnerabilities), in.Limit)]
	}
	return nil, rep, nil
}
