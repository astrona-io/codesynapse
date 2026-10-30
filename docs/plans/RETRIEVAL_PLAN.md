# Graph and vector context reduction

Status: implemented, SaaS phase 4f. Extends phase 4c and [ADR 0001](../adr/0001-search.md); depends on phase 4d authorization and uses phase 4e telemetry. The target domain is CodeSynapse's code intelligence and agent memory. Response budgets, retrieval integrity and reproducible measurements are implemented. Native Milvus BM25/RRF remains conditional: the measured lexical latency does not trigger migration. [Measured results](../RETRIEVAL_RESULTS.md) include the overhead on small files and make no production SLA claim.
Session-scoped tool execution and chat memory must also follow [phase 4g](SESSION_CONTEXT_PLAN.md): immutable runtime identity, inherited parent context and tenant/session/namespace filtering. This requirement does not turn session IDs into metric labels.

## Domain topology

Keep the existing code graph and metadata-only storage policy. Full source bodies and YAML values stay outside both graph and vector stores. Every stored summary passes `redact.Summary`; user/agent memory passes `redact.Free`. Embeddings remain local and cover approved entity names/aliases, not repository source or conversations.

| Node | Properties used for retrieval | Connections |
|---|---|---|
| Organisation repository entry (`AccessRepository`, phase 4d) | `id`, `tenant_id`, `graph_id`, access-policy revision | Authorised users/teams and owning organisation |
| Shared code `Repository` | Existing `id`, repository identity, commit/head, indexed time | Contains packages, documents and dependencies |
| `Package`, `File` | Existing `id`, repository identity, path/name, language | Contains symbols; imports packages |
| `Function`, `Method`, type/interface | Qualified name, signature, line range, hash, redacted summary | Calls/called-by, implements, tested-by |
| `Document`, section/config metadata | Path, heading/key path, line range, redacted summary | Belongs-to repository, mentions symbols |
| `Module`, security finding | Package/version, rule ID, severity, source location | Required-by/imported-by, affects symbol/file |
| `MemoryEntity` | `id`, `tenant_id`, namespace, name, aliases, type, timestamps | Validated `FACT` predicates to other entities in the same namespace |
| Memory fact | Predicate, source client, confidence, created/updated time | Tenant-scoped entity → entity |

Use backend-neutral timestamp names for new fields (`created_at`, `updated_at`); document adapters for existing fields rather than blindly renaming the schema. Do not attach a single tenant owner to a deduplicated code graph. Tenant-owned repository entries authorize a shared `graph_id`; memory/reviews remain in the selected organisation's namespace. New labels/properties require model allowlists, `nodeprops`, HugeGraph schema coverage, store contracts and API types as described in `AGENTS.md`.

The current adapters map logical memory fields as follows; phase 4f introduces no new graph labels or properties:

| Logical field | Neo4j | HugeGraph | Milvus dense index |
|---|---|---|---|
| Entity / fact | `MemoryEntity` / `FACT` | `mem_entity` / `fact` | Entity vector row |
| Entity identifier | `id` | `eid` | `id` |
| Tenant and namespace | `repo` contains the trusted `org|graph` namespace | `tenant_id` and `ns` | `tenant_id` partition key and `ns` |
| Timestamps | `created_at`, `updated_at` | `created_at`, `updated_at` | Authoritative graph supplies them during hydration |
| Display metadata | Name, type, aliases | Name, type, aliases | No source bodies; graph supplies display metadata |

The namespace is resolved from authenticated access, never taken from a tool argument. Neo4j memory queries bind that complete namespace on roots, facts and both endpoints. HugeGraph roots bind the separate tenant and namespace fields. Existing requested memory roots are returned even without facts; missing roots are dropped. This lets recall discard entities deleted between search and hydration without returning stale search metadata.

## Retrieval and execution plan

1. Authenticate the subject and resolve the repository entry through the phase 4d resolver. Construct immutable tenant, graph and content-namespace context. Reject an inaccessible selector before search, including ambiguous-name suggestions.
2. Use exact equality/index lookup for qualified symbols, error codes, policy clause IDs and hostnames. BM25 is lexical relevance, not an exact-match guarantee; preserve punctuation/case rules in dedicated scalar identifier fields where needed. Return exact authorized matches before approximate candidates.
3. For lexical memory searches, rank approved names/aliases with BM25. For intent searches, embed the query locally and search dense vectors. Only entities from the same authorized namespace can participate in either leg.
4. The implemented path requests at most 10 candidates per lexical/dense leg, uses shared Go RRF with k=60, and selects at most 8 current entries after graph hydration. Exact current names/aliases/IDs precede approximate ranks. When the migration conditions below hold, move fusion into Milvus `hybrid_search` with the same tenant/namespace filter on both legs; that conditional backend migration is not enabled by phase 4f.
5. Resolve selected entities/symbols into graph cards, then expand only the relationships needed by the task, normally depth 1–2. Apply `store.ClampDepth`, `store.MaxLimit`, query deadlines and a total response token/byte budget. Reject foreign backend hits defensively before graph hydration.
6. Return qualified names, concise summaries, relevant relationships and file line ranges. Include provenance and retrieval legs; emit full opaque IDs only on request. For uncertainty or insufficient evidence, return a bounded follow-up suggestion rather than reading whole files automatically.

