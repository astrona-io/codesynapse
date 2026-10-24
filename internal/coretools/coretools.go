// Package coretools answers the core repository tools from any graph
// reader. The MCP server uses it against the published graph; the Claude
// Code bridge uses it against a graph of the developer's working tree, so a
// chat sees files it edited before they are synced (docs/PLAN.md, agent
// workflow roadmap step 4). Inputs and outputs are the tools' JSON shapes.
package coretools

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/astrona-io/codesynapse/internal/agentctx"
	"github.com/astrona-io/codesynapse/internal/model"
	"github.com/astrona-io/codesynapse/internal/query"
	"github.com/astrona-io/codesynapse/internal/store"
)

// MaxDiffBytes bounds diffs submitted to get_pr_context.
const MaxDiffBytes = 4 << 20

// Local are the tools a graph of one working tree can answer: they read
// one repository's code graph and nothing stored beside it.
var Local = []string{"minimal_context", "get_symbol_context", "get_call_graph", "get_implementations", "file_outline", "search", "get_pr_context"}

// RepoArg selects the repository.
type RepoArg struct {
	Repo string `json:"repo,omitempty" jsonschema:"repository name; optional when only one is indexed"`
}

// SymbolContextIn is get_symbol_context's input.
type SymbolContextIn struct {
	RepoArg
	Symbol    string `json:"symbol" jsonschema:"qualified_id (e.g. store.(*Store).Get), node id, name, or package path"`
	Verbosity string `json:"verbosity,omitempty" jsonschema:"outline (default) or full"`
	Depth     int    `json:"depth,omitempty" jsonschema:"caller depth for verbosity=full (1-4, default 1)"`
	Limit     int    `json:"limit,omitempty" jsonschema:"max entries per list (default 5 outline, 20 full)"`
	MaxTokens int    `json:"max_tokens,omitempty" jsonschema:"estimated JSON token budget, bytes/4; 256-20000, default 20000"`
}

// SymbolContext answers get_symbol_context.
func SymbolContext(ctx context.Context, r store.Reader, repo string, in SymbolContextIn) (agentctx.SymbolContext, error) {
	switch in.Verbosity {
	case "", agentctx.Outline, agentctx.Full:
	default:
		return agentctx.SymbolContext{}, fmt.Errorf("verbosity must be outline or full, not %q", in.Verbosity)
	}
	out, err := agentctx.Service{R: r}.SymbolContext(ctx, repo, in.Symbol, agentctx.ContextOptions{Verbosity: in.Verbosity, Depth: in.Depth, Limit: in.Limit, MaxTokens: in.MaxTokens})
	return compact(repo, out, err)
}

// CallGraphIn is get_call_graph's input.
type CallGraphIn struct {
	RepoArg
	Symbol    string `json:"symbol" jsonschema:"qualified_id, node id or name"`
	Direction string `json:"direction,omitempty" jsonschema:"callers, callees or both (default both)"`
	Depth     int    `json:"depth,omitempty" jsonschema:"hops, default 2, max 4"`
	Limit     int    `json:"limit,omitempty" jsonschema:"max nodes per direction, default 60"`
	MaxTokens int    `json:"max_tokens,omitempty" jsonschema:"estimated JSON token budget, bytes/4; 256-20000, default 20000"`
}

// CallGraph answers get_call_graph.
func CallGraph(ctx context.Context, r store.Reader, repo string, in CallGraphIn) (agentctx.CallGraph, error) {
	switch in.Direction {
	case "", "both", "callers", "callees":
	default:
		return agentctx.CallGraph{}, errors.New("direction must be callers, callees or both")
	}
	out, err := agentctx.Service{R: r}.CallGraph(ctx, repo, in.Symbol, in.Direction, in.Depth, in.Limit, in.MaxTokens)
	return compact(repo, out, err)
}

// ImplementationsIn is get_implementations' input.
type ImplementationsIn struct {
	RepoArg
	Symbol string `json:"symbol" jsonschema:"interface or concrete type, or a method"`
	Limit  int    `json:"limit,omitempty" jsonschema:"max results, default 50"`
}

// Implementations answers get_implementations.
func Implementations(ctx context.Context, r store.Reader, repo string, in ImplementationsIn) (agentctx.RefList, error) {
	out, err := agentctx.Service{R: r}.Implementations(ctx, repo, in.Symbol, in.Limit)
	return compact(repo, out, err)
}

// PRContextIn is get_pr_context's input.
type PRContextIn struct {
	RepoArg
	Diff      string   `json:"diff,omitempty" jsonschema:"unified diff text (git diff output)"`
	Files     []string `json:"files,omitempty" jsonschema:"modified file paths; every symbol in them counts as changed"`
	Base      string   `json:"base,omitempty" jsonschema:"base git ref (with head) when neither diff nor files is given"`
	Head      string   `json:"head,omitempty" jsonschema:"head git ref, default HEAD"`
	Depth     int      `json:"depth,omitempty" jsonschema:"caller depth for blast radius, default 3"`
	MaxTokens int      `json:"max_tokens,omitempty" jsonschema:"estimated JSON token budget, bytes/4; 256-20000, default 20000"`
}

