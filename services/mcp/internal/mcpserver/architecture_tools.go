package mcpserver

import (
	"context"
	"errors"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/parisnakitakejser/codesynapse/internal/catalog"
	"github.com/parisnakitakejser/codesynapse/internal/store"
)

// The architecture catalog tools read the same internal/catalog service as the
// web API, on a reader fenced to the repository the caller may use. They only
// read; accepted, proposed and superseded records are labelled, and only an
// accepted ADR or active standard or policy is reported as binding.

func (h *handlers) catalogFor(ctx context.Context, repoArg string) (catalog.Service, string, error) {
	repo, err := h.repo(ctx, repoArg)
	if err != nil {
		return catalog.Service{}, "", err
	}
	return catalog.Service{R: store.ForRepository(h.svc.R, repo)}, repo, nil
}

type architectureSearchIn struct {
	RepoArg
	Query       string `json:"query,omitempty" jsonschema:"words to find in id, title, summary, topic, components and labels"`
	Type        string `json:"type,omitempty" jsonschema:"adr, rfc, standard, policy, overview, runbook or diagram"`
	Status      string `json:"status,omitempty" jsonschema:"a status of that type, for example accepted, proposed, superseded, active"`
	Component   string `json:"component,omitempty" jsonschema:"only records for this component"`
	Label       string `json:"label,omitempty" jsonschema:"only records with this label"`
	BindingOnly bool   `json:"binding_only,omitempty" jsonschema:"only the rules in force: accepted ADRs and active standards and policies that nothing replaced"`
	Limit       int    `json:"limit,omitempty" jsonschema:"page size, default 20, max 100"`
	Offset      int    `json:"offset,omitempty" jsonschema:"entries to skip"`
}

func (h *handlers) architectureSearch(ctx context.Context, _ *mcp.CallToolRequest, in architectureSearchIn) (*mcp.CallToolResult, catalog.Page, error) {
	svc, repo, err := h.catalogFor(ctx, in.Repo)
	if err != nil {
		return nil, catalog.Page{}, err
	}
	page, err := svc.List(ctx, repo, catalog.Filter{Type: in.Type, Status: in.Status, Component: in.Component, Label: in.Label, Q: in.Query,
		BindingOnly: in.BindingOnly, Limit: clampInt(in.Limit, 20, 100), Offset: in.Offset})
	return nil, page, err
}

type architectureGetIn struct {
	RepoArg
	ID string `json:"id" jsonschema:"the record ID, for example ADR-0007, from architecture_search"`
}

func (h *handlers) architectureGet(ctx context.Context, _ *mcp.CallToolRequest, in architectureGetIn) (*mcp.CallToolResult, catalog.Detail, error) {
	if in.ID == "" || len(in.ID) > 200 {
		return nil, catalog.Detail{}, errors.New("id is required")
	}
	svc, repo, err := h.catalogFor(ctx, in.Repo)
	if err != nil {
		return nil, catalog.Detail{}, err
	}
	d, err := svc.Get(ctx, repo, in.ID)
	return nil, d, err
}

type architectureContextIn struct {
	RepoArg
	Task       string   `json:"task,omitempty" jsonschema:"the work in a sentence"`
	Files      []string `json:"files,omitempty" jsonschema:"repo-relative files the work touches"`
	Symbols    []string `json:"symbols,omitempty" jsonschema:"symbols the work touches (qualified_id or name)"`
	Components []string `json:"components,omitempty" jsonschema:"components the work is in"`
	Limit      int      `json:"limit,omitempty" jsonschema:"max records, default 10, max 50"`
}

func (h *handlers) architectureContext(ctx context.Context, _ *mcp.CallToolRequest, in architectureContextIn) (*mcp.CallToolResult, catalog.ContextResult, error) {
	svc, repo, err := h.catalogFor(ctx, in.Repo)
	if err != nil {
		return nil, catalog.ContextResult{}, err
	}
	res, err := svc.Context(ctx, repo, catalog.Task{Query: in.Task, Files: in.Files, Symbols: in.Symbols, Components: in.Components, Limit: in.Limit})
	return nil, res, err
}

type architectureIssuesOut struct {
	Issues []catalog.Issue `json:"issues"`
	Source catalog.Source  `json:"source"`
}

func (h *handlers) architectureIssues(ctx context.Context, _ *mcp.CallToolRequest, in RepoArg) (*mcp.CallToolResult, architectureIssuesOut, error) {
	svc, repo, err := h.catalogFor(ctx, in.Repo)
	if err != nil {
		return nil, architectureIssuesOut{}, err
	}
	issues, src, err := svc.Issues(ctx, repo)
	return nil, architectureIssuesOut{Issues: issues, Source: src}, err
}

func registerArchitectureTools(s *mcp.Server, h *handlers, add func(name, desc string) *mcp.Tool) {
	mcp.AddTool(s, add("architecture_search",
		"Search the architecture catalog of a repository: ADRs, RFCs, standards, policies, overviews, runbooks and diagrams kept as files. Filter by type, status, component, label or words; binding_only returns just the rules in force (accepted ADRs, active standards and policies, not superseded). Each entry says whether it binds; a proposal, draft, rejected or superseded record never does. Answers carry the indexed commit."),
		h.architectureSearch)
	mcp.AddTool(s, add("architecture_get",
		"One architecture record: status and its lifecycle, the chain of records it supersedes or is replaced by, the code that carries it out (declared and as resolved), code its text mentions, related records, problems found in it (unresolved references, supersession mismatches, possible conflicts), its sections with line ranges to read, recent commits and the revision it was indexed at. The text is in the file: read the listed lines."),
		h.architectureGet)
	mcp.AddTool(s, add("architecture_context",
		"The architecture that applies to a task: pass files, symbols, components and/or the task in words. Returns the rules in force (accepted decisions, active standards) apart from background (proposals, drafts, overviews, runbooks, superseded records), why each applies, and problems such as records that may disagree, which you must report and not resolve yourself. Call it before designing or changing code."),
		h.architectureContext)
	mcp.AddTool(s, add("architecture_issues",
		"What a person should look at in a repository's architecture records: references that match nothing, records replaced but not marked superseded, supersession cycles, a replacement that is not in force, rules in force that may conflict on the same topic and component, unknown statuses."),
		h.architectureIssues)
}
