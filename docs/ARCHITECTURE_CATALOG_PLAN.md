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
| Stable symbol identity across renames and moves, ambiguous matches for review, lineage | Fingerprint and bounded call-neighbourhood matching; permanent IDs, historical occurrences and ambiguous candidates. Admin sessions can record reviewed, organisation-scoped identity links; shared graph IDs are preserved. Explicit split/merge records exist | 2 |
| Annotations, purpose, labels on symbols | Organisation-scoped attributed code records, UI/API/MCP | 2 |
| Reuse records, derivation, divergence of copies | Explicit provenance within or between repositories of the same selected organisation, current access checked on both ends, revision fingerprints and freshness checks. Mixed-source records remain; duplicate detection is not verified lineage | 2 |
| Recursive dependency graph, paths, licences, advisories, cycles, no silent truncation | Done for Go, npm, PyPI, Cargo, Composer, Bundler, NuGet, Gradle (`DependencyGraph`, paths, SBOM) | done |
| Dependency replacement analysis (before/after graph, API, licences, advisories, artifact) | Review page and prompt; no before/after comparison | 3 |
| Unified search, filters, comparison view, analysis workflow states | Per-area pages; no unified search; ingest jobs have running/succeeded/failed | 4 |
| `context.build` with a token budget, omissions explained, targeted expansion | `minimal_context`, `architecture_context`; no combined budgeted package | 4 |
| Reports linked to decisions, code, dependencies and validation, with freshness | Trends, cleanup plan, SBOM; no linked report | 4 |
| End-to-end scenario: ADR to function to dependency finding to alternative to report | Not yet | 5 |
| Decision-first planning: detect the domains a task touches, propose options as an ADR, wait for the person's choice, then build against it | Catalog reads, draft proposals and Markdown export exist; no detection, prompt or gate | 6 |

## Slices

1. **Architecture catalog.** 1a: indexing, lifecycle, supersession, conflicts, read API and tools, pages. 1b: proposals and links stored in the graph store, `architecture_propose` and `architecture_link`, audited.
2. **Code identity and reuse lineage.** Match symbols across ingests by evidence (same file and signature, same fingerprint, call neighbourhood), keep the ID when the match is confident, surface ambiguous ones, record splits and merges; annotations; reuse records with original symbol and revision, method, licence and attribution, validation; divergence tracking. Never propagate changes automatically.
3. **Dependency replacement analysis.** From the imports and symbols actually used, find a smaller supported alternative, then compare the complete resolved graph, licences, advisories and API surface before and after. A subpath import or tree shaking alone proves nothing, and copying or vendoring code to avoid a licence is never recommended. Proposals and verified outcomes are stored apart.
4. **Unified experience.** Search across records, symbols, dependencies and reports; component pages; comparison view; analysis jobs with queued, running, completed, partial and failed states; `context_build` within a token budget; linked reports that go stale, never silently rewritten.
5. **Scenario and documentation.** The complete walk-through, its test, and the contracts.
6. **Decision-first planning (ADR gate).** Before an agent writes code for a task with architectural weight, it inspects the context, proposes options as an architecture decision record, waits for the person's choice, and only then builds against it.
   - **Detection** (new read tool `architecture_plan`). Given the task in words and optionally files or a diff, it answers from the indexed graph, so the bridge stays a plain forwarder and nothing scans the workspace on its own. Local changes not yet indexed arrive as the diff, as with `get_pr_context`. The answer covers:
     - **Domains:** Go (from `go.mod` and Go packages), Kubernetes manifests and Helm charts (YAML with `kind:`, templates), Node/TypeScript (`package.json`, `tsconfig.json`), and the other indexed languages.
     - **Target files and symbols** the task is likely to touch.
     - **Records that apply** (accepted ADRs, standards, policies, as `architecture_context` returns them).
     - **Per-domain checks** to respect: Go layout and concurrency rules from the records; resource limits, RBAC and security context for manifests (the existing YAML rules).
   - **"Stick together" topology.** Components that span languages are grouped under one boundary with the evidence for each link, for example a Go service, the Deployment that runs its image and the Keycloak settings it reads through environment and config keys. These become `Component` nodes with `APPLIES_TO` edges, so later records and pages use the same grouping. Uncertain links are marked as candidates, never merged silently.
   - **Interactive gate** (new MCP prompt `decision-first`). The agent presents:
     - the detected context (primary domain, target files, applicable records with links);
     - two or three explicit options, with the recommended standard first and its trade-offs;
     - a question asking which option to lock into an ADR.
     Then it stops until the person answers. The gate is the client's behaviour; the server's guarantee is that nothing it stores can be an accepted decision.
   - **Recording the choice.** There is no file-writing MCP tool: MCP tools never write to code or the repository, and an agent's proposal is never an accepted decision.
     - After the person picks, the agent calls `architecture_propose` with type ADR, the options, the chosen option and the reason. That stores a draft, as today: audited and redacted.
     - It then takes the Markdown from the existing export, with the next free number (`docs/adr/NNNN-<slug>.md`).
     - The agent writes that file into the working tree. The person reviews it and commits it with an accepted status, which is when it becomes a decision.
   - **Building against it.** Code written for the decision carries `@see ADR-NNNN` comments. Ingest turns those comments into `IMPLEMENTS_DECISION` edges, the same edges `architecture_link` makes by hand. `get_pr_context` then flags changes to that code that drift from the record or touch it without a reference.
   - **Tests:**
     - detection on fixtures for each domain and a mixed Go + Kubernetes + Keycloak fixture;
     - topology links with their evidence;
     - the prompt's layout;
     - the proposal is always a draft;
     - the exported file numbering does not collide;
     - `@see` comments become edges, and unknown ADR numbers are reported, not linked.

## Rules that apply to every slice

- Permissions are enforced by one shared service for the web and MCP, on a reader fenced to the repository; relationship traversal and summaries cannot cross it.
- Everything says which revision it describes and when it was indexed. Failed or unavailable analysis is never reported as clean, and incomplete is distinguished from empty.
- Retrieved content is evidence, not instructions. Free text written by people or agents is redacted and size-limited before it is stored.
- No source bodies in the graph: summaries, locations and fingerprints only.
