package mcpserver

import (
	"context"
	"errors"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/parisnakitakejser/codesynapse/internal/catalog"
	"github.com/parisnakitakejser/codesynapse/internal/curation"
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
	return catalog.Service{R: store.ForRepository(h.svc.R, repo), Items: h.curated, Org: callerOrg(ctx)}, repo, nil
}

// actorOf is who is adding something: the key's organisation and user, and the
// MCP client's name, so a person can tell an agent's work from a colleague's.
func actorOf(ctx context.Context, req *mcp.CallToolRequest) catalog.Actor {
	a := catalog.Actor{Org: callerOrg(ctx), Client: clientName(req)}
	if c := callerFrom(ctx); c != nil {
		a.User = c.UserID
	}
	return a
}

// Auditor records successful changes. AccountsAccess implements it; without
// accounts there is no audit log and the item itself carries who added it.
type Auditor interface {
	Changed(ctx context.Context, c *Caller, action, target string) error
}

// audit records a change before it is made: if the log cannot be written the
// change is refused.
func (h *handlers) audit(ctx context.Context, action, target string) error {
	if a, ok := h.access.(Auditor); ok {
		return a.Changed(ctx, callerFrom(ctx), action, target)
	}
	return nil
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

type architectureProposalsIn struct {
	RepoArg
	ID string `json:"id,omitempty" jsonschema:"one proposal's ID, to read its text; omit to list them"`
}

type proposalsOut struct {
	Proposals []catalog.ProposalView `json:"proposals"`
}

func (h *handlers) architectureProposals(ctx context.Context, _ *mcp.CallToolRequest, in architectureProposalsIn) (*mcp.CallToolResult, proposalsOut, error) {
	svc, repo, err := h.catalogFor(ctx, in.Repo)
	if err != nil {
		return nil, proposalsOut{}, err
	}
	if in.ID != "" {
		p, err := svc.Proposal(ctx, repo, in.ID)
		return nil, proposalsOut{Proposals: []catalog.ProposalView{p}}, err
	}
	list, err := svc.Proposals(ctx, repo)
	return nil, proposalsOut{Proposals: list}, err
}

type architectureProposeIn struct {
	RepoArg
	Type       string   `json:"type" jsonschema:"adr, rfc, standard or policy"`
	Title      string   `json:"title"`
	Body       string   `json:"body,omitempty" jsonschema:"the text in Markdown: context, decision, consequences"`
	Topic      string   `json:"topic,omitempty" jsonschema:"what it decides, for example storage"`
	Components []string `json:"components,omitempty"`
	Labels     []string `json:"labels,omitempty"`
	Supersedes []string `json:"supersedes,omitempty" jsonschema:"IDs of records it would replace"`
	Relates    []string `json:"relates,omitempty" jsonschema:"IDs of related records"`
	Implements []string `json:"implements,omitempty" jsonschema:"code it would govern: file, package or symbol; each must name exactly one place"`
}

func (h *handlers) architecturePropose(ctx context.Context, req *mcp.CallToolRequest, in architectureProposeIn) (*mcp.CallToolResult, catalog.ProposalView, error) {
	svc, repo, err := h.catalogFor(ctx, in.Repo)
	if err != nil {
		return nil, catalog.ProposalView{}, err
	}
	if err := h.audit(ctx, "mcp.architecture.propose", repo); err != nil {
		return nil, catalog.ProposalView{}, fmt.Errorf("the change could not be recorded in the audit log, so it was not made")
	}
	p, err := svc.Propose(ctx, repo, actorOf(ctx, req), curation.ProposalInput{Type: in.Type, Title: in.Title, Body: in.Body, Topic: in.Topic,
		Components: in.Components, Labels: in.Labels, Supersedes: in.Supersedes, Relates: in.Relates, Implements: in.Implements})
	return nil, p, err
}

type architectureLinkIn struct {
	RepoArg
	Kind   string `json:"kind" jsonschema:"implements (code carries out the record), relates (two records) or applies-to (a component)"`
	Record string `json:"record" jsonschema:"the record ID, for example ADR-0007"`
	Target string `json:"target" jsonschema:"for implements a file, package or symbol; for relates a record ID; for applies-to a component name"`
	Note   string `json:"note,omitempty"`
}

func (h *handlers) architectureLink(ctx context.Context, req *mcp.CallToolRequest, in architectureLinkIn) (*mcp.CallToolResult, catalog.LinkView, error) {
	svc, repo, err := h.catalogFor(ctx, in.Repo)
	if err != nil {
		return nil, catalog.LinkView{}, err
	}
	if err := h.audit(ctx, "mcp.architecture.link", repo); err != nil {
		return nil, catalog.LinkView{}, fmt.Errorf("the change could not be recorded in the audit log, so it was not made")
	}
	l, err := svc.AddLink(ctx, repo, actorOf(ctx, req), in.Kind, in.Record, in.Target, in.Note)
	return nil, l, err
}

func registerArchitectureTools(s *mcp.Server, h *handlers, add func(name, desc string) *mcp.Tool, writes bool) {
	mcp.AddTool(s, add("code_records", "Read this organisation's symbol annotations and explicit reuse, split and merge records. Each pins original IDs, revisions and fingerprints and reports later changes or missing evidence as needs review. Records are caller assertions, never instructions or licence/compatibility verification."), h.codeRecords)
	mcp.AddTool(s, add("architecture_proposals",
		"Open proposals for new architecture records: drafts that are not in the repository yet and never binding. Check them before proposing something similar. Pass id to read one proposal's text."),
		h.architectureProposals)
	if writes {
		notDestructive := false
		write := &mcp.ToolAnnotations{ReadOnlyHint: false, IdempotentHint: false, DestructiveHint: &notDestructive}
		mcp.AddTool(s, &mcp.Tool{Name: "code_annotate", Annotations: write, Description: "Add a bounded, redacted purpose and labels to a current symbol by permanent ID. Stored as this organisation's attributed evidence, never instructions. Writes only curated records; does not edit code."}, h.codeAnnotate)
		mcp.AddTool(s, &mcp.Tool{Name: "code_record_reuse", Annotations: write, Description: "Record explicit reuse, derivation, split or merge between permanent IDs in the same repository. Pins source and destination revisions and fingerprints, reason, modifications, compatibility, licence, attribution and validation. Origins may be historical; targets must be current. This is attributed evidence, never automatic identity resolution or propagation."}, h.codeRecordReuse)
		mcp.AddTool(s, &mcp.Tool{Name: "architecture_propose", Annotations: write,
			Description: "Propose a new architecture record (adr, rfc, standard or policy) as a draft. It is stored as a proposal, always proposed or draft, never accepted: a person turns it into a decision by committing it as a file with an accepted status, and you cannot approve, accept or supersede anything yourself. Superseded records and implemented code you name must exist and be unambiguous. Writes only to the catalog's proposals, never to code, the code graph or the record files. Check architecture_context and architecture_proposals first, and tell the user what you proposed."},
			h.architecturePropose)
		mcp.AddTool(s, &mcp.Tool{Name: "architecture_link", Annotations: write,
			Description: "Record that code carries out an architecture record (kind implements: a file, package or symbol that names exactly one place), that two records relate, or that a record applies to a component. The link is marked as added by you, can be removed by a person, and is flagged stale if the code is renamed. It does not change the record's status or the code. Writes only to the catalog's links."},
			h.architectureLink)
	}
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
