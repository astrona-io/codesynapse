// Mirrors the Go API JSON (internal/model, internal/query, internal/api).
export interface GraphNodeData {
  id: string
  kind: string
  repo: string
  name: string
  qualified_name?: string
  file?: string
  start_line?: number
  end_line?: number
  signature?: string
  summary?: string
  exported?: boolean
  is_test?: boolean
  complexity?: number
  loc?: number
  security_tags?: string[]
  time?: string
  churn?: number
  bytes?: number
  severity?: string
  head?: string
}
export interface NodeRef { id: string; kind: string; name: string; location?: string; summary?: string; depth?: number }
export interface Ranked { node: GraphNodeData; score: number }
export interface Stats {
  repo: string
  nodes: Record<string, number>
  edges: Record<string, number>
  exported: number
  documented: number
  tests: number
  security_tags: Record<string, number>
  total_loc: number
  avg_complexity: number
  formats?: Record<string, number>
  k8s_resources?: number
  helm_charts?: number
}
export interface Hotspot extends NodeRef { complexity: number; file_churn: number; fan_in: number; score: number; security_tags?: string[] }
export interface Dashboard {
  repo: NodeRef
  head?: string
  ingested_at?: string
  stats: Stats
  hotspots: Hotspot[]
  most_called: Ranked[]
  churn_files: Ranked[]
  largest_files: Ranked[]
  undocumented: Ranked[]
  security: Ranked[]
  recent_commits: Ranked[]
  authors: Ranked[]
}
export interface RepoInfo { name: string; head?: string; ingested_at?: string; root?: string; branch?: string; remote?: string; managed?: boolean }
export interface Source { name: string; is_git: boolean; is_go: boolean; content?: Record<string, number>; truncated?: boolean }
export interface Job { id: string; repo: string; kind?: string; branch?: string; task?: string; result?: string; state: 'running' | 'succeeded' | 'failed'; started_at: string; finished_at?: string; nodes?: number; edges?: number; error?: string }
export interface SymbolCard { symbol: GraphNodeData; callers: NodeRef[]; callees: NodeRef[]; tests: NodeRef[]; implementations?: NodeRef[]; docs?: NodeRef[] }
export interface ViewNode extends NodeRef { complexity?: number; security_tags?: string[]; is_test?: boolean }
export interface ViewEdge { from: string; to: string; kind: string; count?: number }
export interface GraphView { nodes: ViewNode[]; edges: ViewEdge[]; root?: string }
export interface ChangedSymbol extends NodeRef { signature?: string; risk: number; level: string; reasons: string[]; security_tags?: string[]; fan_in: number; impacted: number; tests: NodeRef[] }
export interface Report {
  repo: string
  summary: { changed_files: number; changed_symbols: number; impacted_symbols: number; untested_changes: number; security_sensitive: number; max_level: string }
  changes: ChangedSymbol[]
  impacted: NodeRef[]
  files: { path: string; status: string; symbols: number; indexed: boolean }[]
  docs?: NodeRef[]
  checklist?: string[]
}

export interface RuleGuide {
  rule: string; title: string; severity: string; cwe: string[]; why: string; fix: string
  examples: { language: string; vulnerable: string; fixed: string }[]
}
export interface SecFinding {
  id: string; rule: string; title: string; severity: string; cwe?: string[]; location: string; detail: string; language?: string
  owner?: NodeRef; exposed: boolean; tested: boolean; priority: number
}
export interface SecVuln {
  id: string; cve?: string[]; aliases?: string[]; summary: string; severity: string; cwe?: string[]
  module: string; version: string; fixed?: string; reachability: 'called' | 'imported' | 'required'; url: string
  call_sites?: NodeRef[]; priority: number
}
export interface SecurityReport {
  repo: string; score: number; grade: string
  finding_counts: Record<string, number>; vuln_counts: Record<string, number>
  findings: SecFinding[]; vulnerabilities: SecVuln[]
  modules: { total: number; direct: number; indirect: number; vulnerable: number; stdlib?: string }
  rules: { rule: string; title: string; severity: string; count: number }[]
  sensitive_functions?: NodeRef[]
}
export interface Distribution { count: number; min: number; max: number; mean: number; p50: number; p90: number; p99: number; buckets: { label: string; count: number }[] }
export interface MetricsReport {
  generated_at: string
  code: { packages: number; files: number; loc: number; test_loc: number; functions: number; methods: number; types: number; interfaces: number; exported: number; call_edges: number; avg_complexity: number; complex_over_10: number; complex_over_20: number; security_symbols: number }
  tests: { test_functions: number; test_to_code_ratio: number; reachable: number; total: number; reachable_pct: number; exported_untested: number; untested_complex: NodeRef[] }
  docs: { exported: number; documented: number; coverage_pct: number; documents: number; sections: number; mentions: number; symbols_mentioned: number }
  distributions: Record<string, Distribution>
  packages: { name: string; files: number; loc: number; functions: number; afferent: number; efferent: number; instability: number; avg_complexity: number; doc_pct: number; tested_pct: number }[]
  dead_code: NodeRef[]
  benchmark: { method: string; samples: number; graph_tokens: Distribution; baseline_tokens: Distribution; median_savings_x: number; median_reduction_pct: number; total_reduction_pct: number; total_graph_tokens: number; total_baseline_tokens: number; examples: { symbol: string; graph_tokens: number; baseline_tokens: number; files: number; savings_x: number; reduction_pct: number }[] }
}

