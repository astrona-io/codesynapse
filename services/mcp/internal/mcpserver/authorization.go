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
	role accounts.Role
	// target is repository, reuse, review, list, or catalog. Reuse also checks
	// an optional source; review IDs resolve from storage.
	target string
}

var toolPolicies = map[string]toolPolicy{
	"ingestion_report":       {accounts.Viewer, "repository"},
	"repo_list":              {accounts.Viewer, "list"},
	"search_all_repos":       {accounts.Viewer, "list"},
	"rule_guide":             {accounts.Viewer, "catalog"},
	"repo_overview":          {accounts.Viewer, "repository"},
	"search":                 {accounts.Viewer, "repository"},
	"get_symbol_context":     {accounts.Viewer, "repository"},
	"code_identities":        {accounts.Viewer, "repository"},
	"code_identity_reviews":  {accounts.Viewer, "repository"},
	"code_records":           {accounts.Viewer, "repository"},
	"code_annotate":          {accounts.Member, "repository"},
	"code_record_reuse":      {accounts.Member, "reuse"},
	"get_call_graph":         {accounts.Viewer, "repository"},
	"get_implementations":    {accounts.Viewer, "repository"},
	"get_pr_context":         {accounts.Viewer, "repository"},
	"verify_plan":            {accounts.Viewer, "repository"},
	"change_checklist":       {accounts.Viewer, "repository"},
	"handoff_context":        {accounts.Viewer, "repository"},
	"unused_code":            {accounts.Viewer, "repository"},
	"duplicate_code":         {accounts.Viewer, "repository"},
	"cleanup_plan":           {accounts.Viewer, "repository"},
	"architecture_search":    {accounts.Viewer, "repository"},
	"architecture_get":       {accounts.Viewer, "repository"},
	"architecture_context":   {accounts.Viewer, "repository"},
	"architecture_issues":    {accounts.Viewer, "repository"},
	"architecture_proposals": {accounts.Viewer, "repository"},
	"architecture_propose":   {accounts.Member, "repository"},
	"architecture_link":      {accounts.Member, "repository"},
	"repo_trend":             {accounts.Viewer, "repository"},
	"refactor_duplicates":    {accounts.Viewer, "repository"},
	"shared_code":            {accounts.Viewer, "list"},
	"license_report":         {accounts.Viewer, "repository"},
	"minimal_context":        {accounts.Viewer, "repository"},
	"repo_status":            {accounts.Viewer, "repository"},
	"get_config_entries":     {accounts.Viewer, "repository"},
	"get_doc_section":        {accounts.Viewer, "repository"},
	"file_outline":           {accounts.Viewer, "repository"},
	"hotspots":               {accounts.Viewer, "repository"},
	"security_symbols":       {accounts.Viewer, "repository"},
	"attack_surface":         {accounts.Viewer, "repository"},
	"file_history":           {accounts.Viewer, "repository"},
	"git_status":             {accounts.Viewer, "list"},
	"git_diff_summary":       {accounts.Viewer, "list"},
	"git_blame":              {accounts.Viewer, "list"},
	"git_log":                {accounts.Viewer, "list"},
	"gh_pr_status":           {accounts.Viewer, "list"},
	"gh_pr_list":             {accounts.Viewer, "list"},
	"gh_run_failure":         {accounts.Viewer, "list"},
	"gh_run_list":            {accounts.Viewer, "list"},
	"ask_local":              {accounts.Viewer, "list"},
	"security_report":        {accounts.Viewer, "repository"},
	"repo_metrics":           {accounts.Viewer, "repository"},
	"list_reviews":           {accounts.Viewer, "repository"},
	"get_review":             {accounts.Viewer, "review"},
	"search_graph_nodes":     {accounts.Viewer, "repository"},
	"query_subgraph":         {accounts.Viewer, "repository"},
	"recall_context":         {accounts.Viewer, "repository"},
	"submit_review":          {accounts.Member, "repository"},
	"update_review_comment":  {accounts.Member, "review"},
	"upsert_graph_triples":   {accounts.Member, "repository"},
	"offload_turns":          {accounts.Member, "repository"},
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
	if policy.target == "review" {
		if h.reviews == nil {
			return fmt.Errorf("review tools are disabled")
		}
		id := args.ID
		if policy.role == accounts.Member {
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
	if policy.role == accounts.Member {
		if err := h.canWrite(ctx, resolved); err != nil {
			return err
		}
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
