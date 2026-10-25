# ADR 0001: Search inside the stores we run, not in a separate engine

- Status: accepted
- Date: 2026-10-22
- Phase: SaaS 4c (`docs/SAAS.md`)
- Topic: search
- Components: store, memory
- Implements: internal/store, internal/memory, memory.FuseRRF

## Context

CodeSynapse answers agents with small, precise results. Four kinds of things get searched, and they behave differently:

| What | Size per tenant | How it is found today | Embedded? |
|---|---|---|---|
| Code symbols (functions, types, files) | up to hundreds of thousands per graph | exact name, then substring, then graph traversal (callers, implementations) | no: source never leaves the machine as vectors, by policy |
| Agent memory entities | at most 5,000 per organisation and repository (`memory.MaxEntitiesPerRepo`) | name words and vector similarity (local embeddings) | names only, by a local model |
| Doc sections, review comments, commit subjects | thousands per graph | name and path lookup | no |
| Dependencies and findings | hundreds per graph | structured filters | no |

Every query runs for one tenant. Access changes (a removed member, a revoked share, a lost Git token) must take effect on the next request, because the audit log and NIS2 obligations assume they do.

Two shapes were on the table.

**Option A: native hybrid search in the stores we already run.** Lexical ranking (BM25) and dense vectors live next to the data: Milvus 2.6 has BM25 as a built-in function producing sparse vectors, and `hybrid_search` fuses a sparse and a dense request with reciprocal rank fusion (RRF) in one call; HugeGraph has search indexes on properties; Neo4j has Lucene full-text indexes. The tenant filter travels in the same request as the query.

**Option B: a dedicated search engine fed by change data capture.** Elasticsearch, OpenSearch or Solr holds a copy of the searchable text, kept current by a CDC pipeline (Debezium into Kafka, then a sink connector). The engine brings analyzers, highlighting, facets and aggregations.

## Evaluation

| | Option A: in the stores | Option B: separate engine + CDC |
|---|---|---|
| Services to run | none new | the engine (a 3-node cluster for availability), Kafka, Kafka Connect and the connectors |
| Memory footprint | what Neo4j or HugeGraph and Milvus already use | each engine node typically wants several GB of JVM heap; Kafka and Connect add more JVMs |
| Freshness | the read sees the write that came before it | seconds to minutes of lag, more when the pipeline backs up |
| Tenant isolation | the filter (`tenant_id` partition key in Milvus, tenant-rooted traversal in HugeGraph, namespace match in Neo4j) is part of the same request; `vector.Isolate` drops foreign hits again | a second copy of every tenant's data, with its own access rules that must be kept in sync |
| Revoking access | immediate: the authoritative store answers | the copy keeps the data until the pipeline deletes it; a missed delete is a leak |
| Capture source | not needed | Debezium has no Neo4j or HugeGraph connector; Neo4j has its own Kafka connector, HugeGraph none, so we would write dual writes or an outbox |
| Latency | one round trip to a store we already call | a second network hop and a second system to tune |
| Answer size and tokens | we choose the payload: entity names and fused ranks, 3 to 8 entries | the same is possible, but snippets and highlights invite larger answers |
| Licences | Milvus, HugeGraph: Apache-2.0. Neo4j Community (the default store) is GPLv3, run as a separate service | Elasticsearch: Elastic License 2.0, SSPL or AGPLv3, none Apache-2.0 compatible for code we ship; OpenSearch and Solr: Apache-2.0; Kafka and Debezium: Apache-2.0 |
| What it cannot do | facets, highlighting, language-aware analyzers beyond Milvus' built-ins, cross-tenant analytics | nothing we need today |

The licence row is not by itself decisive: Elasticsearch under AGPLv3 could run as an unmodified separate service, the way Neo4j Community does. But the project integrates only Apache-2.0 compatible components where a choice exists, and OpenSearch is a drop-in, so Option B would mean OpenSearch, not Elasticsearch.

## Decision

Option A. Search stays inside the stores that hold the data, and every leg of a query carries the tenant.

How each kind of data is searched:

- **Code symbols:** exact identifier lookup first (`store.Search` ranks exact names before substrings; `qualified_id` resolves ambiguity), then structure through the graph (`get_call_graph`, `get_implementations`). No dense leg, because source is not embedded. (ADR 0002 later added one over descriptions only: name, signature and first doc sentence, local model.)
- **Agent memory:** hybrid. A lexical leg ranks the namespace's entities with BM25 over names, aliases and types (`memory.RankBM25`, behind `store.MemoryStore.MemoryLexical`); a dense leg asks the vector index (`MemoryNearest`, Milvus or Neo4j). Recall fuses the two with RRF (`memory.FuseRRF`, k = 60), deduplicates by entity, keeps 3 to 8 entries and expands them 1 to 2 hops. Ranks are fused, never raw scores, so BM25 values and cosine similarities need no common scale.
- **Doc sections, reviews, commits:** lexical through the graph as today, until a trigger below fires.

The lexical leg for memory runs in-process over the namespace a store has already read through its tenant-rooted lookup, rather than in Milvus. Three reasons: names are a few words long, so BM25 over at most 5,000 of them takes about 5 ms (measured, `BenchmarkRankBM25Full`); lexical recall must work without an embedding model, and Milvus only holds embedded entities; and one ranking function makes the three stores answer identically, which the store contract test checks.

Answers carry entity names, the fused score and which legs found each entry (`via`: vector, name or both). Raw sparse or dense vectors are never returned.

## Consequences

- No new service, no copy of tenant data, no pipeline to monitor. Revocation stays immediate.
- Recall quality improves: before this change the vector and name scores were merged as if they were on one scale, and substring matching treated "service" (in every name) like "payments" (in one). BM25 weighs rare words up; RRF rewards entities both legs agree on.
- We give up facets, highlighting and language-aware stemming. Nothing in the product uses them.
- Search code is spread over three stores. The contract test (`storetest`, "memory lexical ranking") keeps them aligned.

## When to revisit

Move a leg into Milvus' native BM25 and `hybrid_search` (still Option A) when any of these holds:

- longer free text becomes searchable by meaning: doc section bodies, review comments, commit messages;
- a namespace's lexical leg takes more than 50 ms at p95, or memory caps are raised past roughly 50,000 entities;
- lexical ranking needs language-aware analyzers that Milvus provides and our tokenizer does not.

Consider Option B (OpenSearch with CDC) only when one of these is a product requirement:

- faceted search, highlighting or aggregations across large text;
- full-text search over source contents, which would first need a decision to store them;
- a corpus in the tens of millions of documents per tenant;
- an operator who already runs OpenSearch and wants CodeSynapse to index into it.

Even then, the tenant filter must be part of every query and revocation must delete from the copy before the API acknowledges it.
