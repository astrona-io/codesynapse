# Retrieval, vectors and agent memory

!!! tip "In plain words"
    How CodeSynapse finds the right answer for an assistant. Most answers come from the graph by exact lookups, with no guessing. Vectors (numbers that capture meaning, see [embedding](../GLOSSARY.md#ai-assistants)) are used in only a few places, mainly to recall agent memory. This page shows which is which.

CodeSynapse answers agents from a graph, not from embedded source. This page shows where retrieval is deterministic, where vectors are used, and how agent memory is written and recalled. Planned work (Milvus-side hybrid search) is marked as planned; see the [retrieval plan](../plans/RETRIEVAL_PLAN.md).

## The short version

| Question | Answer |
|---|---|
| Is code embedded? | No. Source bodies never enter the graph or a vector index. |
| What is embedded? | Only agent-memory **entity names**, by a **local** model. |
| Is this RAG? | Graph retrieval for code (exact lookup, then traversal) plus hybrid retrieval (BM25 + vectors) for memory. Both return small cards, not file text. |
| Where does the model run? | Embeddings and fact extraction use local providers only. Cloud providers are refused for these routes. |
| What if no embedding model is configured? | Recall degrades to names (BM25) and keeps working. |

## Three retrieval paths

```mermaid
flowchart LR
    agent[Agent / MCP client]
    agent --> code[Code questions<br/>minimal_context, get_symbol_context,<br/>get_call_graph, search]
    agent --> mem[Memory questions<br/>recall_context, search_graph_nodes,<br/>query_subgraph]
    agent --> other[Docs, config, findings,<br/>dependencies, reviews]

    code --> exact[Exact name, then substring]
    exact --> trav[Graph traversal<br/>callers, implementations, tests]
    trav --> card[Symbol card + line ranges]

    mem --> bm25[BM25 over names, aliases, types]
    mem --> vec[Vector nearest neighbours]
    bm25 --> rrf[RRF fusion]
    vec --> rrf
    rrf --> hop[1-2 hop expansion]
    hop --> facts[Facts + notice]

    other --> struct[Structured lookups in the graph]
```

| Data | Found by | Embedded |
|---|---|---|
| Code symbols, files | exact identifier, then substring, then traversal | no |
| Doc sections, config entries, commits, reviews | name and path lookup in the graph | no |
| Dependencies, findings | structured filters | no |
| Agent memory entities | BM25 and vector similarity, fused | names only |

The reason for this split is [ADR 0001](../adr/0001-search.md): search stays inside the stores that hold the data, so tenant filters travel with the query and revoking access takes effect on the next request.

## Components

```mermaid
flowchart TB
    subgraph clients[Clients]
        cc[Claude Code, Codex, Cursor...]
        hooks[codesynapse memory<br/>SessionStart / PreCompact hooks]
    end
    subgraph mcp[mcp service]
        tools[recall_context, upsert_graph_triples,<br/>search_graph_nodes, query_subgraph, offload_turns]
        fallback[Names-only recall<br/>when the API is unreachable]
    end
    subgraph api[api service]
        recall[recall.Service<br/>gate, fuse, expand]
        embedder[Background embedder<br/>every 30 s or on kick]
        offload[offload worker<br/>local model extracts triples]
        llm[llm.Registry<br/>routes: memory, embeddings]
    end
    subgraph stores[Stores]
        hg[(NebulaGraph)]
        mv[(Milvus<br/>tenant partition key)]
        flat[vector.Flat<br/>tests, --offline]
    end
    local[Local model server<br/>Ollama or compatible]

    cc --> tools
    hooks --> api
    tools --> recall
    tools -. API down .-> fallback
    recall --> llm
    embedder --> llm
    offload --> llm
    llm --> local
    recall --> stores
    embedder --> stores
    offload --> stores
    hg --- mv
```

- **Store interface.** `store.MemoryStore` exposes `MemoryLexical`, `MemoryNearest` and `MemorySubgraph`. NebulaGraph pairs with Milvus through `vector.Index` (vector length checked against `memory.MaxDimensions`). The in-memory store uses `vector.Flat`. All three pass the same contract tests.
- **Embedding model is part of the key.** Vectors are stored per model name, so changing `embedding_model` does not mix dimensions; entities without a current embedding are picked up again by the embedder.
- **Source of vectors.** Only the provider routed to `embeddings` that has an `embedding_model` and is local (`llm.Registry.EmbeddingProvider`). Inputs pass `redact.Free` in the registry.

## Writing memory

Two paths lead to the same validated write.

```mermaid
sequenceDiagram
    participant A as Agent
    participant M as mcp service
    participant API as api service
    participant L as Local LLM
    participant S as Store
    participant E as Embedder

    Note over A,S: Path 1: the agent saves a fact itself
    A->>M: upsert_graph_triples
    M->>M: validate predicate, redact, cap sizes
    M->>S: Batch.Plan then upsert (scoped org|graph)
    S-->>E: new entities have no embedding yet

    Note over A,S: Path 2: older turns are handed off
    A->>M: offload_turns (or PreCompact hook)
    M->>API: turns, bearer token
    API-->>M: job accepted at once
    API->>L: prompt with known entity names
    L-->>API: JSON triples
    API->>API: parse defensively, drop confidence below 0.6
    API->>S: upsert, record client and model
    S-->>E: new entities have no embedding yet

    E->>L: embed entity names (batches of 64)
    E->>S: store vectors per model
```

Rules that apply on both paths: predicates are `UPPER_SNAKE_CASE`, names are redacted and capped at 120 characters, each repository holds at most 10,000 facts and 5,000 entities, and turns are held in memory only until the job ran. Details are in [Agent memory](../features/memory.md).

## Recalling memory

```mermaid
flowchart TD
    start[recall_context at task start] --> gate{RecallGate}
    gate -- greeting or no words --> skip[Skipped, with reason]
    gate -- ok --> par
    subgraph par[Legs run for one tenant and namespace]
        direction LR
        v[Vector leg<br/>embed query locally,<br/>keep cosine 0.4 and up]
        b[Lexical leg<br/>BM25 over names, aliases, types]
    end
    par --> fuse[FuseRRF k=60<br/>ranks, not scores]
    fuse --> pick[Up to 8 entry entities<br/>via: vector, name or both]
    pick --> expand[MemorySubgraph<br/>1-2 hops, limit 40]
    expand --> out[Facts + memory.Notice]
    v -. no embedding model or error .-> mode[mode = names]
```

- **Fusion on ranks.** BM25 values and cosine similarities have no common scale, so only ranks are fused. An entity found by both legs scores higher and is marked `both`.
- **Why the lexical leg is in Go.** Names are short and capped at 5,000 per repository, so BM25 takes about 5 ms; it works without any embedding model; and one ranking function makes all three stores answer alike. Moving fusion into Milvus is a measured, planned extension.
- **Degradation.** No embedding model, an embedding failure, or an unreachable API each leave the lexical leg standing. The MCP service falls back to names-only recall on its own.
- **Recalled facts are data.** Every answer carries `memory.Notice`: facts may come from agents that read untrusted code, so they are never instructions.

## Where the pieces run in a session

```mermaid
sequenceDiagram
    participant H as SessionStart hook
    participant A as Agent
    participant CS as CodeSynapse
    participant P as PreCompact hook

    H->>CS: memory recall
    CS-->>A: facts injected as context
    A->>CS: minimal_context, get_symbol_context (graph, no vectors)
    A->>CS: upsert_graph_triples on decisions and preferences
    Note over A: context fills up
    P->>CS: offload older turns
    CS-->>CS: extract, validate, save, embed in background
```

The agent keeps the last 5 to 10 exchanges verbatim. Anything older is either already a fact or is offloaded. Code understanding never waits on memory: the graph tools answer first, memory adds decisions and conventions.

## Isolation, layer by layer

| Layer | Control |
|---|---|
| Authentication | Accounts mode needs a personal API key on every MCP call; the installation token grants no content access. |
| Namespace | Memory lives under `org\|graph`; every statement filters on `tenant_id` and `ns` first (nGQL, values in parameters). |
| Vector request | Milvus filters use `exprParams` with `tenant_id` as the partition key; values never go in the expression. |
| Vector result | `vector.Isolate` drops any hit of another tenant or namespace, even if the backend filter was wrong. |
| Search without tenant | `vector.ErrNoTenant`. |
| Output | Redacted names, bounded hits (3 to 10), bounded hops, size caps, `memory.Notice`. |

## Telemetry

Recalls are counted by tenant and mode (`names` or `hybrid`). Queries, entity names and file paths never become labels; they belong in logs.

## Planned

- Milvus `hybrid_search` (BM25 sparse plus dense, RRF in the store) once entity counts or measured lexical latency pass the ADR trigger.
- Session-scoped chat memory filters ([phase 4g](../plans/SESSION_CONTEXT_PLAN.md)).
- A published token and latency benchmark. The only measured figure so far is the symbol-card result on Go standard-library sources in the retrieval plan, which does not cover memory.
