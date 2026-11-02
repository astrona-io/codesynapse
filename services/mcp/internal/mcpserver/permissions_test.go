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
	"ingestion_report":       {accounts.Read, "repository"},
	"repo_list":              {accounts.Read, "list"},
	"search_all_repos":       {accounts.Read, "list"},
	"rule_guide":             {accounts.Read, "catalog"},
	"repo_overview":          {accounts.Read, "repository"},
	"search":                 {accounts.Read, "repository"},
	"get_symbol_context":     {accounts.Read, "repository"},
	"code_identities":        {accounts.Read, "repository"},
	"code_identity_reviews":  {accounts.Read, "repository"},
	"code_records":           {accounts.Read, "repository"},
	"code_annotate":          {accounts.Write, "repository"},
	"code_record_reuse":      {accounts.Write, "reuse"},
	"get_call_graph":         {accounts.Read, "repository"},
	"get_implementations":    {accounts.Read, "repository"},
	"get_pr_context":         {accounts.Read, "repository"},
	"verify_plan":            {accounts.Read, "repository"},
	"change_checklist":       {accounts.Read, "repository"},
	"handoff_context":        {accounts.Read, "repository"},
	"unused_code":            {accounts.Read, "repository"},
	"duplicate_code":         {accounts.Read, "repository"},
	"cleanup_plan":           {accounts.Read, "repository"},
	"architecture_search":    {accounts.Read, "repository"},
	"architecture_get":       {accounts.Read, "repository"},
	"architecture_context":   {accounts.Read, "repository"},
	"architecture_issues":    {accounts.Read, "repository"},
	"architecture_proposals": {accounts.Read, "repository"},
	"architecture_propose":   {accounts.Write, "repository"},
	"architecture_link":      {accounts.Write, "repository"},
	"repo_trend":             {accounts.Read, "repository"},
	"refactor_duplicates":    {accounts.Read, "repository"},
	"shared_code":            {accounts.Read, "list"},
	"license_report":         {accounts.Read, "repository"},
	"minimal_context":        {accounts.Read, "repository"},
	"repo_status":            {accounts.Read, "repository"},
	"get_config_entries":     {accounts.Read, "repository"},
	"get_doc_section":        {accounts.Read, "repository"},
	"file_outline":           {accounts.Read, "repository"},
	"hotspots":               {accounts.Read, "repository"},
	"security_symbols":       {accounts.Read, "repository"},
	"attack_surface":         {accounts.Read, "repository"},
	"file_history":           {accounts.Read, "repository"},
	"git_status":             {accounts.Read, "list"},
	"git_diff_summary":       {accounts.Read, "list"},
	"git_blame":              {accounts.Read, "list"},
	"git_log":                {accounts.Read, "list"},
	"gh_pr_status":           {accounts.Read, "list"},
	"gh_pr_list":             {accounts.Read, "list"},
	"gh_run_failure":         {accounts.Read, "list"},
	"gh_run_list":            {accounts.Read, "list"},
	"ask_local":              {accounts.Read, "list"},
	"local_draft":            {accounts.Read, "list"},
	"read_ranges":            {accounts.Read, "list"},
	"fetch_source":           {accounts.Read, "list"},
	"security_report":        {accounts.Read, "repository"},
	"repo_metrics":           {accounts.Read, "repository"},
	"list_reviews":           {accounts.Read, "repository"},
	"get_review":             {accounts.Read, "review"},
	"search_graph_nodes":     {accounts.Read, "repository"},
	"query_subgraph":         {accounts.Read, "repository"},
	"recall_context":         {accounts.Read, "repository"},
	"submit_review":          {accounts.Write, "repository"},
	"update_review_comment":  {accounts.Write, "review"},
	"upsert_graph_triples":   {accounts.Write, "repository"},
	"offload_turns":          {accounts.Write, "repository"},
}

// TestBuiltInRolesKeepAccess proves that checking permissions changed no
// one's access to a tool: each built-in repository role holds a tool's
// permission exactly when it was at least the role the tool needed, and
// every tool still resolves its repository the same way.
func TestBuiltInRolesKeepAccess(t *testing.T) {
	roles := []accounts.Role{accounts.Deny, accounts.Guest, accounts.Read, accounts.Write, accounts.Full, accounts.Owner}
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
