package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/astrona-io/codesynapse/internal/accounts"
	"github.com/astrona-io/codesynapse/internal/telemetry"
)

type toolPolicy struct {
	// perm is the repository permission the caller's role must hold on the
	// target repository. list and catalog tools have no single repository:
	// they show only what the caller can read, so theirs is a read level.
	perm accounts.Permission
	// target is repository, reuse, review, comment, list, or catalog. Reuse
	// also checks an optional source; review IDs resolve from storage (the
	// "id" argument for review, "review_id" for comment).
	target string
}

var toolPolicies = map[string]toolPolicy{
	"ingestion_report":       {accounts.RepoCodeRead, "repository"},
	"repo_list":              {accounts.RepoCodeRead, "list"},
	"search_all_repos":       {accounts.RepoCodeRead, "list"},
	"rule_guide":             {accounts.RepoSecurityRead, "catalog"},
	"repo_overview":          {accounts.RepoCodeRead, "repository"},
	"search":                 {accounts.RepoCodeRead, "repository"},
	"get_symbol_context":     {accounts.RepoCodeRead, "repository"},
	"code_identities":        {accounts.RepoArchitectureRead, "repository"},
	"code_identity_reviews":  {accounts.RepoArchitectureRead, "repository"},
	"code_records":           {accounts.RepoArchitectureRead, "repository"},
	"code_annotate":          {accounts.RepoArchitectureWrite, "repository"},
	"code_record_reuse":      {accounts.RepoArchitectureWrite, "reuse"},
	"get_call_graph":         {accounts.RepoCodeRead, "repository"},
	"get_implementations":    {accounts.RepoCodeRead, "repository"},
	"get_pr_context":         {accounts.RepoCodeRead, "repository"},
	"verify_plan":            {accounts.RepoCodeRead, "repository"},
	"change_checklist":       {accounts.RepoCodeRead, "repository"},
	"handoff_context":        {accounts.RepoCodeRead, "repository"},
	"unused_code":            {accounts.RepoCodeRead, "repository"},
	"duplicate_code":         {accounts.RepoCodeRead, "repository"},
	"cleanup_plan":           {accounts.RepoCodeRead, "repository"},
	"architecture_search":    {accounts.RepoArchitectureRead, "repository"},
	"architecture_get":       {accounts.RepoArchitectureRead, "repository"},
	"architecture_context":   {accounts.RepoArchitectureRead, "repository"},
	"architecture_issues":    {accounts.RepoArchitectureRead, "repository"},
	"architecture_proposals": {accounts.RepoArchitectureRead, "repository"},
	"architecture_propose":   {accounts.RepoArchitectureWrite, "repository"},
	"architecture_link":      {accounts.RepoArchitectureWrite, "repository"},
	"repo_trend":             {accounts.RepoCodeRead, "repository"},
	"refactor_duplicates":    {accounts.RepoCodeRead, "repository"},
	"shared_code":            {accounts.RepoCodeRead, "list"},
	"license_report":         {accounts.RepoSecurityRead, "repository"},
	"minimal_context":        {accounts.RepoCodeRead, "repository"},
	"repo_status":            {accounts.RepoCodeRead, "repository"},
	"get_config_entries":     {accounts.RepoCodeRead, "repository"},
	"get_doc_section":        {accounts.RepoCodeRead, "repository"},
	"file_outline":           {accounts.RepoCodeRead, "repository"},
	"hotspots":               {accounts.RepoCodeRead, "repository"},
	"security_symbols":       {accounts.RepoSecurityRead, "repository"},
	"attack_surface":         {accounts.RepoSecurityRead, "repository"},
	"file_history":           {accounts.RepoCodeRead, "repository"},
	"git_status":             {accounts.RepoCodeRead, "list"},
	"git_diff_summary":       {accounts.RepoCodeRead, "list"},
	"git_blame":              {accounts.RepoCodeRead, "list"},
	"git_log":                {accounts.RepoCodeRead, "list"},
	"gh_pr_status":           {accounts.RepoCodeRead, "list"},
	"gh_pr_list":             {accounts.RepoCodeRead, "list"},
	"gh_run_failure":         {accounts.RepoCodeRead, "list"},
	"gh_run_list":            {accounts.RepoCodeRead, "list"},
	"ask_local":              {accounts.RepoCodeRead, "list"},
	"local_draft":            {accounts.RepoCodeRead, "list"},
	"read_ranges":            {accounts.RepoCodeRead, "list"},
	"fetch_source":           {accounts.RepoCodeRead, "list"},
	"security_report":        {accounts.RepoSecurityRead, "repository"},
	"repo_metrics":           {accounts.RepoCodeRead, "repository"},
	"list_reviews":           {accounts.RepoReviewsRead, "repository"},
	"get_review":             {accounts.RepoReviewsRead, "review"},
	"search_graph_nodes":     {accounts.RepoMemoryRead, "repository"},
	"query_subgraph":         {accounts.RepoMemoryRead, "repository"},
	"recall_context":         {accounts.RepoMemoryRead, "repository"},
	"submit_review":          {accounts.RepoReviewsWrite, "repository"},
	"update_review_comment":  {accounts.RepoReviewsWrite, "comment"},
	"upsert_graph_triples":   {accounts.RepoMemoryWrite, "repository"},
	"offload_turns":          {accounts.RepoMemoryWrite, "repository"},
}

