package api

import (
	"cmp"
	"context"
	"errors"
	"math"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/astrona-io/codesynapse/internal/accounts"
	"github.com/astrona-io/codesynapse/internal/telemetry"
)

// Time-series statistics from VictoriaMetrics. Panels are fixed MetricsQL
// queries; the only value interpolated is the step, from seriesRanges.
// Percentiles use increase(), not rate(): the quantile is the same, and
// VictoriaMetrics' rate() reads 0 for a series born inside the window (a
// tool's first call), which increase() counts from zero.
// Tenant scope is not part of the query text: Querier.QueryRange passes it
// as extra_filters, which VictoriaMetrics applies to every series.

type seriesRange struct {
	span, step time.Duration
	window     string // the step as a MetricsQL duration
}

var seriesRanges = map[string]seriesRange{
	"1h":  {time.Hour, time.Minute, "1m"},
	"6h":  {6 * time.Hour, 5 * time.Minute, "5m"},
	"24h": {24 * time.Hour, 15 * time.Minute, "15m"},
	"7d":  {7 * 24 * time.Hour, time.Hour, "1h"},
	"30d": {30 * 24 * time.Hour, 6 * time.Hour, "6h"},
}

type seriesQuery struct {
	name string // series name, or "" to name series by the by label
	expr string // $w is replaced by the step window
	by   string // label naming each series
}

type seriesPanel struct {
	Title, Unit, Help, Empty string
	// Kind is "count" (per step) or "gauge" (latency, a level per step).
	Kind       string
	queries    []seriesQuery
	platform   bool // cross-tenant: platform admins only
	repository bool
}

// Gateway queries shared by the organisation and platform panels; only the
// tenant filter differs.
const (
	gatewayP95     = `histogram_quantile(0.95, sum by (le, route) (increase(gateway_request_duration_seconds_bucket[$w])))`
	gatewayAvg     = `sum by (route) (increase(gateway_request_duration_seconds_sum[$w])) / (sum by (route) (increase(gateway_request_duration_seconds_count[$w])) > 0)`
	gatewayReasons = `sum by (reason) (increase(gateway_fallthrough_total[$w]))`
)

// OTLP can split related gauges across HTTP batches. Only observations whose
// count and freshness samples came from the same export are eligible.
const memoryObserved = `agent_memory_facts_observed_at_seconds and (timestamp(agent_memory_facts_count) == timestamp(agent_memory_facts_observed_at_seconds))`

