# Edge evidence and analysis completeness

Phase 4j.4 records how each call-graph edge was established and how much of each package and file was actually analysed. An agent can then tell a proven caller from a guessed one, and a complete callee list from one with gaps. The [audit](CODE_ARCHITECTURE_AUDIT.md) asked for these fields before any optional SSA analysis, so SSA is not part of this phase.

## Edge evidence

`CALLS`, `REFERENCES`, `IMPLEMENTS` and `DISPATCHES_TO` edges carry an `evidence` property:

| Value | Meaning | Written by |
|---|---|---|
| `typed` | Resolved by go/types | Go extractor, every edge |
| `syntactic` | Resolved from declarations and imports in the source: a same-file symbol, an imported name, `self`/`this` inside a class, a module alias, `Class.static()`, a decorated entry point | Python, JavaScript, TypeScript and Vue extractor |
| `inferred` | Guessed from a function or method name that is unique in the repository; the receiver's type was not known | Python, JavaScript, TypeScript and Vue extractor |
| absent | Unknown: the edge was written before this phase | Older graphs |

When the same edge is found at several call sites, the strongest evidence wins: one typed site proves the edge. Absent evidence never replaces a value, and nothing turns an absent value into `typed`. Stores read values they do not recognise, such as a value written by a newer version, as unknown (`store.ReadEvidence`).

Agents see evidence on `get_call_graph` edges and on the direct callers, callees and tests in `get_symbol_context`. Absent evidence is shown as `unknown`. The graph API returns `evidence` on edges; the field is missing for unknown.

## Analysis records

Package, File, function, method and Repository nodes may carry an `analysis` property: a small JSON record with a `version` (currently 1). A missing record means the graph predates this phase. Readers ignore fields they do not know, so older and newer records can be mixed.

| Field | Meaning |
|---|---|
| `level` | `typed` (Go) or `syntactic` (structural parsers) |
| `status` | `complete`, `partial` (the source had errors) or `truncated` (a work budget ran out) |
| `errors` | Load, parse or type errors |
| `ignored_files` | Go files the build configuration excluded (build tags, GOOS/GOARCH), so their declarations are missing |
| `unresolved_calls` | Call sites through function values or closures, whose target cannot be known statically; calls to other modules are not counted |
| `omitted_bodies` | Function bodies skipped after the budget ran out |
| `partial`, `truncated`, `edges` | Repository totals: files with each status, and resolution edges per evidence value |

For Go, a package with load or type errors is `partial`; the parts go/types could check are still recorded as typed. Each package may spend 100,000 call sites. After that, further function bodies are skipped, counted in `omitted_bodies`, and marked `truncated` on the package, its files and each skipped function. For Python, JavaScript, TypeScript and Vue, a file is `partial` when its brackets do not balance or it ends inside a triple-quoted string. `complete` at the syntactic level only means no such problem was found. It is weaker than a complete typed analysis.

Agents see the file's level and status, and the symbol's unresolved calls, in `get_symbol_context` under `analysis`. `file_outline` returns the file's record, and the repository overview returns the totals.

Not recorded yet: unresolved calls in structural files (the parser cannot tell a library call from an unknown one), and a work budget for structural files, which are already capped at the parser's file size limit.

## Measured against labelled fixtures

Every resolution edge in two small fixtures was listed by hand before the extractors ran. The tests compare the extractors' output with those lists.

| Fixture | Evidence | Edges found | Correct | Precision | Recall |
|---|---|---:|---:|---:|---:|
| Go (`internal/ingest/golang/testdata/evidence`): generics, interfaces, function values, a closure, builtins, a build-tagged file, a package with a type error | typed | 7 | 7 | 100% | 7 of 7 |
| Python and TypeScript (`internal/ingest/lang/evidence_test.go`): imports, `self`/`this`, a class instantiation, method calls on parameters | syntactic | 6 | 6 | 100% | 7 of 7 across both levels |
| | inferred | 2 | 1 | 50% | |

The one wrong inferred edge is the case the audit found: `foreign.save()` in a function whose parameter has no known type is linked to the only `save` method in the repository. The edge is still written, because it is often right (`w.render()` in the same fixture is), but it is now labelled as a guess. Two fixtures this small say nothing about precision on real repositories. They pin behaviour, and the tests fail if it changes.

## Rollout and rollback

- No data migration. Neo4j stores the new edge and node properties as written. HugeGraph's `EnsureSchema` adds the `evidence` property key to existing edge labels and `analysis` to the node label, as it already does for new node fields.
- Existing graphs keep working with unknown evidence and no analysis records until the next sync, which replaces the repository graph with labelled edges. Permanent symbol identities are reconciled as on every ingest.
- `store.Replacement` rejects evidence values it does not know.
- To roll back, deploy the previous version. It ignores the extra properties, and nothing reads them except the code added here.

## Next: optional SSA (4j.4b)

SSA, RTA or VTA call analysis would add edges for calls through function values, the `unresolved_calls` counted above. It is opt-in, so it needs its own evidence value, the existing offline `go list` settings and cancellation, plus a measured cost per package. It builds on these fields and is left for a separate change.
