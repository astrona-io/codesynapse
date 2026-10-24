package telemetry

import "sync"

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
)

// MCP: sessions, tool calls, latency and the size of what agents receive.
var (
	MCPSessions       = Default.ScopedCounter("mcp_sessions_total", "MCP sessions started.", "tenant_id")
	MCPToolCalls      = Default.ScopedCounter("mcp_tool_calls_total", "MCP tool and prompt calls by outcome (ok, error).", "tenant_id", "mcp_tool", "outcome")
	MCPToolDuration   = Default.ScopedHistogram("mcp_tool_duration_seconds", "MCP tool execution time.", latency, "tenant_id", "mcp_tool")
	MCPResponseTokens = Default.ScopedCounter("mcp_response_tokens_total", "Estimated tokens MCP tools returned to agents.", "tenant_id", "mcp_tool")
)

// LLM calls made by CodeSynapse itself (reviews, triage, briefings,
// memory extraction).
var (
	LLMRequests = Default.ScopedCounter("llm_requests_total", "LLM requests by outcome.", "tenant_id", "llm_provider", "llm_model", "task", "outcome")
	LLMInput    = Default.ScopedCounter("llm_tokens_input_total", "Prompt tokens consumed.", "tenant_id", "llm_provider", "llm_model", "task")
	LLMOutput   = Default.ScopedCounter("llm_tokens_output_total", "Completion tokens generated.", "tenant_id", "llm_provider", "llm_model", "task")
	LLMSaved    = Default.ScopedCounter("llm_tokens_saved_total", "Input tokens served from the provider's prompt cache.", "tenant_id", "llm_provider", "llm_model", "task")
	LLMCost     = Default.ScopedCounter("llm_cost_estimated_dollars", "Estimated cost in US dollars from the configured token prices.", "tenant_id", "llm_provider", "llm_model")
	LLMDuration = Default.ScopedHistogram("llm_request_duration_seconds", "LLM request time.", latency, "tenant_id", "llm_provider", "llm_model")
	LLMTTFT     = Default.ScopedHistogram("llm_time_to_first_token_seconds", "Time to the first streamed token (streaming providers only).", ttft, "tenant_id", "llm_provider", "llm_model")
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
	HTTPDuration       = Default.ScopedHistogram("http_request_duration_seconds", "API request time by route pattern.", latency, "route", "method")
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
	LLMCostSaved                = Default.ScopedCounter("llm_cost_saved_estimated_dollars_total", "Estimated cached-input cost savings at the configured prices.", "tenant_id", "llm_provider", "llm_model")
	LLMUsageUnknown             = Default.ScopedCounter("llm_usage_unknown_total", "Model calls without complete reported token usage.", "tenant_id", "llm_provider", "llm_model")
	LLMPriceMissing             = Default.ScopedCounter("llm_pricing_missing_total", "Model calls without sufficient configured prices.", "tenant_id", "llm_provider", "llm_model")
	NetBytesReceived            = Default.ScopedCounter("net_bytes_received_total", "MCP application payload bytes read, excluding HTTP headers and transport overhead.", "tenant_id", "mcp_tool")
	NetBytesSent                = Default.ScopedCounter("net_bytes_sent_total", "MCP application payload bytes successfully written, including stream framing.", "tenant_id", "mcp_tool")
	CloudBytesSent              = Default.ScopedCounter("cloud_bytes_sent_total", "Cloud model request body bytes consumed by HTTP transports, including retries.", "tenant_id", "llm_provider", "llm_model")
	CloudBytesSaved             = Default.ScopedCounter("cloud_bytes_saved_total", "Estimated avoided cloud request bytes with a measured equivalent fallback baseline.", "tenant_id", "llm_provider", "llm_model")
	GraphRowsReturned           = Default.ScopedCounter("db_graph_rows_returned_total", "Graph result rows returned; not internally visited nodes.", "tenant_id", "db_system")
	DBTimeouts                  = Default.ScopedCounter("db_query_timeouts_total", "Database requests that timed out.", "tenant_id", "db_system", "operation")
	AgentMemoryRecalls          = Default.ScopedCounter("agent_memory_recalls_total", "Logical memory recall requests, counted once before retrieval.", "tenant_id")
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
