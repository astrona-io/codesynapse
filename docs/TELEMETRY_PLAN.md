# Tenant telemetry and four dashboard groups

Status: implemented in SaaS phase 4e, after access isolation (4d). The existing Compose stack supplies VictoriaMetrics for development; this phase adds instrumentation and dashboard coverage. Kubernetes wiring waits for the final deployment phase (6a). The request for OTel supersedes the earlier decision to defer it. The approved OTel v1.47.0 SDK/exporter is the default export path; validation and limits are recorded below.
Session-scoped tool execution and chat memory must also follow [phase 4g](SESSION_CONTEXT_PLAN.md): immutable runtime identity, inherited parent context and tenant/session/namespace filtering. This requirement does not turn session IDs into metric labels.

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
| `db_query_duration_seconds` | Histogram, client-observed request latency; fixed `db_system` values include `milvus`, `hugegraph`, `neo4j`, `postgres`; fixed operation names |
| `db_vector_matches_count` | Existing cumulative counter of returned matches; split candidate counts and final fused counts into separately named counters if needed |
| `db_graph_nodes_traversed` | Existing legacy name currently counts returned rows, including edges/facts; it does not measure nodes visited internally |

Add a correctly named `db_graph_rows_returned_total` and migrate panels while documenting the legacy series. Actual Gremlin traversal counts require verified backend profiling support and a defined counting method; expose them separately only when measured, never substitute returned rows or run expensive profiling on every production query. Add error/timeout rates, p50/p95/p99 latency and request volume; configured SLA targets are not measured benefits.

## Dashboard 4: memory and offloading

| Metric | Instrument and meaning |
|---|---|
| `agent_memory_facts_count` | Observable gauge, current durable unique triples per tenant; changes with deletion, expiry and deduplication |
| `agent_memory_recalls_total` | Counter, one logical recall per accepted request, not once for each BM25/vector leg |
| `memory_facts_saved_total` | Retain as a write-operation counter; never label it as current stored facts |
| `memory_offload_turns_total`, `memory_offload_tokens_total` | Retain counts/estimates of offloaded context, separately from billed model-token savings |

Read durable counts from bounded aggregate queries in pages of 100 namespaces. Collection has a 20-second deadline and a ceiling of 2,048 current tenants, reserving additional registry slots for deletion observations. Every page must succeed before publication; a timeout, invalid page or exceeded tenant budget leaves the complete previous snapshot unchanged so it ages stale. No partial tenant set is published. API replicas collect bounded snapshots every 30 seconds and emit `agent_memory_facts_observed_at_seconds`. A panel first retains the newest observation timestamp per tenant, then takes the maximum count among replicas with that timestamp, and only then sums tenants. This avoids adding duplicate replica state or preferring an older, larger count after deletion. Observations older than 90 seconds are omitted; a failed collection leaves its timestamp unchanged. A completed enumeration publishes zero for previously observed tenants that disappeared. Count and observation-time changes are one registry batch: both OTLP and Prometheus snapshots wait for the complete batch. The dashboard also requires the count and observation-time samples to have matching export timestamps. This rejects an old count paired with a newer observation time if the backend accepted only part of an export.

## MetricsQL panels and tenancy

These are fixed server-side panel templates. Tenant access is enforced by the existing VictoriaMetrics `extra_filters` derived from the authenticated scope, including both numerator and denominator of ratios. Never interpolate a raw `$tenant` into MetricsQL. Ordinary panels remain tenant-filtered even for platform administrators. The explicit `tenants` and `platform_*` panels require a platform capability and expose no repository contents. Empty authorised-tenant sets match nothing, including a platform administrator with no organisation memberships.

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
