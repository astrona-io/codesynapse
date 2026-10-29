package telemetry

import (
	"context"
	"slices"
	"sync"
)

// Default holds the metrics every service records; Push sends it.
var Default = NewRegistry()

// Client-reported lifecycle observations, recorded after durable acceptance.
// These are distinct from server-measured MCP/LLM execution instruments.
var (
	AgentHookEvents   = Default.ScopedCounter("agent_hook_events_total", "Accepted assistant lifecycle observations.", "client", "event", "category", "outcome")
	AgentHookDuration = Default.ScopedHistogram("agent_hook_duration_seconds", "Assistant-reported operation duration when available.", []float64{0.01, 0.1, 1, 10, 60, 300, 3600}, "client", "event")
	AgentHookBytes    = Default.ScopedCounter("agent_hook_bytes_total", "Bytes observed in native hook fields, without retaining content.", "client", "event", "kind")
)

var (
	latency = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120, 300}
	ttft    = []float64{0.1, 0.25, 0.5, 1, 2, 4, 8, 16, 32, 64}
	// contextShare buckets the share of a context window a request used.
	contextShare = []float64{0.01, 0.05, 0.1, 0.25, 0.5, 0.75, 0.9, 1}
)

// MCP: sessions, tool calls, latency and the size of what agents receive.
var (
	MCPSessions       = Default.ScopedCounter("mcp_sessions_total", "MCP sessions started.", "tenant_id")
	MCPToolCalls      = Default.ScopedCounter("mcp_tool_calls_total", "MCP tool and prompt calls by outcome (ok, error, refused).", "tenant_id", "mcp_tool", "outcome")
	MCPToolDuration   = Default.ScopedHistogram("mcp_tool_duration_seconds", "MCP tool execution time.", fast, "tenant_id", "mcp_tool").TenantShare(toolShare)
	MCPResponseTokens = Default.ScopedCounter("mcp_response_tokens_total", "Estimated tokens MCP tools returned to agents.", "tenant_id", "mcp_tool")
)

// LLM calls made by CodeSynapse itself (reviews, triage, briefings,
// memory extraction).
var (
	LLMRequests   = Default.ScopedCounter("llm_requests_total", "LLM requests by outcome (ok, error, timeout, rate_limited, refused).", "tenant_id", "llm_provider", "llm_model", "task", "outcome")
	LLMInput      = Default.ScopedCounter("llm_tokens_input_total", "Prompt tokens consumed.", "tenant_id", "llm_provider", "llm_model", "task")
	LLMOutput     = Default.ScopedCounter("llm_tokens_output_total", "Completion tokens generated.", "tenant_id", "llm_provider", "llm_model", "task")
	LLMSaved      = Default.ScopedCounter("llm_tokens_saved_total", "Input tokens served from the provider's prompt cache.", "tenant_id", "llm_provider", "llm_model", "task")
	LLMCost       = Default.ScopedCounter("llm_cost_estimated_dollars", "Estimated cost in US dollars from the configured token prices.", "tenant_id", "llm_provider", "llm_model")
	LLMCompressed = Default.ScopedCounter("llm_compressed_bytes_total", "Prompt bytes a local model removed before a request went to a cloud provider.", "tenant_id", "llm_provider", "llm_model")
	LLMDuration   = Default.ScopedHistogram("llm_request_duration_seconds", "LLM request time.", latency, "tenant_id", "llm_provider", "llm_model")
	LLMTTFT       = Default.ScopedHistogram("llm_time_to_first_token_seconds", "Time to the first streamed token (streaming providers only).", ttft, "tenant_id", "llm_provider", "llm_model")
	// Streamed answers split into reading the prompt (until the first token)
	// and writing the answer, with the tokens of those same answers, so
	// tokens/seconds is a model's read and write speed.
	LLMReadSeconds  = Default.ScopedCounter("llm_read_seconds_total", "Seconds streamed answers spent before their first token (reading the prompt).", "tenant_id", "llm_provider", "llm_model")
	LLMWriteSeconds = Default.ScopedCounter("llm_write_seconds_total", "Seconds streamed answers spent after their first token (writing the answer).", "tenant_id", "llm_provider", "llm_model")
	LLMReadTokens   = Default.ScopedCounter("llm_read_tokens_total", "Prompt tokens of streamed answers.", "tenant_id", "llm_provider", "llm_model")
	LLMWriteTokens  = Default.ScopedCounter("llm_write_tokens_total", "Answer tokens of streamed answers.", "tenant_id", "llm_provider", "llm_model")
	// LLMContextUsed is prompt plus answer over the configured context window.
	LLMContextUsed = Default.ScopedHistogram("llm_context_used_ratio", "Share of the model's context window one request used.", contextShare, "tenant_id", "llm_provider", "llm_model", "task")
)

