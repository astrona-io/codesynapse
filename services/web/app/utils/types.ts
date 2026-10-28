// Mirrors the Go API JSON (internal/model, internal/query, internal/api).
export interface GraphNodeData {
  id: string
  symbol_id?: string
  revision?: string
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
  version?: string
  ecosystem?: string
  license?: string
  indirect?: boolean
  dev?: boolean
  via?: string
  roots?: number
  code_absent?: boolean
  latest?: string
  deprecated?: string
  behind?: number
}
export interface NodeRef { id: string; kind: string; name: string; location?: string; summary?: string; depth?: number }

export interface SymbolLocation { neighbour_count?: number; neighbours_omitted?: number; neighbourhood?: string; node_id: string; name: string; file?: string; signature?: string; revision?: string; fingerprint?: string; start_line?: number; end_line?: number }
export interface SymbolIdentity { id: string; kind: string; language?: string; active: boolean; current: SymbolLocation; history: SymbolLocation[]; history_omitted: number; match: string; candidates: string[] | null; candidates_omitted: number }
export interface IdentityPage { repo: string; revision?: string; indexed_at?: string; available: boolean; symbols: SymbolIdentity[]; total: number; offset: number; omitted: number; truncated: boolean }
export interface CodeRecordInput { method: string; origin_repo?: string; origins: string[]; destinations: string[]; purpose: string; labels: string[]; reason: string; modifications: string; compatibility: string; license: string; attribution: string; validation: string }
export interface CodeRecordView { item: { id: string; client?: string; created_by?: string; created_at: string; code: CodeRecordInput & { sources: { id: string; repo?: string; revision: string; fingerprint: string }[]; targets: { id: string; repo?: string; revision: string; fingerprint: string }[] } }; freshness: string; changed: string[]; source_unavailable: boolean }
export interface CodeRecordPage { records: CodeRecordView[]; truncated: boolean; limit: number; offset: number; total: number; omitted: number; storage_truncated: boolean; sources_restricted: boolean }
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
export interface RepoInfo { name: string; head?: string; ingested_at?: string; root?: string; branch?: string; remote?: string; managed?: boolean; personal?: boolean }
// What a repository has, for its menu (GET /repos/{repo}/sections).
// One result of the site search (GET /search).
export interface SiteSearchHit { kind: string; repo: string; id?: string; name: string; location?: string; file?: string; severity?: string }
export interface SiteSearch { results: SiteSearchHit[]; searched: number; truncated: boolean }

// The dashboard's notifications (GET /notifications).
export interface AppNotification { id: string; kind: 'findings' | 'sync_failed' | 'review'; tone: 'bad' | 'warn' | 'info'; repo: string; title: string; body?: string; link: string; at: string }
export interface AppNotifications { items: AppNotification[]; seen_at?: string; unread: number }

// One of your own sign-in sessions (GET /me/sessions).
export interface SessionView { id: string; device: string; created_at: string; expires_at: string; current: boolean }

// The attack surface of a repository (GET /repos/{repo}/entry-points).
export interface Sink { tag: string; via: string; depth: number }
export interface EntryPoint { kind: 'http' | 'mcp' | 'cli'; name: string; registered: string; handler?: string; inline_handler?: boolean; guards?: string[]; checks_auth?: boolean; reaches?: Sink[] }
export interface AttackSurface { total: number; by_kind: Record<string, number>; unguarded: number; entry_points: EntryPoint[]; notes: string[] }

