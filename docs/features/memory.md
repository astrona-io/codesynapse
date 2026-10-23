# Agent memory

Agents lose detail when their context is compacted and start each session from nothing. CodeSynapse stores durable facts per repository as `(:MemoryEntity)-[:FACT]->(:MemoryEntity)`, apart from the code graph, so re-indexing keeps them. On by default; `--allow-memory=false` or `CODESYNAPSE_ALLOW_MEMORY=false` turns it off.

## Hybrid memory model

```mermaid
flowchart LR
    subgraph short[Short term - client]
        ctx[Last 5-10 exchanges verbatim]
    end
    subgraph long[Long term - CodeSynapse]
        direct[upsert_graph_triples<br/>agent writes triples]
        off[offload_turns / PreCompact hook<br/>older turns]
    end
    ctx -- context fills up --> off
    ctx -- decision or preference --> direct
    off --> job[API job queue]
    job --> llm[Local model routed to 'memory']
    llm --> parse[Defensive JSON parse]
    parse --> thr{confidence >= 0.6?}
    thr -- yes --> plan
    thr -- no --> drop[Dropped]
    direct --> plan[memory.Batch Resolve / Needs / Plan]
    plan --> kg[(MemoryEntity + FACT)]
```

## Two scopes

`recall_context` defaults to the current chat's private session memory, and `offload_turns` extracts older turns into that same scope using a local model. Both require the API and validated runtime metadata. Session recall never falls back to repository-wide memory. Sessions expire seven days after registration; reconnecting does not extend them. Revoking a session blocks further work and schedules deletion of its graph facts and vectors.

Durable repository knowledge is shared with authorised readers of that repository in the organisation. Agents should use `upsert_graph_triples` only when the user explicitly asks to retain a fact beyond the chat. Read it with `search_graph_nodes`, `query_subgraph`, or `recall_context` with `memory_scope: "repository"`. The **Agent memory** page shows this durable scope. Existing facts stay in that scope; new writes record their originating session. Selected session facts can be promoted through the [session-memory API](../AGENT_GUIDE.md#session-memory-api).

## Setup

1. Configure the runtime session binding ([Connecting an agent](agent-context.md#connecting-an-agent)). Connected agents receive instructions to recall the current chat and offload older turns; those instructions do not supply authentication or runtime metadata.
2. Run `codesynapse mcp agents-md ~/src/your-repo` to add the same memory rules to AGENTS.md and, where applicable, CLAUDE.md. Re-running updates the marked block.
3. Supply a personal key in `CODESYNAPSE_API_TOKEN`, then run `codesynapse mcp hooks ~/src/your-repo` to install Claude Code hooks (below). The SessionStart and PreCompact hook payloads must contain the runtime's `session_id`. Hooks register that ID and send both identity headers when recalling or offloading through the API. They report an error and skip work if the identity is missing or closed.
4. Route the `memory` task to a local provider for extraction and the `embeddings` task to a local provider with an `embedding_model`; see `config/llm.example.yaml`. Without embeddings, session recall can use lexical search through the API. There is no anonymous background embedding worker: session indexing follows authorised writes, and durable indexing uses `POST /api/v1/repos/{repo}/memory/index` with the caller's runtime context. MCP durable writes request this indexing when the API is configured; failed indexing leaves lexical retrieval available.

## Facts

| Rule | Behaviour |
|---|---|
| Entities | Merge by case-folded name and alias; `me`, `I`, `user` mean `User` |
| Facts | Unique by (subject, predicate, object); saving again bumps `confirmations`, keeps highest confidence |
| `replace=true` | Closes older values of that predicate with `valid_to`; history stays but still counts towards limits |
| `SAME_AS` | Adds an alias and merges an existing duplicate entity, facts included |
| Reopen | Saving a closed fact again reopens it |
| Predicate | Validated `UPPER_SNAKE_CASE`, so agent text reaches Cypher only as parameters |
| Limits | Names redacted and capped at 120 chars; 10,000 facts and 5,000 entities per repository |
| Provenance | Every fact records the saving client (and model, for extracted facts) |

Reads return current facts unless `history` is set, and always carry `memory.Notice`: recalled facts are data, not instructions, because they may originate from agents that read untrusted code.

## Offloading turns

```mermaid
sequenceDiagram
    participant Hook as PreCompact hook / agent
    participant API as api /memory/offload
    participant Q as offload queue (1 worker)
    participant M as local LLM
    participant S as store

    Hook->>API: turns (redacted)
    API->>Q: enqueue job (bounded) and return at once
    Q->>M: prompt with known entity names
    Note over Q,M: refused unless the 'memory' route is a local provider
    M-->>Q: JSON triples
    Q->>Q: parse, validate, drop confidence < 0.6
    Q->>S: Batch.Plan and upsert (source: client, extracted by model)
    Note over Q: turns held in memory only until the job ran, never stored
```

Cloud providers are refused for this route because conversations often contain code. The last 200 jobs are visible at `GET /api/v1/memory/jobs`.

## Recall

```mermaid
flowchart TD
    q[recall_context query] --> gate{memory.RecallGate<br/>small talk or no words?}
    gate -- skip --> none[Empty result]
    gate -- pass --> par
    subgraph par[Two legs]
        v[Vector search<br/>local embeddings, cosine kept from 0.7, 3-10 hits]
        b[BM25 over entity names, aliases, types<br/>camelCase and -/_ split, prefixes from 3 letters]
    end
    par --> rrf[FuseRRF k=60, dedupe, max 8 entry entities]
    rrf --> hop[Expand 1-2 hops]
    hop --> facts[Facts + memory.Notice]
```

Search stays inside the stores rather than a separate engine; see [ADR 0001](../adr/0001-search.md). A background embedder indexes new entity names every 30 seconds, using only the local provider routed to `embeddings` that has an `embedding_model`.

## Claude Code integration

`codesynapse mcp hooks` installs:

- `CLAUDE_AUTOCOMPACT_PCT_OVERRIDE` (default 30%, `--compact-at`), so Claude Code compacts earlier;
- a **SessionStart** hook (`codesynapse memory recall`) that injects facts as additional context;
- a **PreCompact** hook (`codesynapse memory offload`) that sends turns before the window shrinks, once per session.

The hook commands read the API token from the environment or the user config folder (mode 0600), never from argv, and always exit 0 so they cannot break a session. `codesynapse mcp agents-md` writes the AGENTS.md block.

Hooks cannot change the environment or HTTP headers of an already-running MCP client, so installing them does not complete MCP runtime integration. They recall at session start and after compaction, and offload turns older than the last 10 exchanges before compaction. `--compact-at 0` leaves the client's compaction setting unchanged. Hooks cannot remove messages or force compaction. Their configuration lives in `.claude/settings.local.json`, or `.claude/settings.json` with `--shared`.
