# Immutable session identity and inherited context

Status: implemented in SaaS phase 4g. This reference describes the execution boundary and session-memory storage. Client setup is in [the agent guide](AGENT_GUIDE.md).

## Primary session contract

The trusted calling runtime supplies one primary `session_id` before the first tool invocation. It stays unchanged through reconnects, retries, compaction, delegated work and background continuations. CodeSynapse validates and registers it; it never generates a replacement. A request ID, MCP transport ID, timestamp, token hash or shared default cannot substitute for it.

`internal/execution.Identity` carries `tenant_id`, `subject_id`, `session_id` and `parent_session_id`. Every field is required, accepts 1–128 ASCII letters, digits or `._:-`, and is immutable once bound to a Go context. The parent must equal the primary ID. `Require` also rejects canceled or expired contexts. Repository authorization remains a separate check.

| Identifier | Purpose |
|---|---|
| Primary `session_id` | One chat thread, supplied by its runtime |
| `parent_session_id` | Explicit inherited primary ID on every child operation |
| MCP transport session ID | Protocol connection/resumption; never selects chat memory |
| Request, trace, job or task ID | Operational correlation only |

The accounts registry binds each primary ID globally to exactly one organisation and one user. Registration is idempotent for that binding; another participant or tenant cannot claim it. Membership and account suspension are checked on resolution. Platform privileges do not bypass registration or repository permission.

Bindings expire seven days after registration, without sliding renewal. Revocation and observed expiry leave permanent tombstones, so replay cannot reopen a thread. Limits are 1,024 active and 65,536 retained bindings per organisation. Reaching the retained limit requires a future capacity-policy change; deleting tombstones is unsafe. Long-running chats stop at expiry and must not silently switch IDs.

## Runtime integration

Accounts must be configured. The authenticated runtime registers its supplied ID with:

```http
POST /api/v1/execution/sessions
Content-Type: application/json

{"session_id":"<runtime-provided ID>"}
```

The response contains the validated identity. The personal API key selects its organisation; browser sessions select one through the existing organisation header. Registration is a control-plane operation and does not need execution headers.

Every execution request then sends both headers, outside model-controlled tool arguments:

```text
X-CodeSynapse-Session: <registered runtime ID>
X-CodeSynapse-Parent-Session: <same runtime ID>
```

API middleware authenticates the caller, resolves the binding and injects the immutable context. MCP resolves it before every tool call and binds it to the protocol session. A reconnect can keep the primary ID; a resumed transport cannot switch it. HTTP GET/SSE/resume and DELETE validate the same binding. Initialization alone may proceed without it, but no tool work can.

Stdio requires a personal API key plus `CODESYNAPSE_SESSION_ID` and `CODESYNAPSE_PARENT_SESSION_ID` at startup. Use one process per chat thread. The launcher supplies the runtime values; hooks cannot change the environment of an already-running process. URL-based MCP configuration alone does not establish thread identity. Unsupported clients fail closed until their launcher can register and forward trusted runtime metadata.

Authentication, health, governance and ordinary dashboard reads do not require a chat session. Ingest, repository attachment, sync, AI tasks, fix jobs, memory mutations and tool execution do. Legacy accountless service tokens cannot authorize these operations. The web proxy forwards no runtime headers. Instead, a request authenticated by a browser session cookie that sends neither header is bound to that browser's dashboard session (`accounts.DashboardExecutionSession`). Its ID is `web:` plus a hash of the login session's token hash, the selected organisation and the current seven-day window, so:

- only the signed-in browser reaches it, and a new sign-in starts a new one;
- it is registered, audited, limited and tombstoned like any runtime session, and resolving it rechecks membership and suspension;
- the binding a window opens lives seven days, so it is still active when the next window starts; a long sign-in moves to a new ID at the window boundary (the dashboard has no chat thread to keep);
- signing out makes it unreachable; it then expires and its session memory is pruned.

API keys and the service token never get this binding, and a request that names a session must name a registered one: the dashboard binding is not a fallback for it.

## Propagation and jobs

MCP/API handlers pass the context into graph/vector retrieval, local embedding and model calls. The LLM registry checks execution context before provider lookup or network work. AI and fix runners reject missing context before reading their stores. Subprocess environments remove ambient execution variables and copy the validated identity without changing global environment.

Queued jobs copy the identity and actor/key identifiers, never bearer tokens. Workers restore it onto their lifetime context and recheck session validity, membership, key status and repository permission before work and before persistence. Persisted schedules retain this identity across restart; legacy schedules without it and schedules running without accounts fail closed. Retries keep the original primary ID. General jobs and conversation-offload queues are transient and are not replayed after restart.

Offload turns are redacted before queueing, bounded and held only in memory. Shutdown drops pending buffers. Extraction uses only the local memory model and rechecks authorization before storing facts. The old repository offload endpoint returns HTTP 410; it cannot silently turn a conversation into shared repository memory.

## Session and repository memory

| Scope | Access and storage |
|---|---|
| Session memory | Current participant, tenant, primary session and authorised repository namespace; separate entities, facts and vectors |
| Repository memory | Existing organisation/repository permissions; explicit durable tools and recall; no fabricated session on legacy records |
| Promotion | Explicit selection of current session fact IDs, requiring repository write permission |

`recall_context` defaults to session memory. `memory_scope: "repository"` selects durable recall explicitly. `offload_turns` writes session memory only. `upsert_graph_triples`, `search_graph_nodes` and `query_subgraph` retain their documented repository scope. New durable writes record `origin_session_id`; older records remain without one.

The API exposes these routes under `/api/v1/repos/{repo}`; all require execution headers and repository permission:

