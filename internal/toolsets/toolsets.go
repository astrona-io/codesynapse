// Package toolsets groups the MCP tools (docs/PLAN.md, agent workflow
// roadmap step 2). The MCP server serves a selection; `codesynapse mcp
// install --profile` writes one into client configs.
package toolsets

import (
	"fmt"
	"slices"
	"strings"
)

// Sets maps each toolset to its tools. Every tool belongs to exactly one
// set, and a connection can ask for only the sets it uses, so an agent's
// turns do not carry definitions it never calls. The core set is always
// served; without a selection a connection gets every tool.
var Sets = map[string][]string{
	Core:       {"minimal_context", "get_symbol_context", "get_call_graph", "get_implementations", "file_outline", "search", "get_pr_context", "repo_status", "verify_plan", "change_checklist", "handoff_context", "ask_local"},
	"repos":    {"repo_list", "search_all_repos", "repo_overview", "repo_metrics", "repo_trend", "hotspots", "file_history", "ingestion_report"},
	"docs":     {"get_config_entries", "get_doc_section"},
	"security": {"security_report", "security_symbols", "attack_surface", "rule_guide", "license_report"},
	"architecture": {"architecture_search", "architecture_get", "architecture_context", "architecture_issues",
		"architecture_proposals", "architecture_propose", "architecture_link"},
	"memory":   {"search_graph_nodes", "query_subgraph", "recall_context", "upsert_graph_triples", "offload_turns"},
	"cleanup":  {"unused_code", "duplicate_code", "cleanup_plan", "refactor_duplicates", "shared_code"},
	"reviews":  {"list_reviews", "get_review", "submit_review", "update_review_comment"},
	"git":      {"git_status", "git_diff_summary", "git_blame", "git_log", "gh_pr_status", "gh_pr_list", "gh_run_failure"},
	"identity": {"code_identities", "code_identity_reviews", "code_records", "code_annotate", "code_record_reuse"},
}

// Core is always served.
const Core = "core"

// Known reports whether tool is in a set.
func Known(tool string) bool {
	for _, tools := range Sets {
		if slices.Contains(tools, tool) {
			return true
		}
	}
	return false
}

// Names are the selectable sets, sorted.
func Names() []string {
	names := make([]string, 0, len(Sets))
	for name := range Sets {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

// Parse reads a comma-separated selection ("memory,reviews"). Empty
// or "all" selects every set and returns nil; otherwise core is added and
// the result is sorted, so equal selections compare equal.
func Parse(spec string) ([]string, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" || spec == "all" {
		return nil, nil
	}
	sel := []string{Core}
	for name := range strings.SplitSeq(spec, ",") {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if name == "all" {
			return nil, nil
		}
		if _, ok := Sets[name]; !ok {
			return nil, fmt.Errorf("unknown toolset %q (choose from %s, or all)", name, strings.Join(Names(), ", "))
		}
		if !slices.Contains(sel, name) {
			sel = append(sel, name)
		}
	}
	slices.Sort(sel)
	return sel, nil
}
