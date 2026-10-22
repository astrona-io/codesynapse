# Immutable session identity and inherited context

Status: planned, SaaS phase 4g. The execution-context boundary is a prerequisite for all compliant MCP tool execution in production, independently of the later session-memory storage rollout and adds requirements to phases 4d–4f. It is not implemented by the current MCP transport or memory schema.

## Primary session contract

Each incoming tool request belongs to exactly one primary session supplied by its trusted calling runtime as `ctx.session_id`. The identifier remains unchanged for the entire chat thread, including reconnects, retries, compaction, tool calls and background continuations. CodeSynapse must never create, increment, hash into a replacement, randomize or silently substitute a primary session ID while processing that thread.

Validate the supplied identifier's length/format and bind it to the authenticated tenant and permitted subject/session participants in the accounts/control plane before accepting tool work. A claimed ID alone is not authorization. Reject a mismatched tenant, unapproved participant or attempted session change. Do not place session identity in model-controlled tool arguments or accept arbitrary conversation text as runtime metadata.

The calling runtime supplies the identity before the first tool invocation; if it cannot, fail with an actionable missing-session error. Do not fall back to a request ID, repository ID, user ID, token hash, timestamp, random UUID or a shared `default` session. Session registration validates the upstream-provided value; it does not invent a replacement. A new chat can have a new primary identity assigned by its runtime before execution, but a continuation of an existing chat cannot become a new session automatically.

Keep identifiers distinct:

| Identifier | Lifetime and purpose |
|---|---|
| `session_id` | One immutable primary chat/thread identity, owned by the trusted runtime |
| `parent_session_id` | Required inherited value on delegated work; exactly equal to the primary `session_id` |
| MCP transport session ID | Protocol connection/resumption identifier; may change independently and is never substituted for the primary identity |
| Request, trace, job or task ID | Operational correlation; cannot authorize or select chat memory |

Authentication handshakes and health probes do not execute tools or read/write chat memory. Once tool execution begins, missing primary context is an invalid state, not anonymous or unrestricted access. Scheduled and system-originated work also needs an authenticated parent execution context from its invoking runtime; scheduler code must not invent a chat identity to bypass this check.

## Propagation and authorization

Add a validated, immutable Go context value alongside the phase 4d principal and authorized repository:

```text
ExecutionContext {
  tenant_id: authenticated organisation,
  session_id: validated primary session from the runtime,
  subject_id: authenticated participant,
  repository: authorized repository/content scope
}
```

Construct it only at the trusted ingress boundary; expose read-only accessors. Every sub-agent invocation, subprocess, MCP call, memory recall, model request and database operation inherits the same `session_id`. Delegated task/job envelopes additionally carry `parent_session_id = session_id`. Validate equality when consuming an envelope. For in-process work pass the parent context; for detached jobs serialize only the validated identifiers and actor information needed to re-authorize, never bearer tokens or raw turns.

A durable job record must retain its parent identity across process restarts. A worker rechecks membership, session validity and repository permission before processing and before storing results. Missing context, deleted/expired session, revoked access or conflicting inherited IDs prevent execution. A retry keeps its original primary identity; it can have a distinct attempt ID for diagnostics.

For subprocesses, use an explicit non-secret context envelope or narrowly scoped environment values. Never modify global process environment to represent concurrent requests. For sub-agents, the orchestrator passes the primary ID explicitly; agents do not infer it from prompts. Treat metadata received from a client as untrusted until authentication and session binding validate it. Protect forwarded metadata against spoofing through the existing authenticated service channel and a server-side session lookup; forwarding a raw header alone is insufficient.

MCP HTTP middleware resolves primary runtime metadata before calling handlers. Bind protocol transport sessions to the validated primary context, and reject attempts to change it on resumed calls. Stdio has no HTTP headers: define an explicit startup/runtime integration that supplies and validates the parent context, and refuse tool execution without it. Document client compatibility and upgrade paths in `docs/AGENT_GUIDE.md`; changing a transport session must never replace the primary chat identity.

`internal/agentctx`, model routing and all store/vector calls receive the same validated execution context. Required context checks happen before any side effect, query, embedding or queue insertion. Server-generated prompts do not ask the model to choose scope.

## Session chat-memory schema

Separate ephemeral/session chat memory from existing durable repository knowledge. Session-memory entities, facts, vectors and queued offload records require `tenant_id`, `session_id`, content namespace, stable entity/fact ID and retention timestamps. Both ends of every fact must share the same tenant, session and namespace. Names and summaries continue through `redact.Free`; conversation turns remain transient and local-model-only under the existing offload policy.