### Milvus migration conditions

ADR 0001 keeps memory BM25 in the shared Go ranker today, with Go-side RRF, because short entity names are cheap to rank and some installs have no embeddings. Moving fusion into Milvus is a measured extension, not a claim that phase 4c already does it. Trigger the migration with longer approved metadata, a larger entity cap, or measured lexical p95 above the ADR's budget. Index all eligible lexical entities, including those without dense embeddings; retain a lexical-only path when local embedding service/configuration is unavailable. Define how tombstones, aliases and embedding-model changes are applied and reconciled without stale recall.

Implement a Milvus collection with `tenant_id`, namespace and entity ID scalar fields, approved searchable text, a BM25 sparse field and a dense vector field. Keep the tenant partition key and scoped scalar indexes. Verify BM25/analyzer and RRF support in the pinned Milvus version and REST API before changing the adapter; do not add the heavyweight Go SDK without approval.

The pinned v2.6.4 REST request types support `search` subrequests with `annsField`, `filter`, `exprParams`, `limit` and `params`; the enclosing request uses `rerank.strategy`, `rerank.params` and an output limit. Schema functions use `name`, `type`, `inputFieldNames` and `outputFieldNames`. These field names were checked against the [v2.6.4 server source](https://github.com/milvus-io/milvus/blob/v2.6.4/internal/distributed/proxy/httpserver/request_v2.go). This confirms the request shape, not a tested native-hybrid deployment.

For a future migration, create a new versioned collection rather than changing the existing dense-only collection in place. Add approved name/alias/type text with an analyzer, a BM25 function producing a sparse field, and the dense field for the selected local model/dimension. Backfill every eligible lexical entity, including those without vectors. Verify a representation for missing dense values against the pinned server before enabling the collection; never drop lexical-only entities to make fusion work. Keep shared Go BM25 available until complete namespace coverage and parity are verified. Alias changes, merges and deletions must reconcile by stable entity ID; the graph remains authoritative at hydration. Select the new collection only after deletion, model/dimension-change, filter and quality tests pass. Rollback keeps the previous collection and shared ranker available.

The active dense adapter already separates collections by model hash and dimension, uses strong-consistency reads and parameter-bound namespace filters, and deletes entity IDs across model collections. A changed model is backfilled through `MemoryEmbeddingTodo`; retired collections are not queried by the active model. Nonfinite and zero embeddings are rejected. Embedding absence, invalid output or a two-second vector timeout falls back to lexical retrieval within the overall ten-second recall deadline.

The following filters are for explicitly selected durable repository memory. Session chat memory uses the additional immutable session filters from [phase 4g](SESSION_CONTEXT_PLAN.md) on both hybrid legs and graph hydration. Filters use existing expression parameters, conceptually:

```json
{
  "filter": "tenant_id == {tenant} and ns == {namespace}",
  "exprParams": {"tenant": "<authenticated organisation>", "namespace": "<resolved namespace>"}
}
```

This illustrates parameter binding, not a complete Milvus request. Confirm the exact versioned REST field names in the adapter tests. Never substitute a tenant string into `tenant_id == '...'`. A client-supplied `tenant_id`, namespace, predicate or graph ID cannot widen the scope. `vector.Isolate` remains mandatory after backend filtering.

### Graph lookups

For durable repository memory only, fixed traversal templates start at a tenant/namespace root. Session chat-memory traversals require the phase 4g session-scoped variants:

```cypher
MATCH (e:MemoryEntity {repo: $namespace, name: $name})
MATCH (e)-[f:FACT {repo: $namespace}]->(n:MemoryEntity {repo: $namespace})
WHERE coalesce(f.valid_to, '') = ''
RETURN e.name, f.predicate, n.name
LIMIT $limit
```

```groovy
g.V().hasLabel('mem_entity').has('tenant_id', tenant_id).has('ns', ns)
  .has('name', entity_name).outE('fact')
  .has('tenant_id', tenant_id).has('ns', ns).has('valid_to', '').as('fact')
  .inV().hasLabel('mem_entity').has('tenant_id', tenant_id).has('ns', ns)
  .limit(limit).project('predicate', 'name')
  .by(__.select('fact').values('predicate')).by('name')
```

These are design templates to adapt to the actual stored property names and validate through the shared contract. Values travel only in Cypher parameters/Gremlin bindings; labels and edge types stay constant. For shared code, start from the already-authorized repository entry and use its resolved graph identity on every root and expansion; a global vertex-ID lookup alone is insufficient. Calls/implementations/tests use the existing composed tools rather than issuing unrestricted traversal text. No raw Cypher or Gremlin tool is added.

## Implemented response limits

`minimal_context` estimates the complete structured JSON as `ceil(json_bytes/4)`, including escaped strings, provenance and its own token count. Its default is 1,500; `get_symbol_context`, `get_call_graph` and `get_pr_context` default to 20,000. All normalize `max_tokens` to 256–20,000. Candidate omissions and removed details are explicit. If the minimum subject and envelope do not fit, the request returns a budget error. Public-caller test coverage reports how many eligible callers were checked or omitted; both call directions retain their per-direction node limit.

These are structured-answer limits, not model-token or wire-byte guarantees. The MCP SDK repeats the JSON in text and structured content. The benchmark includes both copies and SDK metadata, while excluding HTTP/SSE framing and shared one-time prompts/tool schemas. Every context operation has a ten-second deadline. Tasks are capped at 8 KiB, selectors/paths at 4 KiB, file lists at 100 and diffs at 4 MiB.

Recall returns at most 8 entries, 2 hops, 80 facts and 32 KiB of serialized result JSON, with a `truncated` flag when evidence may be omitted. Its internal query cap is 4 KiB; the HTTP endpoint retains its existing 2,000-byte cap. Candidate identity, namespace, score and current graph metadata are checked before facts are returned. A foreign backend hit, stale alias or deleted entity cannot supply retained lexical evidence. The graph's fixed memory notice remains present. Session-chat storage and immutable session identity remain phase 4g work.

## Token and latency benchmark

The implemented fixture and its measured limits are documented in [RETRIEVAL_RESULTS.md](../RETRIEVAL_RESULTS.md). The broader workload below remains the acceptance target for a future native-hybrid migration and production SLA; the current report does not claim those measurements.

Create a versioned workload of symbol explanation, callers/callees, change impact, exact error/identifier lookup and cross-turn memory recall. Include rare identifiers, mixed-language terms, no-match cases, stale entities and adversarial tenant collisions. Compare the same task and answer quality across raw-file context, current composite graph tools, lexical-only retrieval, dense-only retrieval and Milvus hybrid plus graph hydration.

For each successful task record raw-context tokens, returned-context tokens, response bytes, round trips, evidence coverage and retrieval quality. Use a pinned tokenizer for the target model or clearly label the existing token estimator. Include wrappers, provenance and tool definitions consistently; separate one-time system prefixes from per-turn context. Do not count a shorter but incomplete answer as a saving.

```text
Reduction % = (Raw Text Tokens - Graph Context Tokens) / Raw Text Tokens * 100
```

Use “not applicable” when the baseline is zero. Illustrative arithmetic only: 20,000 baseline tokens and 2,000 returned tokens would be 90% reduction; this is not a measured CodeSynapse result. Publish per-task results and the aggregate ratio from summed counts, not an unweighted average that hides large tasks. Distinguish avoided context tokens from paid cache-hit tokens in phase 4e dashboards.

A preliminary isolated run on 2026-10-22 over Go 1.27 standard-library sources measured 3,387 symbol-card tokens versus 161,527 baseline tokens across 20 questions (97.9% aggregate, 97.6% median, bytes/4 estimator). The fixture contained 358 packages and 2,935 files; the baseline was defining files plus direct caller/callee/test files. This used the existing pipeline, not the planned retrieval redesign. It does not measure Milvus-side RRF, answer quality or the full workload above.

Measure cold and warm embedding, sparse/dense retrieval, RRF, graph hydration, serialization and total wall-clock p50/p95/p99 under representative concurrency and index size. Report hardware, data size, timeout/error rate, quality at k and local model. Evaluate benefit against the measured baseline; graph lookup may cut context while local embedding adds latency. Keep ADR 0001's lexical p95 50 ms threshold as a migration trigger, not an end-to-end SLA. Set total retrieval SLA targets only after measurement, and fail the rollout if latency or evidence quality regresses beyond the agreed budget.

## Delivery and acceptance

1. Implemented: versioned fixtures, required-evidence checks and full MCP response measurements, with a baseline from `008918b` and published per-task results.
2. Implemented: current-graph hydration, isolated-root parity, candidate deduplication, invalid-embedding fallback and stale/deleted/foreign entity rejection across store contracts. No schema or interface migration was needed.
3. Conditional: native Milvus BM25/RRF, collection migration and backfill remain disabled until ADR 0001's triggers hold. The request shape and migration requirements above guide that work; they are not runtime validation.
4. Implemented: task-specific response budgets and deadlines in `internal/agentctx`, MCP argument forwarding, omission notices and the updated agent guide.
5. Passed: `just test` (race), `just lint` (zero issues), `go vet ./...`, and graph/vector contracts against isolated Neo4j 5.26, authenticated HugeGraph 1.5 and Milvus 2.6.4. Evidence and latency limits are published; production defaults and SLA targets are unchanged.

## Repository-policy inputs

[Phase 4h](REPO_CONFIG_PLAN.md) defines `.codesynapse.yaml` exclusions and annotations; [phase 4i](INGEST_TRANSPARENCY_PLAN.md) reports their effects. Benchmarks must record the effective policy fingerprint and compare equivalent eligible content. File-pruning estimates, annotation context changes and per-question retrieval reduction are separate measurements and must not be summed as if they were independent savings.