// Agent gateway: model requests agents send through CodeSynapse. The model
// the agent asked for is client text, so it is never a label; the route says
// where the request was answered (local, cloud, or rejected before any model).
var (
	GatewayRequests    = Default.ScopedCounter("gateway_requests_total", "Gateway requests by route (local, cloud, rejected) and status class (2xx, 4xx, 5xx, other).", "tenant_id", "route", "class")
	GatewayBytes       = Default.ScopedCounter("gateway_bytes_total", "Gateway request and response bytes by route and direction (in, out).", "tenant_id", "route", "direction")
	GatewayLocalTokens = Default.ScopedCounter("gateway_local_tokens_total", "Tokens the local model reported for gateway answers, by kind (input, output, and cached: the part of input the runtime's prompt cache served).", "tenant_id", "kind")
	// GatewayLocalUnmetered counts local answers whose server reported no usage.
	GatewayLocalUnmetered = Default.ScopedCounter("gateway_local_unmetered_total", "Gateway answers from the local model that reported no token usage.", "tenant_id")
)

// Databases.
var (
	DBDuration     = Default.ScopedHistogram("db_query_duration_seconds", "Database round-trip time by system and operation.", latency, "tenant_id", "db_system", "operation")
	DBErrors       = Default.ScopedCounter("db_query_errors_total", "Failed database round trips.", "tenant_id", "db_system", "operation")
	VectorMatches  = Default.ScopedCounter("db_vector_matches_count", "Vectors returned by vector searches.", "tenant_id", "db_system")
	GraphTraversed = Default.ScopedCounter("db_graph_nodes_traversed", "Rows (nodes, edges, facts) returned by graph reads.", "tenant_id", "db_system")
)

// Agent memory and context offloading.
var (
	MemoryFacts      = Default.ScopedCounter("memory_facts_saved_total", "Facts written to agent memory, by source (tool, offload).", "tenant_id", "source")
	OffloadTurns     = Default.ScopedCounter("memory_offload_turns_total", "Conversation turns handed to background extraction.", "tenant_id")
	OffloadTokens    = Default.ScopedCounter("memory_offload_tokens_total", "Estimated context tokens offloaded from agent sessions.", "tenant_id")
	MemoryRecalls    = Default.ScopedCounter("memory_recall_total", "Memory recalls by mode (text, vector).", "tenant_id", "mode")
	EmbeddedEntities = Default.ScopedCounter("memory_entities_embedded_total", "Memory entities embedded for recall by meaning.", "llm_model")
)

// API and ingestion.
var (
	HTTPRequests       = Default.ScopedCounter("http_requests_total", "API requests by route pattern and status class.", "tenant_id", "route", "method", "code")
	HTTPDuration       = Default.ScopedHistogram("http_request_duration_seconds", "API request time by route pattern.", fast, "route", "method").TenantShare(routeShare)
	IngestRuns         = Default.ScopedCounter("ingest_runs_total", "Repository indexing runs by outcome.", "db_system", "outcome")
	IngestDuration     = Default.ScopedHistogram("ingest_duration_seconds", "Repository indexing time, extraction and storage.", latency, "db_system")
	IngestNodes        = Default.ScopedCounter("ingest_nodes_total", "Nodes written by indexing.", "db_system")
	AnnotationJobs     = Default.ScopedCounter("codesynapse_annotation_jobs_total", "Annotation jobs completed by outcome.", "outcome")
	AnnotationDuration = Default.ScopedHistogram("codesynapse_annotation_duration_seconds", "Annotation time from queue acceptance to completion.", latency, "outcome")
)