var seriesPanels = map[string]seriesPanel{
	"annotation_jobs": {Title: "Organisation annotation outcomes", Unit: "jobs", Kind: "count",
		Help:    "Local annotation jobs across the selected organisations, separate from ingestion outcomes. This panel is not repository-specific.",
		queries: []seriesQuery{{expr: `sum by (outcome) (increase(codesynapse_annotation_jobs_total[$w]))`, by: "outcome"}}},
	"ingest_files": {Title: "File processing across runs", Unit: "files", Kind: "count", repository: true,
		Help:    "Persisted file accounting: encountered files and confirmed file writes. Exact counts belong to each ingestion report; these increases span runs.",
		queries: []seriesQuery{{name: "scanned", expr: `sum(increase(codesynapse_files_scanned_total[$w]))`}, {name: "written", expr: `sum(increase(codesynapse_files_indexed_total[$w]))`}}},
	"ingest_skips": {Title: "Whole-file exclusions across runs", Unit: "files", Kind: "count", repository: true,
		Help:    "Whole-file skip decisions from persisted report checkpoints. Kubernetes-only exclusions are stage decisions and do not count as skipped files.",
		queries: []seriesQuery{{expr: `sum by (skip_reason) (increase(codesynapse_files_skipped_total[$w]))`, by: "skip_reason"}}},
	// Background jobs and indexing (docs/TELEMETRY_PLAN.md, phase 3), for
	// Statistics → Sync. Outcomes: ok, error, left_behind, cancelled.
	"jobs_finished": {Title: "Finished jobs", Unit: "jobs", Kind: "count",
		Help:    "Indexing, sync, AI and fix jobs that ended, by outcome. Left behind means another process resumes the job.",
		Empty:   "No jobs finished in this window.",
		queries: []seriesQuery{{expr: `sum by (outcome) (increase(codesynapse_jobs_finished_total[$w]))`, by: "outcome"}}},
	"job_failures": {Title: "Failed jobs", Unit: "jobs", Kind: "count",
		Help:    "Jobs that ended in an error, by kind. Cancelled jobs and jobs left for another process are not failures.",
		Empty:   "No jobs failed in this window.",
		queries: []seriesQuery{{expr: `sum by (kind) (increase(codesynapse_jobs_finished_total{outcome="error"}[$w]))`, by: "kind"}}},
	"job_wait": {Title: "Job queue wait", Unit: "seconds", Kind: "gauge",
		Help:    "95th percentile time from queueing a job to a process starting it, by kind.",
		Empty:   "No jobs started in this window.",
		queries: []seriesQuery{{expr: `histogram_quantile(0.95, sum by (le, kind) (increase(codesynapse_job_wait_seconds_bucket[$w])))`, by: "kind"}}},
	"job_duration": {Title: "Job run time", Unit: "seconds", Kind: "gauge",
		Help:    "95th percentile time from a process starting a job to its end, by kind.",
		Empty:   "No jobs finished in this window.",
		queries: []seriesQuery{{expr: `histogram_quantile(0.95, sum by (le, kind) (increase(codesynapse_job_duration_seconds_bucket[$w])))`, by: "kind"}}},
	"ingest_runs": {Title: "Indexing runs", Unit: "runs", Kind: "count",
		Help:    "Repository indexing runs by outcome.",
		Empty:   "No repository was indexed in this window.",
		queries: []seriesQuery{{expr: `sum by (outcome) (increase(ingest_runs_total[$w]))`, by: "outcome"}}},
	"ingest_duration": {Title: "Indexing time", Unit: "seconds", Kind: "gauge",
		Help:  "Median and 95th percentile time to extract and store a repository.",
		Empty: "No repository was indexed in this window.",
		queries: []seriesQuery{
			{name: "p50", expr: `histogram_quantile(0.5, sum by (le) (increase(ingest_duration_seconds_bucket[$w])))`},
			{name: "p95", expr: `histogram_quantile(0.95, sum by (le) (increase(ingest_duration_seconds_bucket[$w])))`},
		}},
	"ingest_phases": {Title: "Indexing phase time", Unit: "seconds", Kind: "gauge",
		Help:    "Average time per run of each indexing phase: prepare (clone or fetch), ingest (extraction and analysis) and publish (storing the graph).",
		Empty:   "No indexing phase ran in this window.",
		queries: []seriesQuery{{expr: `sum by (phase) (increase(codesynapse_ingest_phase_seconds_total[$w])) / (sum by (phase) (increase(codesynapse_ingest_phase_total[$w])) > 0)`, by: "phase"}}},
	"ingest_extract": {Title: "Extraction time by language", Unit: "seconds", Kind: "count",
		Help:    "Time spent parsing each language while indexing. JavaScript, TypeScript and Vue share one parser, as do Java and Kotlin.",
		Empty:   "No extraction ran in this window.",
		queries: []seriesQuery{{expr: `sum by (language) (increase(codesynapse_ingest_extract_seconds_total[$w]))`, by: "language"}}},
	// Outbound calls and the audit log (docs/TELEMETRY_PLAN.md, phase 4).
	"git_operations": {Title: "Git operations", Unit: "operations", Kind: "count",
		Help:    "Clones, fetches, access checks (ls_remote) and pushes to remote repositories, by outcome. No access means the host refused the credentials; redirected means the repository moved.",
		Empty:   "No remote git call in this window.",
		queries: []seriesQuery{{expr: `sum by (outcome) (increase(codesynapse_git_operations_total[$w]))`, by: "outcome"}}},
	"git_duration": {Title: "Git operation time", Unit: "seconds", Kind: "gauge",
		Help:    "95th percentile time of a remote git call, by operation.",
		Empty:   "No remote git call in this window.",
		queries: []seriesQuery{{expr: `histogram_quantile(0.95, sum by (le, operation) (increase(codesynapse_git_operation_seconds_bucket[$w])))`, by: "operation"}}},
	"lookups": {Title: "External lookups", Unit: "requests", Kind: "count",
		Help:    "Requests to osv.dev (vulnerabilities), deps.dev (licences) and the git hosts' APIs (organisation listings), by service and outcome. Only package names, versions and organisation names leave the installation.",
		Empty:   "No external lookup in this window.",
		queries: []seriesQuery{{expr: `sum by (series) (label_join(increase(codesynapse_lookups_total[$w]), "series", " ", "service", "outcome"))`, by: "series"}}},
	"lookup_latency": {Title: "External lookup latency", Unit: "seconds", Kind: "gauge",
		Help:    "95th percentile time for an external service to answer, by service.",
		Empty:   "No external lookup in this window.",
		queries: []seriesQuery{{expr: `histogram_quantile(0.95, sum by (le, service) (increase(codesynapse_lookup_seconds_bucket[$w])))`, by: "service"}}},
	"audit_write_latency": {Title: "Audit write latency", Unit: "seconds", Kind: "gauge",
		Help:    "95th percentile time to append an entry to the audit log, by the service that wrote it. A request waits for its entry.",
		Empty:   "No audit entry was written in this window.",
		queries: []seriesQuery{{expr: `histogram_quantile(0.95, sum by (le, source) (increase(codesynapse_audit_write_seconds_bucket[$w])))`, by: "source"}}},
	"tool_calls": {Title: "Tool calls", Unit: "calls", Kind: "count",
		Help:    "MCP tool and prompt calls per step, by tool.",
		queries: []seriesQuery{{expr: `sum by (mcp_tool) (increase(mcp_tool_calls_total[$w]))`, by: "mcp_tool"}}},
	"tool_errors": {Title: "Failed tool calls", Unit: "calls", Kind: "count",
		Help:    "Calls that ran and returned an error, by tool. Calls the server refused are counted under refused tool calls.",
		queries: []seriesQuery{{expr: `sum by (mcp_tool) (increase(mcp_tool_calls_total{outcome="error"}[$w]))`, by: "mcp_tool"}}},
	// Phase 5 (docs/TELEMETRY_PLAN.md): refusals and local answers.
	"tool_refusals": {Title: "Refused tool calls", Unit: "calls", Kind: "count",
		Help:    "Calls the server turned down before the tool ran (permission, repository scope or policy), by tool. Names a client invents count as unknown.",
		Empty:   "No tool call was refused in this window.",
		queries: []seriesQuery{{expr: `sum by (mcp_tool) (increase(mcp_tool_calls_total{outcome="refused"}[$w]))`, by: "mcp_tool"}}},
	"local_answers": {Title: "Answered on the developer's machine", Unit: "calls", Kind: "count",
		Help:    "Calls the Claude Code bridge answered locally (git and GitHub tools, ask_local, local_draft, fetch_source, and code tools while the checkout has unsynced changes), by tool. Refused reports went to the server instead.",
		Empty:   "No call was answered locally in this window.",
		queries: []seriesQuery{{expr: `sum by (mcp_tool) (increase(mcp_local_answers_total{outcome="recorded"}[$w]))`, by: "mcp_tool"}}},
	"sessions": {Title: "Agent sessions", Unit: "sessions", Kind: "count",
		Help:    "MCP sessions started per step.",
		queries: []seriesQuery{{name: "sessions", expr: `sum(increase(mcp_sessions_total[$w]))`}}},
	"agent_hook_events": {Title: "Assistant hook activity", Unit: "events", Kind: "count",
		Help:    "Accepted client-reported lifecycle observations by event; separate from MCP calls.",
		queries: []seriesQuery{{expr: `sum by (event) (increase(agent_hook_events_total[$w]))`, by: "event"}}},
	"agent_hook_bytes": {Title: "Assistant observed bytes", Unit: "bytes", Kind: "count",
		Help:    "Bytes seen in hook fields by kind. Content is never stored; repeated observations may count the same content again.",
		queries: []seriesQuery{{expr: `sum by (kind) (increase(agent_hook_bytes_total[$w]))`, by: "kind"}}},
	"tool_latency": {Title: "Tool latency", Unit: "seconds", Kind: "gauge",
		Help: "Median and 95th percentile time to answer a tool call.",
		queries: []seriesQuery{
			{name: "p50", expr: `histogram_quantile(0.5, sum by (le) (increase(mcp_tool_duration_seconds_bucket[$w])))`},
			{name: "p95", expr: `histogram_quantile(0.95, sum by (le) (increase(mcp_tool_duration_seconds_bucket[$w])))`},
		}},
	"served_tokens": {Title: "Tokens served to agents", Unit: "tokens", Kind: "count",
		Help:    "Estimated tokens MCP tools returned, by tool.",
		queries: []seriesQuery{{expr: `sum by (mcp_tool) (increase(mcp_response_tokens_total[$w]))`, by: "mcp_tool"}}},
	"llm_tokens": {Title: "AI model tokens", Unit: "tokens", Kind: "count",
		Help: "Tokens CodeSynapse's own AI tasks sent and received; cached = input tokens served from the provider's prompt cache; those tokens may still be billed.",
		queries: []seriesQuery{
			{name: "input", expr: `sum(increase(llm_tokens_input_total[$w]))`},
			{name: "output", expr: `sum(increase(llm_tokens_output_total[$w]))`},
			{name: "cached", expr: `sum(increase(llm_tokens_saved_total[$w]))`},
		}},
	// By provider, for the AI page: jobs run in the worker, whose calls the API
	// process's own counters (llm.Registry.Usage) never see.
	"llm_provider_requests": {Title: "AI requests by provider", Unit: "requests", Kind: "count",
		Help:    "Requests CodeSynapse's AI tasks made to each model provider, from every process.",
		queries: []seriesQuery{{expr: `sum by (llm_provider) (increase(llm_requests_total[$w]))`, by: "llm_provider"}}},
	"llm_provider_failures": {Title: "Failed AI requests by provider", Unit: "requests", Kind: "count",
		Help:    "Requests that did not succeed (errors, timeouts, rate limits and refusals), by model provider.",
		queries: []seriesQuery{{expr: `sum by (llm_provider) (increase(llm_requests_total{outcome!="ok"}[$w]))`, by: "llm_provider"}}},
	"llm_failure_causes": {Title: "Why AI requests failed", Unit: "requests", Kind: "count",
		Help:    "Failed model requests by cause: timeout, rate_limited (HTTP 429), refused (rejected credentials or a model that declined) and error (everything else). Taken from status codes, never from error text.",
		Empty:   "No AI request failed in this window.",
		queries: []seriesQuery{{expr: `sum by (outcome) (increase(llm_requests_total{outcome!="ok"}[$w]))`, by: "outcome"}}},
	"llm_provider_input": {Title: "AI input tokens by provider", Unit: "tokens", Kind: "count",
		Help:    "Prompt tokens sent to each model provider.",
		queries: []seriesQuery{{expr: `sum by (llm_provider) (increase(llm_tokens_input_total[$w]))`, by: "llm_provider"}}},
	"llm_provider_output": {Title: "AI output tokens by provider", Unit: "tokens", Kind: "count",
		Help:    "Completion tokens each model provider returned.",
		queries: []seriesQuery{{expr: `sum by (llm_provider) (increase(llm_tokens_output_total[$w]))`, by: "llm_provider"}}},
	"llm_cost": {Title: "AI model cost (estimate)", Unit: "USD", Kind: "count",
		Help:    "Estimated from configured input, output and cache prices. Calls with missing usage or prices are excluded; see measurement gaps.",
		queries: []seriesQuery{{expr: `sum by (llm_model) (increase(llm_cost_estimated_dollars[$w]))`, by: "llm_model"}}},
	"llm_savings": {Title: "Prompt-cache cost savings (estimate)", Unit: "USD", Kind: "count",
		Help:    "Uncached input cost minus cache-read cost at the prices configured when the call ran. Missing prices or usage are excluded.",
		queries: []seriesQuery{{name: "estimated savings", expr: `sum(increase(llm_cost_saved_estimated_dollars_total[$w]))`}}},
	"llm_coverage": {Title: "Model measurement gaps", Unit: "calls", Kind: "count",
		Help: "Calls without reported token usage or configured prices. Their cost is unknown.",
		queries: []seriesQuery{
			{name: "usage unknown", expr: `sum(increase(llm_usage_unknown_total[$w]))`},
			{name: "prices missing", expr: `sum(increase(llm_pricing_missing_total[$w]))`},
		}},
	"bandwidth": {Title: "MCP payload bandwidth", Unit: "bytes/s", Kind: "gauge",
		Help: "Request bytes read and response bytes written, excluding HTTP headers, TLS and TCP overhead.",
		queries: []seriesQuery{
			{name: "received", expr: `sum(rate(net_bytes_received_total[$w]))`},
			{name: "sent", expr: `sum(rate(net_bytes_sent_total[$w]))`},
		}},
	"cloud_traffic": {Title: "Cloud model request payloads", Unit: "bytes", Kind: "count",
		Help:    "Encoded cloud request bytes consumed by HTTP transports, including retry attempts. Local-only calls do not imply savings.",
		queries: []seriesQuery{{name: "sent", expr: `sum(increase(cloud_bytes_sent_total[$w]))`}}},
	"cloud_savings": {Title: "Cloud payload avoided (estimate)", Unit: "%", Kind: "gauge",
		Help:    "Requires an equivalent configured cloud fallback and measured serialized payload. Provider prompt caching does not imply avoided network bytes.",
		Empty:   "Not measured: no comparable cloud fallback traffic in this window.",
		queries: []seriesQuery{{name: "avoided", expr: `100 * sum(increase(cloud_bytes_saved_total[$w])) / ((sum(increase(cloud_bytes_sent_total[$w])) + sum(increase(cloud_bytes_saved_total[$w]))) > 0)`}}},
	// Phase 5: the model gateway's time per route and why requests offered
	// to the local model went to the cloud.
	"gateway_latency": {Title: "Gateway response time (p95)", Unit: "seconds", Kind: "gauge",
		Help:    "95th percentile time to answer a gateway request, by route: local model, cloud, or rejected before any model saw it.",
		Empty:   "No gateway request in this window.",
		queries: []seriesQuery{{expr: gatewayP95, by: "route"}}},
	"gateway_latency_avg": {Title: "Gateway response time (average)", Unit: "seconds", Kind: "gauge",
		Help:    "Average time to answer a gateway request, by route: local model, cloud, or rejected before any model saw it.",
		Empty:   "No gateway request in this window.",
		queries: []seriesQuery{{expr: gatewayAvg, by: "route"}}},
	"gateway_fallthrough": {Title: "Requests the local model did not answer", Unit: "requests", Kind: "count",
		Help:    "Gateway requests the local model did not answer, by reason. queue_timeout and answer_timeout mean it was too slow; the other reasons are requests it does not take or failed on.",
		Empty:   "Every request offered to the local model was answered in this window.",
		queries: []seriesQuery{{expr: gatewayReasons, by: "reason"}}},
	"gateway_timeouts": {Title: "Local model timeouts", Unit: "requests", Kind: "count",
		Help:    "Requests that waited too long for the local model's queue slot (queue_timeout) or for its answer (answer_timeout).",
		Empty:   "No local model timeout in this window.",
		queries: []seriesQuery{{expr: `sum by (reason) (increase(gateway_fallthrough_total{reason=~"queue_timeout|answer_timeout"}[$w]))`, by: "reason"}}},
	"gateway_queue_wait": {Title: "Local model queue wait", Unit: "seconds", Kind: "gauge",
		Help:  "Time gateway requests waited for the local model's queue slot: average and 95th percentile.",
		Empty: "No request waited for the local model in this window.",
		queries: []seriesQuery{
			{name: "average", expr: `sum(increase(gateway_queue_wait_seconds_sum[$w])) / (sum(increase(gateway_queue_wait_seconds_count[$w])) > 0)`},
			{name: "p95", expr: `histogram_quantile(0.95, sum by (le) (increase(gateway_queue_wait_seconds_bucket[$w])))`},
		}},
	"llm_ttft": {Title: "AI time to first token", Unit: "seconds", Kind: "gauge",
		Help:    "95th percentile wait for the first streamed token, by model.",
		queries: []seriesQuery{{expr: `histogram_quantile(0.95, sum by (le, llm_model) (increase(llm_time_to_first_token_seconds_bucket[$w])))`, by: "llm_model"}}},
	"db_latency": {Title: "Database latency", Unit: "seconds", Kind: "gauge",
		Help:    "95th percentile round trip by database.",
		queries: []seriesQuery{{expr: `histogram_quantile(0.95, sum by (le, db_system) (increase(db_query_duration_seconds_bucket[$w])))`, by: "db_system"}}},
	"db_latency_tail": {Title: "Database latency distribution", Unit: "seconds", Kind: "gauge",
		Help: "Client-observed latency across database requests. These measurements do not establish an SLA guarantee.",
		queries: []seriesQuery{
			{name: "p50", expr: `histogram_quantile(0.5, sum by (le) (increase(db_query_duration_seconds_bucket[$w])))`},
			{name: "p95", expr: `histogram_quantile(0.95, sum by (le) (increase(db_query_duration_seconds_bucket[$w])))`},
			{name: "p99", expr: `histogram_quantile(0.99, sum by (le) (increase(db_query_duration_seconds_bucket[$w])))`},
		}},
	"db_requests": {Title: "Database requests", Unit: "requests", Kind: "count",
		Help:    "Measured database operations by backend, including failed requests.",
		queries: []seriesQuery{{expr: `sum by (db_system) (increase(db_query_duration_seconds_count[$w]))`, by: "db_system"}}},
	"db_errors": {Title: "Database errors and timeouts", Unit: "requests", Kind: "count",
		Help: "Failed database requests. Timeouts are a subset of errors, so these series must not be added together.",
		queries: []seriesQuery{
			{name: "errors", expr: `sum(increase(db_query_errors_total[$w]))`},
			{name: "timeouts", expr: `sum(increase(db_query_timeouts_total[$w]))`},
		}},
	"db_rows": {Title: "Graph rows and vector matches", Unit: "rows", Kind: "count",
		Help: "Rows graph reads returned and vectors vector searches matched.",
		queries: []seriesQuery{
			{expr: `sum by (db_system) (increase(db_graph_rows_returned_total[$w]))`, by: "db_system"},
			{name: "vector matches", expr: `sum(increase(db_vector_matches_count[$w]))`},
		}},
	"memory": {Title: "Agent memory", Unit: "events", Kind: "count",
		Help: "Facts saved (by agents or extracted from offloaded turns), turns offloaded and recalls.",
		queries: []seriesQuery{
			{name: "facts saved", expr: `sum(increase(memory_facts_saved_total[$w]))`},
			{name: "turns offloaded", expr: `sum(increase(memory_offload_turns_total[$w]))`},
			{name: "recalls", expr: `sum(increase(agent_memory_recalls_total[$w]))`},
		}},
	"memory_facts": {Title: "Durable memory facts", Unit: "facts", Kind: "gauge",
		Help:    "Current unique stored facts per organisation. Replicas are deduplicated; observations older than 90 seconds are omitted. Deletion and deduplication can lower the count.",
		Empty:   "No fresh durable-memory count is available in this window.",
		queries: []seriesQuery{{name: "stored facts", expr: `sum(max by (tenant_id) (agent_memory_facts_count and ((` + memoryObserved + `) == on (tenant_id) group_left max by (tenant_id) (` + memoryObserved + `)) and ((` + memoryObserved + `) > (time() - 90))))`}}},
}