Use compound identities/indexes including tenant, session and namespace. Extend model allowlists, `nodeprops`, HugeGraph property definitions, vector contracts and migration coverage together. A session deletion/expiry removes its entities, edges and vectors without touching another tenant/session. Deleted sessions cannot be recreated by replaying an old worker envelope. Enforce per-session and per-tenant caps, including maximum active sessions and retained facts.

Existing repository-wide memory is a different scope with different intended visibility. Do not silently give it a fabricated session ID or expose it as session history. Keep legacy records explicitly classified as repository memory; every future write still requires the originating execution context as provenance. Promoting a session fact into durable repository memory is an explicit authorized operation, never an automatic fallback when session context is missing. Session chat-memory recall cannot include repository-wide facts unless the caller explicitly invokes that distinct, repository-authorized retrieval path.

## Database filters

Require all three scope components on every session-memory operation: authenticated tenant, immutable session and authorized content namespace. Parameters below are resolved by middleware, not the LLM.

Milvus conceptual filter, applied identically to sparse and dense requests:

```json
{
  "filter": "tenant_id == {tenant} and session_id == {session} and ns == {namespace}",
  "exprParams": {
    "tenant": "<ctx.tenant_id>",
    "session": "<ctx.session_id>",
    "namespace": "<authorized content namespace>"
  }
}
```

Verify exact REST field names against the pinned Milvus API during implementation. Preserve the tenant partition key; do not partition by unbounded session IDs. Extend `vector.Isolate` to validate tenant, session and namespace on returned session-memory hits, including fetch-by-ID, delete and hybrid-search paths. Never build quoted scalar expressions by interpolating runtime IDs into query text.

Constant HugeGraph/Gremlin template with bound values:

```groovy
g.V().hasLabel('SessionMemoryEntity')
  .has('tenant_id', tenant_id).has('session_id', session_id).has('ns', ns)
  .has('entity_id', entity_id)
  .outE('SESSION_FACT').has('tenant_id', tenant_id).has('session_id', session_id)
  .inV().hasLabel('SessionMemoryEntity')
  .has('tenant_id', tenant_id).has('session_id', session_id).has('ns', ns)
  .limit(limit)
```

Equivalent Neo4j template:

```cypher
MATCH (a:SessionMemoryEntity {
  tenant_id: $tenant_id, session_id: $session_id, ns: $ns, entity_id: $entity_id
})-[f:SESSION_FACT {tenant_id: $tenant_id, session_id: $session_id}]->
(b:SessionMemoryEntity {tenant_id: $tenant_id, session_id: $session_id, ns: $ns})
RETURN a.name, f.predicate, b.name
LIMIT $limit
```

Use separate session-memory labels/edge types to prevent legacy repository-memory reads from accidentally seeing chat history. Add them to the fixed schema allowlists only as part of the reviewed implementation. Validate scoped endpoints before upsert, and make deletion equally scoped. Any lookup by opaque entity ID must still include the tenant/session/namespace checks. The shared code graph remains authorized by repository rather than being duplicated per chat session.

## Telemetry and audit

Primary and parent session IDs are correlation data, not metric dimensions. They must never become VictoriaMetrics labels. Use access-controlled, redacted structured logs/traces and audit records where needed, with retention and no raw conversation content. Tenant/tool/provider/model metric labels continue to follow phase 4e. Context validation failures use a fixed reason enum; do not put the rejected ID in a metric label or error response.

## Delivery and acceptance

1. Specify the trusted runtime metadata contract and client capability negotiation. Add session registration/binding and immutable context accessors; no in-handler session generation API.
2. Enforce context at API/MCP ingress and every tool boundary. Propagate through agent tasks, subprocesses, model calls, queue envelopes and workers. Cover both HTTP and stdio clients.
3. Add session-memory schema/vector migrations and scoped APIs while preserving explicitly separate durable repository memory. Define retention, deletion, cancellation and explicit promotion behavior.
4. Add adversarial tests for missing identity, changed identity during a thread, forged parent IDs, cross-tenant reuse, same-tenant cross-session access, guessed entity IDs, duplicate names, expired sessions, worker retries after revocation, transport reconnects and concurrent goroutine context mixing. Mock a backend returning a foreign-session hit and require it to be discarded.
5. Run Go/lint, PostgreSQL and all graph/vector contract suites. Confirm session IDs are absent from metric labels and model-chosen fields cannot affect scope. Update `AGENTS.md`, `docs/AGENT_GUIDE.md` and client setup documentation before enabling session-memory features in production.
