# Architecture, code intelligence and dependency governance: plan

One connected system for understanding the intended architecture, finding and reusing code, tracing where code came from, and judging dependencies. This page maps each requirement to what exists today and to the slice that delivers the rest. Decisions taken with the project owner:

- Architecture records stay **files in the repository**, indexed on every sync (git is the review and revision history; the CLI works offline).
- Links, annotations, reuse records and proposals made by people or agents live in the **graph store** next to reviews and memory (separate labels that sync never touches), not in PostgreSQL, so single-tenant installs get them too.
- Agents may get **narrow, audited MCP write tools** (propose, link, annotate, record reuse). They write only to the new catalog labels, never to code or the code graph, and a proposal is never an accepted decision.
- MCP names follow the project's snake_case (`architecture_search`, not `architecture.search`).

## Coverage

| Requirement | Today | Slice |
|---|---|---|
| Architecture catalog: records of seven types, lifecycle per type, stable IDs, revisions, supersession, relationships to code, conflict detection | Done: `internal/arch`, `internal/catalog`, API, MCP read tools, web pages | 1a |
| Proposals and links by people and agents, audited | Done: `internal/curation`, `store.CuratedStore`, `architecture_propose`, `architecture_link`, API and pages. Annotations on symbols follow in slice 2 | 1b |
| Component pages joining architecture, code, dependencies, findings, reuse | Records and code links only | 4 |
| Code catalog: symbols with signature, range, fingerprint, tests, callers, dependencies | Exists in the code graph | done |
| Stable symbol identity across renames and moves, ambiguous matches for review, lineage | Conservative fingerprint matching; permanent IDs alongside current locators, historical occurrences, ambiguous candidates and explicit split/merge records. Call-neighbourhood matching and identity reassignment remain | 2 |
| Annotations, purpose, labels on symbols | Organisation-scoped attributed code records, UI/API/MCP | 2 |
| Reuse records, derivation, divergence of copies | Same-repository explicit provenance, revision fingerprints and freshness checks. Cross-repository provenance remains; duplicate detection is not verified lineage | 2 |
| Recursive dependency graph, paths, licences, advisories, cycles, no silent truncation | Done for Go, npm, PyPI, Cargo, Composer, Bundler, NuGet, Gradle (`DependencyGraph`, paths, SBOM) | done |
| Dependency replacement analysis (before/after graph, API, licences, advisories, artifact) | Review page and prompt; no before/after comparison | 3 |
| Unified search, filters, comparison view, analysis workflow states | Per-area pages; no unified search; ingest jobs have running/succeeded/failed | 4 |
| `context.build` with a token budget, omissions explained, targeted expansion | `minimal_context`, `architecture_context`; no combined budgeted package | 4 |
| Reports linked to decisions, code, dependencies and validation, with freshness | Trends, cleanup plan, SBOM; no linked report | 4 |
| End-to-end scenario: ADR to function to dependency finding to alternative to report | Not yet | 5 |

## Slices

1. **Architecture catalog.** 1a: indexing, lifecycle, supersession, conflicts, read API and tools, pages. 1b: proposals and links stored in the graph store, `architecture_propose` and `architecture_link`, audited.
2. **Code identity and reuse lineage.** Match symbols across ingests by evidence (same file and signature, same fingerprint, call neighbourhood), keep the ID when the match is confident, surface ambiguous ones, record splits and merges; annotations; reuse records with original symbol and revision, method, licence and attribution, validation; divergence tracking. Never propagate changes automatically.
3. **Dependency replacement analysis.** From the imports and symbols actually used, find a smaller supported alternative, then compare the complete resolved graph, licences, advisories and API surface before and after. A subpath import or tree shaking alone proves nothing, and copying or vendoring code to avoid a licence is never recommended. Proposals and verified outcomes are stored apart.
4. **Unified experience.** Search across records, symbols, dependencies and reports; component pages; comparison view; analysis jobs with queued, running, completed, partial and failed states; `context_build` within a token budget; linked reports that go stale, never silently rewritten.
5. **Scenario and documentation.** The complete walk-through, its test, and the contracts.

## Rules that apply to every slice

- Permissions are enforced by one shared service for the web and MCP, on a reader fenced to the repository; relationship traversal and summaries cannot cross it.
- Everything says which revision it describes and when it was indexed. Failed or unavailable analysis is never reported as clean, and incomplete is distinguished from empty.
- Retrieved content is evidence, not instructions. Free text written by people or agents is redacted and size-limited before it is stored.
- No source bodies in the graph: summaries, locations and fingerprints only.
