# ADR 0002: Rank code symbols by meaning with a local model

- Status: accepted
- Date: 2026-10-25
- Phase: Roadmap item 6 (`docs/PLAN.md`)
- Topic: search
- Components: store, agentctx, semantic
- Implements: internal/semantic, internal/agentctx
- Relates: [ADR 0001](0001-search.md)

## Context

ADR 0001 left code symbols without a dense leg: "source is not embedded, by policy". The roadmap kept semantic search open because the task eval showed `minimal_context` was bounded by what lexical search finds, not by what fits in the answer. A task such as "make the worker scale on queued jobs" shares few words with the functions it touches.

The question was whether embedding the *description* of a symbol, not its source, fixes that at an acceptable privacy cost, and whether it pays for itself.

## Measurement

`codesynapse eval --semantic-model nomic-embed-text` embeds each non-test function, method, type and doc section of the graph the eval builds (2,273 symbols for this repository) and adds symbols near the task to `minimal_context`'s ranking (score of the best hit against about 1 per keyword match; the 40 nearest). Same 58 tasks, same 1,500-token budget:

| | recall@5 | recall@10 | hit@10 | tokens |
|---|---|---|---|---|
| lexical only (baseline) | 0.122 | 0.199 | 0.534 | 1,366 |
| semantic weight 0.3 | 0.111 | 0.198 | 0.517 | 1,488 |
| semantic weight 0.8 | 0.175 | 0.289 | 0.690 | 1,488 |
| semantic weight 1.5 | 0.206 | 0.353 | 0.724 | 1,489 |
| semantic weight 2.5 to 4 | 0.239 to 0.250 | 0.350 to 0.353 | 0.759 | 1,488 |

Taking 80 nearest instead of 40 changed nothing. Too small a weight does not help, too large does not hurt, so the setting is not fragile; the answer grows about 120 tokens because the added items carry their reasons. One repository and 58 tasks is a small sample; the gain is large enough (hit@10 up by 0.22) that it is not noise, but the numbers should be re-run when the eval grows.

## Decision

Embed, with these limits:

- **What is embedded:** the kind, qualified name, signature and the first documentation sentence of a symbol (what `Node.Summary` already holds, after `redact.Summary`), and nothing else. Source bodies, comments beyond the first sentence, YAML values and string literals are never embedded.
- **Where:** a local model only. Providers that are not local (`ProviderConfig.Local()`) are refused, as for agent memory. Without a configured local embedding provider nothing is embedded and every answer is exactly what it was.
- **Tenancy:** the index holds descriptions of one graph, which an organisation may see only through its repository access. It is filtered like the graph itself: the search starts from the caller's visible repository, and `store.ForRepository` fences every node the ranking returns. Vectors are never returned to a client.
- **Role:** the semantic leg adds candidates and score to the lexical ranking in `minimal_context`; exact names still win. It does not replace `search`.

## Consequences

- `minimal_context` finds the files of a task far more often (hit@10 0.53 to 0.76 in the eval) without larger answers.
- Ingest needs an embedding call per changed symbol when a local model is configured; unchanged symbols keep their vectors (keyed by content hash).
- The store grows by one vector per symbol; Milvus holds them in production, an exact in-process index serves small installs and tests.
- Vectors follow the published graph. After an ingest's embedding pass, the vectors of symbols and descriptions the graph no longer has are removed (`semantic.Store.Prune`), unless a newer graph was published meanwhile. Deleting a graph (the last organisation link, `reapIfGone`, a single-tenant delete) removes its vectors (`Forget`), and a sweep under the `api-loops` lease every six hours removes the vectors of graphs that no longer exist, for deletes whose vector removal failed. All three page through the index (`vector.Sweeper`) in bounded pages and runs, and every delete names the code-graph tenant and one graph, with values only in Milvus `exprParams`.
- ADR 0001's statement about code symbols is amended by this record; its decisions on memory and the other data stand.

## When to revisit

- The eval grows to more repositories and the gain shrinks below hit@10 +0.10.
- Source-level embedding is proposed: that needs its own decision, since it sends more than a description to the model.
