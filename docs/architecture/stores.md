# Stores and backends

All persistence goes through the `store.Store` interface in `internal/store`. Two implementations must behave identically: HugeGraph in production and an in-memory twin for tests and offline use.

```mermaid
flowchart TB
    subgraph consumers[Consumers]
        query[query / agentctx]
        reviews[reviews]
        memory[memory]
        usage[usage]
    end
    iface{{store.Store<br/>Reader, Writer, ReviewStore, MemoryStore}}
    consumers --> iface

    iface --> mem[memory/<br/>tests and --offline]
    iface --> hg[hugegraphstore/<br/>Gremlin over HTTP]

    hg -.-> milvus[(Milvus<br/>memory vectors)]
    hg --> hgdb[(HugeGraph)]

    contract[storetest.Run<br/>contract + parity tests] -. verifies .-> mem
    contract -. verifies .-> hg
```

| | memory | hugegraphstore |
|---|---|---|
| Query language | Go | Gremlin (constant scripts, values in `bindings`) |
| Use | Unit tests, `--offline` | Every deployment |
| Vectors | `vector.Flat` in process | Milvus REST v2 |
| Licence | in repo | Apache-2.0 |

Neo4j was supported until October 2026. It was removed so the whole data path is Apache-2.0 and there is one production backend to keep correct; graphs are not migrated, so an upgraded installation re-indexes.

## Why contract tests matter

`storetest.Run` is the single definition of correct behaviour. `just test-hugegraph` runs it, and `TestParityWithReference`, against throwaway HugeGraph and Milvus containers; the memory store runs it in `just test`. A new `store.Reader` method is not done until both pass.

## Backend-specific rules

**HugeGraph**

- Vertex IDs are hashed (128-byte cap), write batches are size-bounded (Gremlin's 64 KB request default) and scripts read before they write.
- Gremlin-groovy executes code on the server, so HugeGraph always runs with a password, is never published to the host, and no raw-Gremlin tool exists.
- Statistics rules live once in `store.StatsBuilder`; HugeGraph aggregates groups server-side and feeds them through it.

**Milvus**

```mermaid
flowchart LR
    q[Query text] --> emb[Local embedding model]
    emb --> search[Search with tenant_id partition key<br/>+ exprParams template]
    search --> iso[vector.Isolate<br/>drop any hit of another tenant or namespace]
    iso --> out[Hits]
    notenant[No tenant] -. vector.ErrNoTenant .-> search
```

Only entity names are embedded, and only by a local model.

## Scoping

`store.ForRepository` wraps a store so node IDs, graph traversals and contextual answers are fenced to one repository. MCP and API both use it, so a crafted ID from another repository cannot be read.

## Limits

`store.MaxLimit`, `store.MaxDepth`, request body caps and query timeouts apply everywhere. New endpoints keep them.