export interface RepoSummary {
  name: string; branch?: string; head?: string; remote?: string; managed: boolean; ingested_at?: string
  age_hours: number; stale: boolean; nodes: number; edges: number; loc: number; files: number; docs: number
  configs: number; templates: number; modules: number; grade: string; score: number
  findings: Record<string, number>; vulns: Record<string, number>; vulns_called: number
  test_reach_pct: number; doc_coverage_pct: number; avg_complexity: number; reduction_pct: number
  dead_functions: number; dead_loc: number; unused_modules: number
  formats?: Record<string, number>; k8s_resources?: number; helm_charts?: number
  schedule?: { state: 'manual' | 'paused' | 'active'; interval_minutes: number; next_run?: string }
}
export interface WorkspaceOverview {
  generated_at: string
  totals: {
    repos: number; nodes: number; edges: number; loc: number
    node_kinds: Record<string, number>; edge_kinds: Record<string, number>
    findings: Record<string, number>; vulns: Record<string, number>; vulns_called: number
    grades: Record<string, number>; stale: number; reduction_pct: number; tokens_saved: number
    security_tags: Record<string, number>; vulnerable_modules: number; dead_loc: number; unused_modules: number
  }
  repos: RepoSummary[]
  jobs: { total: number; running: number; succeeded: number; failed: number; avg_duration_sec: number; max_duration_sec: number; recent_failures: Job[] }
}

export interface CleanupItem extends NodeRef { category: string; confidence: string; loc: number; security?: string[]; reason: string }
export interface CleanupReport {
  summary: { functions: number; live: number; test_only: number; unreachable: number; dead_loc: number; dead_pct: number; security_relevant: number; unused_modules: number; cves_removable: number; dead_files: number; unused_values: number; unused_template_defines: number; test_support_packages: number }
  items: CleanupItem[]
  files: { file: string; symbols: number; loc: number; test_only: boolean }[]
  modules: { path: string; version: string; vulnerabilities?: string[] }[]
  unused_values: NodeRef[]
  unused_template_defines: NodeRef[]
  test_support_packages: string[]
}

export interface ReviewComment {
  id: string; file: string; line?: number; end_line?: number; severity: string; category: string; message: string
  suggestion?: string; rule?: string; symbol?: string; symbol_id?: string; status: 'open' | 'accepted' | 'rejected' | 'deferred' | 'resolved' | 'wontfix'
  status_note?: string; status_by?: string; updated_at?: string
  fix?: { state: 'queued' | 'running' | 'pushed' | 'failed'; job?: string; branch?: string; commit?: string; pr_url?: string; compare_url?: string; model?: string; error?: string; at: string }
}
export interface ReviewCounts { total: number; open: number; accepted?: number; rejected?: number; deferred?: number; resolved: number; wontfix: number; by_severity: Record<string, number> }
export interface ReviewSummary { id: string; kind?: string; repo: string; title: string; verdict: string; author: string; source: string; commit?: string; created_at: string; updated_at: string; counts: ReviewCounts }
export interface Review extends Omit<ReviewSummary, 'counts'> { summary: string; base?: string; head?: string; comments: ReviewComment[] }

export interface AIProvider {
  name: string; kind: string; model: string; endpoint: string; local: boolean; has_key: boolean; key_env?: string
  sends_source: boolean; max_output_tokens: number; max_input_tokens: number; context_window?: number; tasks: string[]
  usage: { requests: number; failures: number; input_tokens: number; output_tokens: number; avg_seconds: number; last_error?: string }
}