export interface RepoSections {
  packages: number; symbols: number; documents: number; config_files: number; k8s_resources: number; helm_charts: number
  records: number; proposals: number; dependencies: number; licences: boolean; findings: number; routes: number
  snapshots: number; reviews: number; memory_facts: number; admin: boolean
}
export interface Source { name: string; is_git: boolean; is_go: boolean; content?: Record<string, number>; truncated?: boolean }
export interface Job { id: string; repo: string; kind?: string; branch?: string; task?: string; result?: string; state: 'running' | 'succeeded' | 'failed'; started_at: string; finished_at?: string; nodes?: number; edges?: number; error?: string; waiting?: boolean; phase?: string; step?: number; steps?: number }
export interface DuplicateInfo { kind: 'exact' | 'near'; similarity: number; scope: string; copies: number; others: DuplicateMember[] }
export interface SymbolCard { resolved_from?: string; duplicate?: DuplicateInfo; symbol: GraphNodeData; callers: NodeRef[]; callees: NodeRef[]; tests: NodeRef[]; implementations?: NodeRef[]; docs?: NodeRef[] }
export interface ViewNode extends NodeRef { complexity?: number; security_tags?: string[]; is_test?: boolean }
// evidence: typed, dataflow, syntactic or inferred for CALLS/REFERENCES/IMPLEMENTS/DISPATCHES_TO; absent means unknown (indexed before it was recorded).
export interface ViewEdge { from: string; to: string; kind: string; count?: number; evidence?: 'typed' | 'dataflow' | 'syntactic' | 'inferred' }
export interface GraphView { nodes: ViewNode[]; edges: ViewEdge[]; root?: string }
export interface ChangedSymbol extends NodeRef { signature?: string; risk: number; level: string; reasons: string[]; security_tags?: string[]; fan_in: number; impacted: number; tests: NodeRef[]; duplicate?: DuplicateInfo }
export interface Report {
  repo: string
  summary: { changed_files: number; changed_symbols: number; impacted_symbols: number; untested_changes: number; security_sensitive: number; duplicated_changes?: number; max_level: string }
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
  owner?: NodeRef; code_owners?: string[]; exposed: boolean; tested: boolean; priority: number; suppression_reason?: string
}
export interface SecVuln {
  id: string; cve?: string[]; aliases?: string[]; summary: string; severity: string; cwe?: string[]
  module: string; version: string; via?: string; roots?: number; fixed?: string; reachability: 'called' | 'imported' | 'required'; url: string
  call_sites?: NodeRef[]; priority: number
}
export interface SecurityReport {
  repo: string; score: number; grade: string
  finding_counts: Record<string, number>; vuln_counts: Record<string, number>
  findings: SecFinding[]; vulnerabilities: SecVuln[]
  suppressed?: SecFinding[]; suppressed_counts?: Record<string, number>
  modules: { total: number; direct: number; indirect: number; vulnerable: number; stdlib?: string; deprecated?: number; behind?: number }
  deprecated?: { module: string; ecosystem?: string; version: string; latest?: string; reason: string; direct: boolean }[]
  rules: { rule: string; title: string; severity: string; count: number }[]
  owners?: { owner: string; findings: number; counts: Record<string, number> }[]
  sensitive_functions?: NodeRef[]
  head?: string; indexed_at?: string; checked?: string[]; not_checked?: string[]; unanalysed_findings?: number
  coverage?: { excluded_files?: number; kubernetes_excluded_files?: number; kubernetes_disabled?: boolean; inventory_incomplete?: boolean }
}
export interface ImageScan {
  uploaded_at: string; images: string[]; counts: Record<string, number>; total: number; truncated: boolean
  findings: { image: string; package: string; version: string; id: string; severity: string; fixed?: string; title?: string }[]
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
  benchmark: { policy_digest?: string; method: string; samples: number; graph_tokens: Distribution; baseline_tokens: Distribution; median_savings_x: number; median_reduction_pct: number; total_reduction_pct: number; total_graph_tokens: number; total_baseline_tokens: number; examples: { symbol: string; graph_tokens: number; baseline_tokens: number; files: number; savings_x: number; reduction_pct: number }[] }
}

export interface RepoSummary {
  name: string; branch?: string; head?: string; remote?: string; managed: boolean; personal?: boolean; ingested_at?: string
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
  modules: { path: string; ecosystem?: string; version: string; vulnerabilities?: string[] }[]
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
  sends_source: boolean; max_output_tokens: number; max_input_tokens: number; input_budget_policy?: 'estimate' | 'require_exact'; context_window?: number; tasks: string[]
  usage: { requests: number; failures: number; input_tokens: number; cached_tokens?: number; output_tokens: number; avg_seconds: number; compressed_bytes?: number; running?: number; waiting?: number; last_error?: string }
  // auto: models picked from what the server serves (model: auto); served is its last answer.
  auto?: boolean; embedding_model?: string
  prices?: { input: number; output: number; cached_input?: number; source?: string; checked?: string; feed?: string }
  served?: { models: ServedModel[]; error?: string; at: string }
}
export interface ServedModel { name: string; chat: boolean; embedding: boolean; thinking?: boolean; params_b?: number; context?: number; bytes?: number }