// platformHealthPanels show the process and platform state of the
// installation (metrics coverage plan, phase 1): platform admins only, never
// scoped to a tenant. Process gauges are per instance, so panels take the
// maximum per service (job) and never sum them, except the loops lease, whose
// sum is how many processes think they lead.
var platformHealthPanels = map[string]seriesPanel{
	"platform_api_requests": {Title: "API requests by status", Unit: "requests", Kind: "count",
		Help:    "API and worker requests per step by status class, signed in or not.",
		queries: []seriesQuery{{expr: `sum by (code) (increase({__name__=~"(platform_)?http_requests_total"}[$w]))`, by: "code"}}},
	"platform_api_latency": {Title: "API latency", Unit: "seconds", Kind: "gauge",
		Help: "Median and 95th percentile API request time across routes.",
		queries: []seriesQuery{
			{name: "p50", expr: `histogram_quantile(0.5, sum by (le) (increase({__name__=~"(platform_)?http_request_duration_seconds_bucket"}[$w])))`},
			{name: "p95", expr: `histogram_quantile(0.95, sum by (le) (increase({__name__=~"(platform_)?http_request_duration_seconds_bucket"}[$w])))`},
		}},
	"platform_job_queue": {Title: "Job queue", Unit: "jobs", Kind: "gauge",
		Help: "Jobs a worker could claim now, and jobs queued or running, reported by the process running the scheduler.",
		queries: []seriesQuery{
			{name: "waiting", expr: `sum(max by (kind) (platform_codesynapse_jobs_waiting))`},
			{name: "queued or running", expr: `sum(max by (kind) (platform_codesynapse_jobs_active))`},
		}},
	"platform_loops_lease": {Title: "Scheduler holders", Unit: "processes", Kind: "gauge",
		Help:    "API processes that hold the scheduler lease. Anything but 1 means no process, or more than one, runs the scheduler and maintenance loops.",
		queries: []seriesQuery{{name: "holders", expr: `sum(platform_codesynapse_loops_lease_held)`}}},
	"platform_lease_events": {Title: "Lease changes", Unit: "events", Kind: "count",
		Help:    "Scheduler lease taken, lost or not renewed, and durable job leases not renewed or given up.",
		queries: []seriesQuery{{expr: `sum by (series) (label_join(increase(platform_codesynapse_lease_events_total[$w]), "series", " ", "lease", "event"))`, by: "series"}}},
	"platform_goroutines": {Title: "Goroutines", Unit: "goroutines", Kind: "gauge",
		Help:    "Live goroutines, the highest instance of each service.",
		queries: []seriesQuery{{expr: `max by (job) (platform_go_goroutines)`, by: "job"}}},
	"platform_heap": {Title: "Heap in use", Unit: "bytes", Kind: "gauge",
		Help:    "Heap held by live and not yet swept objects, the highest instance of each service.",
		queries: []seriesQuery{{expr: `max by (job) (platform_go_heap_bytes)`, by: "job"}}},
	"platform_cpu": {Title: "CPU", Unit: "cores", Kind: "gauge",
		Help:    "CPU used by Go code and the Go runtime, summed over each service's instances. The runtime updates it at each garbage collection, so it lags a little.",
		queries: []seriesQuery{{expr: `sum by (job) (rate(platform_process_cpu_seconds_total[$w]))`, by: "job"}}},
	"platform_gc_pause": {Title: "Garbage collection pauses", Unit: "%", Kind: "gauge",
		Help:    "Share of time the busiest instance of each service was stopped for garbage collection (estimate).",
		queries: []seriesQuery{{expr: `100 * max by (job) (rate(platform_go_gc_pause_seconds_total[$w]))`, by: "job"}}},
	"platform_db_pool": {Title: "PostgreSQL connections", Unit: "connections", Kind: "gauge",
		Help:    "Connections idle and in use, and the pool limit, the highest instance of each kind.",
		queries: []seriesQuery{{expr: `max by (state) (platform_db_pool_connections{db_system="postgres"})`, by: "state"}}},
	"platform_db_pool_waits": {Title: "Waits for a PostgreSQL connection", Unit: "acquires", Kind: "count",
		Help:    "Requests that found every pooled connection busy and had to wait.",
		queries: []seriesQuery{{name: "waits", expr: `sum(increase(platform_db_pool_acquire_waits_total{db_system="postgres"}[$w]))`}}},
	"platform_telemetry_drops": {Title: "Measurements dropped", Unit: "samples", Kind: "count",
		Help: "New label combinations an instrument refused at its series cap, by instrument, and log lines not shipped.",
		queries: []seriesQuery{
			{expr: `sum by (metric) (increase(telemetry_dropped_series_total[$w]))`, by: "metric"},
			{name: "log lines", expr: `sum(increase(platform_log_lines_dropped_total[$w]))`},
		}},
	"platform_sign_in": {Title: "Sign-in", Unit: "attempts", Kind: "count",
		Help: "Password, API key, session and single sign-on checks that failed, by method and outcome, and the ones that succeeded. A burst of bad credentials or rate limiting suggests someone guessing.",
		queries: []seriesQuery{
			{expr: `sum by (series) (label_join(increase({__name__=~"(platform_)?codesynapse_auth_attempts_total",outcome!="ok"}[$w]), "series", " ", "method", "outcome"))`, by: "series"},
			{name: "ok", expr: `sum(increase({__name__=~"(platform_)?codesynapse_auth_attempts_total",outcome="ok"}[$w]))`},
		}},
	"platform_audit_log": {Title: "Audit log problems", Unit: "events", Kind: "count",
		Help: "Audit log writes that failed, by source, and archive sealing runs that failed or found sealed files but no archive. A failed write refuses the request or withholds an MCP result.",
		queries: []seriesQuery{
			{expr: `sum by (series) (label_join(increase({__name__=~"(platform_)?codesynapse_audit_writes_total",outcome="error"}[$w]), "series", " ", "source", "outcome"))`, by: "series"},
			{expr: `sum by (series) (label_join(increase(platform_codesynapse_audit_seals_total{outcome=~"error|no_archive"}[$w]), "series", " ", "outcome"))`, by: "series"},
		}},
	"platform_telemetry_exports": {Title: "Failed metric exports", Unit: "exports", Kind: "count",
		Help:    "Metric exports that failed, by exporter. A failure is counted when the next export succeeds.",
		queries: []seriesQuery{{expr: `sum by (exporter) (increase(platform_telemetry_export_failures_total[$w]))`, by: "exporter"}}},
	// The web server (telemetry phase 6) labels requests by route pattern,
	// never by the visited path.
	"platform_web_requests": {Title: "Web requests by status", Unit: "requests", Kind: "count",
		Help:    "Pages and dashboard API calls the web server answered, by status class.",
		queries: []seriesQuery{{expr: `sum by (status_class) (increase(platform_web_requests_total[$w]))`, by: "status_class"}}},
	"platform_web_latency": {Title: "Web latency", Unit: "seconds", Kind: "gauge",
		Help: "Median and 95th percentile time the web server took to answer, page renders and API calls together.",
		queries: []seriesQuery{
			{name: "p50", expr: `histogram_quantile(0.5, sum by (le) (increase(platform_web_request_duration_seconds_bucket[$w])))`},
			{name: "p95", expr: `histogram_quantile(0.95, sum by (le) (increase(platform_web_request_duration_seconds_bucket[$w])))`},
		}},
	"platform_web_proxy": {Title: "Dashboard calls that did not reach the API", Unit: "requests", Kind: "count",
		Help:    "Calls the web server refused itself, or that found the API unreachable, timed out or failed otherwise.",
		Empty:   "Every dashboard call in this window reached the API.",
		queries: []seriesQuery{{expr: `sum by (outcome) (increase(platform_web_proxy_requests_total{outcome!="ok"}[$w]))`, by: "outcome"}}},
}

