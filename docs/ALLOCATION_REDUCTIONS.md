# Allocation reductions

Phase 4j.3 acts on the [audit](CODE_ARCHITECTURE_AUDIT.md) profiles: `memory.Store.Search` accounted for 98% of allocation space in context selection, and regexp work plus a per-line owner scan dominated the structural parsers. Public interfaces, store contracts and the graph schema are unchanged. No dependency was added.

## In-memory search

`ReplaceRepo` now builds a search index for each repository:

- every node's name and qualified name, lowercased once with `strings.ToLower` (the folding `model.ContainsFold` uses), with its kind and original qualified-name length;
- those entries sorted in Search's tie-break order: qualified-name length, then ID;
- positions sorted by folded name, so exact matches come from a binary search.

A query folds itself once, takes the exact matches, then scans the sorted entries and stops when `limit` results are collected. Nothing is sorted at query time, and a query with no matches allocates nothing. The order is unchanged: exact names first, then qualified-name length, then ID. The index belongs to one repository graph, so the repository fence is the same as before, and nothing is cached across repositories or tenants.

Search checks the context before it starts and every 1,024 candidates. At the measured no-match speed, about 13 ns per candidate, a cancelled scan stops within roughly 13 µs. Before this change Search ignored cancellation.

The previous implementation is kept as `searchReference` in `internal/store/memory/search_test.go`. `TestSearchMatchesReference` and `FuzzSearchMatchesReference` require identical results for exact, common-prefix, no-match, mixed-kind, empty, Unicode and length-changing queries; the store contract still runs. Neo4j and HugeGraph search inside the database and are not changed; they need their own profiles first.

Measured on an Apple M5 Pro, 18 processors, `go test -bench -count 5`, limit 20. Times are the median of five run means.

| Workload | Before | After | Allocated before → after |
|---|---:|---:|---:|
| 100 nodes, exact | 88.6 µs | 2.3 µs | 249,742 B / 1,536 → 16,400 B / 2 |
| 100 nodes, common prefix | 87.1 µs | 2.5 µs | 247,983 B / 1,426 → 16,384 B / 1 |
| 100 nodes, no match | 9.8 µs | 1.2 µs | 3,992 B / 200 → 0 |
| 100 nodes, mixed kinds | 8.2 µs | 1.2 µs | 2,672 B / 134 → 0 |
| 10,000 nodes, exact | 19.2 ms | 2.4 µs | 43.4 MB / 298,978 → 16,400 B / 2 |
| 10,000 nodes, common prefix | 18.2 ms | 2.3 µs | 43.2 MB / 289,075 → 16,384 B / 1 |
| 10,000 nodes, no match | 1.28 ms | 127 µs | 399,992 B / 20,000 → 0 |
| 10,000 nodes, mixed kinds | 1.94 ms | 7.7 µs | 2.06 MB / 25,545 → 16,384 B / 1 |

With one goroutine per processor on 10,000 nodes (`BenchmarkSearchParallel`), throughput per search is 2.1–2.4 µs for matching queries and 9.7 µs for no match. The read lock is not a measurable bottleneck at this size.

The index moves cost to ingest. `ReplaceRepo` with 10,000 nodes went from 10.4–11.0 ms to 13.9–15.3 ms, from 37.5 to 38.7 MB and from 40,129 to 60,131 allocations. The extra allocations are lowercase copies of names that contain capitals. One search on a large repository recovers the difference.

`minimal_context` (`BenchmarkAuditMinimalContext`), which calls Search repeatedly:

| Workload | Before | After | Allocated before → after |
|---|---:|---:|---:|
| 100 nodes | 2.28 ms | 1.73 ms | 3.42 MB → 2.40 MB |
| 10,000 nodes | 94.3 ms | 1.73 ms | 219.6 MB → 2.38 MB |
| 10,000 nodes, escaping-heavy summaries | 94.6 ms | 2.35 ms | 220.6 MB → 3.37 MB |

### Graph copies

The audit also found repeated graph copies during ingest. `store.Replacement` copies nodes and edges out of `model.Graph`, and the memory store copies them again into its maps, so callers may keep using their graph afterwards. Sharing the graph instead would need a rule that callers never touch a graph after handing it to a store. No caller follows such a rule today, so this phase leaves the copies alone.

## Structural parsers

The TypeScript parser found each line's owning symbol by scanning every symbol, once per line. It now fills a line → owner table once, visiting symbols in the same order with the same "latest start wins" rule.

Each Python and JavaScript security rule's regexp now runs only when the line contains one of the literals every match needs, such as `"hashlib."` for weak-hash detection. Case-insensitive expressions use letter-free literals only (`"("`, `"+"`, `"${"`). `TestRuleGuards` enforces that and checks seed lines for every rule; `FuzzRuleGuards` checks that no line the regexp matches is skipped by its literals. Full extraction of the test repository and a generated 2,100-node corpus produces the same node and edge sets as before.

| Workload (`BenchmarkAuditStructuralParse`) | Before | After | Allocated before → after |
|---|---:|---:|---:|
| Python, 10 functions | 194 µs | 61 µs | 13,036 B / 190 → 13,013 B / 190 |
| Python, 1,000 functions | 19.8 ms | 6.1 ms | 1.24 MB / 18,020 → 1.24 MB / 18,021 |
| TypeScript, 10 functions | 302 µs | 151 µs | 14,001 B / 114 → 14,371 B / 115 |
| TypeScript, 1,000 functions | 34.2 ms | 15.3 ms | 1.36 MB / 10,026 → 1.39 MB / 10,027 |

The owner table is the one extra allocation. In the TypeScript profile after the change, the call-site regexp (`jsCall`, run on every line) takes about 40% of CPU, and rules guarded only by `(` or `+` still run on most code lines. Reducing those needs a different matcher and is left for a later change.

## Budgets

Target workloads have not been agreed, so no numeric budget is enforced. Proposed regression limits, to be confirmed before they become CI checks:

- Search on 10,000 nodes: under 50 µs and at most one allocation per matching query; no allocation for a query with no matches.
- `minimal_context` on 10,000 nodes: under 10 ms and 10 MB.
- `ReplaceRepo` on 10,000 nodes: no more than 1.5× the pre-index time.
- Structural parse, 1,000 functions: Python under 10 ms, TypeScript under 25 ms.

These are microbenchmarks on one machine and are no substitute for the end-to-end measurements the audit lists before any service SLA.