| Method and suffix | Operation |
|---|---|
| `GET /session-memory` | Read this session's facts; optional `q` and bounded `limit` |
| `POST /session-memory` | Upsert validated `triples` with optional `source` |
| `DELETE /session-memory` | Clear this namespace permanently for this session |
| `POST /session-memory/recall` | Recall from this session; `query`, current facts only |
| `POST /session-memory/offload` | Queue local extraction from `turns` |
| `POST /session-memory/promote` | Copy 1–50 selected `fact_ids` into durable repository memory |
| `POST /memory/index` | Refresh a bounded batch of durable entity embeddings under this session |

Promotion does not delete session facts. Deleting session memory does not delete promoted facts. A cleared namespace remains closed for that thread; the server cannot assign another ID to get around deletion. Memory job list/detail responses are restricted to the requesting execution identity.

## Graph and vector schema

`store.SessionMemoryStore` has identical contracts in memory, Neo4j and HugeGraph. The shared engine validates compound IDs, fact endpoints, predicates, scope and retention before an adapter commits a bounded snapshot. Backend transactions enforce revision checks and quotas, including writes that bypass the engine.

| Record | Scope and fields |
|---|---|
| `SessionMemoryScope` | `tenant_id`, `session_id`, `ns`, bounded JSON `doc`, revision, expiry, deletion tombstone, fact count |
| `SessionMemoryEntity` | Same three scope fields; compound entity ID, redacted entity document, expiry, optional embedding |
| `SESSION_FACT` | Scoped endpoints; same scope fields, fact ID, predicate, redacted fact document, expiry |
| `SessionMemoryRevocation` | Tenant/session tombstone covering every namespace |
| `SessionMemoryTenant` (Neo4j) | Tenant transaction lock for concurrent quota checks |

Neo4j uses `id` for entity IDs; HugeGraph uses `eid`. Session entities never carry the shared code `Node` label, and session edges never use durable `FACT`. The authorised code graph is shared by repository rather than duplicated per thread. No new generic code-node properties are required in `nodeprops`.

Each namespace permits at most 512 facts, 1,024 entities and a 24 KiB serialized snapshot. A session permits 16 retained namespaces and 2,048 live facts; a tenant permits 2,048 retained scope roots and 65,536 live facts. Deleted and expired roots count toward namespace/root caps. The snapshot size can bind before the numeric limits; backend request caps can reduce the accepted size further. Retained-root caps are lifetime limits until a safe tombstone compaction policy exists. HugeGraph serializes commits on its single server instance; this is not a distributed cluster-lock guarantee.

Reads load the authoritative scope snapshot with tenant, session and namespace filters. Materialized edge traversals preserve the same scope on both endpoints. Representative bound queries are:

```cypher
MATCH (a:SessionMemoryEntity {
  tenant_id: $tenant, session_id: $session, ns: $ns, id: $entity
})-[f:SESSION_FACT {tenant_id: $tenant, session_id: $session, ns: $ns}]->
(b:SessionMemoryEntity {tenant_id: $tenant, session_id: $session, ns: $ns})
RETURN a, f, b LIMIT $limit
```

```groovy
g.V().hasLabel('SessionMemoryEntity')
  .has('tenant_id',tenant).has('session_id',session).has('ns',ns).has('eid',entity)
  .outE('SESSION_FACT').has('tenant_id',tenant).has('session_id',session).has('ns',ns)
  .inV().hasLabel('SessionMemoryEntity')
  .has('tenant_id',tenant).has('session_id',session).has('ns',ns).limit(limit)
```

Scripts are constants and values travel through bindings. Snapshot reads additionally check revocation and expiry; a raw traversal is not a substitute for that check.

Milvus uses separate session collections per embedding model/dimension, preserving the tenant partition key. Compound physical vector IDs prevent identical logical IDs in different sessions from overwriting one another. Searches send:

```json
{
  "filter": "tenant_id == {tenant} && session_id == {session} && ns == {ns} && expires_at > {now}",
  "exprParams": {
    "tenant": "<authenticated tenant>",
    "session": "<registered primary session>",
    "ns": "<authorised namespace>",
    "now": 0
  }
}
```

`now` is the server's current Unix time in milliseconds. Returned hits are checked again for tenant, session, namespace, expiry and valid scores, then hydrated against a fresh authoritative graph snapshot. Session recall combines bounded BM25 over scoped names/aliases/types with local dense vectors and reciprocal-rank fusion. Missing embeddings retain lexical recall. Neo4j session vectors use bounded exact similarity within the selected scope.

Embedding refresh runs under an authorised execution after session extraction/upsert, after durable MCP writes, or through explicit indexing. There is no anonymous global embedder. Failed optional embedding refresh leaves lexical recall available.

## Retention, audit and verification

Memory expiry is fixed to the registry binding's deadline. Explicit session revocation immediately blocks execution and attempts graph/vector cleanup. A minute-based worker retries cleanup and handles expired sessions in bounded batches; expired memory is also removed on access. Tombstones prevent old workers from recreating data. Cleanup failures cannot restore execution access.

Primary and parent session IDs are never metric labels. Audit records may contain session targets under existing access controls; errors do not echo rejected IDs. Prompt text and raw turns stay out of telemetry.

Shared contracts cover conflicting identity, cross-tenant/session reads, guessed IDs, duplicate names, revoked/expired writes, direct adapter revival, immutable expiry, bounded retained roots, and foreign vector hits. API/MCP tests cover HTTP and stdio propagation, transport rebinding, membership revocation, job inheritance and canceled work. The release checks include Go race tests, lint/vet and PostgreSQL, Neo4j, HugeGraph and Milvus integration contracts.