func init() {
	for name, panel := range platformHealthPanels {
		panel.platform = true
		seriesPanels[name] = panel
	}
}

// maxNamedSeries matches the chart palette (four hues, never cycled): the
// rest folds into one neutral "other".
const maxNamedSeries = 4

// SeriesPanel is one chart's data, timestamps aligned across series.
type SeriesPanel struct {
	Panel  string       `json:"panel"`
	Title  string       `json:"title"`
	Unit   string       `json:"unit"`
	Kind   string       `json:"kind"`
	Help   string       `json:"help"`
	Empty  string       `json:"empty"`
	Range  string       `json:"range"`
	Step   int          `json:"step_seconds"`
	Times  []int64      `json:"times"`
	Series []SeriesLine `json:"series"`
}

// SeriesLine is one named series; nil values are steps without data.
type SeriesLine struct {
	Name   string     `json:"name"`
	Values []*float64 `json:"values"`
	Total  float64    `json:"total"`
}

func (s *Server) handleSeries(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Metrics == nil {
		s.problem(w, http.StatusConflict, errors.New("metrics are not configured (CODESYNAPSE_METRICS_URL)"))
		return
	}
	name, rng := r.URL.Query().Get("panel"), cmp.Or(r.URL.Query().Get("range"), "24h")
	p, ok := seriesPanels[name]
	if !ok {
		s.problem(w, http.StatusNotFound, errors.New("unknown panel"))
		return
	}
	if _, ok := seriesRanges[rng]; !ok {
		s.problem(w, http.StatusBadRequest, errors.New("range must be 1h, 6h, 24h, 7d or 30d"))
		return
	}
	var tenants []string // nil: every tenant (platform admins, single-tenant installs)
	caller, _ := principal(r.Context())
	if p.platform && !s.platformPanelView(w, r, []string{name}, rng) {
		return
	}
	if !p.platform {
		tenants = callerTenants(r)
	}
	repositoryID := ""
	if p.repository {
		if s.cfg.Accounts == nil {
			s.accountError(w, accounts.ErrNotFound)
			return
		}
		contentScope, err := s.scopeOf(r, caller)
		if err != nil {
			s.accountError(w, err)
			return
		}
		repo := r.URL.Query().Get("repo")
		reach, ok := contentScope.reach[repo]
		if !ok || reach.Ambiguous || !reach.Role.AtLeast(accounts.Viewer) {
			s.accountError(w, accounts.ErrNotFound)
			return
		}
		runs, err := s.cfg.Accounts.St.ListIngestRuns(r.Context(), reach.OrgID, repo, 1)
		if err != nil {
			s.accountError(w, err)
			return
		}
		if len(runs) == 0 || !runs[0].MetricCoverage {
			s.problem(w, http.StatusConflict, errors.New("repository ingestion metrics are not available; use the exact ingestion report"))
			return
		}
		repositoryID = runs[0].RepositoryID
		tenants = []string{reach.OrgID}
	}
	out, err := s.runPanel(r.Context(), name, rng, p, tenants, repositoryID)
	if err != nil {
		s.problem(w, http.StatusBadGateway, err)
		return
	}
	out.Series = foldSeries(out.Series, p.Kind, len(out.Times))
	writeJSON(w, http.StatusOK, out)
}

