# Tenant telemetry and four dashboard groups

!!! tip "In plain words"
    Telemetry means numbers about how the system behaves, such as how many requests ran and how long they took. This page explains what CodeSynapse measures, how every measurement is tied to the right organisation, and the rule that no prompts, code, file paths or personal details ever go into a metric.

Status: implemented in SaaS phase 4e, after access isolation (4d). The existing Compose stack supplies VictoriaMetrics for development; this phase adds instrumentation and dashboard coverage. Kubernetes wiring waits for the final deployment phase (6a). The request for OTel supersedes the earlier decision to defer it. The approved OTel v1.47.0 SDK/exporter is the default export path; validation and limits are recorded below.
Session-scoped tool execution and chat memory must also follow [phase 4g](SESSION_CONTEXT_PLAN.md): immutable runtime identity, inherited parent context and tenant/session/namespace filtering. This requirement does not turn session IDs into metric labels.

Codex and Cursor lifecycle observations also use the existing bounded registry and audit store. The [assistant hook collection contract](operations/assistant-hooks.md#collection-contract) documents every field, native event, identity check, retention rule, delivery limit and exported `agent_hook_*` instrument. Client-reported hook activity stays separate from server-measured MCP and LLM execution.

## Attribution and export

Carry a typed telemetry context through accepted MCP tools, database operations, model calls and queued tasks. Every tenant operation in these dashboards has the following attributes:

| Attribute | Trusted source |
|---|---|
| `tenant_id` | Authenticated organisation context, validated by `TenantLabel`; never a tool argument |
| `mcp_tool` | Accepted tool name through `ToolLabel`; fixed `none` for API/background work |
| `llm_provider` | Configured provider for the measured operation; fixed `none` when there is no model |
| `llm_model` | Configured model for that operation; fixed `none` when there is no model |

Tenant IDs are bounded, validated identifiers, but cardinality still grows with tenants. Cap active series and allowed label combinations per instrument, including histogram buckets. These registry budgets are per process; backend series still multiply with the number of replicas, which operators must account for separately. Do not preallocate the Cartesian product. Preserve the tenant dimension: never merge tenants into a tenant-visible overflow bucket. Drop excess series with a separate platform-only dropped-series counter, and test that registry overflow cannot create unscoped data.

A tool can call several models: attribute each model operation separately. Count the tool's transport bytes once, using provider/model `none`, rather than copying those bytes into each model series. Durable fact counts likewise use `none` for tool/provider/model because they describe stored state rather than one call. Unauthenticated requests and startup work have no trusted tenant: record separate bounded platform metrics, not a fabricated tenant ID. Cross-tenant jobs split observations by the tenant whose work they perform.

Keep repository names, paths, prompts, code, hostnames, user/key/session IDs and client-supplied model/tool names out of metric attributes. Use redacted logs for request diagnostics. Resources identify `service.name`, version and deployment environment; replica identity must distinguish cumulative streams without adding user-controlled labels.

Keep `internal/telemetry` as the bounded aggregation source and expose its cumulative snapshots through an official OTel SDK `Producer`, `PeriodicReader` and `MeterProvider`. The registry enforces attribute allowlists and per-instrument budgets before aggregation, including explicit histogram buckets. Do not create a second SDK accumulator for the same observations; the producer path avoids SDK overflow series that could lose tenant scope. Export cumulative OTLP/HTTP metrics directly to the internal VictoriaMetrics `/opentelemetry/v1/metrics` endpoint, or through an operator-managed Collector. Keep authentication in secrets, queues/timeouts/retries bounded, and shutdown flushes time-limited. A telemetry outage must not block requests. No public scrape listener is added. The [VictoriaMetrics OTLP integration](https://docs.victoriametrics.com/victoriametrics/integrations/opentelemetry/) documents the endpoint and temporality requirements; [OTel metric views](https://opentelemetry.io/docs/specs/otel/metrics/sdk/) provide attribute filtering and cardinality controls.

`CODESYNAPSE_METRICS_EXPORTER` selects `otlp` (default) or the legacy `prometheus` push format. Both use the existing `CODESYNAPSE_METRICS_URL`, `CODESYNAPSE_METRICS_USER` and environment-only password. OTLP uses cumulative sums and explicit histograms, with a zero primer for newly exported cumulative streams so VictoriaMetrics counts a large first sample. Resources identify `codesynapse-api` or `codesynapse-mcp`, application version, `CODESYNAPSE_ENVIRONMENT` (default `unknown`) and an opaque process instance UUID. VictoriaMetrics 1.110 preserves the dotted resource label names `service.name`, `service.version`, `service.instance.id` and `deployment.environment.name`; `job` and `instance` aliases are also exported. Instance identity changes on process restart and is never the immutable chat/session ID planned in phase 4g.

Select one exporter per metric stream during migration. Do not push the same observations through the current Prometheus-text path and OTLP into the same database. Verify exported names, suffixes, units and histogram series against dashboard queries before switching. Preserve first-sample and counter-reset behavior already covered by the VictoriaMetrics integration test.

## Dashboard 1: tokens and model cost

| Metric | Instrument and meaning |
|---|---|
| `llm_tokens_input_total` | Counter, prompt tokens reported by the provider; normalize whether cache-read tokens are included |
| `llm_tokens_output_total` | Counter, completion tokens reported by the provider |
| `llm_tokens_saved_total` | Counter, input tokens served from an actual provider prefix/prompt cache; do not infer hits from repeated text |
| `llm_cost_estimated_dollars` | Counter, estimated USD from the configured input/output/cache-read/cache-write rates at call time |
| `llm_cost_saved_estimated_dollars_total` | Counter, comparable uncached input cost minus actual cached-input cost, calculated with that call's prices |
| `llm_usage_unknown_total`, `llm_pricing_missing_total` | Counters of calls whose usage or configured prices are missing; their cost is unknown |
| `llm_read_seconds_total`, `llm_write_seconds_total`, `llm_read_tokens_total`, `llm_write_tokens_total` | Counters, streamed answers only: seconds before the first token and after it, with the prompt and answer tokens of those same answers. Tokens divided by seconds is a model's read and write speed. Local OpenAI-compatible servers are asked to stream (`stream_options.include_usage`); one that refuses is asked whole from then on and gets no speed. Loading a model into memory happens before its first token, so read speed right after a load is low |
| `llm_context_used_ratio` | Histogram by task: prompt plus answer tokens over the provider's configured `context_window` |

`GET /api/v1/stats/models?range=1h|6h|24h|7d|30d` returns one row per model for the window: requests and failures, input and output tokens, average prompt, read and write tokens per second, the median and 95th percentile of context use, and the same counts per task, with `local` and `context_window` from the configuration. Its queries are fixed MetricsQL with the caller's organisations as `extra_filters`, like the series panels.

`ask_local` adds two daily counters to the usage buckets: `(local:read)`, the tokens a local model read, and `(local:avoided)`, the files' tokens (bytes/4) minus the answer the agent got instead. The second joins tokens avoided on Statistics. `local_draft` adds `(local:drafts)`, one per draft, and the tokens the model read to `(local:read)`; it adds nothing to `(local:avoided)`, because the agent still reads and checks every draft and what writing it saved is not measured.

Cached tokens can still be billed and sent over the network. Label the panel “Input tokens served from cache”; do not present it as tokens never sent. Keep graph-context reduction separate from provider caching. Provider usage omissions are “unknown”, not zero cost. Track missing-price calls separately; do not silently price them at zero. Retry attempts count actual usage when reported. Do not claim visibility into tokens consumed by an external agent's own model.

## Dashboard 2: network and cloud bandwidth

| Metric | Instrument and measurement point |
|---|---|
| `net_bytes_received_total` | Counter, MCP request-body bytes actually read by transport middleware |
| `net_bytes_sent_total` | Counter, MCP response-body bytes successfully written, including stream frames |
| `cloud_bytes_sent_total` | Counter, encoded request-body bytes actually consumed by cloud LLM HTTP transports, including retry attempts |
| `cloud_bytes_saved_total` | Counter, estimated avoided request-body bytes for a documented, equivalent cloud fallback that was skipped |

Choose one representation and document it: application payload bytes, excluding HTTP headers/TLS/TCP overhead. If compression is used, instrument at a defined side of it and keep baseline and actual counts comparable. These are not provider bandwidth invoices. Count partial writes, cancellations and retries correctly without buffering full prompts or response streams.

Local execution alone does not prove avoided cloud traffic. Increment `cloud_bytes_saved_total` only when a configured cloud fallback would have received a known serialized payload and equivalent local handling avoided it. Do not serialize/send otherwise prohibited source to invent a baseline. A pipeline that only ever used local embeddings has no cloud-savings baseline: show “not measured”. No provider prefix-cache token count is converted into byte savings.

Local/cloud classification comes from provider configuration and `ProviderConfig.Local()`, not client input. Instrument only through `services/api/internal/llm` so redaction and routing cannot be bypassed. MCP framing bytes unassignable to an accepted tool use a bounded transport category instead of a client-provided name.

## Dashboard 3: database performance

| Metric | Instrument and interpretation |
|---|---|
| `db_query_duration_seconds` | Histogram, client-observed request latency; fixed `db_system` values `nebulagraph`, `milvus` and `postgres`; fixed operation names (`read`/`write` for NebulaGraph, the REST path for Milvus, the statement verb for PostgreSQL) |
| `db_vector_matches_count` | Existing cumulative counter of returned matches; split candidate counts and final fused counts into separately named counters if needed |
| `db_graph_nodes_traversed` | Existing legacy name currently counts returned rows, including edges/facts; it does not measure nodes visited internally |

Add a correctly named `db_graph_rows_returned_total` and migrate panels while documenting the legacy series. Actual traversal counts require verified backend profiling support and a defined counting method; expose them separately only when measured, never substitute returned rows or run expensive profiling on every production query. Add error/timeout rates, p50/p95/p99 latency and request volume; configured SLA targets are not measured benefits.

## Worker queue depth

`platform_codesynapse_jobs_waiting{kind}` is a gauge of the jobs a worker could claim now (queued, or running with an expired lease), per job kind. It has no tenant, so it exports as a platform series. The API process that holds the `api-loops` lease sets it every 15 seconds (`reportJobQueue`), so replicas never report it twice; a kind that drained reports 0. It is the signal an autoscaler adds workers on; `platform_codesynapse_jobs_active{kind}` also counts the jobs a process is running. Scale workers on active, not waiting: waiting is 0 while every worker is busy, and scaling down then stops a running job. A kind that never had a job has no series, which an autoscaler reads as 0.

## Dashboard 4: memory and offloading

| Metric | Instrument and meaning |
|---|---|
| `agent_memory_facts_count` | Observable gauge, current durable unique triples per tenant; changes with deletion, expiry and deduplication |
| `agent_memory_recalls_total` | Counter, one logical recall per accepted request, not once for each BM25/vector leg |
| `memory_facts_saved_total` | Retain as a write-operation counter; never label it as current stored facts |
| `memory_offload_turns_total`, `memory_offload_tokens_total` | Retain counts/estimates of offloaded context, separately from billed model-token savings |
| `flow_step_calls_total`, `flow_step_tokens_total` | Per step of the chat-memory flow (fixed `step` and `kind` values in `internal/telemetry/flow.go`, tenant-scoped); see docs/operations/chat-memory-flow.md |

Read durable counts from bounded aggregate queries in pages of 100 namespaces. Collection has a 20-second deadline and a ceiling of 2,048 current tenants, reserving additional registry slots for deletion observations. Every page must succeed before publication; a timeout, invalid page or exceeded tenant budget leaves the complete previous snapshot unchanged so it ages stale. No partial tenant set is published. API replicas collect bounded snapshots every 30 seconds and emit `agent_memory_facts_observed_at_seconds`. A panel first retains the newest observation timestamp per tenant, then takes the maximum count among replicas with that timestamp, and only then sums tenants. This avoids adding duplicate replica state or preferring an older, larger count after deletion. Observations older than 90 seconds are omitted; a failed collection leaves its timestamp unchanged. A completed enumeration publishes zero for previously observed tenants that disappeared. Count and observation-time changes are one registry batch: both OTLP and Prometheus snapshots wait for the complete batch. The dashboard also requires the count and observation-time samples to have matching export timestamps. This rejects an old count paired with a newer observation time if the backend accepted only part of an export.

## MetricsQL panels and tenancy

These are fixed server-side panel templates. Tenant access is enforced by the existing VictoriaMetrics `extra_filters` derived from the authenticated scope, including both numerator and denominator of ratios. Never interpolate a raw `$tenant` into MetricsQL. Ordinary panels remain tenant-filtered even for platform administrators. The explicit `platform_*` panels and the admin panels of `/api/v1/admin/stats/series` require a platform capability and expose no repository contents. Empty authorised-tenant sets match nothing, including a platform administrator with no organisation memberships.

Cloud egress avoided, estimated percent over one hour:

```metricsql
100 * sum(increase(cloud_bytes_saved_total[1h]))
/ ((sum(increase(cloud_bytes_sent_total[1h]))
    + sum(increase(cloud_bytes_saved_total[1h]))) > 0)
```

Present “no comparable traffic” when the denominator is zero or the baseline is unavailable; the denominator filter removes zero-traffic results. Pre-initialize paired counters for admitted label sets so absent series do not change the ratio's meaning.

Estimated token-cost savings over 24 hours:

```metricsql
sum(increase(llm_cost_saved_estimated_dollars_total[24h]))
```

The proposed `tokens_saved * 0.000003` is valid only for a hypothetical $3/million-token difference between uncached and cached rates. It is not a general provider price. Calculate savings at observation time so later price changes do not rewrite history.

MCP ingress and egress rates, bytes/second:

```metricsql
sum(rate(net_bytes_received_total[5m]))
sum(rate(net_bytes_sent_total[5m]))
```

Database p95 latency:

```metricsql
histogram_quantile(0.95,
  sum by (le, db_system) (rate(db_query_duration_seconds_bucket[5m])))
```

Keep time windows and panel IDs allowlisted in `services/api/internal/api/series.go`; the UI uses `useApiGet` and the existing plain SVG/CSS charts. The Statistics page groups panels into these four categories with units, missing-data states and “estimated” labels where required. A measured zero remains a plotted value; absent series display “No measurements recorded”, and an absent cloud baseline displays “Not measured”. Cost values retain sub-cent precision in the API; small nonzero amounts display as `<$0.001` rather than `$0.000`. Database failures and their timeout subset are not added into a misleading combined headline.

## Traces

One request is followed through the API, its job, the worker, NebulaGraph and model calls (docs/SAAS.md, 5e).

- **Export:** OTLP/HTTP to `CODESYNAPSE_TRACES_URL`, the full traces endpoint. Compose runs VictoriaTraces (Apache-2.0) at `http://victoriatraces:10428/insert/opentelemetry/v1/traces` with the metrics login; its UI is at `http://localhost:10428/select/vmui`.
  - `CODESYNAPSE_TRACES_SAMPLE` keeps that share of new traces (default 1); children follow their parent's decision.
  - Spans are batched in a bounded queue, so a slow or absent trace store drops spans and never blocks work.
  - Without a URL nothing is exported, but trace context still crosses processes.
- **Spans:**
  - API requests, named by route pattern (`POST /api/v1/repos/{repo}/sync`), with method, route and status;
  - MCP HTTP requests and one span per MCP method, with the tool or prompt name;
  - `job <kind>`, with kind, opaque job ID and attempt, and its `job.prepare`, `job.ingest` and `job.publish` phases;
  - `nebula read`/`write`;
  - `llm complete`, with the configured provider and model and the token counts.
- **No content:** spans never carry paths, repository names, people, prompts, code, arguments, scripts or bindings. A test checks that no span names or attributes contain the repository name.
- **Across processes:** the queueing request's `traceparent` is stored on the job row (migration 0015). The worker that runs the job, now or after a restart, continues the trace.
- **Trust:** the API continues an incoming `traceparent` only from `CODESYNAPSE_TRUSTED_PROXIES` (the dashboard). The MCP server never continues one: an outside caller could otherwise force sampling or splice into other traces.
- **One ID:** with a valid span, the request ID (`X-Request-ID`, the audit entry's `request_id`) is the trace ID. Log lines written with a context carry `trace_id` and `span_id`. An audit entry, a log line and a trace therefore lead to each other.

## Logs

The services ship their log lines to VictoriaLogs (Apache-2.0) as well as writing them to stderr, so lines from every replica can be searched in one place, next to their traces.

- **Export:** JSON lines to `CODESYNAPSE_LOGS_URL`, the base URL (the shipper appends `/insert/jsonline`), with `CODESYNAPSE_LOGS_USER` and `CODESYNAPSE_LOGS_PASSWORD`. Compose runs VictoriaLogs at `http://victorialogs:9428` with the metrics login; its UI is at `http://localhost:9428/select/vmui`. Without a URL, services log to stderr only.
- **Fields:** each shipped line is JSON whatever `--log-format` says, and adds `service`, `environment`, `instance` (the opaque process ID metrics use) and `version`. The streams are `service`, `environment` and `instance`. Lines written with a context carry `trace_id` and `span_id`, as on stderr.
- **Bounds:** lines wait in a 4 MiB queue and go out every two seconds in batches of at most 1 MiB. A line over 64 KiB, a full queue or a failed batch drops lines; logging never waits on the network. Drops are counted in `platform_log_lines_dropped_total` (`reason`: `too_long`, `queue_full`, `send_failed`). Shipping failures are reported on stderr only, at most once a minute, so a failing store cannot feed itself. On shutdown what is left is sent within five seconds.
- **Personal data:** the lines are the ones the services already write. That includes the structured `audit` line, which names the actor's email and client address. VictoriaLogs is therefore an operator store: internal, password protected, never published beyond localhost in compose, reachable only from the services in the chart (network policy), and it keeps lines `LOGS_RETENTION` (compose) or `victorialogs.retention` (chart), 30 days by default.
- **Not the audit log:** the hash-chained audit table in PostgreSQL stays the record of truth; VictoriaLogs is for searching. A shipped audit line can be dropped like any other.

## Implementation and checks

The approved dependency set is `go.opentelemetry.io/otel/sdk`, `go.opentelemetry.io/otel/sdk/metric`, and `go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp`, each at `v1.47.0`, with matching OTel API modules and required transitive dependencies. The upstream licence is Apache-2.0 with BSD-3-Clause portions. Matching API and transitive dependencies are recorded in `go.mod` and `go.sum`.

On an Apple M5 Pro, two recent recording benchmark runs measured 52–54 ns, 16 bytes and one allocation per unscoped counter update, versus 245–344 ns, 112 bytes and two allocations with trusted attribution. Earlier quieter runs measured 235–239 ns for the attributed path. These are local microbenchmarks, not request-latency or production SLA measurements.

A separate warm-export benchmark used 100 tenants with one counter and one histogram each, sending to an in-process HTTP sink. Across two runs, the legacy Prometheus path took 22–45 ms with about 64.7 MB and 88,600 allocations per export; OTLP took 1.04–1.33 ms with about 486.5 KB and 4,135 allocations. The benchmark includes local collection, serialization and HTTP handling, excludes real network and VictoriaMetrics processing, and does not measure the first-export primer. It does not establish production bandwidth or latency savings.

Throwaway VictoriaMetrics checks cover cumulative OTLP first samples, histogram values, gauge values, process restart/replica separation, trusted attributes and resource labels. Dashboard integration checks cover fresh/stale gauge cohorts, duplicate replicas, scoped cloud ratios, absent/zero baselines, small nonzero dollar values and all fixed MetricsQL expressions. Concurrent exporter tests cover atomic count/timestamp snapshots, and backend integration tests reject unmatched sample timestamps after a partially accepted export. Shutdown uses a five-second deadline; exporter failure and retry tests verify recording remains independent of export.

Validation passed: `just test` (the full Go race suite), `just lint` (zero issues), `go vet ./...`, and `just web-check` after incorporating the merged main branch. Native OTLP and dashboard queries passed against a throwaway VictoriaMetrics instance. Database contracts and the offline graph review covered the changed store paths; startup and MCP transport tests covered the flagged entry points. Formatting checks confirmed that zero values, missing measurements, byte units and small nonzero dollar values remain distinct.

The four dashboard groups use fixed tenant-filtered queries. The implementation retains the legacy `db_graph_nodes_traversed` alias for compatibility while panels use `db_graph_rows_returned_total`; no removal date is scheduled in this phase. Current durable fact counts are separate from saved-fact operation counters; logical recalls count once per accepted request.

No equivalent cloud fallback currently exists, so avoided cloud bytes remain unmeasured. Missing provider usage or prices leave cost unknown. Local benchmark results do not establish a production SLA. Immutable session enforcement, ingestion reporting and bootstrap-root separation remain in their own planned phases; Kubernetes manifests remain deferred to final deployment.

## Ingestion and annotation extension

[Phase 4i](INGEST_TRANSPARENCY_PLAN.md) adds the requested file counters, pruning/token reports and annotation-job visibility after [phase 4h repository policy](REPO_CONFIG_PLAN.md). Its `repo` metric label is a bounded, opaque repository-entry ID from authenticated server state; names and paths remain outside time-series labels. Exact per-run reports remain available when a repository is not admitted to per-repository metric series.

## Root governance follow-up

The requested bootstrap-root separation belongs to the planned [phase 4d follow-up](ROOT_BOOTSTRAP_PLAN.md). Root may inspect platform telemetry, health and audit logs, but must provision a distinct standard owner and require a fresh standard-user login before organisation work. Phase 4e's platform panels do not implement or weaken that future restriction.

## Metrics coverage plan

Status: phases 1 (platform health), 3 (jobs and indexing), 4 (outbound calls and security events), 5 (sharper existing families) and 6 (web server) are implemented; the others are proposed. This section lists what the services measure today, where measurement is missing or thin, and the instruments that would close the gaps, in the order we would build them. Every proposal follows the rules above and the Telemetry rules in `AGENTS.md`. Where a rule blocks a useful label, the proposal says so instead of bending the rule.

### What is measured today

All instruments live in `internal/telemetry/metrics.go`, plus `flow.go`, `logship.go` and the durable file totals in `ingest.go`. Scoped instruments always carry `tenant_id`, `mcp_tool`, `llm_provider` and `llm_model`, filled from the context. A sample without a trusted tenant is exported as `platform_<name>`. Each instrument holds at most 5,000 label combinations per process (`maxSeries`). A histogram shares that budget with its buckets, so a 15-bucket histogram holds about 277 combinations. Excess combinations are dropped and counted in `telemetry_dropped_series_total{metric}`.

| Area | Instruments | Recorded in | Read by |
|---|---|---|---|
| MCP tools | `mcp_sessions_total`, `mcp_tool_calls_total{outcome}`, `mcp_tool_duration_seconds`, `mcp_response_tokens_total`, `net_bytes_received_total`, `net_bytes_sent_total` | `services/mcp/internal/mcpserver/telemetry.go`, `transport_metrics.go` | Statistics → Activity: `sessions`, `tool_calls`, `tool_errors`, `tool_refusals`, `tool_latency`, `served_tokens`, `bandwidth`, `local_answers` (`mcp_local_answers_total{outcome}`, recorded in `services/api/internal/api/usage_local.go`); Administration → Statistics → Usage (`/api/v1/admin/stats/series`) |
| Models | `llm_requests_total{task,outcome}`, `llm_tokens_input/output/saved_total`, `llm_cost_estimated_dollars`, `llm_cost_saved_estimated_dollars_total`, `llm_usage_unknown_total`, `llm_pricing_missing_total`, `llm_request_duration_seconds`, `llm_time_to_first_token_seconds`, `llm_read/write_seconds_total`, `llm_read/write_tokens_total`, `llm_context_used_ratio{task}`, `llm_compressed_bytes_total`, `cloud_bytes_sent_total` | `services/api/internal/llm/llm.go`, `transport.go` | Activity: `llm_*`, `cloud_traffic`; Statistics → Models (`GET /api/v1/stats/models`); the `llm_cost` admin panel under Administration → Statistics → Usage |
| Agent gateway | `gateway_requests_total{route,class}`, `gateway_bytes_total{route,direction}`, `gateway_local_tokens_total{kind}` (input, output, cached), `gateway_local_unmetered_total`, `gateway_request_duration_seconds{route}`, `gateway_fallthrough_total{reason,route}`, `gateway_queue_wait_seconds`, `gateway_local_answer_seconds{outcome}`, `gateway_first_byte_seconds{route}` | `services/api/internal/api/gateway.go` | Statistics → Models; Activity: `gateway_latency`, `gateway_latency_avg`, `gateway_timeouts`, `gateway_queue_wait`, `gateway_fallthrough` |
| Databases | `db_query_duration_seconds{db_system,operation}`, `db_query_errors_total`, `db_query_timeouts_total`, `db_graph_rows_returned_total` (and the legacy `db_graph_nodes_traversed`), `db_vector_matches_count` | `internal/store/nebulastore/client.go`, `internal/vector/milvus/milvus.go`, `internal/accounts/pgstore/trace.go` | Activity: `db_latency`, `db_latency_tail`, `db_requests`, `db_errors`, `db_rows` |
| Memory | `memory_facts_saved_total{source}`, `memory_offload_turns_total`, `memory_offload_tokens_total`, `memory_recall_total{mode}`, `agent_memory_recalls_total`, `memory_entities_embedded_total`, `agent_memory_facts_count`, `agent_memory_facts_observed_at_seconds`, `platform_memory_facts_collection_errors_total`, `flow_step_calls_total`, `flow_step_tokens_total` | `services/api/internal/offload`, `services/api/internal/recall`, `services/api/internal/api/memorymetrics.go`, `services/mcp/internal/mcpserver/memory.go` | Activity: `memory`, `memory_facts`; the flow counters have no panel |
| Assistant hooks | `agent_hook_events_total`, `agent_hook_duration_seconds`, `agent_hook_bytes_total` | `services/api/internal/api/agentactivity.go` | Activity: `agent_hook_events`, `agent_hook_bytes` |
| API | `http_requests_total{route,method,code}`, `http_request_duration_seconds{route,method}` | `services/api/internal/api/api.go` (API and worker processes) | No panel |
| Indexing | `ingest_runs_total{db_system,outcome}`, `ingest_duration_seconds`, `ingest_nodes_total`, `codesynapse_annotation_jobs_total`, `codesynapse_annotation_duration_seconds`, `codesynapse_files_scanned/indexed/skipped_total{repo}` (durable totals, fenced exporter) | `services/api/internal/api/jobs.go`, `ingestion_metrics.go`, `services/api/internal/annotation/worker.go` | Repository → Ingestion: `ingest_files`, `ingest_skips`, `annotation_jobs`; the run counters have no panel |
| Job queue | `platform_codesynapse_jobs_waiting{kind}`, `platform_codesynapse_jobs_active{kind}`, set by the `api-loops` holder | `services/api/internal/api/jobqueue_metric.go` | Worker autoscaling in the Helm chart; Administration → Statistics → Jobs: `jobs_queue` |
| Worker roles | `platform_codesynapse_worker_roles{role}` (`ingest`, `ai`, `fix`), 1 per role a worker process runs, set at start-up; no tenant | `internal/telemetry/metrics.go` (`SetWorkerRoles`), `services/api/internal/api/api.go` | Administration → Statistics → Jobs: `jobs_worker_roles` |
| Web server | `platform_web_requests_total{route,method,status_class}`, `platform_web_request_duration_seconds{route}`, `platform_web_proxy_requests_total{outcome}` (OTel JS SDK, platform only) | `services/web/server/utils/webMetrics.ts`, `services/web/server/plugins/metrics.ts`, `services/web/server/api/cg/[...path].ts` | Administration → Overview: `platform_web_requests`, `platform_web_latency`, `platform_web_proxy` |
| Telemetry itself | `telemetry_dropped_series_total{metric}`, `platform_log_lines_dropped_total{reason}`, `platform_ingest_metric_export_rejections_total{reason}`, `platform_codesynapse_ingest_metric_series_dropped_total` | `internal/telemetry` | No panel |

Traces cover API requests, MCP methods, jobs with their `job.prepare`/`job.ingest`/`job.publish` phases, NebulaGraph reads and writes, and model calls. Daily `:McpUsage` buckets (`internal/usage`) hold per-repository tool counts, bridge local answers and hook-reported workflow counts. They feed the Statistics totals and are not OTel metrics.

### Gaps

Each gap is a question an operator or an organisation cannot answer from metrics today. All were checked against the code.

1. **Background jobs.** Only queue depth exists. There is no count of finished jobs by kind and outcome, no time a job waited before a worker claimed it, and no run time per kind. Only indexing (`ingest_runs_total`) and annotation have outcome counters; AI, fix, clone and switch jobs have none. A job left for another process (`errLeaseLost`, `leftBehind`) is not counted.
2. **Leases.** Nothing records whether a process holds the `api-loops` lease, how often it changes hands, or how often durable job leases fail to renew. A cluster where no replica runs the scheduler looks healthy.
3. **Indexing detail.** `ingest_duration_seconds` is one number per run. Phase times exist only as trace spans. There is no split by language or by cause of failure.
4. **Outbound calls.** git clone, fetch, `ls-remote` and push (`internal/gitx`), OSV lookups (`internal/vuln`), deps.dev lookups (`internal/license/depsdev.go`) and forge listings (`internal/forge`) have no metrics. A slow or failing provider shows up only as a failed or slow job.
5. **Sign-in.** Password logins, API-key and session checks, rate-limit refusals (`TakeAttempt`) and SSO callbacks have no metrics. They are in the audit log only, so a burst of failed logins cannot drive an alert.
6. **Audit log.** Audit writes, their failures (which refuse the request, or withhold an MCP result) and archive sealing (`internal/auditarchive`) are not measured.
7. **MCP outcomes.** `mcp_tool_calls_total` has `ok` and `error` only, so a permission refusal looks like a bug. Bridge local answers (`POST /api/v1/usage/local`) are in the daily buckets and the audit log, not in the minute-level series, so the share of calls answered on the developer's machine is not visible next to server calls.
8. **Gateway.** `recordGateway` receives the request duration and drops it. When the local model cannot answer and the request falls through to the cloud, only the final route is counted, not why it fell through.
9. **Model errors.** `llm_requests_total` has `ok` and `error`. Timeouts, rate limits and refused requests are not told apart.
10. **Process health.** There are no Go runtime or process metrics (goroutines, heap, GC pauses, CPU), no connection-pool metrics, and no counter for failed metric exports, which are only logged.
11. **Series caps.** Some instruments hit the 5,000 cap early. `mcp_tool_duration_seconds` has about 70 accepted tool and prompt names, so at about 277 combinations it starts dropping once roughly four organisations use every tool. `http_request_duration_seconds` has about 200 routes and fills its budget with one or two active organisations. Drops are counted, but no panel or alert shows them.
12. **Unused data.** API request metrics, indexing run counters, chat-memory flow counters, the job queue gauges and the dropped-series counters are recorded but have no panel.

The web dashboard (Nuxt) had no metrics of its own: failures that never reached the API, such as the API being unreachable, were not seen. Phase 6 added them (see "Phase 6: what shipped").

### Proposed instruments

Conventions for new instruments:

- Extend an existing family where one exists (`mcp_`, `llm_`, `gateway_`, `http_`, `db_`). New subsystems use the `codesynapse_` prefix, like `codesynapse_jobs_active`.
- Tenant work uses `ScopedCounter`/`ScopedHistogram` and the context-aware methods. The tenant comes from `telemetry.WithTenant` on the authenticated caller or the job's stored identity, never from arguments. Work without a trusted tenant becomes `platform_<name>` automatically. Process state with no tenant uses an unscoped instrument with a `platform_` name.
- Every label has a fixed value set, enforced by an allowlist next to the instrument, as `flow.go` does for steps. Anything else maps to `other` or is dropped.
- Long-running work uses a coarse bucket set, `slow` = 1, 5, 15, 60, 300, 900, 1800, 3600 and 7200 seconds (9 buckets, about 416 combinations per histogram).
- Where a histogram per label value would hit the cap, a pair of counters (`..._seconds_total` and `..._total`) gives the average instead, as `llm_read_seconds_total` already does.

The alerts named in the last column are the rule list for phase 2 (alerting); an alert on an instrument from a later phase is added with that phase. Until phase 2 ships they are future work.

| # | Instrument | Kind, unit | Labels (bounded values) | Scope | Combinations per tenant | Recorded in | Panel or alert (alerts: phase 2 rules) |
|---|---|---|---|---|---|---|---|
| 1 | `codesynapse_jobs_finished_total` | counter, jobs | `kind` (`ingest`, `sync`, `clone`, `switch`, `ai`, `ai-auto`, `fix`, `other`), `outcome` (`ok`, `error`, `left_behind`, `cancelled`) | tenant | 32 | the job runner in `services/api/internal/api/jobs.go`, where a job ends | Statistics → Sync: "Background jobs"; alert on the error share |
| 2 | `codesynapse_job_wait_seconds` | histogram (`slow`), s | `kind` | tenant | 6 | on claim: claim time minus queue time | "Job wait"; alert when p95 wait passes 15 minutes |
| 3 | `codesynapse_job_duration_seconds` | histogram (`slow`), s | `kind` | tenant | 6 | the job runner, next to item 1 | "Job run time" |
| 4 | `platform_codesynapse_lease_events_total` | counter, events | `lease` (`api_loops`, `job`), `event` (`acquired`, `lost`, `renew_failed`) | platform | 6 | `services/api/internal/api/singleton.go`, durable job lease renewal in `jobs_durable.go` | platform panel; alert on `lost` |
| 5 | `platform_codesynapse_loops_lease_held` | gauge, 0 or 1 | none (the instance comes from the resource) | platform | 1 | `singleton.go` | alert when the sum over fresh instances is not 1 for 2 minutes |
| 6 | `codesynapse_ingest_phase_seconds_total`, `codesynapse_ingest_phase_total` | counters, s and runs | `phase` (`prepare`, `ingest`, `publish`), `outcome` (`ok`, `error`) | tenant | 12 | the `phase` helper in `jobs.go` that already opens the spans | "Indexing time by phase" |
| 7 | `codesynapse_ingest_extract_seconds_total` | counter, s | `language`: one fixed value per parser in `internal/ingest` (`go`, `python`, `jsts` for JavaScript, TypeScript and Vue, `jvm` for Java and Kotlin, `rust`, `csharp`, `php`, `ruby`, `docs`, `config` for YAML, templates and Helm), plus `other` | tenant | 11 | each extractor in `internal/ingest`; divided by the durable file totals for an average | "Extraction time by language" |
| 8 | `codesynapse_git_operations_total` | counter, operations | `operation` (`clone`, `fetch`, `ls_remote`, `push`), `outcome` (`ok`, `error`, `no_access`, `redirected`, `timeout`) | tenant; platform for operator mounts | 20 | `gitx.Clone`, `FetchCheckout`, `PushBranch` and the access check, mapping the `gitx` sentinel errors | "Git operations"; alert on the `error` and `timeout` share |
| 9 | `codesynapse_git_operation_seconds` | histogram (`slow`), s | `operation` | tenant | 4 | same | "Git operation time" |
| 10 | `codesynapse_lookups_total` | counter, requests | `service` (`osv`, `depsdev`, `forge_github`, `forge_gitlab`, `forge_forgejo`), `outcome` (`ok`, `error`, `timeout`, `rate_limited`) | tenant, else platform | 20 | `internal/vuln`, `internal/license/depsdev.go`, the `internal/forge` clients | "External lookups"; alert on `rate_limited` |
| 11 | `codesynapse_lookup_seconds` | histogram (`latency`), s | `service` | tenant, else platform | 5 | same | "External lookup latency" |
| 12 | `codesynapse_auth_attempts_total` | counter, attempts | `method` (`password`, `api_key`, `session`, `sso`), `outcome` (`ok`, `bad_credentials`, `rate_limited`, `suspended`, `expired`, `refused`) | platform before authentication; tenant once an organisation is known | 24 | `handleLogin` (`auth.go`), the bearer and cookie middleware, `handleSSOCallback` (`sso.go`) | platform "Sign-in" panel; alert on bursts of `rate_limited` and `bad_credentials` |
| 13 | `codesynapse_audit_writes_total` | counter, entries | `source` (`api`, `mcp`, `service`), `outcome` (`ok`, `error`) | tenant, else platform | 6 | `accounts.Service.Record`, the HTTP audit middleware, `services/mcp/internal/mcpserver/audit.go` | alert on any `error`: a failed write refuses the request or withholds an MCP result |
| 14 | `codesynapse_audit_write_seconds` | histogram (`latency`), s | `source` | tenant, else platform | 3 | same | "Audit write latency" |
| 15 | `platform_codesynapse_audit_seals_total` | counter, runs | `outcome` (`ok`, `error`, `nothing_to_seal`, `no_archive`) | platform | 4 | `auditarchive.Seal`, run under the `api-loops` lease | alert on `error` or repeated `no_archive` |
| 16 | `mcp_tool_calls_total` (existing) | counter | `outcome` gains `refused` (permission, scope or policy refusal) | tenant | +1 per tool | `services/mcp/internal/mcpserver/telemetry.go` | `tool_errors` stops counting refusals as errors |
| 17 | `mcp_local_answers_total` | counter, calls | `mcp_tool`: only `gittools.Tools`, `ghtools.Tools`, `coretools.Local`, `ask_local` and `local_draft`, registered with `AllowTools` in the API; `outcome` (`recorded`, `refused`) | tenant | about 30 | `services/api/internal/api/usage_local.go` | "Answered on the developer's machine" next to `tool_calls` |
| 18 | `gateway_request_duration_seconds` | histogram (`latency`), s | `route` (`local`, `cloud`, `rejected`) | tenant | 3 | `recordGateway`, from the duration it already receives | Statistics → Models |
| 19 | `gateway_fallthrough_total` | counter, requests | `reason`: a fixed set taken from the return points of `serve` in `services/api/internal/gateway/local.go` (see below); `route`: where the request went after all (`cloud`, or `local` on the OpenAI-compatible paths) | tenant | 14 × 2 | `local.go` | "Requests the local model did not answer" |
| 20 | `llm_requests_total` (existing) | counter | `outcome` gains `timeout`, `rate_limited` and `refused`, mapped from status codes and context errors, never from error text | tenant | +3 per model and task | `llm.go` | `llm_provider_failures` split by cause |
| 21 | `platform_go_goroutines`, `platform_go_heap_bytes`, `platform_go_gc_pause_seconds_total`, `platform_process_cpu_seconds_total` | gauges and counters | none (service and instance come from the resource) | platform | 1 each | a 15-second sampler of the standard library's `runtime/metrics` in `internal/app`, one per process | platform "Process health"; alerts on heap and goroutine growth |
| 22 | `platform_db_pool_connections`, `platform_db_pool_acquire_waits_total`, `platform_db_pool_acquire_wait_seconds_total` | gauge, connections; counters, acquires and s | `db_system` (`postgres`); the gauge also `state` (`idle`, `in_use`, `max`) | platform | 3 + 1 + 1 | pgx pool statistics, sampled with item 21 | alert when acquire waits keep increasing, or `in_use` equals `max` |
| 23 | `platform_telemetry_export_failures_total` | counter, exports | `exporter` (`otlp`, `prometheus`, `ingest`) | platform | 3 | the push and export loops started in `internal/app/metrics.go` | alert on any increase |

Process gauges (items 5, 21 and 22) are per instance. Panels show them per `service.instance.id` or take the maximum. They are never summed into a durable count. Item 5 is the one exception that is summed, because the sum means "how many replicas think they lead".

**Must not become labels**, in these or any later instruments: repository names, graph IDs and repository-entry IDs (the three `codesynapse_files_*_total` metrics stay the only exception), git hosts, forge owners and logins, remote URLs, branch names and revisions, file paths and file names, user IDs, emails, API key IDs, session IDs (runtime, login and transport), job IDs, IdP issuers and client IDs, client addresses and user agents, error messages and provider status text, prompt or answer text, model names a client asked for, and raw HTTP paths (route patterns only). The `language` label in item 7 names an extractor, never a file.

### Cap pressure

Gap 11 needs a decision before more tenant histograms are added. The options, from least to most change:

- Keep the cap, and add a platform panel and an alert on `telemetry_dropped_series_total`, so drops are visible.
- Give histograms with many label values a smaller bucket set, for example 8 buckets for `mcp_tool_duration_seconds`. That roughly doubles the combinations they hold.
- Replace the per-tool or per-route histogram with a counter pair (`..._seconds_total`, `..._total`) per tool or route for averages, and keep one histogram per tenant, without the tool or route, for percentiles.
- Cap each tenant's share of an instrument's budget, so one organisation cannot use it all up and push every other organisation's new series into the dropped count. Drops past a tenant's share are counted like any other drop.
- Allow a cap above 5,000 for named instruments, recorded in `metrics.go` with the reason.

Decided: the smaller bucket set for the high-cardinality families, together with the per-tenant cap (see decisions). The platform panel for `telemetry_dropped_series_total` comes with phase 1 either way.

### Phases

Ordered by value for the effort. Each phase is one pull request with its panels, a test that labels stay inside their sets, a test that a sample without a tenant becomes `platform_`, and the VictoriaMetrics integration check.

1. **See the platform** (small, done). Items 4, 5, 21, 22 and 23; a platform panel for `telemetry_dropped_series_total` and `platform_log_lines_dropped_total`; panels for the existing API request metrics and job queue gauges. No new tenant series.
2. **Alerting** (medium, shipped). Optional vmalert, under the rules in decision 1, with the first rules (see "Alerting" below). It comes after phase 1 because most of its first rules read phase 1 metrics; each later phase adds the rules named for its instruments in the table.
3. **Jobs and indexing** (medium, shipped). Items 1, 2, 3, 6 and 7, and panels for the existing `ingest_runs_total` and `ingest_duration_seconds` on Statistics → Sync.
4. **Outbound calls and security events** (medium, shipped). Items 8 to 15. Sign-in counters are mostly platform series, because most failures have no trusted tenant.
5. **Sharper existing families** (small, shipped). Items 16 to 20, and the cap decision applied to `mcp_tool_duration_seconds` and `http_request_duration_seconds`. New outcome values change what the `tool_errors` and `llm_provider_failures` panels count, so the same change updates those panel queries and their help text.
6. **Web server** (small, shipped). Metrics from the Nuxt server, with the JavaScript OpenTelemetry SDK, under the rules in decision 3.

### Phase 1: what shipped

- **Instruments** (block "Platform health" in `internal/telemetry/metrics.go`): `platform_codesynapse_lease_events_total{lease,event}`, `platform_codesynapse_loops_lease_held`, `platform_go_goroutines`, `platform_go_heap_bytes`, `platform_go_gc_pause_seconds_total`, `platform_process_cpu_seconds_total`, `platform_db_pool_connections{db_system,state}`, `platform_db_pool_acquire_waits_total{db_system}`, `platform_db_pool_acquire_wait_seconds_total{db_system}` and `platform_telemetry_export_failures_total{exporter}`. Labels pass allowlists (`RecordLease`, `ExportFailed`, `TrackPool`); anything else is dropped. Unscoped gauges are new (`Registry.Gauge`) and must be named `platform_*`.
- **Runtime sampler** (`internal/app/runtime.go`): every process that exports metrics reads `runtime/metrics` and the tracked pools every 15 seconds. Cumulative runtime values become counter increases, so the registry stays the only accumulator. CPU is `/cpu/classes/total` minus `/cpu/classes/idle`; the runtime updates these at each garbage collection (at least every 2 minutes), so the CPU panel is coarse. GC pause seconds are estimated from the pause histogram's bucket midpoints.
- **Pool: no `waiting` state.** pgx reports no count of callers waiting right now. The gauge has `idle`, `in_use` and `max`, and two counters record acquires that had to wait and how long they waited. An alert uses `increase(platform_db_pool_acquire_waits_total[5m]) > 0` or `in_use == max`.
- **Leases.** The `api-loops` lease records `acquired`, `lost` and `renew_failed` (a failed `TakeLease` call). Durable job leases record `renew_failed` and `lost` only: a claim is not a lease event, and job counts arrive with phase 3. A renewal that fails because the job ended is not counted. The `held` gauge is 1 in a process without accounts, which always runs the loops.
- **Export failures** are counted by the OTLP exporter, the Prometheus push loop and the ingestion exporter, before any log throttling.
- **Panels** (platform admins only; Administration → Overview → "Platform health"): `platform_api_requests`, `platform_api_latency`, `platform_job_queue`, `platform_loops_lease`, `platform_lease_events`, `platform_goroutines`, `platform_heap`, `platform_cpu`, `platform_gc_pause`, `platform_db_pool`, `platform_db_pool_waits`, `platform_telemetry_drops` (with `platform_log_lines_dropped_total`) and `platform_telemetry_exports`. Process panels take the maximum per service (`job`), never a sum, except the lease holder count.

### Phase 3: what shipped

- **Instruments** (`internal/telemetry/metrics.go`, helpers and label sets in `internal/telemetry/jobs.go`): `codesynapse_jobs_finished_total{kind,outcome}`, `codesynapse_job_wait_seconds{kind}` and `codesynapse_job_duration_seconds{kind}` (both `slow`), `codesynapse_ingest_phase_seconds_total` and `codesynapse_ingest_phase_total{phase,outcome}`, `codesynapse_ingest_extract_seconds_total{language}`. All are tenant-scoped; the tenant is the job's organisation (`JobOptions.OrgID`, which `Start` checked against the job's execution identity), and a job without one is recorded as the `platform_` twin.
- **Kinds.** `ingest` (the non-durable mounted-source and CLI-style run) joins the list, and any unknown kind is recorded as `other`, so the set is 8 kinds. A durable job is counted under its durable kind (`switch` stays `switch` although it runs as a clone).
- **Outcomes.** `left_behind` is a durable job `leftBehind` keeps for another process, or one whose durable record could not be claimed (`StartJob` failed on a lost lease). `cancelled` is `context.Canceled` only; the one-hour job timeout counts as `error`. Unknown outcomes are not recorded.
- **Wait and run time.** Wait runs from queueing (or, for a job a worker claims, the record's creation on its first attempt) to the job taking its slot; a job started again after an interrupted attempt reports no second wait. Run time runs from that slot to the end, and is not recorded for a job that never started.
- **Phases** are timed in the `phase` helper beside the existing spans (`job.prepare`, `job.ingest`, `job.publish`), so the metric and the span cover the same work.
- **Languages.** `internal/ingest` stays free of telemetry: `ingest.Options.Extracted` reports each extractor's time (`lang.Result.Parsing` per language), and the API maps the names to the fixed `language` set.
- **Per-tenant share.** `Histogram.TenantShare(n)` caps the series each `tenant_id` may hold; the two job histograms use one series per kind (8). Drops past the share are counted in `telemetry_dropped_series_total` like any other drop. Together with the 9-bucket `slow` set this is decision 2 applied to new tenant histograms.
- **Panels** (Statistics → Sync, "Jobs and indexing over time"): `jobs_finished` (by outcome), `job_failures` (errors by kind), `job_wait` and `job_duration` (p95 by kind), `ingest_runs` (by outcome), `ingest_duration` (p50, p95), `ingest_phases` (average per run of each phase) and `ingest_extract` (by language).
- **Job failure alert input.** The error share reads `codesynapse_jobs_finished_total{outcome="error"}` against all outcomes, summed over tenants, plus the `platform_codesynapse_jobs_finished_total` twin for jobs without an organisation.

### Phase 4: what shipped

- **Instruments** (block "Outbound calls and security events" in `internal/telemetry/metrics.go`, helpers and label sets in `internal/telemetry/outbound.go`): items 8 to 15 as listed. They count per tenant only: the helpers drop `mcp_tool` and the model to `none`, so an audit write inside a tool call does not open a series per tool. The three histograms take a per-tenant share of one series per label value (`TenantShare`).
- **Git.** `gitx.runRemote` times every `clone`, `fetch`, `ls-remote` (the access check, recorded as `ls_remote`) and `push`. The outcome comes from `gitx.ErrNoAccess`, `gitx.ErrRedirected` and the context's deadline, never from git's output; a call whose context was cancelled is not counted. The tenant is the one on the job's or request's context; operator mounts and the single-tenant install have none and count as the `platform_` twin.
- **Lookups.** osv.dev (`internal/vuln`), deps.dev (`internal/license`) and the forge clients (`forge_github`, `forge_gitlab`, `forge_forgejo` for Forgejo and Gitea) record around their one `http.Client.Do`. 2xx and 404 (the package or organisation is unknown) are `ok`, 429 and a 403 with `X-RateLimit-Remaining: 0` are `rate_limited`, a deadline or network timeout is `timeout`, a cancelled request is not counted, anything else is `error`. No host, owner or package name is a label.
- **Sign-in.** `handleLogin` (`password`), the bearer and cookie checks in `authenticate` (`api_key`, `session`; the installation token counts as `api_key`), the gateway's key check, `handleSSOCallback` (`sso`) and the MCP key check count each credential that was presented. Outcomes come from the `accounts` sentinel errors (`ErrUnauthorized`, `ErrRateLimited`, `ErrSuspended`, `ErrKeyRevoked`/`ErrSSOState` as `expired`, root, forbidden and SSO refusals as `refused`); unknown errors are not counted. The tenant is the organisation the server resolved for a successful check, and only then: every failure is a `platform_` series, never attributed to the organisation a client asked for.
- **Audit writes.** `accounts.Service.Record` times `AddEvent`, so the HTTP middleware, the MCP audit and service events are all covered. The source comes from the context (`WithAuditSource`): `api` for everything inside an API request, `mcp` inside the MCP server, `service` otherwise. The tenant is the one on the context (the API sets it from the request's resolved organisation), never the entry's `OrgID`.
- **Seals.** `auditarchive.Seal` records `ok`, `error` or `nothing_to_seal`; the retention loop records `no_archive` when sealed files exist but no archive is configured, and `error` when it could not read them.
- **Panels.** Statistics → Sync gains `git_operations` (by outcome) and `git_duration` (p95 by operation); Statistics → Activity gains a group "External services and the audit log" with `lookups` (by service and outcome), `lookup_latency` (p95 by service) and `audit_write_latency` (p95 by source); Administration → Overview gains `platform_sign_in` (failures by method and outcome, plus successes) and `platform_audit_log` (failed writes by source, seals that failed or found no archive).
- **Alerts** (`deploy/alerts/outbound.yaml`, `deploy/alerts/security.yaml`): see the table below. The label guard now also allows `operation`, `service`, `method` and `source`, all fixed sets.

### Phase 5: what shipped

- **Instruments** (block "Sharper existing families" in `internal/telemetry/metrics.go`, helpers and label sets in `internal/telemetry/outcomes.go`): `mcp_local_answers_total{outcome}`, `gateway_request_duration_seconds{route}` (`latency` buckets, 12 series per tenant) and `gateway_fallthrough_total{reason,route}`. Added later for the gateway's timeouts and response times: `gateway_queue_wait_seconds` (wait for the local model's queue slot, recorded only when the local model was asked; `queueWait` buckets from 10 ms to 120 s), `gateway_local_answer_seconds{outcome}` (the local model's own time, `answered` or `failed`; `latency` buckets) and `gateway_first_byte_seconds{route}` (time to the first byte of a streamed answer, `local` or `cloud`; `ttft` buckets). All are tenant-scoped and recorded by `recordGateway` from `gateway.Meta`. A sample without a tenant becomes the `platform_` twin.
- **Refused tool calls.** `mcp_tool_calls_total` has a third outcome, `refused`. The authorization middleware marks the call in its context at each point where it audits a denial: an unknown tool, a repository outside the caller's reach, or a failed permission check. The telemetry middleware reads the mark. A call that ran and failed stays `error`, so `tool_errors` no longer includes refusals.
- **Local answers.** `POST /api/v1/usage/local` records `recorded` once the call is audited and counted, and `refused` when the body, tool, session or execution check fails or the audit entry cannot be written. The tool label comes from `AllowTools`, which accepts exactly `gittools.Tools`, `ghtools.Tools`, `coretools.Local`, `ask_local` and `local_draft`, the same list the endpoint validates. Any other name is recorded as `unknown`.
- **Gateway fall-through reasons** come from the return points of the gateway's local attempt: `not_eligible`, `too_large`, `has_thinking`, `has_image`, `excluded` (the model is on the installation's or organisation's exclude list), `no_local_model`, `queue_timeout`, `answer_timeout`, `cancelled`, `model_error`, `bad_status`, `empty_answer`, `truncated` and `bad_answer` (`telemetry.GatewayReasons`). The first version's `busy`, `unavailable` and `no_first_chunk` were split so timeouts can be told apart from errors: `queue_timeout` and `answer_timeout` are the two timeouts (`telemetry.GatewayTimeout`), `cancelled` is a client that gave up, and an answer cut off mid-stream is `truncated`. Anything outside the set is recorded as `other`. The gateway package stays free of telemetry: the reason travels in `Meta.Fallthrough` and the API records it beside the duration.
- **Model outcomes.** `llm_requests_total` adds `timeout`, `rate_limited` and `refused`. `outcomeOf` (`services/api/internal/llm/outcome.go`) looks only at the error's type, status code and context error: HTTP 408 and 504, `context.DeadlineExceeded` and a network timeout are `timeout`; 429 is `rate_limited`; 401, 403 and the model's own refusal stop reason (`ErrDeclined`) are `refused`; everything else, `context.Canceled` included, is `error`. Error text never decides. The Anthropic stream path now returns the same `httpError` as the other providers, so its status codes are seen.
- **Series cap (decision 2).** `mcp_tool_duration_seconds` and `http_request_duration_seconds` use the 8-bucket `fast` set (10 ms to 30 s). Each tenant may hold 48 tool series and 64 route series; drops past the share are counted in `telemetry_dropped_series_total`.
- **Panels (decision 5).** `tool_errors` keeps its query (`outcome="error"`), and its help now says refusals are counted apart. `llm_provider_failures` counts every outcome other than `ok`. New panels: `tool_refusals` and `local_answers` (by tool), `llm_failure_causes` (by outcome), `gateway_latency` (p95 by route) and `gateway_fallthrough` (by reason); later `gateway_latency_avg` (average by route), `gateway_timeouts` (the two timeout reasons) and `gateway_queue_wait` (average and p95). They are on Statistics → Activity; the gateway panels have their own "Model gateway" group there instead of Statistics → Models, which has no time series. The per-model failure count on Statistics → Models (`stats/models`) also counts every outcome other than `ok`.

### Phase 6: what shipped

- **Packages** (`services/web/package.json`, exact versions, all Apache-2.0): `@opentelemetry/api` 1.9.1, `@opentelemetry/sdk-metrics` 2.12.0, `@opentelemetry/resources` 2.12.0 and `@opentelemetry/exporter-metrics-otlp-http` 0.223.0 (OTLP over HTTP with JSON, so no protobuf runtime). The licence check walks them in `.output/server/node_modules`.
- **Instruments** (`services/web/server/utils/webMetrics.ts`). The web server has no trusted tenant, so every series is `platform_*`:
  - `platform_web_requests_total{route,method,status_class}`: every request the Nuxt server answers, page renders and `/api/cg` proxy calls alike. `method` is one of seven values (`OTHER` for the rest) and `status_class` is `1xx` to `5xx` or `other`.
  - `platform_web_request_duration_seconds{route}`: the same requests' time, with the API's latency buckets.
  - `platform_web_proxy_requests_total{outcome}`: `/api/cg` calls by `ok`, `refused` (the proxy's own method, path, origin or size check), `unreachable`, `timeout` and `error`. The outcome comes from the error's name and code (`ECONNREFUSED`, `ENOTFOUND`, `UND_ERR_*`, `TimeoutError` and the like), never its text. Every outcome starts at zero, like the Go exporter's first-value rule. An unreachable API now answers 502 and a timeout 504 instead of a bare 500.
  - `platform_telemetry_export_failures_total{exporter="otlp"}` and `telemetry_dropped_series_total{metric}`, the same names as the Go services, so the existing panels and `CodeSynapseMetricsExportFailing` cover the web server.
- **Route patterns** (`services/web/server/utils/routePattern.ts`), never visited paths. A page render is labelled by the vue-router definition it matched (`/repos/{repo}/security`), which `app/plugins/metrics-route.server.ts` hands to the server through the request context. A proxy call is labelled from allowlists of the API's top-level segments and repository sub-resources (`/api/cg/repos/{repo}/security`, `/api/cg/accounts/*`); anything else becomes `other`, so a list that drifts from the API only loses detail. Assets are `static`.
- **Caps.** `platform_web_requests_total` holds at most 2,000 label combinations and the histogram 300 routes. Further combinations are dropped and counted per metric.
- **Export.** `CODESYNAPSE_METRICS_URL`, `_USER` and `_PASSWORD` from the environment, the same as the Go services. Every 15 seconds, OTLP/HTTP to `<url>/opentelemetry/v1/metrics` with cumulative temporality and an 8-second timeout. `CODESYNAPSE_METRICS_EXPORTER` must be `otlp` (the default); `prometheus` stops the server at start. The resource is `service.name=codesynapse-web`, `job`, `instance` (a random ID per process), `service.version` (`CODESYNAPSE_VERSION`, set by the image build) and `deployment.environment.name` (`CODESYNAPSE_ENVIRONMENT`). Failed pushes are logged at most once a minute, with the error but not the URL. Without a URL, nothing is recorded.
- **Errors.** h3 skips its after-response hook when Nitro's error handler has already answered, so the plugin also records from Nitro's `error` hook and marks the event so a request is counted once.
- **Wiring.** The `web` service in `docker-compose.yml` and the chart's web Deployment set the metrics variables. The chart's VictoriaMetrics NetworkPolicy now admits the web pods. No chart values were added.
- **Panels** (platform admins only; Administration → Overview): `platform_web_requests` (by status class), `platform_web_latency` (p50, p95) and `platform_web_proxy` (calls that did not reach the API, by outcome).
- **Alert.** `CodeSynapseDashboardAPIUnreachable` (`deploy/alerts/web.yaml`, critical) fires when more than 5% of the proxy's calls find the API unreachable or time out for 10 minutes, with at least one call a minute. Calls the proxy refused are left out.

### Alerting

Phase 2 adds alerting for operators. It is off by default. Status: shipped (vmalert v1.155.0); how to turn it on is in [Alerting](operations/alerting.md).

- **Component.** A `vmalert` service in `docker-compose.yml` behind the `alerting` profile (`COMPOSE_PROFILES=alerting just up`), and `alerting.enabled` in the parent chart (`templates/alerting.yaml`, not a subchart, because it needs the chart's metrics URL, public URL and environment): pinned image, one replica, internal only, no published port, a NetworkPolicy that refuses every connection to it, and `values.schema.json` updated in the same change. The metrics store's policy admits it. It reads from VictoriaMetrics with the existing read credentials (`METRICS_PASSWORD`).
- **Receiver.** Any Alertmanager-compatible endpoint the operator names. `ALERTMANAGER_URL` (required) and optionally `ALERTMANAGER_USERNAME`/`ALERTMANAGER_PASSWORD` or `ALERTMANAGER_BEARER_TOKEN` come from the environment (Compose) or a Secret (chart, `alerting.existingSecret` or `global.existingSecret`), and reach vmalert as environment (`-envflag.enable`), never as flags. An empty URL stops vmalert at start. CodeSynapse ships no receiver of its own.
- **Rules as files.** Rules live in `deploy/alerts/*.yaml`, are mounted read-only into vmalert and packaged with the chart (`deploy/helm/codesynapse/alerts` links to that directory). `just lint-alerts` (part of `just checks`, a CI step) runs `vmalert -dryRun`, the unit tests in `deploy/alerts/tests` with vmalert-tool, and a label guard; both images are the release Compose ships. `just chart-check` renders the chart with alerting on and off.
- **What alerts may say.** Rules read `platform_*` metrics, or aggregate tenant series with `sum by` or `max by` over an allowlist of labels (`metric`, `reason`, `db_system`, `le`, `kind`, `exporter`, `operation`, `service`, `method`, `source`); the label guard refuses `without`, any other grouping label and an expression without aggregation, so `tenant_id` never reaches an alert. Alert labels and annotations never carry repository names, paths, users, API keys, sessions or other request content. Annotations are constant text with the value and the label above; they link to the Statistics page (`-external.url`), not to a tenant's data.

The rules as first planned:

| Rule | Reads | Fires when |
|---|---|---|
| Scheduler lease lost | `platform_codesynapse_lease_events_total{event="lost"}` | any increase in 10 minutes |
| No scheduler holder | `platform_codesynapse_loops_lease_held` (fresh instances) | sum is not 1 for 2 minutes |
| Job failure rate | `codesynapse_jobs_finished_total`, summed over tenants | `error` share above 20% over 30 minutes, with at least 10 jobs |
| Audit write failures | `codesynapse_audit_writes_total{outcome="error"}` and its `platform_` twin | any increase |
| Metrics export failing | `platform_telemetry_export_failures_total` | increases for 5 minutes |
| Series cap overflow | `telemetry_dropped_series_total` | any increase in 1 hour, per instrument |
| Store latency | `db_query_duration_seconds`, summed over tenants, per `db_system` | p95 above 1 s for 10 minutes |
| Store errors | `db_query_errors_total` and timeouts, summed over tenants, per `db_system` | error share above 5% for 10 minutes |

Phase 2 shipped before phase 1 merged, so it ships only rules whose metrics already exist; no rule reads a metric that does not exist yet.

Shipped in phase 2:

| Rule | File | Reads | Fires when |
|---|---|---|---|
| `CodeSynapseSeriesCapOverflow` | `telemetry.yaml` | `telemetry_dropped_series_total`, per `metric` | any increase in 1 hour |
| `CodeSynapseLogShippingDropping` | `telemetry.yaml` | `platform_log_lines_dropped_total`, per `reason` | any increase in 10 minutes, for 15 minutes |
| `CodeSynapseIngestMetricsRejected` | `telemetry.yaml` | `platform_ingest_metric_export_rejections_total`, per `reason` | any increase in 1 hour |
| `CodeSynapseIngestMetricsCapacity` | `telemetry.yaml` | `platform_codesynapse_ingest_metric_series_dropped_total` | any increase in 1 hour |
| `CodeSynapseStoreSlow` | `stores.yaml` | `db_query_duration_seconds` and its `platform_` twin, summed per `db_system` | p95 above 1 s for 10 minutes |
| `CodeSynapseStoreErrors` | `stores.yaml` | `db_query_errors_total` over `db_query_duration_seconds_count` (and `platform_` twins), per `db_system` | error share above 5% for 10 minutes, with at least one call a minute; timeouts are already errors, so they are not added again |
| `CodeSynapseSchedulerLeaseLost` | `jobs.yaml` | `platform_codesynapse_lease_events_total{lease="api_loops",event="lost"}`, summed | any increase in 15 minutes (added once phase 1 merged) |
| `CodeSynapseNoSchedulerHolder` | `jobs.yaml` | `platform_codesynapse_loops_lease_held`, summed over instances (no report counts as 0) | below 1 for 10 minutes (added once phase 1 merged) |
| `CodeSynapseMetricsExportFailing` | `telemetry.yaml` | `platform_telemetry_export_failures_total`, per `exporter` | any increase in 10 minutes, for 15 minutes (added once phase 1 merged) |
| `CodeSynapseJobFailureRate` | `jobs.yaml` | `codesynapse_jobs_finished_total` and its `platform_` twin, `error` over all outcomes per `kind` | share above 20% over 30 minutes, with at least 10 jobs (added once phase 3 merged) |
| `CodeSynapseJobsNotClaimed` | `jobs.yaml` | `platform_codesynapse_jobs_waiting` and `_active`, maximum per `kind` | jobs wait and none of that kind runs, for 15 minutes |
| `CodeSynapseDashboardAPIUnreachable` | `web.yaml` | `platform_web_proxy_requests_total`, `unreachable` and `timeout` over all but `refused`, summed | share above 5% over 10 minutes, for 10 minutes, with at least one call a minute (phase 6) |

`CodeSynapseJobsNotClaimed` was not in the first list: the queue gauges already exist and a missing worker role (the fix worker is off by default) is the commonest reason jobs stall.

Added in phase 4:

| Rule | File | Reads | Fires when |
|---|---|---|---|
| `CodeSynapseGitFailing` | `outbound.yaml` | `codesynapse_git_operations_total` and its `platform_` twin, `error` and `timeout` over all outcomes per `operation` | share above 20% over 30 minutes, with at least 10 calls; `no_access` and `redirected` are about one repository, so they do not count |
| `CodeSynapseLookupsRateLimited` | `outbound.yaml` | `codesynapse_lookups_total{outcome="rate_limited"}` and its `platform_` twin, per `service` | any increase in 10 minutes, for 15 minutes |
| `CodeSynapseSignInBurst` | `security.yaml` | `codesynapse_auth_attempts_total` and its `platform_` twin, `bad_credentials` and `rate_limited`, per `method` | more than 30 in 10 minutes |
| `CodeSynapseAuditWriteFailing` | `security.yaml` | `codesynapse_audit_writes_total{outcome="error"}` and its `platform_` twin, per `source` | any increase in 10 minutes (critical) |
| `CodeSynapseAuditSealFailing` | `security.yaml` | `platform_codesynapse_audit_seals_total{outcome="error"}` | any increase in 25 hours (sealing runs once a day) |
| `CodeSynapseAuditNotArchived` | `security.yaml` | `platform_codesynapse_audit_seals_total{outcome="no_archive"}` | at least 2 in 49 hours |

The rest of the alert column in the instrument table follows the same way: each later phase adds the rules named for its instruments, with a unit test in `deploy/alerts/tests`.

### Decisions (2026-10-28)

1. **Alerts: vmalert, optional.** vmalert (Apache-2.0 image, no Go dependency) is supported as an optional component: a Compose service and a chart toggle or store-style subchart beside the existing VictoriaMetrics setup. It evaluates rules against the internal VictoriaMetrics and sends to an Alertmanager-compatible receiver the operator configures; the receiver URL and credentials come from the environment or a Secret only, never from values files or flags. The dashboard notification feed stays the channel people in organisations see. Phase 2 delivers it; see "Alerting" below.
2. **Series cap.** Histograms with many label values get a smaller bucket set, and each tenant gets a capped share of an instrument's budget so one organisation cannot use it all up. Both are applied in phase 5 to `mcp_tool_duration_seconds` and `http_request_duration_seconds`, and every new tenant histogram follows them.
3. **Web server metrics: approved,** including the JavaScript OpenTelemetry SDK (`@opentelemetry/*` packages, Apache-2.0). The same rules hold as for the Go services: a bounded set of series per instrument with dropped series counted, route patterns and never raw paths, OTLP to the internal VictoriaMetrics only, and no request content, cookies, organisation slugs or client addresses as labels. Phase 6 adds them.
4. **Runtime metrics: approved as planned.** Item 21 samples the standard library's `runtime/metrics`. The OTel contrib runtime package is not added, because it would be a new dependency and a second accumulator beside the registry.
5. **Outcome values and panels.** A change that adds outcome values (`refused`, `timeout`, `rate_limited`) to an existing family updates the panel queries that count that family, such as `tool_errors` and `llm_provider_failures`, in the same change. This holds for every later change too.

### Open questions

- **Bridge.** Item 17 counts local answers the server recorded. When the server refuses or cannot record one, the bridge sends that call to the server, where `mcp_tool_calls_total` counts it. This is intended, not a gap.
