# Phase 4f retrieval measurements

The versioned workload preserves all required evidence across ten tasks. It does **not** show aggregate token savings on tiny files: budget and provenance metadata increase the returned context. These measurements justify bounded responses and retaining the existing lexical backend, not a new production SLA.

Measured on 2026-10-22 with Go 1.27.3, darwin/arm64, Apple M5 Pro, 18 logical CPUs, concurrency one. Fixture `retrieval-v1` contains four synthetic files (1,134 source bytes) and 15 graph nodes. Fixture SHA-256: `f71a25e030badb4d6fbe2a6834e924a67ef14105ed8eb210773575d4dffa084e`. The baseline uses `008918b` agent-context sources with the same fixture and measurement harness.

Both raw-file and graph answers use the SDK's text-plus-structured JSON envelope, server metadata and completion type. A test compares this representation with real SDK success/error calls. Tokens are `ceil(bytes/4)`, not a model tokenizer. Shared system prefixes/tool definitions and HTTP/SSE framing are excluded. The raw baseline assumes an equivalent structured file-reading tool; clients that consume only text have different costs.

| Task | Raw baseline tokens | Before | After | Required evidence retained |
|---|---:|---:|---:|---|
| Symbol explanation | 572 | 730 | 806 | Yes |
| Callers and callees | 572 | 688 | 751 | Yes |
| Change impact | 572 | 1,251 | 1,314 | Yes |
| Error identifier | 205 | 211 | 274 | Yes |
| Policy clause | 205 | 210 | 273 | Yes |
| Hostname | 205 | 222 | 285 | Yes |
| Mixed-language identifier | 205 | 220 | 283 | Yes |
| Minimal context | 572 | 454 | 537 | Yes |
| No match | 0 | 65 | 65 | Expected error |
| Foreign repository ID | 0 | 54 | 54 | Expected denial |

The eight nonzero baselines sum to 3,108 tokens. Returned context totals 3,986 before and 4,523 after: aggregate reduction is −28.25% and −45.53%, respectively. Zero baselines are not applicable; incomplete answers are excluded rather than counted as savings. The minimal-context task alone reduces 572 to 537 estimated tokens (6.12%), while adding metadata relative to its earlier 454-token answer. These figures do not predict large-file savings or generated-answer quality.

Each task ran 100 times against fresh and reused in-process stores; store construction is excluded. After-change warm total p95, including serialization, ranges from 0.007 to 0.190 ms across tasks. Cold means a fresh memory store, not a cleared OS cache. Embedding, network, Neo4j/HugeGraph/Milvus latency and production concurrency are outside these timings. There is no measured end-to-end SLA benefit.

The real shared BM25 ranker over 5,000 synthetic short entity names measured p50 4.361 ms, p95 4.740 ms and p99 5.006 ms. This remains below ADR 0001's 50 ms lexical migration trigger. The entity cap, searchable text policy and analyzer requirements are unchanged, so native Milvus BM25/RRF is not activated. Dense and RRF quality checks use fixed synthetic vectors/candidates with one relevant entity: all modes recall it, lexical precision is 1 and dense/fused precision is 0.5. Those checks test rank handling, not model accuracy.

Reproduce the after report with the command in [the fixture guide](../internal/metrics/testdata/README.md). Reports include fixture/policy hashes, per-task p50/p95/p99, errors, serialized bytes and completeness. For the before comparison, use the identical harness/fixture with baseline `internal/agentctx` sources through Go's `-overlay` mechanism. Retain reports outside the repository; compare fixture and policy hashes before comparing totals. Future larger-corpus or concurrent benchmarks must be reported separately.

Security and lifecycle validation is separate from latency measurement: the shared graph contracts exercise isolated/missing roots and namespace boundaries; Milvus contracts exercise model/dimension selection, updates, deletion and scoped search. Corrupt-backend tests cover foreign IDs, duplicate/invalid scores, stale entities and aliases, dangling facts, cancelled requests and malformed embeddings. No production backend, collection schema or default search engine was migrated.