export type LicenseCategory = 'public-domain' | 'permissive' | 'weak-copyleft' | 'strong-copyleft' | 'network-copyleft' | 'restricted' | 'unknown'
export interface LicenseVerdict { level: 'ok' | 'review' | 'conflict'; reason: string }
export interface LicenseEntry {
  ecosystem: string; name: string; version: string; license?: string; source?: string
  category: LicenseCategory; direct: boolean; dev?: boolean; verdict: LicenseVerdict
}
export interface LicenseReport {
  project: { license: string; source: string }; category: LicenseCategory
  total: number; ok: number; review: number; conflict: number; unknown: number
  by_license: { license: string; category: LicenseCategory; count: number }[]
  issues: LicenseEntry[]; truncated?: number; online: boolean
}

export interface DependencyPackage {
  ecosystem: string; name: string; version?: string; license: string
  direct: boolean; dev?: boolean; via?: string; roots?: number; latest?: string; deprecated?: string; behind?: number
}
export interface DependencyList { packages: DependencyPackage[]; by_ecosystem: { ecosystem: string; count: number }[]; truncated?: boolean }

export interface MemoryEntity { id: string; repo: string; name: string; type: string; created_at: string; updated_at: string }
export interface MemoryFact {
  id: string; repo: string; subject: string; predicate: string; object: string
  source: string; origin_session_id?: string; confirmations: number; created_at: string; updated_at: string
}
export interface MemoryGraph { notice: string; entities: MemoryEntity[]; facts: MemoryFact[] }

export interface UsageDay { day: string; sessions: number; calls: number; errors: number; response_tokens: number; avoided_tokens: number }
export interface UsageTool { tool: string; kind: string; calls: number; errors: number; response_tokens: number; avg_tokens: number; avg_ms: number; max_ms: number; ranges_returned: number; ranges_read: number }
export interface WorkspaceStats {
  generated_at: string; days: number
  agents: {
    edits: { edits: number; writes: number; failed: number; patch_bytes: number; write_bytes: number; baseline_bytes: number; avoided_bytes: number; estimated_output_saved: number; avoided_pct: number }
    tokens: { input: number; output: number; cached_input: number; turns: number; total: number; input_pct: number; output_pct: number }
    context_total_tokens: number; context_saved_pct: number
    sessions: number; calls: number; errors: number; response_tokens: number; avoided_tokens: number; avoided_method: string
    // ask_local: tokens local models read, and its part of avoided_tokens.
    local_read_tokens?: number; local_avoided_tokens?: number; local_briefs?: number
    unindexed?: { repos: { repo: string; skipped: number }[]; skipped: number; avoided_per_session: number }
    // How sessions began, reported by the Claude Code plugin's hooks.
    workflow: { first_codesynapse: number; first_native: number; reads_turned_back: number; first_share_pct: number; files_edited: number; files_edited_found: number; edit_recall_pct: number; bash: Record<string, number> | null; bash_total: number }
    avg_latency_ms: number; max_latency_ms: number; daily: UsageDay[]; tools: UsageTool[]
    clients: { client: string; sessions: number; calls: number }[]
    repos: { repo: string; calls: number; response_tokens: number; avoided_tokens: number }[]
  }
  memory: {
    entities: number; facts: number; history?: number; confirmations: number; sources: Record<string, number>
    repos: { repo: string; entities: number; facts: number; history?: number; confirmations: number; fact_chars: number; sources: Record<string, number>; last_updated?: string }[]
    saves: number; recalls: number; recall_tokens: number
    // Recalls split by the tool that read memory (recall_context, search_graph_nodes, query_subgraph).
    recalls_by_tool?: Record<string, number>
    // What the hooks did on their own (per-prompt recall, offloading at compaction).
    auto_recalls?: number; auto_recall_tokens?: number; offloads?: number; offloaded_turns?: number; offload_facts?: number
    model: { tokens_per_fact: number; tokens_per_recall: number; recalls_per_session: number; measured: boolean }
    // The window day by day (empty days included), and the agents that used memory.
    daily?: { day: string; saves: number; recalls: number; auto_recalls: number; offloads: number; offload_facts: number }[]
    clients?: { client: string; saves: number; recalls: number }[]
  }
  licenses: {
    dependencies: number; ok: number; review: number; conflict: number; unknown: number
    unlicensed: string[]; conflicts: { repo: string; count: number }[]
    by_license: { license: string; category: LicenseCategory; count: number }[]; projects: Record<string, number>
  }
  reviews: { reviews: number; comments: number; by_status: Record<string, number>; by_kind: Record<string, number>; by_source: Record<string, number>; verdicts: Record<string, number>; fixes: Record<string, number> }
  hygiene: { no_update_automation: string[]; renovate_issues: { repo: string; count: number }[]; top_rules: { rule: string; severity: string; findings: number; repos: number }[] }
  ai: { provider: string; model: string; local: boolean; usage: { requests: number; failures: number; input_tokens: number; output_tokens: number; avg_seconds: number; last_error?: string } }[]
  workspace: WorkspaceOverview['totals']
  jobs: WorkspaceOverview['jobs']
  repos: RepoSummary[]
}

