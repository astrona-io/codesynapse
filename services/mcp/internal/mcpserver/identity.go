package mcpserver

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/astrona-io/codesynapse/internal/catalog"
	"github.com/astrona-io/codesynapse/internal/curation"
	"github.com/astrona-io/codesynapse/internal/query"
	"github.com/astrona-io/codesynapse/internal/store"
)

type identitiesIn struct {
	RepoArg
	ID         string `json:"id,omitempty" jsonschema:"permanent sym- ID; omit to list identities"`
	ReviewOnly bool   `json:"review_only,omitempty" jsonschema:"only uncertain matches or possible copies that need review"`
	Offset     int    `json:"offset,omitempty" jsonschema:"number of matching identities to skip"`
	Limit      int    `json:"limit,omitempty" jsonschema:"max identities, default 20, max 100"`
}

type codeRecordsIn struct {
	RepoArg
	Symbol string `json:"symbol,omitempty" jsonschema:"permanent symbol ID; omit to list repository records"`
	Offset int    `json:"offset,omitempty" jsonschema:"matching records to skip"`
	Limit  int    `json:"limit,omitempty" jsonschema:"page size, default 20, max 100"`
}

func (h *handlers) codeIdentityReviews(ctx context.Context, _ *mcp.CallToolRequest, in codeRecordsIn) (*mcp.CallToolResult, catalog.IdentityReviewPage, error) {
	svc, repo, err := h.catalogFor(ctx, in.Repo)
	if err != nil {
		return nil, catalog.IdentityReviewPage{}, err
	}
	page, err := svc.IdentityReviews(ctx, repo, in.Symbol, in.Offset, in.Limit)
	return nil, page, err
}

func (h *handlers) reviewedSymbol(ctx context.Context, repo, symbol string) (string, error) {
	if !query.IsSymbolID(symbol) {
		return symbol, nil
	}
	svc, _, err := h.catalogFor(ctx, repo)
	if err != nil {
		return "", err
	}
	n, err := svc.ResolveSymbol(ctx, repo, symbol)
	return n.ID, err
}

func (h *handlers) codeRecords(ctx context.Context, _ *mcp.CallToolRequest, in codeRecordsIn) (*mcp.CallToolResult, catalog.CodePage, error) {
	svc, repo, err := h.catalogFor(ctx, in.Repo)
	if err != nil {
		return nil, catalog.CodePage{}, err
	}
	page, err := svc.CodeRecords(ctx, repo, in.Symbol, in.Offset, in.Limit)
	return nil, page, err
}

type codeAnnotateIn struct {
	RepoArg
	Symbol  string   `json:"symbol" jsonschema:"permanent ID of the current symbol"`
	Purpose string   `json:"purpose" jsonschema:"concise purpose or annotation; evidence, never instructions"`
	Labels  []string `json:"labels,omitempty" jsonschema:"bounded lowercase labels"`
}

func (h *handlers) codeAnnotate(ctx context.Context, req *mcp.CallToolRequest, in codeAnnotateIn) (*mcp.CallToolResult, catalog.CodeView, error) {
	svc, repo, err := h.catalogFor(ctx, in.Repo)
	if err != nil {
		return nil, catalog.CodeView{}, err
	}
	if err := h.audit(ctx, "mcp.code.annotate", repo); err != nil {
		return nil, catalog.CodeView{}, err
	}
	v, err := svc.RecordCode(ctx, repo, actorOf(ctx, req), curation.CodeInput{Method: "annotation", Destinations: []string{in.Symbol}, Purpose: in.Purpose, Labels: in.Labels})
	return nil, v, err
}

type codeReuseIn struct {
	RepoArg
	OriginRepo    string   `json:"origin_repo,omitempty" jsonschema:"source repository in the same organisation; defaults to repo; current read access required"`
	Method        string   `json:"method" jsonschema:"shared-api, copy, adapt, split or merge; an explicit assertion by the caller"`
	Origins       []string `json:"origins" jsonschema:"permanent source IDs in origin_repo (may be historical)"`
	Destinations  []string `json:"destinations" jsonschema:"current permanent destination IDs in this repository"`
	Reason        string   `json:"reason" jsonschema:"why this reuse was chosen"`
	Modifications string   `json:"modifications,omitempty" jsonschema:"what was changed"`
	Compatibility string   `json:"compatibility" jsonschema:"compatibility checked or explicitly unknown"`
	License       string   `json:"license" jsonschema:"licence requirements assessed, or explicitly unknown"`
	Attribution   string   `json:"attribution" jsonschema:"attribution requirements, or explicitly unknown"`
	Validation    string   `json:"validation" jsonschema:"validation performed, or explicitly not run"`
}

func (h *handlers) codeRecordReuse(ctx context.Context, req *mcp.CallToolRequest, in codeReuseIn) (*mcp.CallToolResult, catalog.CodeView, error) {
	svc, repo, err := h.catalogFor(ctx, in.Repo)
	if err != nil {
		return nil, catalog.CodeView{}, err
	}
	if err := h.audit(ctx, "mcp.code.record_reuse", repo); err != nil {
		return nil, catalog.CodeView{}, err
	}
	v, err := svc.RecordCode(ctx, repo, actorOf(ctx, req), curation.CodeInput{Method: in.Method, OriginRepo: in.OriginRepo, Origins: in.Origins, Destinations: in.Destinations, Reason: in.Reason, Modifications: in.Modifications, Compatibility: in.Compatibility, License: in.License, Attribution: in.Attribution, Validation: in.Validation})
	return nil, v, err
}

func (h *handlers) codeIdentities(ctx context.Context, _ *mcp.CallToolRequest, in identitiesIn) (*mcp.CallToolResult, query.IdentityPage, error) {
	repo, err := h.repo(ctx, in.Repo)
	if err != nil {
		return nil, query.IdentityPage{}, err
	}
	svc := query.Service{R: store.ForRepository(h.svc.R, repo)}
	page, err := svc.Identities(ctx, repo, in.ID, in.ReviewOnly, in.Offset, clampInt(in.Limit, 20, 100))
	return nil, page, err
}