var (
	toolsMu sync.RWMutex
	tools   = map[string]bool{}
)

// AllowTools registers the tool and prompt names the MCP server serves;
// ToolLabel maps anything else (a client can send any name) to "unknown".
func AllowTools(names ...string) {
	toolsMu.Lock()
	defer toolsMu.Unlock()
	for _, n := range names {
		tools[n] = true
	}
}

// ToolLabel is the mcp_tool value for name.
func ToolLabel(name string) string {
	toolsMu.RLock()
	defer toolsMu.RUnlock()
	if tools[name] {
		return name
	}
	return "unknown"
}

// Coverage and savings are observations, not assumptions about missing usage.
var (
	LLMCostSaved       = Default.ScopedCounter("llm_cost_saved_estimated_dollars_total", "Estimated cached-input cost savings at the configured prices.", "tenant_id", "llm_provider", "llm_model")
	LLMUsageUnknown    = Default.ScopedCounter("llm_usage_unknown_total", "Model calls without complete reported token usage.", "tenant_id", "llm_provider", "llm_model")
	LLMPriceMissing    = Default.ScopedCounter("llm_pricing_missing_total", "Model calls without sufficient configured prices.", "tenant_id", "llm_provider", "llm_model")
	NetBytesReceived   = Default.ScopedCounter("net_bytes_received_total", "MCP application payload bytes read, excluding HTTP headers and transport overhead.", "tenant_id", "mcp_tool")
	NetBytesSent       = Default.ScopedCounter("net_bytes_sent_total", "MCP application payload bytes successfully written, including stream framing.", "tenant_id", "mcp_tool")
	CloudBytesSent     = Default.ScopedCounter("cloud_bytes_sent_total", "Cloud model request body bytes consumed by HTTP transports, including retries.", "tenant_id", "llm_provider", "llm_model")
	CloudBytesSaved    = Default.ScopedCounter("cloud_bytes_saved_total", "Estimated avoided cloud request bytes with a measured equivalent fallback baseline.", "tenant_id", "llm_provider", "llm_model")
	GraphRowsReturned  = Default.ScopedCounter("db_graph_rows_returned_total", "Graph result rows returned; not internally visited nodes.", "tenant_id", "db_system")
	DBTimeouts         = Default.ScopedCounter("db_query_timeouts_total", "Database requests that timed out.", "tenant_id", "db_system", "operation")
	AgentMemoryRecalls = Default.ScopedCounter("agent_memory_recalls_total", "Logical memory recall requests, counted once before retrieval.", "tenant_id")
	// JobsWaiting is the work a worker could claim now, per job kind (a
	// fixed set). Set by the one API process that holds the loops lease, so
	// replicas never double it; it has no tenant, so it exports as platform_.
	JobsWaiting = Default.ScopedGauge("codesynapse_jobs_waiting", "Jobs queued or interrupted that a worker could claim now, per kind.", "kind")
	// JobsActive adds the jobs a process is running: the workers the work needs.
	JobsActive                  = Default.ScopedGauge("codesynapse_jobs_active", "Jobs queued or running, per kind.", "kind")
	MemoryFactsCurrent          = Default.ScopedGauge("agent_memory_facts_count", "Current unique active durable memory facts per tenant.", "tenant_id")
	MemoryFactsObservedAt       = Default.ScopedGauge("agent_memory_facts_observed_at_seconds", "Time of the last complete durable memory count snapshot.", "tenant_id")
	MemoryFactsCollectionErrors = Default.Counter("platform_memory_facts_collection_errors_total", "Failed or incomplete durable memory count collections.")
)

// File-accounting metrics are exported from durable totals, never Default counters.
const (
	FilesScannedMetric        = "codesynapse_files_scanned_total"
	FilesIndexedMetric        = "codesynapse_files_indexed_total"
	FilesSkippedMetric        = "codesynapse_files_skipped_total"
	IngestDroppedSeriesMetric = "platform_codesynapse_ingest_metric_series_dropped_total"
)