// Time series from GET /api/v1/stats/series (VictoriaMetrics).
export type SeriesRange = '1h' | '6h' | '24h' | '7d' | '30d'
export interface SeriesLine {
  name: string
  values: (number | null)[]
  total: number
}
export interface SeriesPanel {
  panel: string
  title: string
  unit: string
  kind: 'count' | 'gauge'
  help: string
  range: SeriesRange
  empty: string
  step_seconds: number
  times: number[]
  series: SeriesLine[]
}

// Accounts (GET /api/v1/auth/status, /me and the settings endpoints).
export interface AuthStatus {
  mode: 'single' | 'accounts'
  needs_setup?: boolean
  signup_open?: boolean
  // The administrator made at first start still exists.
  initial_admin?: boolean
}
export interface AccountUser {
  id: string
  email: string
  username?: string
  name: string
  platform_admin: boolean
  // The account that set up the installation: governance only.
  bootstrap_root?: boolean
  // The administrator made at first start: replace it with your own account.
  auto_created?: boolean
  created_at: string
}
export type SetupState = 'pending' | 'complete' | 'ambiguous'
export interface SetupInput { org_name?: string; org_id?: string; email: string; username?: string; name: string; password: string }
// Whether keys of the organisation may use the agent gateway, and whether the
// server switch (CODESYNAPSE_GATEWAY) is on at all.
export interface OrgGateway {
  enabled: boolean
  available: boolean
}

export interface AccountOrg {
  id: string
  slug: string
  name: string
  created_at: string
  default_repo_access: 'NONE' | 'READ_ONLY'
  policy_revision: number
}
export type Role = 'owner' | 'admin' | 'member' | 'viewer' | 'guest'
export interface Membership {
  org: AccountOrg
  user: AccountUser
  role: Role
  added_at: string
}
export interface Me {
  user: AccountUser
  via: 'session' | 'key' | 'service'
  org_id?: string
  memberships: Membership[]
  // Governance only (the bootstrap root): no organisation work.
  restricted?: boolean
  setup?: SetupState
}
export interface ApiKey {
  id: string
  org_id: string
  name: string
  prefix: string
  created_at: string
  last_used_at?: string
  expires_at?: string
  revoked_at?: string
}
export interface GitToken {
  org_id: string
  host: string
  preview: string
  updated_at: string
}
// Your own token (GET /me/git-tokens): only for repositories in your space.
export interface UserGitToken {
  host: string
  preview: string
  created_at: string
  updated_at: string
}
// One repository from POST /imports/list. private is set only when the
// listing used your own token (space "me").
export interface ImportCandidate {
  name: string
  repo: string
  url: string
  default_branch?: string
  description?: string
  archived?: boolean
  fork?: boolean
  private?: boolean
  pushed_at?: string
  added: boolean
}

// Relationship-based access (teams, repository grants and shares).
export interface TeamMember { user: AccountUser; relation: 'member' | 'maintainer' }
export interface Team { id: string; org_id: string; slug: string; name: string; role?: Role; members: TeamMember[] }
export interface RepoGrant {
  subject_type: 'user' | 'team'
  subject_id: string
  relation: 'admin' | 'editor' | 'viewer' | 'none'
  role: Role | 'none'
  label: string
  external: boolean
  created_at: string
}
export interface ReachView { org_id: string; org: string; graph: string; role: Role; via: string; shared: boolean }