// runPanel runs a panel's queries over a range, its lines unfolded and
// aligned on the same steps. tenants nil means every tenant; repositoryID
// is set for repository panels.
func (s *Server) runPanel(ctx context.Context, name, rng string, p seriesPanel, tenants []string, repositoryID string) (SeriesPanel, error) {
	sr := seriesRanges[rng]
	// The last step is the current one, still filling: recent activity
	// shows at once instead of when the step completes.
	end := time.Now().Truncate(sr.step).Add(sr.step)
	if p.Kind == "gauge" {
		// Freshness uses evaluation time; a future step would hide current gauges.
		end = time.Now().Truncate(time.Second)
	}
	start := end.Add(-sr.span)
	out := SeriesPanel{Panel: name, Title: p.Title, Unit: p.Unit, Kind: p.Kind, Help: p.Help, Empty: cmp.Or(p.Empty, "No measurements recorded in this window."), Range: rng, Step: int(sr.step / time.Second), Series: []SeriesLine{}}
	for t := start.Add(sr.step); !t.After(end); t = t.Add(sr.step) {
		out.Times = append(out.Times, t.Unix())
	}
	index := make(map[int64]int, len(out.Times))
	for i, t := range out.Times {
		index[t] = i
	}
	for _, q := range p.queries {
		var res []telemetry.Series
		var err error
		if p.repository {
			res, err = s.cfg.Metrics.QueryRangeForRepo(ctx, strings.ReplaceAll(q.expr, "$w", sr.window), start.Add(sr.step), end, sr.step, tenants, repositoryID)
		} else {
			res, err = s.cfg.Metrics.QueryRange(ctx, strings.ReplaceAll(q.expr, "$w", sr.window), start.Add(sr.step), end, sr.step, tenants)
		}
		if err != nil {
			return SeriesPanel{}, err
		}
		for _, ts := range res {
			line := SeriesLine{Name: cmp.Or(q.name, ts.Labels[q.by], "none"), Values: make([]*float64, len(out.Times))}
			for _, pt := range ts.Points {
				if i, ok := index[pt.T]; ok && !math.IsInf(pt.V, 0) && !math.IsNaN(pt.V) {
					v := pt.V
					line.Values[i] = &v
					if p.Kind == "count" {
						line.Total += v
					} else {
						line.Total = math.Max(line.Total, v)
					}
				}
			}
			if p.Kind == "count" {
				// A step without samples had no activity: 0, so lines span
				// the window. Levels (latency) keep their gaps.
				for i, v := range line.Values {
					if v == nil {
						zero := 0.0
						line.Values[i] = &zero
					}
				}
			}
			out.Series = append(out.Series, line)
		}
	}
	return out, nil
}