var ingestExportRejections = Default.Counter("platform_ingest_metric_export_rejections_total", "Durable ingest metric snapshots rejected before export.", "reason")

// ---- Platform health (metrics coverage plan, phase 1) ----
//
// Process state that belongs to no tenant: plain platform_ instruments, never
// scoped, with no tenant, tool or model attribute. Service and instance come
// from the export resource, never from a label. Every label has a fixed value
// set enforced by the helpers below; other values are dropped, not echoed.
var (
	// LeaseEvents counts lease changes: the api-loops lease that runs the
	// scheduler and maintenance loops, and this process's durable job leases.
	LeaseEvents = Default.Counter("platform_codesynapse_lease_events_total", "Lease changes by lease (api_loops, job) and event (acquired, lost, renew_failed).", "lease", "event")
	// LoopsLeaseHeld is 1 while this process runs the singleton loops. The sum
	// over fresh instances is how many processes think they lead: it must be 1.
	LoopsLeaseHeld = Default.Gauge("platform_codesynapse_loops_lease_held", "1 while this process holds the api-loops lease and runs the scheduler, else 0.")
	// WorkerRoles is 1 for each job role (ingest, ai, fix) this process
	// runs and 0 for the others; summed over instances it is how many
	// processes run each role.
	WorkerRoles = Default.Gauge("platform_codesynapse_worker_roles", "1 for each job role (ingest, ai, fix) this process runs, else 0.", "role")

	// Sampled from the standard library's runtime/metrics (internal/app);
	// no second SDK accumulator and no contrib runtime package.
	GoGoroutines = Default.Gauge("platform_go_goroutines", "Live goroutines in this process.")
	GoHeapBytes  = Default.Gauge("platform_go_heap_bytes", "Heap memory occupied by live objects and objects not yet swept.")
	// GoGCPauseSeconds is estimated from the runtime's pause histogram
	// (bucket midpoints), so it is close but not exact.
	GoGCPauseSeconds = Default.Counter("platform_go_gc_pause_seconds_total", "Estimated time the process spent stopped for garbage collection.")
	// ProcessCPUSeconds is the runtime's CPU estimate for Go code and the Go
	// runtime (total minus idle). The runtime updates it at each garbage
	// collection, at least every two minutes, so it lags a little.
	ProcessCPUSeconds = Default.Counter("platform_process_cpu_seconds_total", "Estimated CPU time spent running Go code and the Go runtime.")

	// Connection pools, summed over the pools of one db_system in a process.
	DBPoolConnections = Default.Gauge("platform_db_pool_connections", "Pool connections by database and state (idle, in_use, max).", "db_system", "state")
	// The pool keeps no count of callers waiting now; these count the
	// acquires that found no free connection and how long they waited.
	DBPoolAcquireWaits       = Default.Counter("platform_db_pool_acquire_waits_total", "Connection acquires that had to wait for a free connection.", "db_system")
	DBPoolAcquireWaitSeconds = Default.Counter("platform_db_pool_acquire_wait_seconds_total", "Time acquires spent waiting for a free connection.", "db_system")

	exportFailures = Default.Counter("platform_telemetry_export_failures_total", "Metric exports that failed, by exporter (otlp, prometheus, ingest).", "exporter")
)

// Lease names and events: the fixed values of LeaseEvents.
const (
	LeaseLoops = "api_loops"
	LeaseJob   = "job"

	LeaseAcquired    = "acquired"
	LeaseLost        = "lost"
	LeaseRenewFailed = "renew_failed"
)

// RecordLease counts one lease event; values outside the fixed sets are dropped.
func RecordLease(ctx context.Context, lease, event string) {
	if (lease != LeaseLoops && lease != LeaseJob) || (event != LeaseAcquired && event != LeaseLost && event != LeaseRenewFailed) {
		return
	}
	LeaseEvents.IncContext(ctx, lease, event)
}

// SetLoopsLeaseHeld records whether this process runs the singleton loops.
func SetLoopsLeaseHeld(ctx context.Context, held bool) {
	v := 0.0
	if held {
		v = 1
	}
	LoopsLeaseHeld.SetContext(ctx, v)
}