// GET /api/v1/attention: what needs attention, worst first.
export type AttentionLevel = 'critical' | 'high' | 'medium' | 'low'
export interface AttentionItem {
  level: AttentionLevel
  kind: 'security' | 'vulnerability' | 'licence' | 'access' | 'sync' | 'stale' | 'automation' | 'quality' | 'regression'
  repo?: string
  count?: number
  title: string
  detail: string
  action: string
  link: string
}
// GET /api/v1/agents/status: what agents did today.
export interface AgentStatus { sessions: number; calls: number; workflow: number }
export interface Attention {
  generated_at: string
  items: AttentionItem[]
  levels: Partial<Record<AttentionLevel, number>>
  repos: number
  clean: string[]
  tokens_saved: number
  // From agents' calls over the last agent_days days (estimates).
  agent_days: number
  agent_calls: number
  tokens_returned: number
  tokens_avoided: number
  findings: number
  vulns: number
  grades: Record<string, number>
}

// Platform administration.
export interface AdminOrg extends AccountOrg { members: number; roles: Record<string, number>; repositories: number; lost_access: number; teams: number }
export interface AdminUser extends AccountUser { disabled_at?: string; orgs: { slug: string; name: string; role: Role }[]; active_keys: number; last_key_use?: string }
export interface HealthCheck { ok: boolean; detail: string; seconds: number }
export interface AdminHealth {
  version: string; checked: string
  graph: HealthCheck; accounts: HealthCheck; metrics: HealthCheck; audit: HealthCheck
  counts: { users: number; suspended: number; admins: number; orgs: number; graphs: number; audit_events_checked: number }
  jobs: { total: number; running: number; succeeded: number; failed: number }
  unowned: string[]
}
// One agent (chat) session, from the MCP audit log.
export interface AgentSession {
  id: string; actor: string; client?: string; key?: string; first: string; last: string
  calls: number; errors: number; bytes: number; duration_ms: number
  tools: Record<string, number>; repos: string[]
}
export interface AgentSessionList { sessions: AgentSession[]; days: number; truncated: boolean; scope: 'own' | 'organisation' }
export interface SessionCall {
  at: string; method: string; name?: string; repo?: string; outcome: string; error?: string
  duration_ms: number; bytes: number; args?: Record<string, string>
}
export interface AgentSessionDetail { id: string; actor: string; key?: string; calls: SessionCall[]; truncated: boolean; limit: number }
export interface AuditEvent {
  id: number; at: string; request_id?: string; user_id?: string; actor_email?: string; via?: string; key_id?: string; org_id?: string
  ip?: string; user_agent?: string; action: string; target?: string; outcome: string; status?: number; detail?: string; hash: string
  mcp?: AuditMCP
}
// An MCP request in the audit log (docs/SAAS.md, 5c).
export interface AuditMCP {
  method: string; name?: string; repo?: string; session_id?: string; transport_id?: string; client?: string
  duration_ms: number; bytes: number; args_digest?: string; args?: Record<string, string>; error?: string
}

export interface DuplicateMember { id: string; name: string; package?: string; file: string; start_line: number; end_line: number; loc: number }
export interface DuplicateGroup { hash: string; kind: 'exact' | 'near'; similarity: number; copies: number; loc: number; redundant_loc: number; scope: 'file' | 'package' | 'repository'; members: DuplicateMember[] }
export interface DuplicatesReport {
  summary: { groups: number; duplicated_functions: number; redundant_loc: number; near_groups: number; near_redundant_loc: number; function_loc: number; percent: number }
  groups: DuplicateGroup[]
  truncated?: boolean
}
export interface SharedCodeMember extends DuplicateMember { repo: string }
export interface SharedCodeGroup { hash: string; kind: 'exact' | 'near'; similarity: number; repos: string[]; copies: number; loc: number; redundant_loc: number; members: SharedCodeMember[] }
export interface SharedCodeSuggestion { kind: 'library' | 'framework'; repos: string[]; groups: number; redundant_loc: number; name: string; candidates: string[]; rationale: string }
export interface SharedCodeReport { repos: number; groups: SharedCodeGroup[]; suggestions: SharedCodeSuggestion[]; redundant_loc: number; partial?: string[] }

