# Tenant telemetry and four dashboard groups

Status: planned, SaaS phase 4e, after access isolation (4d). The existing Compose stack supplies VictoriaMetrics for development; this phase adds instrumentation and dashboard coverage. Kubernetes wiring waits for the final deployment phase (6a). The request for OTel supersedes the earlier decision to defer it. Before adding dependencies, list the exact Go SDK/exporter modules, versions and licences for approval under the repository's dependency rule.
Session-scoped tool execution and chat memory must also follow [phase 4g](SESSION_CONTEXT_PLAN.md): immutable runtime identity, inherited parent context and tenant/session/namespace filtering. This requirement does not turn session IDs into metric labels.

## Attribution and export

Carry a typed telemetry context through accepted MCP tools, database operations, model calls and queued tasks. Every tenant operation in these dashboards has the following attributes:

| Attribute | Trusted source |
|---|---|
| `tenant_id` | Authenticated organisation context, validated by `TenantLabel`; never a tool argument |
| `mcp_tool` | Accepted tool name through `ToolLabel`; fixed `none` for API/background work |
| `llm_provider` | Configured provider for the measured operation; fixed `none` when there is no model |
| `llm_model` | Configured model for that operation; fixed `none` when there is no model |

Tenant IDs are bounded, validated identifiers, but cardinality still grows with tenants. Cap active series and allowed label combinations per instrument, including histogram buckets and replicas. Do not preallocate the Cartesian product. Preserve the tenant dimension: never merge tenants into a tenant-visible overflow bucket. Drop excess series with a separate platform-only dropped-series counter, and test that SDK overflow cannot create unscoped data.

A tool can call several models: attribute each model operation separately. Count the tool's transport bytes once, using provider/model `none`, rather than copying those bytes into each model series. Durable fact counts likewise use `none` for tool/provider/model because they describe stored state rather than one call. Unauthenticated requests and startup work have no trusted tenant: record separate bounded platform metrics, not a fabricated tenant ID. Cross-tenant jobs split observations by the tenant whose work they perform.

Keep repository names, paths, prompts, code, hostnames, user/key/session IDs and client-supplied model/tool names out of metric attributes. Use redacted logs for request diagnostics. Resources identify `service.name`, version and deployment environment; replica identity must distinguish cumulative streams without adding user-controlled labels.

Extend `internal/telemetry` behind its existing API with an OTel meter provider, bounded views, explicit histogram buckets and a periodic push reader. Export cumulative OTLP/HTTP metrics directly to the internal VictoriaMetrics `/opentelemetry/v1/metrics` endpoint, or through an operator-managed Collector. Keep authentication in secrets, queues/timeouts/retries bounded, and shutdown flushes time-limited. A telemetry outage must not block requests. No public scrape listener is added. The [VictoriaMetrics OTLP integration](https://docs.victoriametrics.com/victoriametrics/integrations/opentelemetry/) documents the endpoint and temporality requirements; [OTel metric views](https://opentelemetry.io/docs/specs/otel/metrics/sdk/) provide attribute filtering and cardinality controls.

Select one exporter per metric stream during migration. Do not push the same observations through the current Prometheus-text path and OTLP into the same database. Verify exported names, suffixes, units and histogram series against dashboard queries before switching. Preserve first-sample and counter-reset behavior already covered by the VictoriaMetrics integration test.

## Dashboard 1: tokens and model cost

| Metric | Instrument and meaning |
|---|---|
| `llm_tokens_input_total` | Counter, prompt tokens reported by the provider; normalize whether cache-read tokens are included |
| `llm_tokens_output_total` | Counter, completion tokens reported by the provider |
| `llm_tokens_saved_total` | Counter, input tokens served from an actual provider prefix/prompt cache; do not infer hits from repeated text |
| `llm_cost_estimated_dollars` | Counter, estimated USD from the configured input/output/cache-read/cache-write rates at call time |
| `llm_cost_saved_estimated_dollars_total` | New counter, comparable uncached input cost minus actual cached-input cost, calculated with that call's prices |

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

Read durable counts from bounded aggregate queries; paginate tenant enumeration without silently dropping tenants. One elected exporter owns the gauge per tenant in a scaled deployment, or the backend deduplicates replicas with a documented rule. Do not sum identical per-replica gauges. Surface stale count observations rather than showing old values as current.

## MetricsQL panels and tenancy

These are fixed server-side panel templates. Tenant access is enforced by the existing VictoriaMetrics `extra_filters` derived from the authenticated scope, including both numerator and denominator of ratios. Never interpolate a raw `$tenant` into MetricsQL. Platform-wide aggregate panels require a platform capability and expose no repository contents. Empty authorised-tenant sets match nothing.

Cloud egress avoided, estimated percent over one hour:

```metricsql
100 * sum(increase(cloud_bytes_saved_total[1h]))
/ clamp_min(
    sum(increase(cloud_bytes_sent_total[1h]))
    + sum(increase(cloud_bytes_saved_total[1h])), 1)
```

Present “no comparable traffic” when the denominator is zero or the baseline is unavailable; do not interpret the clamped numeric result as achieved savings. Pre-initialize paired counters for admitted label sets so absent series do not change the ratio's meaning.

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

Keep time windows and panel IDs allowlisted in `services/api/internal/api/series.go`; the UI uses `useApiGet` and the existing plain SVG/CSS charts. Group the Statistics page into these four categories with units, missing-data states and “estimated” labels where required.

## Implementation and checks

1. Inventory existing instruments and attribution gaps, then define normalized context, unit/name compatibility and per-instrument series budgets. Approve the exact OTel dependencies before adding them.
2. Add the OTel exporter and test cumulative counters/histograms in a throwaway VictoriaMetrics instance. Exercise restarts, first samples, exporter failure, overflow and replica separation.
3. Instrument MCP transport and model transports with streaming byte counters; add provider-usage normalization, cost savings and a defensible cloud-fallback baseline.
4. Add durable memory gauges and recall counting; correct the graph-row panel and retain a documented compatibility period for legacy names.
5. Add the four dashboard groups and fixed query templates. Test tenant A cannot query B, forged attributes are ignored, no unauthenticated series appears in tenant panels, and multiple replicas do not double-count durable state. Test cancellation, retry, cache-hit and local-only cases with known byte/token totals.
6. Run `just test`, `just lint`, `just web-check`, and VictoriaMetrics integration tests. Record instrumentation CPU/memory/latency overhead against an uninstrumented baseline before enabling it by default. Update `AGENTS.md` when the existing telemetry implementation changes.

## Ingestion and annotation extension

[Phase 4i](INGEST_TRANSPARENCY_PLAN.md) adds the requested file counters, pruning/token reports and annotation-job visibility after [phase 4h repository policy](REPO_CONFIG_PLAN.md). Its `repo` metric label is a bounded, opaque repository-entry ID from authenticated server state; names and paths remain outside time-series labels. Exact per-run reports remain available when a repository is not admitted to per-repository metric series.
