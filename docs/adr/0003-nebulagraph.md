# ADR 0003: NebulaGraph replaces HugeGraph as the graph store

- Status: accepted
- Date: 2026-10-25
- Phase: Graph store replacement (`docs/PLAN.md`)
- Topic: store
- Components: store, app, deploy
- Implements: internal/store/nebulastore, docker-compose.yml, deploy/helm/codesynapse/charts/nebula
- Relates: [ADR 0001](0001-search.md)

## Context

HugeGraph was the only production backend after Neo4j was removed. It served the project, but three costs kept growing:

- Gremlin-groovy runs code on the server, so the store needed a password, a network wall and a rule that no raw-Gremlin tool exists. Every fix for that lived in our code, not in the database.
- A repository replacement was one transaction bounded by `edge.tx_capacity` and `vertex.tx_capacity` (at most 1,000,000), which capped the size of an indexable repository.
- The image runs as root and rewrites its own configuration at start, so the Helm chart carried exceptions for it, and we maintained a patched image only to keep scanners quiet.

## Decision

Use NebulaGraph 3.8 (Apache-2.0) through the official `nebula-go` driver, and remove HugeGraph. Graphs are not migrated: an upgraded installation re-indexes. Reviews, curated records and agent memory are in the same graph and start empty.

How the properties we relied on carry over:

- **Statements are constants, values are parameters.** nGQL parameters cannot stand for vertex IDs, tags or columns. Tags and columns are backtick-quoted from allowlists. IDs are hashes (one lowercase letter plus 32 hex characters) written into the text through `literal(vid(...))`, which panics on anything else. This is the one documented exception to "values only in parameters", and it holds because the text it admits can only be hex.
- **No transactions.** `ReplaceRepo` takes a lease (`UPSERT ... WHEN`), writes in place stamped with a run, then deletes the rows of other runs. Readers filter on the live run. A crashed run is cleaned up by the next one. The size cap of the old transaction is gone. The same lease protects access publication and session memory commits. Usage counters add with `UPSERT ... SET calls = calls + $n`.
- **Indexed columns are never NULL.** A composite index misses rows whose later columns are NULL, and a lookup on one column may use any index that starts with it.
- **Tenant data** (agent memory) carries `tenant_id` and `ns`, and every statement over it filters on both first.
- **Authentication is always on.** The shipped stack and the chart replace the well-known first root password with `NEBULA_PASSWORD` (NebulaGraph accepts at most 24 characters) before any service connects, and graphd is never published.
- **Contract.** `storetest` is split into small contracts (code graph, records, reviews, usage, memory, access, session memory, runtime facts) that the memory store and the Nebula store both pass. `just test-nebula` runs it against throwaway containers.

## Consequences

- One production backend again, with the same behaviour as the in-memory twin, proved by the contract.
- The chart runs metad, storaged and graphd in one pod as non-root with a read-only root filesystem, plus a registration sidecar. It is a single instance, like the other in-chart stores; a cluster is linked from outside (`nebula.externalAddrs`).
- Schema changes reach graphd and storaged on their heartbeat, so the stack runs with a 2 s interval and `ensure schema` waits.
- Re-index after the upgrade; recreate the Milvus volume if it was written by an older release.

## Not decided here

MCP and the Nebula role: the MCP service connects with a read-only (GUEST) role and sends its few writes (memory, reviews, curated records, usage) through the API. Until that change lands the MCP service uses the same credentials as the API.