// PRContext answers get_pr_context. Base and head refs need the checkout
// the graph was built from to be readable here.
func PRContext(ctx context.Context, r store.Reader, repo string, in PRContextIn) (agentctx.PRContext, error) {
	return PRContextAgainst(ctx, r, nil, "", repo, in)
}

// PRContextAgainst also compares the findings of the changed files with
// base, a graph of the change's base named by baseLabel (nil: no
// comparison). r must then be a graph of the head.
func PRContextAgainst(ctx context.Context, r, base store.Reader, baseLabel, repo string, in PRContextIn) (agentctx.PRContext, error) {
	if len(in.Diff) > MaxDiffBytes {
		return agentctx.PRContext{}, fmt.Errorf("diff exceeds %d bytes", MaxDiffBytes)
	}
	opts := agentctx.PROptions{Diff: []byte(in.Diff), Files: in.Files, Base: in.Base, Head: in.Head, Depth: in.Depth, MaxTokens: in.MaxTokens, BaseGraph: base, BaseLabel: baseLabel}
	if len(opts.Diff) == 0 && len(opts.Files) == 0 && in.Base != "" {
		n, err := r.Node(ctx, model.NodeID(repo, model.KindRepository, repo))
		if err != nil {
			return agentctx.PRContext{}, err
		}
		if n.Root == "" {
			return agentctx.PRContext{}, errors.New("repository root unknown; pass a diff or files instead")
		}
		if _, err := os.Stat(n.Root); err != nil {
			return agentctx.PRContext{}, errors.New("repository checkout is not reachable from this server; pass a diff or files instead")
		}
		opts.Root = n.Root
	}
	out, err := agentctx.Service{R: r}.PRContext(ctx, repo, opts)
	return compact(repo, out, err)
}

// MinimalContextIn is minimal_context's input.
type MinimalContextIn struct {
	RepoArg
	Task      string   `json:"task" jsonschema:"what you are trying to do, in plain words; include symbol names you know"`
	Files     []string `json:"files,omitempty" jsonschema:"files already known to be relevant (boosted)"`
	MaxTokens int      `json:"max_tokens,omitempty" jsonschema:"estimated JSON token budget, bytes/4; 256-20000, default 1500"`
}

// MinimalContext answers minimal_context.
func MinimalContext(ctx context.Context, r store.Reader, repo string, in MinimalContextIn) (agentctx.MinimalContext, error) {
	if strings.TrimSpace(in.Task) == "" {
		return agentctx.MinimalContext{}, errors.New("task is required")
	}
	out, err := agentctx.Service{R: r}.MinimalContext(ctx, repo, in.Task, in.Files, in.MaxTokens)
	return compact(repo, out, err)
}

// SearchIn is search's input.
type SearchIn struct {
	RepoArg
	Query string   `json:"query" jsonschema:"case-insensitive name substring"`
	Kinds []string `json:"kinds,omitempty" jsonschema:"filter by node kind: Function, Method, InterfaceMethod, Type, Package, File, Document, Section, Config, ConfigEntry, Template, TemplateDef, Finding, Vulnerability, Module"`
	Limit int      `json:"limit,omitempty" jsonschema:"max results, default 20"`
}

// RefsOut is a list of nodes.
type RefsOut struct {
	Results []query.Ref `json:"results"`
}

// Search answers search.
func Search(ctx context.Context, r store.Reader, repo string, in SearchIn) (RefsOut, error) {
	if strings.TrimSpace(in.Query) == "" {
		return RefsOut{}, errors.New("query is required")
	}
	kinds := make([]model.NodeKind, 0, len(in.Kinds))
	for _, k := range in.Kinds {
		nk := model.NodeKind(k)
		if !nk.Valid() {
			return RefsOut{}, fmt.Errorf("unknown kind %q", k)
		}
		kinds = append(kinds, nk)
	}
	ns, err := r.Search(ctx, repo, in.Query, kinds, in.Limit)
	if err != nil {
		return RefsOut{}, err
	}
	out := RefsOut{Results: make([]query.Ref, 0, len(ns))}
	for _, n := range ns {
		out.Results = append(out.Results, query.RefOf(n))
	}
	return compact(repo, out, nil)
}

// FileIn names a file.
type FileIn struct {
	RepoArg
	File string `json:"file" jsonschema:"repo-relative path, e.g. internal/store/store.go"`
}

// OutlineOut is file_outline's answer.
type OutlineOut struct {
	File     string               `json:"file"`
	Analysis *model.Analysis      `json:"analysis,omitempty"`
	Entries  []query.OutlineEntry `json:"entries"`
}

// FileOutline answers file_outline.
func FileOutline(ctx context.Context, r store.Reader, repo string, in FileIn) (OutlineOut, error) {
	q := query.Service{R: r}
	entries, err := q.Outline(ctx, repo, in.File)
	if err != nil {
		return OutlineOut{}, err
	}
	if entries == nil {
		entries = []query.OutlineEntry{}
	}
	analysis, err := q.FileAnalysis(ctx, repo, in.File)
	if err != nil {
		return OutlineOut{}, err
	}
	return compact(repo, OutlineOut{File: in.File, Analysis: analysis, Entries: entries}, nil)
}
