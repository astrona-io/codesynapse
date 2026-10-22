# Code architecture audit and upgrade assessment

Status: planned, SaaS phase 4j. Run after repository configuration/annotation (4h) and ingestion transparency (4i), before SSO and the final Kubernetes phase. Use the retrieval benchmarks from 4f and ingestion measurements from 4i as the baseline.

## Purpose and scope

Audit the Go implementation of parsing, semantic indexing, diffing/patching and context selection. Produce an evidence-based keep/upgrade/replace assessment and a staged migration roadmap. Package names below are comparison candidates, not approved dependencies or predetermined replacements.

Inspect the repository, graph, dependency manifests and existing benchmarks first. Record exact versions and implementation locations. Ask the operator only about facts that the repository cannot establish, such as target workload sizes, interactive editing requirements, deployment constraints and language priorities. Resolve material unknowns before final recommendations.

## Current architecture inventory

Start with these locations and verify their behavior at audit time:

| Domain | Evidence to collect |
|---|---|
| Multi-language parsing | `internal/ingest/golang`, `internal/ingest/lang`, `go.mod`: AST/CST libraries, type-checking, structural parser heuristics, parse-error recovery, incremental parsing and generated-file handling |
| Semantic analysis and indexing | `internal/model`, `internal/query`, `internal/agentctx`, graph stores and Go extraction: symbol identity, definitions/references, implementations, call edges, dependency resolution and incremental invalidation |
| Diffing, patching and matching | `internal/gitx`, `internal/review`, `services/api/internal/fixes`: Git/unified diff parsing, model rewrite format, patch matching, stale-file protection, comments/format preservation and validation before a fix branch is pushed |
| Context budgeting and tracing | `internal/agentctx`, `internal/memory`, `internal/metrics`, `services/api/internal/recall` and `llm`: token estimates, provider budgets, graph traversal limits, chunk selection, hydration, source attribution and retrieval benchmarks |

Separate batch indexing guarantees from live-editor behavior. Do not assume a parser intended for complete repositories can safely serve partial buffers, or that syntactic trees supply semantic call resolution.

## Evaluation and measurement

Evaluate each domain against:

- **Malformed and partial code:** incomplete declarations, syntax errors, truncated buffers and mixed valid/invalid files. Measure retained symbols and recovery quality, not just successful parsing.
- **Language coverage:** Go, TypeScript, Python, Rust and Java. Identify which operations share a common graph model and which require language-specific semantics or external indexers.
- **Structural precision:** rename/move detection, repeated code, stale patches, indentation and comment preservation. Validate edited files with the relevant parser/toolchain without weakening untrusted-repository execution controls.
- **Latency and memory:** cold/warm indexing, incremental changes, retrieval and patch application; p50/p95 latency, peak memory, allocations, CPU and lock contention. Attribute costs with Go benchmarks and profiles before calling them bottlenecks.
- **Operational constraints:** CGo policy, cross-compilation, grammar ownership, cancellation, concurrency safety, dependency maintenance and licences. Keep tenant/session isolation, input caps, local-only source handling and fix-path restrictions intact.

Use a reproducible corpus with small and large repositories, malformed buffers, multiple languages and adversarial patch fixtures. Record hardware, toolchain, versions, corpus revision and effective ingestion-policy fingerprint. Keep token estimates distinct from provider-reported usage and report uncertainty.

## Required deliverables

### 1. Stack audit and gap analysis

For Parsing, Indexing, Diffing/Patching and Context Tracing, provide:

| Field | Required content |
|---|---|
| Current choice | Libraries, exact versions, implementation locations and observed behavior |
| Pros and limitations | Supported guarantees, measured costs, missing capabilities and evidence |
| Verdict | Keep, upgrade or replace, with rationale and conditions |

Distinguish measured bottlenecks from suspected ones. Include a no-migration option when the current implementation meets the required workload.

### 2. Package comparisons

Verify maintenance, supported versions, APIs and licences against primary upstream sources at audit time. Compare:

- Native Go AST/type analysis against Tree-sitter, including `smacker/go-tree-sitter` and `tree-sitter/go-tree-sitter`: partial syntax, incremental trees, grammar coverage, CGo overhead, parser ownership and concurrency.
- Existing `golang.org/x/tools` extraction against SSA/callgraph packages, LSP integration and SCIP (`sourcegraph/scip`): semantic fidelity, index format versus indexer responsibilities, external processes, freshness and storage cost.
- Existing Git/unified diff and file rewrite paths against `sergi/go-diff` character matching and structural tree-diff approaches: comments, formatting, ambiguous matches, stale input detection and language support. A text-diff package alone does not establish AST-aware patching.
- Existing context estimates and graph/scope chunking against tokenizer candidates, including `pkoukk/tiktoken-go`: model coverage, exactness, throughput, memory and fallback behavior. Evaluate `tmc/langchaingo` only if orchestration requirements justify it; token counting alone is not a reason to add a framework.

Provide a comparison matrix with benefits, limitations, integration effort and benchmark results. Do not add a dependency during assessment; repository dependency approval rules still apply.

### 3. Migration and architectural roadmap

For each justified change, specify a separate reviewable phase with:

1. The capability gap and acceptance criteria.
2. Interface/adapter design and language-specific boundaries.
3. A bounded prototype and comparative benchmark against the existing implementation.
4. CGo/native resource lifetimes, parser-per-worker or pooling rules, cancellation, memory limits and concurrency tests where applicable.
5. Graph/schema/index migration, stable identifiers, incremental invalidation and mixed-version compatibility.
6. Shadow or opt-in rollout, parity checks, rollback and dependency approval.
7. Required tests, documentation and a commit, push and PR before moving to the next implementation phase.

Prioritize measured correctness and user-facing gaps before speculative rewrites. Keep Kubernetes work in the final deployment phase; the assessment and prototypes must run in local development and disposable test environments.
