package telemetry

import "sync"

// Default holds the metrics every service records; Push sends it.
var Default = NewRegistry()

var (
	latency = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120, 300}
	ttft    = []float64{0.1, 0.25, 0.5, 1, 2, 4, 8, 16, 32, 64}
)

// MCP: sessions, tool calls, latency and the size of what agents receive.
var (
	MCPSessions       = Default.Counter("mcp_sessions_total", "MCP sessions started.", "tenant_id")
	MCPToolCalls      = Default.Counter("mcp_tool_calls_total", "MCP tool and prompt calls by outcome (ok, error).", "tenant_id", "mcp_tool", "outcome")
	MCPToolDuration   = Default.Histogram("mcp_tool_duration_seconds", "MCP tool execution time.", latency, "tenant_id", "mcp_tool")
	MCPResponseTokens = Default.Counter("mcp_response_tokens_total", "Estimated tokens MCP tools returned to agents.", "tenant_id", "mcp_tool")
)

// LLM calls made by CodeSynapse itself (reviews, triage, briefings,
// memory extraction).
var (
	LLMRequests = Default.Counter("llm_requests_total", "LLM requests by outcome.", "tenant_id", "llm_provider", "llm_model", "task", "outcome")
	LLMInput    = Default.Counter("llm_tokens_input_total", "Prompt tokens consumed.", "tenant_id", "llm_provider", "llm_model", "task")
	LLMOutput   = Default.Counter("llm_tokens_output_total", "Completion tokens generated.", "tenant_id", "llm_provider", "llm_model", "task")
	LLMSaved    = Default.Counter("llm_tokens_saved_total", "Input tokens served from the provider's prompt cache.", "tenant_id", "llm_provider", "llm_model", "task")
	LLMCost     = Default.Counter("llm_cost_estimated_dollars", "Estimated cost in US dollars from the configured token prices.", "tenant_id", "llm_provider", "llm_model")
	LLMDuration = Default.Histogram("llm_request_duration_seconds", "LLM request time.", latency, "tenant_id", "llm_provider", "llm_model")
	LLMTTFT     = Default.Histogram("llm_time_to_first_token_seconds", "Time to the first streamed token (streaming providers only).", ttft, "tenant_id", "llm_provider", "llm_model")
)

// Databases.
var (
	DBDuration     = Default.Histogram("db_query_duration_seconds", "Database round-trip time by system and operation.", latency, "tenant_id", "db_system", "operation")
	DBErrors       = Default.Counter("db_query_errors_total", "Failed database round trips.", "tenant_id", "db_system", "operation")
	VectorMatches  = Default.Counter("db_vector_matches_count", "Vectors returned by vector searches.", "tenant_id", "db_system")
	GraphTraversed = Default.Counter("db_graph_nodes_traversed", "Rows (nodes, edges, facts) returned by graph reads.", "tenant_id", "db_system")
)

// Agent memory and context offloading.
var (
	MemoryFacts      = Default.Counter("memory_facts_saved_total", "Facts written to agent memory, by source (tool, offload).", "tenant_id", "source")
	OffloadTurns     = Default.Counter("memory_offload_turns_total", "Conversation turns handed to background extraction.", "tenant_id")
	OffloadTokens    = Default.Counter("memory_offload_tokens_total", "Estimated context tokens offloaded from agent sessions.", "tenant_id")
	MemoryRecalls    = Default.Counter("memory_recall_total", "Memory recalls by mode (text, vector).", "tenant_id", "mode")
	EmbeddedEntities = Default.Counter("memory_entities_embedded_total", "Memory entities embedded for recall by meaning.", "llm_model")
)

// API and ingestion.
var (
	HTTPRequests   = Default.Counter("http_requests_total", "API requests by route pattern and status class.", "tenant_id", "route", "method", "code")
	HTTPDuration   = Default.Histogram("http_request_duration_seconds", "API request time by route pattern.", latency, "route", "method")
	IngestRuns     = Default.Counter("ingest_runs_total", "Repository indexing runs by outcome.", "db_system", "outcome")
	IngestDuration = Default.Histogram("ingest_duration_seconds", "Repository indexing time, extraction and storage.", latency, "db_system")
	IngestNodes    = Default.Counter("ingest_nodes_total", "Nodes written by indexing.", "db_system")
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