export interface PlanTarget { id?: string; name: string; location?: string }
export interface PlanStep {
  id: string; kind: string; title: string; why: string
  value: number; effort: 1 | 2 | 3; safety: 'high' | 'medium' | 'low'; priority: number
  lines?: number; advisories?: number; targets: PlanTarget[]; verify: string; link?: string; group?: { hash: string; kind: string }
}
export interface CleanupPlan { steps: PlanStep[]; summary: { steps: number; lines_up_to: number; advisories: number }; notes: string[] }

export interface RefactorCopy extends NodeRef { signature?: string; exported?: boolean; security_tags?: string[]; liveness?: string; callers: NodeRef[]; tests: NodeRef[] }
export interface RefactorPlan {
  hash: string; kind: string; similarity: number; scope: string; name: string; home: string; approach: string
  copies: RefactorCopy[]; steps: string[]; risks: string[]; truncated?: boolean
}

// GET /api/v1/repos/{repo}/history
export interface HistorySnapshot {
  at: string; head?: string; grade?: string; score: number
  findings?: Record<string, number>; vulns?: Record<string, number>; vulns_called: number
  modules: number; direct: number; licence_conflicts: number; licence_review: number
  functions: number; function_loc: number; dead_functions: number; dead_loc: number; unused_modules: number
  dup_loc: number; near_loc: number; dup_pct: number; test_reach_pct: number; doc_coverage_pct: number
}
export interface HistorySeries { key: string; label: string; unit?: string; lower_is_better: boolean; values: number[] }
export interface HistoryChange { key: string; label: string; from: number; to: number; detail: string }
export interface HistoryTrend {
  snapshots: HistorySnapshot[]; series: HistorySeries[]
  regressions: HistoryChange[]; improvements: HistoryChange[]; since?: string
}

// GET /api/v1/repos/{repo}/architecture
export interface ArchSource { repo: string; head?: string; indexed_at?: string; path?: string }
export interface ArchEntry {
  id: string; node_id: string; type: string; status: string; binding: boolean; title: string; summary?: string
  owner?: string; topic?: string; date?: string; components?: string[]; labels?: string[]; location: string
  supersedes?: string[]; superseded_by?: string[]; fingerprint?: string; issues: number
}
export interface ArchPage {
  entries: ArchEntry[]; total: number; limit: number; offset: number; source: ArchSource; issues: number; truncated?: boolean
  facets: { types: Record<string, number>; statuses: Record<string, number>; components: Record<string, number>; labels: Record<string, number> }
}
export interface ArchIssue { kind: string; severity: 'warning' | 'info'; message: string; records: string[] }
export interface ArchChainLink { id: string; title: string; status: string; relation: 'supersedes' | 'superseded-by'; depth: number }
export interface ArchDetail extends ArchEntry {
  raw_status?: string; format?: string; lifecycle: string[]; chain: ArchChainLink[]
  implemented_by: (NodeRef & { declared?: string; source: 'record' | 'link'; link_id?: string; by?: string; stale?: boolean; problem?: string })[]; mentions: NodeRef[]; links: ArchLink[]; related: ArchEntry[]; problems: ArchIssue[]
  outline: { name: string; lines: string; level?: number }[]; revisions: { commit: string; time?: string; subject?: string }[]
  source: ArchSource; unresolved?: string[]
}
export interface ArchLink {
  id: string; kind: 'implements' | 'relates' | 'applies-to'; record: string; target: string; note?: string
  created_at: string; created_by?: string; client?: string; resolved?: NodeRef[]; stale?: boolean; problem?: string
}
export interface ArchProposal {
  id: string; type: string; title: string; body?: string; status: string; state: 'open' | 'withdrawn' | 'merged'; binding: boolean
  topic?: string; components?: string[]; labels?: string[]; supersedes?: string[]; relates?: string[]
  implements?: { ref: string; found: boolean; names?: string[]; why?: string }[]
  suggested_id: string; suggested_path: string; created_at: string; created_by?: string; client?: string; notes: string[]
}