// Worker roles: the fixed values of WorkerRoles' role label, the roles of
// codesynapse-api worker --roles.
const (
	RoleIngest = "ingest"
	RoleAI     = "ai"
	RoleFix    = "fix"
)

// WorkerRoleNames lists the roles in a stable order.
func WorkerRoleNames() []string { return []string{RoleIngest, RoleAI, RoleFix} }

// SetWorkerRoles records the job roles this process runs: 1 for each role
// in roles, 0 for every other known role. Unknown roles are dropped.
func SetWorkerRoles(ctx context.Context, roles []string) {
	for _, role := range WorkerRoleNames() {
		v := 0.0
		if slices.Contains(roles, role) {
			v = 1
		}
		WorkerRoles.SetContext(ctx, v, role)
	}
}

// Exporter names: the fixed values of platform_telemetry_export_failures_total.
const (
	ExporterOTLP       = "otlp"
	ExporterPrometheus = "prometheus"
	ExporterIngest     = "ingest"
)

// ExportFailed counts one failed metric export. A failure is counted even
// when the export carrying the count fails too: it arrives with the next one.
func ExportFailed(ctx context.Context, exporter string) {
	switch exporter {
	case ExporterOTLP, ExporterPrometheus, ExporterIngest:
		exportFailures.IncContext(ctx, exporter)
	}
}

// ---- end platform health ----

// Background jobs and indexing (docs/TELEMETRY_PLAN.md, metrics coverage
// plan, phase 3). Record through the helpers in jobs.go, which keep every
// label inside its fixed set. The tenant is the job's stored organisation.
var (
	// slow buckets long-running work: one second to two hours.
	slow = []float64{1, 5, 15, 60, 300, 900, 1800, 3600, 7200}

	JobsFinished = Default.ScopedCounter("codesynapse_jobs_finished_total", "Background jobs that ended, by kind and outcome (ok, error, left_behind, cancelled).", "kind", "outcome")
	// The job histograms hold one series per kind and tenant; the share
	// keeps one organisation from filling the budget the others share.
	JobWait     = Default.ScopedHistogram("codesynapse_job_wait_seconds", "Time from queueing a job to a process claiming it, by kind.", slow, "kind").TenantShare(len(jobKinds) + 1)
	JobDuration = Default.ScopedHistogram("codesynapse_job_duration_seconds", "Time from claiming a job to its end, by kind.", slow, "kind").TenantShare(len(jobKinds) + 1)
	// A phase's average is seconds_total over total; a histogram per phase
	// and outcome would cost more of the budget than the average is worth.
	IngestPhaseSeconds = Default.ScopedCounter("codesynapse_ingest_phase_seconds_total", "Seconds indexing jobs spent per phase (prepare, ingest, publish) and outcome.", "phase", "outcome")
	IngestPhaseRuns    = Default.ScopedCounter("codesynapse_ingest_phase_total", "Indexing job phases run, per phase and outcome.", "phase", "outcome")
	IngestExtract      = Default.ScopedCounter("codesynapse_ingest_extract_seconds_total", "Seconds indexing spent in each extractor, by language.", "language")
)

