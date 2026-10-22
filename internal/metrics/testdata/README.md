# Retrieval workload

`retrieval_v1.json` holds synthetic source files, their graph metadata and the
evidence required for ten tasks. The tests load metadata directly into the
memory store; this measures retrieval, not parser accuracy. Increment the
fixture version when changing its content or evidence requirements.

Run the evidence checks with the normal test suite. To write timing and size
measurements without the race detector:

```sh
CODESYNAPSE_BENCH_CPU='your CPU model' \
CODESYNAPSE_BENCH_LABEL='commit or comparison label' \
CODESYNAPSE_RETRIEVAL_REPORT=/private/tmp/codesynapse-retrieval.json \
go test ./internal/metrics -run '^TestRetrievalReport$' -count=1 -v
```

Each task runs 100 times against newly constructed and reused in-process
stores. Store construction is excluded. “Cold” means a fresh store, not a
cleared operating-system cache. Retrieval, serialization and total warm
latencies are reported separately using nearest-rank p50, p95 and p99.
Concurrency is one. Errors include expected no-match and foreign-ID errors;
read `expected_error` before interpreting the counts.

The raw baseline contains each task's listed files once, with paths. Both
baseline and returned context use the pinned MCP SDK's JSON-RPC result shape:
successful results contain `structuredContent` and the duplicate JSON in
`content[].text`. Both copies, escaping and metadata count toward
`ceil(bytes/4)`. A test compares this envelope with an actual SDK tool call,
including the text-only error result. HTTP/SSE framing is excluded.
The envelope also includes the SDK's server metadata (`codesynapse`, fixed
version `fixture-v1`) and completion result type under default negotiation.
One-time system prompts and tool schemas are excluded from both baselines;
the raw-file baseline assumes an equivalent structured tool response. These
are serialized response estimates, not a claim about what a client sends to
its model, and `max_tokens` bounds the structured payload only. There is no target-model tokenizer or
LLM judge. Evidence checks require exact string values in structured results;
they test retrieval coverage, not the correctness of a generated explanation.
An incomplete answer never contributes to the aggregate savings calculation.
Zero baselines and incomplete answers have `null` reduction. Negative
reductions remain negative: graph metadata can exceed the size of tiny files.
The aggregate uses summed token counts, not mean task percentages.

The memory measurement ranks 5,000 synthetic short entity names with the real
BM25 function, then applies Go RRF to those results and fixed dense candidates.
It records whether lexical p95 exceeds ADR 0001's 50 ms migration trigger.
Dense candidates are synthetic; the report makes no embedding accuracy,
Milvus-side fusion, network, graph-hydration or production-SLA claim.

For before/after comparisons, run the same fixture and harness against both
revisions and check their fixture and policy hashes. Keep reports outside the
repository. Record CPU model, Go version, concurrency and workload changes;
do not compare a race-enabled run with a normal timing run.