// foldSeries keeps the largest series, ordered by name so a series keeps
// its colour while the same set is shown (colour follows the entity, not
// its rank), and sums the rest into "other" (counts only; levels such as
// latency cannot be summed and are dropped).
func foldSeries(lines []SeriesLine, kind string, n int) []SeriesLine {
	slices.SortStableFunc(lines, func(a, b SeriesLine) int { return cmp.Or(cmp.Compare(b.Total, a.Total), cmp.Compare(a.Name, b.Name)) })
	byName := func(ls []SeriesLine) []SeriesLine {
		slices.SortStableFunc(ls, func(a, b SeriesLine) int { return cmp.Compare(a.Name, b.Name) })
		return ls
	}
	if len(lines) <= maxNamedSeries {
		return byName(lines)
	}
	keep := byName(slices.Clone(lines[:maxNamedSeries])) // appending must not overwrite lines[maxNamedSeries]
	if kind != "count" {
		return keep
	}
	other := SeriesLine{Name: "other", Values: make([]*float64, n)}
	for _, l := range lines[maxNamedSeries:] {
		other.Total += l.Total
		for i, v := range l.Values {
			if v == nil {
				continue
			}
			sum := *v
			if other.Values[i] != nil {
				sum += *other.Values[i]
			}
			other.Values[i] = &sum
		}
	}
	return append(keep, other)
}

// callerTenants are the organisations whose metrics the caller may read, as
// extra_filters; nil means every tenant (single-tenant installs).
func callerTenants(r *http.Request) []string {
	sc := scopeFrom(r.Context())
	if sc.all {
		return nil
	}
	tenants := []string{}
	for _, m := range sc.orgs {
		tenants = append(tenants, m.Org.ID)
	}
	return tenants
}