// Outbound calls, sign-in and the audit log (docs/TELEMETRY_PLAN.md, metrics
// coverage plan, phase 4). Record through the helpers in outbound.go, which
// keep every label inside its fixed set and drop the tool and model the
// context may carry, so each tenant holds only the series listed here. Hosts,
// owners, remotes, logins, users, keys and error text are never labels.
var (
	GitOperations = Default.ScopedCounter("codesynapse_git_operations_total", "Remote git calls by operation (clone, fetch, ls_remote, push) and outcome (ok, error, no_access, redirected, timeout).", "operation", "outcome")
	GitDuration   = Default.ScopedHistogram("codesynapse_git_operation_seconds", "Remote git call time by operation.", slow, "operation").TenantShare(len(gitOperations))

	Lookups        = Default.ScopedCounter("codesynapse_lookups_total", "External lookups by service (osv, depsdev, forge_github, forge_gitlab, forge_forgejo) and outcome (ok, error, timeout, rate_limited).", "service", "outcome")
	LookupDuration = Default.ScopedHistogram("codesynapse_lookup_seconds", "External lookup time by service.", latency, "service").TenantShare(len(lookupServices))

	// Most failed attempts have no trusted tenant and export as platform_.
	AuthAttempts = Default.ScopedCounter("codesynapse_auth_attempts_total", "Sign-in and credential checks by method (password, api_key, session, sso) and outcome (ok, bad_credentials, rate_limited, suspended, expired, refused).", "method", "outcome")

	AuditWrites        = Default.ScopedCounter("codesynapse_audit_writes_total", "Audit log entries written, by source (api, mcp, service) and outcome (ok, error).", "source", "outcome")
	AuditWriteDuration = Default.ScopedHistogram("codesynapse_audit_write_seconds", "Audit log write time by source.", latency, "source").TenantShare(len(auditSources))
	// Sealing runs under the api-loops lease for the whole installation.
	AuditSeals = Default.Counter("platform_codesynapse_audit_seals_total", "Audit archive sealing runs by outcome (ok, error, nothing_to_seal, no_archive).", "outcome")
)

// ---- Sharper existing families (docs/TELEMETRY_PLAN.md, metrics coverage
// plan, phase 5). Record through the helpers in outcomes.go, which keep every
// label inside its fixed set. ----
var (
	// fast buckets the request paths that answer within seconds: eight
	// buckets instead of latency's fifteen leave room for many more tool,
	// route and tenant series under the same budget.
	fast = []float64{0.01, 0.05, 0.1, 0.25, 0.5, 1, 5, 30}

	// MCPLocalAnswers counts calls the Claude Code bridge answered on the
	// developer's machine; mcp_tool comes from WithTool, limited to the
	// local tool set the API registers.
	MCPLocalAnswers = Default.ScopedCounter("mcp_local_answers_total", "MCP calls answered on the developer's machine, by outcome (recorded, refused).", "outcome")
	// GatewayDuration shares its budget per tenant like the job histograms:
	// three routes times a few tools and models per organisation.
	GatewayDuration    = Default.ScopedHistogram("gateway_request_duration_seconds", "Gateway request time by route (local, cloud, rejected).", latency, "route").TenantShare(gatewayShare)
	GatewayFallthrough = Default.ScopedCounter("gateway_fallthrough_total", "Gateway requests the local model did not answer, by reason and the route that took them instead (cloud, rejected, or local where no cloud follows).", "reason", "route")
	// queueWait reaches past the gateway's wait for the local model, so a
	// slow queue shows how slow before it turns into a fall-through.
	queueWait = []float64{0.01, 0.1, 0.5, 1, 2.5, 5, 10, 20, 30, 60, 120}
	// GatewayQueueWait is how long a request waited for the local model's
	// queue slot, whether it got one or not.
	GatewayQueueWait = Default.ScopedHistogram("gateway_queue_wait_seconds", "Time gateway requests waited for the local model's queue slot.", queueWait).TenantShare(gatewayShare)
	// GatewayLocalAnswer is the local model's own time, from its slot to an
	// answer or to the failure that sent the request on.
	GatewayLocalAnswer = Default.ScopedHistogram("gateway_local_answer_seconds", "Time the local model took from its queue slot to an answer or a failure, by outcome (answered, failed).", latency, "outcome").TenantShare(gatewayShare)
	// GatewayFirstByte covers streamed answers only: the time the agent
	// waits before anything arrives.
	GatewayFirstByte = Default.ScopedHistogram("gateway_first_byte_seconds", "Time to the first byte of a streamed gateway answer, by route (local, cloud).", ttft, "route").TenantShare(gatewayShare)
)

// Per-tenant shares of the request-path histograms. One organisation calling
// every tool or route still fits; one flooding new combinations (a model
// allowlist, many tools under many models) stops at its share instead of
// taking the series the other organisations need.
const (
	toolShare    = 48
	routeShare   = 64
	gatewayShare = 12
)

// ---- end sharper existing families ----