type reachKey struct{}

// authorizationMiddleware resolves a fresh permission snapshot for each call.
// A resumed transport session never retains an earlier grant.
func (h *handlers) authorizationMiddleware() mcp.Middleware {
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			if method != "tools/call" {
				return next(ctx, method, req)
			}
			p, ok := req.GetParams().(*mcp.CallToolParamsRaw)
			if !ok {
				return nil, fmt.Errorf("invalid tool parameters")
			}
			policy, known := toolPolicies[p.Name]
			if !known {
				markRefused(ctx)
				if h.access != nil {
					if err := h.access.Denied(ctx, callerFrom(ctx), "unknown"); err != nil {
						return nil, fmt.Errorf("recording authorization denial: %w", err)
					}
				}
				return nil, fmt.Errorf("tool has no authorization policy")
			}
			reach, restricted, err := h.reach(ctx)
			if err == nil && restricted {
				ctx = context.WithValue(ctx, reachKey{}, reach)
				err = h.authorizeTool(ctx, policy, p.Arguments)
			}
			if err != nil {
				markRefused(ctx)
				if h.access != nil {
					if auditErr := h.access.Denied(ctx, callerFrom(ctx), p.Name); auditErr != nil {
						return nil, fmt.Errorf("recording authorization denial: %w", auditErr)
					}
				}
				return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: err.Error()}}}, nil
			}
			telemetry.AllowTools(p.Name)
			ctx = telemetry.WithTool(ctx, p.Name)
			return next(ctx, method, req)
		}
	}
}

func (h *handlers) authorizeTool(ctx context.Context, policy toolPolicy, raw json.RawMessage) error {
	if policy.target == "list" || policy.target == "catalog" {
		return nil
	}
	var args struct {
		Repo       string `json:"repo"`
		ID         string `json:"id"`
		ReviewID   string `json:"review_id"`
		OriginRepo string `json:"origin_repo"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return fmt.Errorf("invalid tool arguments")
	}
	repo := args.Repo
	if policy.target == "review" || policy.target == "comment" {
		if h.reviews == nil {
			return fmt.Errorf("review tools are disabled")
		}
		id := args.ID
		if policy.target == "comment" {
			id = args.ReviewID
		}
		r, err := h.reviews.GetReview(ctx, id)
		if err != nil {
			return fmt.Errorf("no such review")
		}
		if err := h.canSeeReview(ctx, r.Repo, r.OrgID); err != nil {
			return err
		}
		repo = r.Repo
	}
	resolved, err := h.repo(ctx, repo)
	if err != nil {
		return err
	}
	if err := h.holds(ctx, resolved, policy.perm); err != nil {
		return err
	}
	if source := strings.TrimSpace(args.OriginRepo); policy.target == "reuse" && source != "" && source != resolved {
		svc, _, err := h.catalogFor(ctx, resolved)
		if err != nil {
			return err
		}
		_, err = svc.ReadRepository(ctx, source)
		return err
	}
	return nil
}
