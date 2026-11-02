package mcpserver

import (
	"testing"

	"github.com/astrona-io/codesynapse/internal/accounts"
	"github.com/astrona-io/codesynapse/internal/testutil/assert"
)

// roleTools is every tool's policy from before tools checked permissions,
// with the least role it needed then. It is frozen: a tool added later is
// checked by TestEveryToolHasPolicy only.
var roleTools = map[string]struct {
	least  accounts.Role
	target string
}{
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
	"local_draft":            {accounts.Viewer, "list"},
	"read_ranges":            {accounts.Viewer, "list"},
	"fetch_source":           {accounts.Viewer, "list"},
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

// TestBuiltInRolesKeepAccess proves that checking permissions changed no
// one's access to a tool: each built-in repository role holds a tool's
// permission exactly when it was at least the role the tool needed, and
// every tool still resolves its repository the same way.
func TestBuiltInRolesKeepAccess(t *testing.T) {
	roles := []accounts.Role{accounts.None, accounts.Guest, accounts.Viewer, accounts.Member, accounts.Admin, accounts.Owner}
	for name, old := range roleTools {
		pol, ok := toolPolicies[name]
		if !assert.True(t, ok, "tool gone: "+name) {
			continue
		}
		target := old.target
		if name == "update_review_comment" {
			target = "comment" // the same review, named by review_id
		}
		assert.Equal(t, pol.target, target, name)
		for _, r := range roles {
			assert.Equal(t, r.Has(pol.perm), r.AtLeast(old.least), name+" as "+string(r)+" ("+string(pol.perm)+")")
		}
	}
}

// TestEveryToolHasAPermission keeps every tool tied to a known repository
// permission; list and catalog tools only ever read.
func TestEveryToolHasAPermission(t *testing.T) {
	for name, pol := range toolPolicies {
		assert.True(t, pol.perm.Valid() && pol.perm.Resource() == accounts.ResourceRepository, name+": "+string(pol.perm))
		if pol.target == "list" || pol.target == "catalog" {
			assert.Equal(t, pol.perm.Level(), accounts.LevelRead, name)
		}
	}
}
