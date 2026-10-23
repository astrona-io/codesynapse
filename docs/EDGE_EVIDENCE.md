# Edge evidence and analysis completeness

Phase 4j.4 records how each call-graph edge was established and how much of each package and file was actually analysed. An agent can then tell a proven caller from a guessed one, and a complete callee list from one with gaps. Phase 4j.4b adds an optional [data-flow analysis](#optional-data-flow-analysis-4j4b) for Go calls through function values.

## Edge evidence

`CALLS`, `REFERENCES`, `IMPLEMENTS` and `DISPATCHES_TO` edges carry an `evidence` property:

| Value | Meaning | Written by |
|---|---|---|
| `typed` | Resolved by go/types | Go extractor, every edge |
| `dataflow` | A call through a function value linked to a function whose value can reach it, found by SSA and variable type analysis (VTA). A possible target, not a proven one | Go extractor, only with the optional analysis |
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
| `dataflow` | `vta` when the optional analysis ran for the package, `skipped` when it could not (type errors, too large, failed); absent when it is off |
| `dataflow_calls` | Unresolved call sites the optional analysis linked to at least one repository function |
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

## Optional data-flow analysis (4j.4b)

Calls through function values (`unresolved_calls`) have no typed edge. With `--dataflow` on `codesynapse ingest`, or `CODESYNAPSE_INGEST_DATAFLOW=true` for the API, the Go extractor also builds SSA for the well-typed repository packages it already loaded and runs [VTA](https://pkg.go.dev/golang.org/x/tools/go/callgraph/vta) over their functions. It adds a CALLS edge with `dataflow` evidence for each call site whose possible targets include a named repository function. No new dependency, no extra `go list` run, and the offline `go list` settings are unchanged.

- Typed edges are never replaced: when the type checker already linked the same caller and callee, the site is not counted again. SSA turns some value calls into static ones (`fn := f; fn()`), so those are linked too.
- Interface calls stay with `DISPATCHES_TO`. Closures have no node: calls inside one belong to the enclosing function, and a closure as a target is not linked.
- Packages with type errors are skipped (`dataflow: skipped`), since SSA needs well-typed code. Test files are not analysed.
- Work is bounded at 200,000 SSA functions; above that the analysis is skipped for every package and the typed graph is kept. Cancellation is checked before each package is built and before VTA. VTA itself cannot be interrupted, which is why it is bounded. x/tools marks VTA experimental, so a panic inside it also skips the analysis rather than failing ingest.

Measured results:

| Where | Dataflow edges | Correct | Notes |
|---|---:|---:|---|
| Labelled Go fixture | 2 | 2 | `fn := double; fn(2)` and `f(x)` inside a generic `Map`; recall 2 of 2 |
| This repository (68 packages) | 16 | 16, checked by hand | A table of lockfile parsers, a package variable tests replace (`canRead = gitx.CanRead`), `install` and `Push` function fields, a prompt renderer |

On this repository the analysis linked 6 of the 4,389 dynamic call sites SSA sees. 183 call only closures, which have no node. 4,200 have no target VTA can see, because SSA is built only for repository code (dependencies come from export data without bodies). Values that come from libraries, such as the `cancel` from `context.WithCancel`, or that arrive as parameters of exported functions, have no known origin. Building SSA for every dependency would cover more sites, at a much higher cost, and is not done.

Cost, on an Apple M5 Pro, median of the later runs:

| Workload | Without | With | Change |
|---|---:|---:|---:|
| Go extraction of this repository | 626 ms, 502 MB allocated | 869 ms, 780 MB allocated | +39% time, +55% allocation; heap size about the same |
| `BenchmarkAuditGoExtract`, 1,000 functions | 31.8 ms, 13.6 MB | 36.8 ms, 20.8 MB | +16% time, +52% allocation |

To roll back, turn the setting off and re-sync: the next replacement has no dataflow edges. Versions before 4j.4b read `dataflow` as unknown evidence.