export interface RepositoryConfig {
  version: number
  ignore: { paths: string[] | null }
  scanners: { kubernetes: { enabled?: boolean; ignore_paths: string[] | null }; security: { ignore_rules: { rule_id: string; paths: string[] }[] | null } }
  annotation: { enabled?: boolean; provider_task: string; max_batch_size: number; ignore_paths: string[] | null; taxonomy_hints: string[] | null }
}
export interface PolicyDiagnostic { code: string; field: string; line: number }
export interface RepoPolicyPreview { digest: string; config: RepositoryConfig; diagnostics: PolicyDiagnostic[]; requires_exclusion_approval: boolean }
export interface RepoPolicy {
  indexed: { present: boolean; digest?: string; config: RepositoryConfig; diagnostics?: PolicyDiagnostic[]; exclusions_approved: boolean; excluded_files: number; kubernetes_excluded_files: number; kubernetes_disabled: boolean; inventory_incomplete?: boolean }
  approval: { org_id: string; graph: string; digest: string; config_json: string; reason: string; actor_id: string; allow_exclusions: boolean; allow_suppressions: boolean; allow_annotation: boolean; updated_at: string; expires_at?: string }
  approval_active: boolean
}
export interface RepositoryAnnotation {
  org_id: string; graph: string; symbol_id: string; commit: string; revision: string; fingerprint: string; policy_digest: string; job_id: string; origin_session_id: string; summary: string; taxonomy: string[]; confidence?: number; updated_at: string
}


export interface IngestFile {
  path: string; outcome: string; skip_reason?: string; diagnostic?: string; size_bytes: number | null
  stages?: { stage: string; outcome: string; diagnostic?: string }[]
}
export interface IngestReport {
  version: number; run_id: string; revision?: string; policy_digest?: string; extractor_version: string
  started_at: string; finished_at?: string; status: string; graph_commit: string
  total_files_scanned: number; files_indexed: number; files_written: number; files_reused: number
  files_skipped: number; files_failed: number; files_unprocessed: number; skip_breakdown: Record<string, number>
  discovery_complete: boolean; pruned_directories: number; estimated_tokens_saved_on_ingest: number | null
  estimate: { name: string; version: number; bytes_per_token: number; baseline: string; observed_metadata_bytes: number; covered_files: number; unknown_files: number; excluded_binary_files: number; partial: boolean; explanation: string }
}
export interface IngestRun {
  report: IngestReport; repository_id: string; metric_coverage: boolean; last_exported_at?: string
}
export interface IngestionOverview {
  latest_attempt: IngestRun | null; last_successful: IngestRun | null; metrics: IngestReport | null
  history: IngestRun[]; next: string; annotation_notice: string
}

// Single sign-on (docs/SSO_PLAN.md). The client secret never comes back,
// only its preview.
export interface IdentityProvider {
  org_id: string; name: string; issuer: string; client_id: string; secret_preview: string
  scopes?: string[]; groups_claim: string; default_role?: Role
  role_groups?: Record<string, Role>; team_groups?: Record<string, string>
  enabled: boolean; created_by: string; updated_at: string
}
export interface ExternalIdentity { id: string; org_id: string; issuer: string; user_id: string; email: string; linked_at: string; last_login_at?: string }

export interface IdentityReviewPage { reviews: { item: { id: string; created_at: string; identity_review: { from: string; to: string; relation: string; reason: string; revision: string; supersedes: string[] } }; state: string }[]; total: number; offset: number; omitted: number; truncated: boolean }

// GET /api/v1/stats/models: one row per model over a metrics window.
export interface ModelTaskStat { task: string; requests: number; failures: number; input_tokens: number; output_tokens: number }
export interface ModelStat {
  provider: string; model: string; local: boolean; configured: boolean; context_window?: number
  requests: number; failures: number; input_tokens: number; output_tokens: number; cached_tokens?: number; avg_prompt_tokens: number
  read_tokens_per_second: number | null; write_tokens_per_second: number | null
  context_p50: number | null; context_p95: number | null
  tasks: ModelTaskStat[]
  cost_dollars?: number | null; compressed_bytes?: number
}
// Agent gateway traffic; saved_dollars is null when a local answer reported no usage.
export interface GatewayStat {
  requests: number; local: number; cloud: number; rejected: number; failed: number; local_share_pct: number
  bytes_in: number; bytes_out: number; local_input_tokens: number; local_output_tokens: number
  local_unmetered: number; saved_dollars: number | null
}
export interface ModelStats {
  range: string; models: ModelStat[]
  // Cloud spend and the dollars prompt compression saved; null when a price is missing.
  spent_dollars?: number | null; compression_saved_dollars?: number | null
  gateway?: GatewayStat
  reference_price?: { provider: string; model: string; input_per_mtok: number; output_per_mtok: number; cached_input_per_mtok?: number; estimate?: boolean; source?: string; checked?: string }
}
