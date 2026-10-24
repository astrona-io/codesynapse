# Plan and architecture

## Goal

Make coding assistants cheaper and better on real repositories. An agent that greps and reads whole files spends most of its context on code it doesn't need. A graph that already knows every symbol's location, signature, summary, callers, tests and docs can answer most navigation and review questions in a fraction of the tokens, and with better precision than text search.

Second goal: give humans the same picture in a dashboard, so code quality work (hotspots, untested public API, risky changes) is visible.

Inspired by [code-review-graph](https://github.com/tirth8205/code-review-graph). What we do differently:

| | code-review-graph | CodeSynapse |
|---|---|---|
| Call resolution | tree-sitter AST, heuristic + "inferred" edges | go/types: exact static calls, interface dispatch via `DISPATCHES_TO` |
| Store | single SQLite file | HugeGraph (+ Milvus), multi-repo, shared by services |
| HTTP auth | Host/Origin checks | bearer tokens, refuses unauthenticated non-loopback binds |
| Write tools | `apply_refactor` writes files | none; all tools read-only |
| Secrets | names sanitised | summaries redacted, no source bodies, hashed author e-mails |
| Security review | – | graded review: type-aware code rules, YAML rules, CVEs with reachability, exposure and test context |
| Languages | 40+ | Go (type-checked), Python, JS/TS/Vue, Markdown, YAML, Go templates/Helm |
| Dependencies | – | Go, npm, PyPI, Cargo, Composer, Bundler, NuGet and Gradle with CVEs; symbol-level reachability for Go |
| Dead code | – | reachability-based liveness, unused modules/config, security-first ordering |
| Fix guidance | – | per-rule guide with vulnerable → fixed examples per language |

## Architecture

```
             ┌──────────────────┐  MCP (stdio/http)  ┌───────────┐
 agents ───▶ │ codesynapse-mcp  │ ─────────────────▶ │           │
             └──────────────────┘                    │ HugeGraph │
 browser ──▶ web (Nuxt 4) ─▶ /api/cg ─▶ codesynapse-api ─▶ │           │
                                                     └───────────┘
 CI / dev ─▶ codesynapse CLI (ingest, audit, stats, review --offline) ─▲
```

Every reader goes through `internal/query` (and `internal/agentctx` for the composite agent tools), so MCP, API and CLI return the same answers. `internal/store/memory` implements the same `store.Store` interface for tests and `--offline` mode.

### Ingest pipeline

1. **Go** (`internal/ingest/golang`): `go/packages` loads all packages with tests. Pass 1 handles primary packages; pass 2 adds `_test.go` files from test variants. For each declaration it records the location, signature, doc summary, cyclomatic complexity, a body hash, and calls resolved through `types.Info.Uses`. Calls to sensitive external APIs become `External` nodes with security tags. Interface satisfaction is computed with `types.Implements` across in-repo types.
2. **Docs** (`internal/ingest/docs`): Markdown files are split into heading sections with line ranges and summaries. Inline code spans that name an exported symbol become `MENTIONS` edges (ambiguous short names are skipped).
3. **History** (`internal/ingest/githistory`): the last N commits with numstat give `Commit`/`Author` nodes, `MODIFIES` edges and per-file churn.
4. **Persist**: validate a bounded replacement, then publish nodes, edges and stale removals atomically. HugeGraph stages bounded chunks outside the code labels and publishes them in one transaction. Recheck authorization and the pinned policy before commit; failed publication preserves the previous graph.

### Schema

Every code node has the label `Node` plus its kind, and the properties `id`, `repo`, `kind` and `name`. IDs are `repo|Kind|local`.

| Node | Key properties |
|---|---|
| Repository | head, branch, url (origin, no credentials), root, time (ingested at), meta (JSON, including repository_policy), metrics (JSON), license, license_source, licenses (JSON report), analysis (JSON totals) |
| Package | qualified_name, summary, analysis (JSON) |
| File | file, loc, churn, is_test, analysis (JSON) |
| Function / Method | qualified_name, file, start_line, end_line, signature, summary, complexity, loc, body_hash, exported, is_test, security_tags, analysis (JSON, only when calls were unresolved or the body skipped) |
| InterfaceMethod | as Method, no body |
| Type | signature (struct/interface/…), summary |
| External | qualified_name, security_tags (sensitive APIs only) |
| Document / Section | file, start_line, end_line, level, summary |
| Commit | name (short hash), summary (subject), time |
| Author | name (e-mail stored only as a hash in the ID) |
| Config / ConfigEntry | YAML file; Kubernetes resource (`Kind/name`) or key path, with line range |
| Template / TemplateDef | Go template or Helm template file; `{{define}}` block |
| Finding | rule, severity, cwe, file, line, summary (never the secret) |
| Module | path, version, indirect, ecosystem, dev, license (SPDX), license_source |
| Vulnerability | id, aliases (CVE/GHSA), severity, cwe, version, fixed_version, reachability, url |
| McpUsage (not `:Node`) | key (`day\|repo\|tool\|client`), day, repo, tool, client, calls, errors, response_tokens, duration_ms, max_duration_ms |
| MemoryEntity (not `:Node`) | id (`repo\|mem\|name`), repo, name, type, aliases, created_at, updated_at; joined by `:FACT` relationships with id, repo, predicate, source, confirmations, confidence, valid_from, valid_to, created_at, updated_at |
| SessionMemoryScope | tenant_id, session_id, ns, bounded JSON snapshot, revision, fixed expiry, deletion tombstone, fact count |
| SessionMemoryEntity (not `:Node`) | tenant_id, session_id, ns, compound entity ID, redacted entity document, expiry, optional embedding; joined only by scoped `:SESSION_FACT` |
| SessionMemoryRevocation | tenant_id, session_id; permanent replay tombstone |

Edges: `CONTAINS`, `DEFINES`, `HAS_METHOD`, `CALLS` (with `count`), `DISPATCHES_TO`, `IMPLEMENTS`, `IMPORTS`, `MENTIONS`, `MODIFIES` (with `count` = lines changed), `AUTHORED`, `HAS_FINDING`, `DEPENDS_ON`, `AFFECTS`, `INCLUDES`, `USES_VALUE`.

`CALLS`, `REFERENCES`, `IMPLEMENTS` and `DISPATCHES_TO` carry `evidence`: `typed`, `dataflow` (optional Go SSA/VTA analysis), `syntactic` or `inferred`, absent (unknown) on graphs indexed before phase 4j.4. The `analysis` records say how completely each package and file was analysed and what was left out. See [edge evidence](EDGE_EVIDENCE.md).

Access projections are separate from code nodes. `Organization` carries `tenant_id`, `default_repo_access` and `policy_revision`; tenant-local `User` carries `user_id`, `active` and `inherits_default`; `Team` carries `team_id`; `AccessRepository` carries `repo_id`, `graph_id` and `usable`. `MEMBER_OF`, `BELONGS_TO` and `HAS_ACCESS` connect only these access vertices within a tenant. Grants carry `role` and numeric `rank`. HugeGraph maps each source/target pair to a fixed physical edge label. Revision/digest-scoped IDs and completion records make rebuilds retryable; PostgreSQL remains authoritative. See [the access-control implementation](ACCESS_CONTROL_PLAN.md#implementation-and-upgrade).

Code declarations also carry `symbol_id` (a random permanent identity) and
`revision`; the existing `id` remains the current graph locator. A repository's
`identities` property stores the versioned identity baseline, including removed
declarations, matching evidence and at most ten earlier occurrences per symbol.
There are at most 50,000 retained identities and 64 MiB of encoded state: exhaustion fails ingest rather
than dropping identities. Matching work and review candidates are bounded;
omissions are explicit. The baseline and graph are replaced together, using
the stored baseline read before the existing serialized ingest job. Avoid
concurrent CLI/API ingests of the same repository. No source bodies are stored.

`Curated` code records add organisation-scoped annotations and explicit
reuse, adaptation, split and merge lineage. An optional source repository may
be another repository in the same selected organisation. Every creation checks
source read access and destination write access; every read rechecks source
access and omits the whole record if it is lost. An allowed but unavailable
source is marked as needing review, never clean. Each record pins symbol
IDs, revisions and fingerprints, actor/client and the author's reason,
modifications, compatibility, licence/attribution and validation assessment.
Freshness and endpoint changes are computed on reads, without rewriting the
historical record. These are attributed claims, not independently verified
outcomes. Mixed-source-repository records and identity reassignment remain
future work. See [the agent contracts](AGENT_GUIDE.md#permanent-code-identities-and-reuse-evidence).

### Risk scoring

`review.Score` is a transparent, additive 0-100 score: direct callers (×3, max 25), transitive impact (max 20), exported (+10), complexity (+5/10/15), security tags (+15 high-risk / +8 other, max 30), and +15 when no test reaches the symbol. Levels: low < 20 ≤ medium < 40 ≤ high < 60 ≤ critical. Every point added comes with a reason string, so reviewers and agents can see why something scored high.

## Decisions

- **Apache HugeGraph as the graph database** with Milvus for memory vectors, plus an in-memory twin. Neo4j was supported until October 2026 and then removed: HugeGraph and Milvus are Apache-2.0 (Neo4j Community is GPL-3.0), scale out (HugeGraph on distributed storage, Milvus with tenant partitions), and keep tenant isolation in the data itself. Both are spoken over HTTP with the standard library: the Milvus Go SDK pulls in ~470 packages and a cgo CPU probe that crashes `-race` tests on macOS. HugeGraph specifics: hashed vertex IDs (128-byte cap), size-bounded write batches (Gremlin's 64 KB request default), read-before-write scripts (no index lookups after uncommitted writes), and repository replacement as one transaction bounded by `edge.tx_capacity`/`vertex.tx_capacity` (HugeGraph's maximum is 1,000,000; the store checks `CODESYNAPSE_HUGEGRAPH_TX_CAPACITY` before writing and rolls back on failure). Statistics rules live once in `store.StatsBuilder`; HugeGraph aggregates groups server-side and feeds them through it.
- **An in-memory twin behind the same interface.** The contract test (`storetest.Run`) keeps HugeGraph and the memory store identical, which lets everything except the Gremlin itself be tested without a database.
- **No raw Gremlin over MCP.** Scripts execute code on the server. A curated tool set is safer and cheaper for agents anyway.
- **No LLM calls inside CodeSynapse.** Outputs are deterministic context; the agent using them is the LLM. This keeps keys and code out of third-party APIs.
- **Short names in answers, full IDs for lookups.** Names drop import-path prefixes (`store.(*Store).Get`); the ID is there when the agent needs to be exact.
- **Docker compose first.** `just up` is the main way to run the stack; the binaries also run standalone.
- **Agent-facing API follows `docs/AGENT_GUIDE.md`.** Few, composite tools (`get_symbol_context`, `get_pr_context`, `minimal_context`) beat many small ones: every tool definition costs every agent context on every turn. Superseded tools (`symbol_card`, `callers`, `callees`, `impact`) were removed rather than kept as aliases.
- **Token benchmark reported as "reduced by X%".** Percentages read better than multipliers for operators; the multiplier stays as a secondary figure.

## Agent integration (Agent & Prompt Guide)

`docs/AGENT_GUIDE.md` is the contract between CodeSynapse and assistants. Implementation:

| Guide item | Where |
|---|---|
| System instructions | `Instructions` in `services/mcp/internal/mcpserver/server.go`, sent on MCP initialize |
| `get_symbol_context` (outline/full, risk score, package view) | `agentctx.SymbolContext` |
| `get_call_graph` (direction, depth ≤ 4, entry points) | `agentctx.CallGraph` |
| `get_implementations` | `agentctx.Implementations` |
| `get_pr_context` (diff / files / refs) | `agentctx.PRContext` on top of `review.Analyze` |
| `minimal_context` (`max_tokens`) | `agentctx.MinimalContext`: keyword extraction, ranking, greedy packing |
| `qualified_id` suggestions | `agentctx.Resolve` → `SuggestionError` |
| Prompts `refactor-analysis`, `pr-impact-review` (+ `package-onboarding`, `bug-investigation`) | `services/mcp/internal/mcpserver/prompts.go` |
| One-step setup in Claude Code, Codex, Gemini, OpenCode, Cursor | `codesynapse mcp install` / `just mcp` |

Follow-ups: measure `minimal_context` recall on real tasks (does it surface the files that end up edited?), and add a `security-review` MCP prompt mirroring guide prompt E.

## Multi-language indexing

`internal/ingest/lang` adds Python, JavaScript, TypeScript and Vue with pure-Go structural parsers (no cgo, static binaries):

- **Python:** indentation-based blocks with string/docstring masking; classes, methods, functions, decorators (decorated functions are entry points), `import`/`from` (absolute, relative, src layout), calls through imports, `self.`/`cls.`, module aliases.
- **JS/TS/Vue:** a masking pass blanks strings, comments, regex and template text (keeping `${}` code), then brace depth finds functions, arrow functions, classes and methods; ES imports, `require`, re-exports, dynamic imports, `@/`/`~/` aliases; `<script>` of Vue SFCs with line offsets; `<script setup>` bindings are entry points (used by the template).
- **Call resolution fallbacks:** a method or (JS) function name unique in the repository resolves even without an import (Nuxt auto-imports, `obj.method()` on untyped values); class calls also link to the constructor. Ambiguous names never create edges.
- **Tests:** pytest conventions, `*.test.*`/`*.spec.*`/`__tests__`; module-level test code becomes a synthetic `<tests>` function so test reachability works.
- **Dependencies:** lockfiles feed the same OSV scan; reachability is imported/required (advisories rarely list symbols). npm and PyPI are read from `package-lock.json`, `yarn.lock`, `pnpm-lock.yaml`, `poetry.lock`, `uv.lock`, `Pipfile.lock` and `requirements*.txt`; `internal/vuln/lockfiles.go` adds `Cargo.lock` (crates.io), `composer.lock` (Packagist), `Gemfile.lock` (RubyGems), `packages.lock.json` (NuGet) and `gradle.lockfile` (Maven coordinates), each with the manifest next to it for which dependencies the project declares (Cargo.toml and workspace members, composer.json, the Gemfile DEPENDENCIES section, NuGet `type: Direct`, the Gradle build file). Cargo, Composer, Bundler and NuGet record requirement edges, so they get dependency paths; Gradle lockfiles do not. Source files of these languages are not indexed, so their reachability is always required. Maven `pom.xml` is not read (no resolved graph). Licences come from the lockfile when it records them (Composer) and from deps.dev for Cargo, NuGet and Maven when lookups are on.

Known limits: no type inference for Python/JS, so dynamic dispatch and higher-order calls are under-reported; tree-sitter (roadmap) would give exact parsing and more languages.

## Architecture catalog

New node kinds `Record` (an architecture record kept as a file) and `Component`, and edges `SUPERSEDES` (record to the older record), `APPLIES_TO` (record to component), `IMPLEMENTS_DECISION` (code symbol, file or package to the record) and `RELATES_TO`. A Record node holds `DocType` (adr, rfc, standard, policy, overview, runbook, diagram), `Status` (canonical word of that type's lifecycle, or `unknown`), the file and line range, a content fingerprint (`BodyHash`), a redacted summary and `Meta` (JSON: owner with e-mail addresses removed, date, topic, labels, components, supersession and relation references as record IDs, the code references with the nodes they resolved to, and what could not be resolved). The stable ID is the front matter `id`, else the file name number (`ADR-0007`, `RFC-0004`), else derived from the path; duplicates get a suffix and a note. `internal/arch` reads front matter, `- Status:` bullets and `## Status` sections, resolves references after the code graph is merged (an ambiguous name links nothing and is reported), and never stores more of a record's text than a short summary. `internal/catalog` is the single service behind the API and the MCP tools: `List`, `Get`, `Issues`, `Context`. Only an accepted ADR or an active standard or policy that nothing has superseded is binding. See `docs/ARCHITECTURE_CATALOG_PLAN.md` for the remaining slices.

## Proposals and links

`store.CuratedStore` (`SaveCurated`, `GetCurated`, `ListCurated`, `DeleteCurated`, `DeleteAllCurated`) keeps `curation.Item` documents per repository apart from the code graph, like reviews: a `curated` label in HugeGraph, a map in the memory store, with the `curated` contract test. An item is a **proposal** (`type` adr, rfc, standard or policy; `status` fixed to the draft word of the type; `state` open or withdrawn, `merged` computed when a record with the same title exists; code references and record IDs validated when it is made) or a **link** (`implements`, `relates`, `applies-to`; record must exist; an implements target must resolve to one place, and is resolved again at every read so a rename shows as stale). `curation.PrepareProposal/PrepareLink` validate, redact (`redact.Free`) and size-limit; items carry `OrgID` (organisations sharing a graph never see each other's), `CreatedBy` and `Client`. `catalog.Service` gains `Propose`, `Proposals`, `Proposal`, `Withdraw`, `Export`, `AddLink`, `RemoveLink`, `Links`, and folds links into `Get`, `List` (applies-to components), `Context` (a linked symbol, file or package brings its record, with who linked it, and open proposals that match are listed apart) and `Issues` (`stale-link`). There is no operation that sets a status: the only way to a decision is a committed file. API routes need Member to write; MCP `architecture_propose` and `architecture_link` need Member, are registered only with `--allow-catalog` and a store that implements `CuratedStore`, and in accounts mode write an audit event (`mcp.architecture.propose|link`) before the change through the optional `Auditor` interface. The API deletes a repository's items with the repository.

## History and trends

`store.HistoryStore` (`AddSnapshot`, `Snapshots`, `DeleteSnapshots`) keeps `history.Snapshot` documents per repository, apart from the code graph like reviews: a `snapshot` vertex label in HugeGraph, a map in the memory store; covered by the `history` contract test (oldest first, same time replaces, cap of 500, isolated per repository, untouched by ReplaceRepo and DeleteRepo). `snapshot.Take` reads the stored reports of a repository (security report, cleanup, duplicates, licences, metrics) into a snapshot identified by the ingest time; `snapshot.Record` runs after a successful `ReplaceRepo` in the API job and in `codesynapse ingest` and only logs failures. `history.Build` orders snapshots, builds a series per metric and compares the last two with `Compare`, using the per-metric thresholds in `internal/history`. Served by `GET /api/v1/repos/{repo}/history`, the `repo_trend` MCP tool and the Trends page; the attention list adds a `regression` item per repository that got worse. The API deletes a repository's snapshots when it deletes the repository.

## Merging duplicates

`internal/refactorplan.ForGroup` takes one group of the stored duplicate report (hash and kind) and, from the graph only, returns the suggested shared name (the common or most frequent bare name, public if a copy is), a home (same file, same package, or the closest shared directory), the approach, each copy with its callers (tests excluded) and the tests that reach it, risks (exported copies, several packages, signatures that differ after dropping receivers and names, untested copies, unreachable copies, sensitive tags, edited copies) and ordered steps. Served by `GET /api/v1/repos/{repo}/duplicates/refactor` and `refactor_duplicates`; the cleanup plan's merge steps carry `group` (hash, kind) and a link that opens the plan on the Duplicates page. No source is read and nothing is written.

## Cleanup plan

`internal/cleanupplan` ranks the stored findings: `Assemble` decodes the Repository node's cleanup, duplicates and licence reports, takes vulnerabilities from `security.Build` and the top ten hotspots with whether any test reaches them, and `Build` turns them into steps. Each step has `value` (0 to 100), `effort` (1 to 3) and `safety` (high, medium, low); `priority = value x {1.0, 0.8, 0.6} / {1.0, 1.5, 2.0}`, ties broken by a stable step ID. Dead code is grouped per directory (security-relevant first), unused modules and vulnerable upgrades per module (an indirect one targets the first link of its dependency path; modules removed anyway are not also upgraded), licence conflicts exclude development dependencies, exact duplicates are ranked by redundant lines with effort by scope. Exported and test-only unused code, low-severity uncalled advisories and licence reviews are counted in `notes`, not planned. Served by `GET /api/v1/repos/{repo}/cleanup/plan`, the `cleanup_plan` MCP tool and the Code health Plan page. Nothing edits code.

## Dependency graph and SBOM

`Module -DEPENDS_ON-> Module` edges (from `vuln.Module.Requires` plus the Go package-import edges) record which module requires which, next to the existing `Repository -DEPENDS_ON-> Module`. The new `store.Reader.DependencyGraph(repo)` returns every Module, every Vulnerability and the module-to-module edges, complete up to `MaxDependencies` (20,000; 100,000 edges) and flagged `Truncated` beyond, because `List` stops at 500 and an SBOM must not be silently partial. It is implemented in the memory and HugeGraph (one constant script) stores and covered by the `dependency_graph` contract test. `internal/sbom` turns it into CycloneDX 1.5 (`Build`, `PURL`, `Marshal`); the output is deterministic for an ingest. Served by `GET /api/v1/repos/{repo}/sbom` (repository viewer) and `codesynapse sbom`; the web proxy forwards the one `attachment` Content-Disposition with a plain file name.

## Dependency paths

`vuln.Module` carries `Requires` (module keys it requires, from npm `package-lock.json` v2/v3 and `uv.lock`/`poetry.lock`), and `vuln.GoGraph` reads Go edges from `go list -deps -test -json` (an import crossing from one module into another; local toolchain, `CGO_ENABLED=0`, `GOTOOLCHAIN=local`, `GOPROXY=off` unless the user opts into network, no shell). `vuln.AssignPaths` then sets `Via` (shortest chain from a declared dependency, multi-source breadth-first, ties broken by name) and `Roots` (declared dependencies that reach it; skipped above 5 million roots times edges) on indirect modules, stored as `Node.Via` and `Node.Roots` (`via`, `roots` in `nodeprops` and the HugeGraph schema). `security.Vulnerability` repeats them. No new edge kinds: the chain is a string, not a traversal. Yarn and pnpm lockfiles and lockfile v1 carry no edges yet.

## Duplicate code

`internal/ingest/golang/clone.go` fingerprints each non-test, non-generated function or method of at least 6 lines and 5 statements into `Node.CloneHash`: the type-checked AST with local names and literal values abstracted (locals by order of appearance, literals by kind) and everything that carries meaning kept (callees, types, fields, imported packages, operators, receiver type left out). Copies that differ only in naming or constants share the hash; no source is stored. `internal/duplicates` groups by hash at ingest into `Repository.duplicates` (groups with scope file/package/repository, redundant lines, at most 200 groups and 2,000 fingerprints) and compares reports at request time with `Across`: only the repositories the caller may see are passed in, groups need two repositories, and sets of repositories that keep copying the same things (3 groups or 50 redundant lines) get a "library" recommendation, "framework" when three or more areas are involved. Served by `GET /api/v1/repos/{repo}/duplicates` and `GET /api/v1/shared-code`, the MCP tools `duplicate_code` and `shared_code`, `codesynapse duplicates [path...]` and the Duplicates and Shared code pages. `internal/ingest/lang/clone.go` does the same for Python, JavaScript, TypeScript and Vue from tokens, because those parsers have no scopes: comments, docstrings, strings and numbers are dropped, called functions and member names are kept, every other identifier is a variable numbered by first appearance, and Python indentation levels count as structure. Near-miss clones: every fingerprinted node also carries `clone_sig`, a 32-slot (16-bit) MinHash over 3-token shingles of the same normalised tokens. `duplicates` compares one signature per distinct fingerprint (functions of 10+ lines, size ratio at least 0.5) with 8-band LSH, keeps pairs with estimated similarity of at least 0.7 and clusters by leader (the largest variant) rather than by chaining. Near groups carry `kind: near` and `similarity`; their redundant lines count only the differing variants, so `Summary.RedundantLOC` (exact) and `Summary.NearRedundantLOC` never overlap.

## Unused code (cleanup)

`internal/cleanup` runs at ingest on the complete graph. Roots are `main`/`init`, exported API outside `internal/`/`cmd/`, methods called implicitly by the standard library (`String`, `Error`, `ServeHTTP`, …), package-level initialisers, decorated or registered functions, and Vue setup bindings. Everything reachable over `CALLS`, `DISPATCHES_TO` and `REFERENCES` (functions used as values, a new edge) is live; code reachable only from tests is test-only; the rest is unreachable. Packages only tests use are test support and not counted. The report lists functions with confidence and security relevance, files with nothing live, unused direct Go modules with removable CVEs, unused Helm values and template defines, plus a safe-removal checklist. Every function and module node carries `liveness`.

Running it on CodeSynapse itself found and removed three dead helpers.

## Security coverage

A grade only means something next to what was checked. At ingest, `security.CoverageOf` counts what the rules read (code files per language, YAML files and unparseable ones, Kubernetes resources, workflows, Compose files, Helm templates scanned, dependencies) and `security.Inventory` counts file kinds without rules (Dockerfile, Terraform, shell, Java, ...); the result is stored on the Repository node (`coverage`). Helm templates are scanned by `config.neutralize`: lines holding only template actions become blank and inline actions become a placeholder the rules treat as templated, keeping line numbers; the Kubernetes and secret rules then run on literal values. `security.Build` reports coverage as checked / not checked, the indexed head and time, counts every finding through `FindingCounts` (List stops at `store.MaxLimit`; findings beyond it count at full weight), and grades `N/A` when nothing was scannable.

## Dependency licences

`internal/license` checks every dependency's licence against the repository's own. The project licence comes from licence files at the root (text detection, SPDX headers), then `package.json`, then `pyproject.toml`. Dependency licences come from disk first: `package-lock.json` entries, `node_modules/*/package.json`, `vendor/` and the Go module cache, and installed packages' `METADATA` in `site-packages`. What is still unknown is looked up on deps.dev when `CODESYNAPSE_LICENSE_LOOKUP` / `--license-lookup` is on (ecosystem, name and version only). Licences are classified (public domain, permissive, weak/strong/network copyleft, restricted, unknown; SPDX `OR` takes the least restrictive choice, `AND` the most) and compared with the project's under a distribution model: strong or network copyleft and non-open-source licences conflict with permissive or proprietary projects, weak copyleft and unknown licences need a review, GPL-2.0-only rejects Apache-2.0 and GPL-3.0. Development dependencies turn conflicts into reviews. Every verdict carries its reason and the licence's source. The report is computed at ingest and stored on the Repository node, so it covers every dependency (not just `store.MaxLimit`); it is served by `GET /api/v1/repos/{repo}/licenses`, the **Licences** page, the `license_report` MCP tool and `codesynapse licenses` (`--fail-on conflict|review` for CI). Guidance, not legal advice.

## Agent memory

Agents lose detail when their context is compacted and start each session from nothing. The MCP server (unless started with `--allow-memory=false`) stores durable facts per repository as `(:MemoryEntity)-[:FACT]->(:MemoryEntity)` (`internal/memory`, `store.MemoryStore`), apart from the code graph so re-indexing keeps them. Entities merge by case-folded name; facts by (subject, predicate, object), where saving again bumps `confirmations`; `replace` makes a predicate single-valued. The predicate is a relationship property validated as `UPPER_SNAKE_CASE`, so the edge label stays fixed and agent text only reaches Gremlin as bindings. Names pass `redact`, are capped at 120 characters, and a repository holds at most 10,000 facts and 5,000 entities. Every new fact records its originating runtime session as well as the MCP client that saved it; reads carry a notice that facts are data, not instructions (they may come from agents that read untrusted code). Tools: `upsert_graph_triples`, `search_graph_nodes`, `query_subgraph`, prompt `context-offload`; the **Agent memory** page lists and deletes facts (`GET`/`DELETE /api/v1/repos/{repo}/memory`). Context pruning itself stays with the client.


**Session memory and recall.** Phase 4g separates private chat facts from durable repository knowledge. `offload_turns` and the PreCompact hook queue local extraction through `POST /api/v1/repos/{repo}/session-memory/offload`. Turns are redacted before the bounded in-memory queue and discarded after processing or shutdown. Jobs inherit the registered execution identity and recheck permission before persistence. `recall_context` defaults to the same tenant/session/namespace; `memory_scope: "repository"` explicitly selects durable facts. Session recall combines scoped BM25 and local dense embeddings with reciprocal-rank fusion, then validates a fresh graph snapshot before bounded hydration. Session entities/facts/vectors use separate storage and fixed seven-day expiry. Explicit promotion copies selected facts into repository memory with origin provenance. Legacy durable records remain unchanged, and the old repository offload endpoint returns 410. Embedding refresh runs under the originating session after writes or explicit indexing; there is no anonymous background embedder. Missing embeddings leave lexical recall available. See [session identity, APIs and retention](SESSION_CONTEXT_PLAN.md) and [runtime client setup](AGENT_GUIDE.md).
## Statistics

**Live activity (by the minute and hour).** `internal/telemetry` keeps Prometheus-style counters and histograms in each service and pushes them every 15 seconds to VictoriaMetrics (no scrape listener; VictoriaMetrics stays internal and password protected). Every series carries `tenant_id` from the authenticated caller. Recorded: MCP sessions, tool calls, latency and response tokens; LLM requests, input, output and cached ("saved") tokens, estimated cost from configured prices, request time and time to first token (streaming providers); database round trips by `db_system` (hugegraph, milvus, postgres) and operation; graph rows and vector matches; memory facts saved, turns offloaded, recalls and embeddings; API requests by route pattern; indexing runs. A series' first push adds a 0 sample at its creation time, because VictoriaMetrics' `increase()` otherwise takes a large first value (one call returning 37k tokens) for a counter that predates collection and counts nothing. `GET /api/v1/stats/series?panel=…&range=1h|6h|24h|7d|30d` answers fixed panels at 1-minute to 6-hour steps; percentiles use `increase()` because `rate()` reads 0 for series born inside the window. The Statistics page charts them with an in-house SVG line chart (four validated series colours, never cycled; the rest folds into "other").

`GET /api/v1/stats?days=7|30|90` and the **Statistics** page combine everything measured across the workspace. Agent activity comes from MCP usage counters (`internal/usage`): a receiving middleware counts sessions (`initialize`, or `server/discover` for sessionless clients), tool calls and prompt requests per day, repository, tool and client, with errors, response tokens (bytes/4) and latency, and flushes aggregates every 30 seconds to `:McpUsage` nodes (180 days kept). Tokens avoided are estimated for navigation tools from each repository's ingest-time token benchmark (file-reading baseline ÷ graph answer). Context offloading shows stored facts, saves, recalls and measured tokens per recall, and a turn-by-turn model of one session with and without offloading (client compaction vs. offloading at a target fraction of the window) whose assumptions are editable on the page. The rest aggregates existing reports: security grades, findings and vulnerabilities by severity, the most widespread rules, licence verdicts and repository licences, update automation and Renovate issues, review decisions and fix branches, AI provider usage and sync health.

## Fix guidance

Rules are static, human-readable files: `rules/code/*.yaml` and `rules/config/*.yaml`, one per rule (id, title, severity, CWE, languages, why, fix, references, vulnerable → fixed examples), validated against `rules/schema.json` and embedded with `go:embed`. Detectors only hold the matching logic and look rules up by id; `TestEveryDetectorHasARule` keeps both sides in sync. Changing a severity or wording is a YAML edit. It is served by `GET /api/v1/rules`, the `rule_guide` MCP tool and the **How to fix** panel on each finding.

## Code reviews over MCP

Agents submit structured reviews with `submit_review` (title, summary, verdict, comments anchored to file:line with severity, category, message, suggestion, optional security rule). CodeSynapse validates and redacts them (`internal/reviews`), anchors each comment to the symbol at its line, and stores them as `:Review` nodes apart from repository graphs, so re-indexing keeps them. The **Reviews** page lists reviews and groups open comments by file; people resolve or reopen comments there, agents with `update_review_comment`. The `code-review` prompt walks an agent through review with the graph tools. These two tools are the only MCP writes, scoped to the review store.

Each comment card shows the code around its lines: `GET /api/v1/reviews/{id}/code` reads them on request with `git cat-file` at the reviewed commit (falling back to the working tree, e.g. after a shallow fetch), redacts each line and returns at most 80 lines per comment; nothing is stored. When the suggestion has a fenced code block, the page applies it to `line..end_line` and shows a before/after line diff. Locations link to the lines on GitHub, GitLab or Forgejo at that commit (provider from the host or a configured organisation).

Follow-ups: link comments to the diff hunk they were made on and flag them "outdated" when that code changes; per-review export to Markdown or a GitHub PR review.

## AI providers and background work

`services/api/internal/llm` holds providers (`openai`-compatible HTTP and the Anthropic SDK), a registry with per-task routing (global default plus `review`, `security-triage`, `briefing`; editable through `PUT /api/v1/ai/routing`, persisted in the workspace) and usage counters. `services/api/internal/aitasks` turns tasks into review documents: triage in batches sized from the output budget (6–40 findings; up to 200 per run, partial results kept); all task sizes scale with the provider's input budget, which defaults to `context_window` minus the output budget and a 1,024-token framing reserve. [Final prompt checks](PROMPT_BUDGETS.md) enforce a labelled canonical-JSON byte estimate before every completion. Tasks include review of a diff (comments only on files in the diff), and briefings (`kind: report`). After a successful sync, `Server.automation` compares the old and new head and finding IDs and, if the schedule asks for it, runs one background job that triages new medium+ findings and reviews `old..new`. Comment decisions are open, accepted, rejected, deferred and resolved.

Decisions: model output is untrusted (redacted, size-limited, validated against known files and enums); remote providers get no source beyond a requested diff; budgets are per request and small by default because local hardware is slow; jobs share one semaphore with ingest so a laptop is never running two heavy things at once.

Fix branches (`services/api/internal/fixes`, opt-in with `CODESYNAPSE_ALLOW_PUSH`): for accepted comments the "fix" model rewrites each affected file whole (it must fit the output budget) in a temporary worktree at the indexed head; CodeSynapse enforces [fix validation policy 1](FIX_VALIDATION.md), accepts Go function-body edits from a local model against a pinned current review, commits only the validated files, pushes a new `codesynapse/fix-<review>-<rand>` branch and opens a draft PR through the GitHub API (falling back to a normal PR, then to a compare link). Comment fix state (running, pushed, failed, branch, PR) is stored on the comment; the comment stays accepted until a person marks it fixed. Follow-up: detect merged fix branches on sync and mark comments fixed automatically; GitLab merge requests.

Next: Postgres for reviews, decisions with an audit trail, a persistent job queue, finding/score history and AI usage; an in-process TTL cache for overview/security/metrics invalidated on ingest; container security (Dockerfile rules and opt-in Trivy image scans).

## Repository policy and generated annotations

`internal/config` loads optional `.codesynapse.yaml` once per ingest and shares its immutable matcher with extractors, inventory, dependency discovery and annotation. `Repository.meta.repository_policy` records the canonical configuration, digest, diagnostics and exclusion coverage. A changed policy needs a new approval. Global and Kubernetes exclusions require an active matching approval from every organisation linked to a shared graph; offline CLI commands require `--approved-policy-digest`.

PostgreSQL migration `0008_repository_policy.sql` adds organisation/repository approval and annotation tables. Approvals retain the actor, reason, expiry and separate exclusion, suppression and annotation permissions. Annotation records carry symbol fingerprints, policy/commit/revision, originating session and job ID. These tables cascade on unlink; annotations do not add graph labels or vector entries. Current reads validate policy and symbol fingerprints, allowing reuse across ingests only when the indexed symbol metadata is unchanged.

`services/api/internal/annotation` runs a bounded queue through the local `code-annotation` LLM route. Inputs contain indexed names, signatures and summaries; outputs pass schema validation and redaction. Runtime identity, the original credential, access, policy and revision are checked again before persistence. Graph publication, repository linking and annotation publication share the accounts policy transaction. This holds the governance lock during graph writes; HugeGraph publication currently assumes one server instance. See the [configuration reference](REPO_CONFIG_PLAN.md) for limits and API routes.

Approved suppressions affect the calling organisation's detailed security report. Raw findings remain stored, and aggregate overview/history grades remain conservative. Phase 4i adds exact per-run file accounting and savings dashboards alongside this policy coverage.

## Ingestion accounting

`internal/ingestreport` records one bounded outcome per encountered file, separating whole-file skips from extraction-stage exclusions. A file is indexed only after its contribution is confirmed in a committed graph snapshot. Unsupported formats, failed stages and incomplete discovery remain visible. Metadata-only context estimates carry their baseline and partial coverage; they are not provider token savings.

Accounts migration `0009_ingest_reports.sql` stores tenant-owned run checkpoints, reports, permanent replay guards, stable repository metric IDs and cumulative totals. The shared `Repository.meta.ingest_receipt` contains only a hashed commit marker. API publication reconciles old receipts under the existing accounts policy transaction before replacing a graph. A process crash after graph commit can therefore finalize its prepared report without replaying the write or reviving a revoked actor.

Report reads require repository READ and omit execution identity. The API exposes bounded history and paginated file decisions; `ingestion_report` supplies bounded MCP summaries. The dashboard keeps the last successful report separate from the latest attempt. Absolute cumulative file metrics are exported by one fenced owner with stable resource identity and sample timestamps, using the existing OTLP or Prometheus receiver. Metric admission is capped independently from exact accounting; the UI reports gaps and export lag.

See the [ingestion reporting reference](INGEST_TRANSPARENCY_PLAN.md) for discovery scope, persistence limits, restart behavior and validation. Annotation outcomes remain separate from ingestion counts.

## Repository lifecycle

Repositories are added by URL (cloned into the workspace volume) or mounted under `./sources`. **Auto-sync** schedules (off, 5/15/30 min, 1 h, 6 h, daily; default 30 min, counted from the end of the previous sync; pausable) are stored in `workspace/.codesynapse/schedules.json` and run by a scheduler in the API; the next run and last result show in the UI. Removing a repository deletes its graph, schedule and optionally its clone. Private repositories use a server-side token set with `just git-token`; `GET /api/v1/settings` reports the hosts with a token and a masked preview (first and last five characters; tokens under 20 characters show only `***`), never the full value.

## Operator overview

`GET /api/v1/overview` and the **Overview** page aggregate every indexed repository: totals by node and edge kind, LOC, grade distribution, findings and CVEs by severity, called CVEs, vulnerable modules, functions per sensitive API, index freshness (stale after 7 days), the workspace-wide context reduction, and ingest job health (success/failure, durations, recent errors). Repositories are listed worst security score first.

## Status

Done:
- Go extraction with tests, interfaces, security tags, complexity, doc summaries and ~20 type-aware security rules
- Markdown docs, YAML (Kubernetes, compose, GitHub Actions, Helm values) and Go templates/Helm charts, with YAML security rules
- git history; dependencies from go.mod with OSV vulnerabilities and called/imported/required reachability (stdlib checked at the building Go version)
- HugeGraph and memory stores with shared contract tests (verified against HugeGraph on throwaway containers)
- Query layer, impact/risk analysis (including findings), security report with grading, metrics and token benchmark (reduction %)
- Agent guide implemented: composite tools, `qualified_id` suggestions, four MCP prompts, server instructions
- Python, JavaScript, TypeScript and Vue indexing with security rules; npm and PyPI dependency CVEs
- Unused-code analysis (liveness, unused modules/config) and per-rule fix guides with examples
- Rule catalogue as static YAML files in `rules/` (45 rules), embedded and schema-validated
- Dependency update automation: `dependency-updates-missing` when a repository has manifests (go.mod, package.json, Python, Dockerfiles, compose, workflows, charts) but no Renovate or Dependabot config; the Renovate config (JSON/JSON5, read without following symlinks) is checked by `renovate-config-invalid`, `renovate-disabled`, `renovate-vulnerability-alerts-off`, `renovate-automerge-no-release-age` and `renovate-no-digest-pinning`. These findings hang off the Repository node, anchored to the top-level manifest or the config line; absence checks are skipped when the config extends a preset from outside Renovate's built-ins
- Code reviews over MCP with a Reviews page; auto-sync schedules; repository removal; private-repo tokens
- UX pass: repository summary with health verdicts and next steps, plain-language labels and inline explanations, Kubernetes & Helm page, content detection for sources
- MCP server (18 read-only tools + 2 review write tools, 5 prompts, stdio + HTTP with bearer auth) and `codesynapse mcp install` for local agents
- API with dashboard, operator overview, graph explorer endpoints, ingest/clone/sync jobs
- Nuxt 4 dashboard in the Astrona style: overview, repositories, dashboard, security (clickable rules, fix examples), cleanup, graph, explorer, metrics, docs, packages, review
- docker compose (Docker or Podman), justfile, golangci-lint clean, race tests, fuzzers
- AI providers (MLX/Ollama/vLLM/OpenAI-compatible, Anthropic) with global + per-area routing, AI triage/review/briefing jobs, background review and triage after sync, accept/reject/defer/fixed decisions

## Roadmap

Ordered by value.

1. **Incremental ingest.** Measured on this repository (7,600 nodes, 27,000 edges, 9.2 MB): extraction takes 3.4 s, writing the graph to HugeGraph 9 to 22 s, and a republish cost as much as the first one. So publication goes first.
   - **Incremental publication (done):** every node vertex carries `content_hash` and `edges_hash`. `ReplaceRepo` reads them under the repository's stage lease and writes only new and changed nodes (in place, so edges into them survive), the outgoing edges that changed, and removals, in one transaction (`internal/store/hugegraphstore/delta.go`). It falls back to the full write when nothing is stored, when more than half the nodes changed, or when the stored state is not what was read. A real one-file edit publishes 3 nodes and 5 edges in 2.7 s instead of a full rewrite. The store contract checks successive publications against the in-memory reference. Two sources of run-to-run noise were fixed with it: duplicate module versions now resolve deterministically (direct first, then the highest version), and Helm template summaries are sorted.
   - **Next:** cut the remaining fixed cost (reading every hash and a count per publish); a `watch` mode and a git post-commit hook for the CLI. Re-extracting only changed packages saves little while the whole module type-checks in about 3.5 s, so it waits until a repository makes it matter.
2. **More ecosystems for CVEs:** container base images, Maven, Cargo, Composer. Grype could back this as a second scanner.
3. **Compact IDs** relative to the module path, to cut repeated prefixes from answers.
4. **tree-sitter parsers** for exact Python/JS/TS syntax and more languages (Java, Rust, C#, PHP), via a WASM runtime to stay cgo-free; type inference for TypeScript (tsc) and Python (pyright) as optional enrichers.
5. **Unused npm/PyPI dependencies**, with tooling-aware exceptions (bundler plugins, CLIs, type packages).
6. **Semantic search** with Milvus vectors over summaries and signatures (would also improve `minimal_context`), local embeddings by default.
7. **GitHub Action** running `codesynapse audit --fail-on high` and `review --offline`, posting the result as a PR comment.
8. **Scheduled sync** of registered repositories and webhooks from the forge.
9. **Read-only HugeGraph role** for the MCP and API services, separate from the ingest writer.
11. **One-command install** (done): `codesynapse install` (namespace, random-credential Secret made once, the chart at the CLI's version, `helm test`), `codesynapse status` (release and every component's ready replicas; exit 1 when not ready; `--wait`, `--test`) and `codesynapse uninstall` (data kept unless `--keep-data=false`), driving the local `helm` and `kubectl` like `cilium install`, with a `--local` preset for laptop clusters.
10. **Repository navigation that fits the repository** (done, except per-person pins). The repository menu used to be a fixed list of about twenty links, so a small Python service showed Kubernetes & Helm, Packages, Architecture and Licences pages that could only be empty.
    - **What the repository has:** `GET /api/v1/repos/{repo}/sections` (`services/api/internal/api/sections.go`) counts packages, symbols, Markdown documents, YAML and Helm files, architecture records and proposals, dependencies, the licence report, findings, history snapshots, reviews and agent-memory facts, and says whether the caller administers the repository. Counts only, from the stored graph and reports, scoped like every repository route; reviews and proposals count only what the caller's organisation sees.
    - **The menu** (`services/web/app/layouts/default.vue`): groups Overview, Quality and security, Dependencies, Work, Explore and (with accounts) Settings. Each link with a rule moves into a collapsed "More" group with its reason ("no Markdown documents indexed") when its count is zero, so a deep link or a later sync never strands anyone; "More" opens when the page shown is in it. Summary, Security, Code health, Symbols, Graph, Impact of a change, AI assistant and Ingestion reports always stay. Until the counts arrive, or if they fail, every link shows. The counts refresh when the repository is re-indexed.
    - **Still open:** pinning or hiding links per person and repository, which needs a per-user preference store.

## Security roadmap

What would put security more in front of developers, ordered by value for the effort:

1. **New vs. fixed findings per change** (done for `get_pr_context` through the bridge and `review --offline`; the PR comment follows with the GitHub Action, roadmap item 7). Fingerprint findings (rule + owner + normalised location) and compare against the base branch, so `get_pr_context`, `review` and the PR comment say "this change introduces 1 high, fixes 2" instead of listing the whole backlog. This is what makes developers act.
2. **Triage and suppressions with an audit trail.** `// codesynapse:ignore <rule> <reason>` in code and YAML, plus UI triage states (accepted risk, false positive, fixed-by) stored in the graph with who/when/why. Without this, noise erodes trust.
3. **Taint tracking.** Follow request input through assignments, struct fields and calls to SQL/exec/filesystem/HTTP sinks; today's request-input rules only see direct use.
4. **Attack-surface map.** Index HTTP routes (`mux.HandleFunc`, Echo/Gin/chi registrations), CLI commands and MCP tools as `Route` nodes linked to handlers; answer "which routes reach `exec`/SQL/filesystem" and "which routes have no auth middleware".
5. **Security trend over time.** Snapshot grade and counts per ingest and per branch; chart them on the overview and alert when a sync regresses.
6. **Secrets in git history.** Scan commit diffs (redacted, values never stored) for credentials that were removed from HEAD but still need rotation.
7. **SARIF and SBOM/VEX export.** SARIF for GitHub code scanning and IDEs; CycloneDX SBOM from `Module` nodes with VEX "not affected / not called" statements derived from reachability.
8. **Dependency hygiene.** Outdated and retracted module versions, deprecated modules, OpenSSF Scorecard and licence data (deps.dev), surfaced next to CVEs.
9. **Dockerfile and Terraform rules.** Root `USER`, `ADD` from URLs, unpinned base images, secrets in `ENV`/`ARG`; open security groups, public buckets, unencrypted storage.
10. **Ownership.** Parse `CODEOWNERS` and route findings to owning teams on the overview.
11. **Agent workflow.** A `security-review` MCP prompt (guide prompt E) and a `security_context` view in `get_symbol_context` showing sensitive calls, reaching routes and findings before an agent edits a symbol.
12. **Pre-commit gate.** `codesynapse audit --changed --fail-on high` limited to staged files, fast enough for a hook.

## Agent workflow roadmap

Proposed 2026-10-23. The analysis is strong; the gaps are in how it fits an agent's edit loop. The tool surface has grown to about 45 tools, 10 prompts and 11 instruction rules, against the "few composite tools" decision above. The graph goes stale as soon as the agent edits a file. Nothing yet shows that the tools help on real tasks: [the phase 4f measurements](RETRIEVAL_RESULTS.md) report a −45% reduction on a tiny fixture. And answers stop at context, while agents also need to know what to run and whether they made things worse.

Ordered so that each step can be measured by the first one. New tools stay read-only; none of this changes the write-tool rules in `AGENTS.md`.

1. **Task eval from git history** (done: `codesynapse eval`, `just eval` and a CI gate; method and first numbers in [docs/benchmarks/task-eval](benchmarks/task-eval/README.md): over 58 tasks `minimal_context` reaches recall@10 0.189 and hit@10 0.466, keyword search 0.171 and 0.517). Each commit becomes a task (its message) with a ground truth (the files it touched). Score `minimal_context` and `search` on recall@5/@10 and returned tokens, on this repository and a few others, and fail CI on regressions. No model needed. This settles the open follow-up "does `minimal_context` surface the files that end up edited?" and turns later steps into measurements instead of opinions.
2. **Tool profiles** (done: nine toolsets with `core` always served, chosen per connection with `/mcp?toolsets=`, `--toolsets` / `CODESYNAPSE_MCP_TOOLSETS` on the server, and `codesynapse mcp install --profile` (default `core`); the server default stays `all` so existing setups keep working, and the instructions and prompts shrink with the selection; see [the agent guide](AGENT_GUIDE.md#toolsets)). About eight core tools by default (`minimal_context`, `get_symbol_context`, `get_call_graph`, `get_implementations`, `file_outline`, `search`, `get_pr_context`, `repo_status`); the rest in toolsets (`security`, `architecture`, `memory`, `cleanup`, `reviews`) chosen with a server flag or `codesynapse mcp install --profile`. The server instructions shrink with the toolset. Every agent turn saves the definitions it never uses.
3. **Incremental ingest** (roadmap item 1; incremental publication done: a one-file change writes a few nodes instead of the whole graph). Prerequisite for the overlay below.
4. **Working-tree overlay** (done, as local answers: while the checkout has unsynced changes, `codesynapse mcp bridge` answers the core repository tools (now eight, with `verify_plan`) from a graph of the working tree built on the machine, marked `source: worktree`; other tools and clean trees go to the server; the core tools' answers moved to `internal/coretools` so both share them; no new server write path). The stdio bridge indexes dirty files locally and lays them over the published graph; answers mark them `source: worktree`. `review --offline` already indexes the working tree, so this reuses that path. The graph stays right in the middle of a task, which is when agents rely on it most.
5. **Leaner answers** (mostly done: core-tool answers drop the repository from node IDs and the resolver takes `Kind|local` back; Go signatures use package names instead of import paths; `minimal_context` states the revision once; budget metadata only when something was omitted. Search answers are 6% smaller with the same results; `minimal_context` fits more items in the same budget, hit@10 0.466 → 0.500. Still open: a `terse` mode, and module-relative IDs). Compact IDs (roadmap item 3), a `terse` answer mode, and budget metadata only when something was omitted. Step 1 shows whether it worked.
6. **New vs. fixed findings per change** (done: `security_delta` in `get_pr_context` through the bridge and in `codesynapse review --offline`, which prints "this change introduces 1 high; fixes 2 medium"; findings are matched by rule, owning symbol and file, and counted) (security roadmap item 1) in `get_pr_context`, so an agent can check "no new high finding" before it finishes.
7. **`verify_plan`** (done: a core tool, also answered by the bridge from the working tree; Go, Python and JavaScript/TypeScript tests, build and lint per language). From a diff, the exact commands to run: `go test ./pkg -run 'TestA|TestB'`, `pytest path::test`, plus lint and build per language touched, ranked by how directly each test reaches the change. Agents lose many turns running too much or the wrong thing.
8. **Change checklist** (done: `change_checklist`, a core tool also answered by the bridge; interface implementations and mocks, callers through the interface, references, importers, templates, docs and decisions, with notes on what the graph cannot see). For a rename or signature change: every call site, implementation, mock, doc mention and config reference, each with file:line and edge evidence. The agent works through a list instead of grepping. Built on the existing call graph and `MENTIONS` edges.
9. **Claude Code plugin** (done: `plugins/codesynapse` with the bridge, a workflow skill and SessionStart, PreCompact, Read and Stop hooks, installable from this repository's marketplace; an edit needs no hook, since the bridge sees the working tree change). Package `codesynapse mcp install` as a plugin with a skill and hooks: a `Read` of a large file suggests `file_outline` and the ranges to read, an edit refreshes the overlay, and `Stop` runs `get_pr_context` and shows what it flags. The tool order then lives in a skill rather than only in prose.
10. **Subagent briefing** (done: `handoff_context`, a core tool also answered by the bridge; starting points, rules in force, covering tests, findings and open issues from memory in one paste-ready brief under a token budget). `handoff_context(task, scope)` returns a small bundle a parent agent pastes into a subagent's prompt: relevant symbols, rules in force, tests and open issues. Parent and child session IDs already exist. Subagents start with almost no context, which is where the graph saves the most.
11. **Runtime facts** (done: `internal/runtimefacts` reads Go coverage profiles, LCOV and JUnit XML, maps blocks to the functions that contain them and test cases to test nodes, flags flaky tests (passed and failed in one run), and keeps the facts apart from the code graph, pinned to the commit measured; `get_symbol_context`, `get_pr_context` and `verify_plan` show them. Locally the bridge and `review --offline` read reports in the checkout; from CI, `codesynapse runtime upload` sends them to `PUT /api/v1/repos/{repo}/runtime`, stored per organisation namespace in HugeGraph as chunked, gzipped documents (`runtime_head`/`runtime_chunk`). Flakiness across runs, rather than within one, is still open: it needs a history of uploads). Import coverage profiles (`go test -coverprofile`, LCOV) and CI test results (JUnit XML) as test-reach facts, so "tested" means measured rather than statically reachable, and flag flaky tests so agents don't chase them.
12. **Attack-surface map** (security roadmap item 4). Also answers the everyday question "which endpoint ends up here?".
13. **Retrieval usefulness.** Through the plugin hooks, count whether the agent read the ranges a tool returned. Aggregates only, under the telemetry rules: no paths, content or session IDs as labels. Shows which answers are wasted tokens and gives ranking work evidence.
14. **More languages** (roadmap item 4). Last, because steps 1–13 raise the value of the languages already indexed first.

More ideas, not yet ordered:

- **Bridge transport** (decided 2026-10-24: no gRPC). The bridge carries small JSON answers whose time is spent in the graph query, Go's client already reuses HTTP/2 connections, Streamable HTTP is the MCP standard other clients need, and gRPC would add a second listener with its own auth and audit path and a new dependency. If it ever matters: measure transport against server time in the bridge first, then response compression or mutual TLS for the bridge connection.

- **Cross-repository impact.** When `get_pr_context` sees a changed exported symbol in a library, list its callers in the other repositories the caller may see, by matching `External` nodes to the library's module path. Breaking changes show up before release, not after.
- **Dependency upgrade impact.** For `module@version`, list our call sites into that module; when both versions are in the local module cache, also the exported API that changed between them. No network, in line with the `go list` defaults.
- **Project conventions.** Derive the house style from the graph (test helper packages, assertion style, error wrapping, logger, where tests live) and return it with `minimal_context`, so generated code matches the repository without the agent reading examples.
- **Agent session timeline.** A dashboard page built from the MCP audit log: which tools an agent session called, in order, and what it got back in size. Helps a person reviewing an agent's PR see what it looked at, under the existing tenant scoping.
- **Context vs. edit comparison.** After an agent's change, compare the files it edited with the files the tools returned. Misses become new eval cases for step 1.

## Organisations

`internal/forge` lists the repositories of an organisation or user on GitHub (also Enterprise Server), Forgejo/Gitea/Codeberg or GitLab; one client per host, API address derived from the host, token sent to that host only, no redirects. The API keeps watched organisations in `<workspace>/.codesynapse/orgs.json` (`services/api/internal/api/orgs.go`), each with a provider and host (entries saved earlier default to GitHub). An organisation's key is `login` on github.com and `host/login` elsewhere, and repositories are named the same way (`owner/name` / `host/owner/name`), so the same organisation and repository on two providers never collide; clones live in `<workspace>/<name>`. Hosts other than github.com pass the SSRF check of `gitx.ValidateRemote` and ports are not supported. Tokens are per host (`CODESYNAPSE_GIT_TOKENS=host=token,...`). Each organisation is rescanned every 5 minutes and, with `auto_index` on, clone + index jobs are queued for repositories that are not indexed yet (archived repositories and forks only when asked for). Cloned repositories then follow the default 30-minute auto-sync. Endpoints: `GET/POST /api/v1/orgs` (body: `provider`, `host`, `org`), `PUT/DELETE /api/v1/orgs/{key}`, `POST /api/v1/orgs/{key}/scan`, `POST /api/v1/orgs/{key}/repos/{name}/index` (`{key}` URL-encoded). Stopping to watch an organisation keeps its indexed repositories. The dashboard page is `/orgs`. That watch list is installation-wide, so with accounts it is unavailable; instead `POST /api/v1/imports/list` (`services/api/internal/api/imports.go`, Member in the selected organisation, signed-in session only) lists an organisation's public repositories anonymously, marks the ones the organisation already has, and returns clone URLs built from the validated host, login and name. The dashboard page `/import` adds the picked ones one at a time through `POST /api/v1/repos`, so every addition gets the usual access checks, linking and initial admin grant. Private repositories are added by URL; listing never opens an organisation's Git token, and nothing is watched or rescanned.

## Tenant controls, telemetry and retrieval roadmap

The SaaS roadmap includes completed phases [platform isolation and repository permissions](ACCESS_CONTROL_PLAN.md) (4d), [OTel telemetry](TELEMETRY_PLAN.md) (4e), [graph/vector context reduction](RETRIEVAL_PLAN.md) (4f), and [immutable session context](SESSION_CONTEXT_PLAN.md) (4g). Phase 4g requires accounts and runtime-provided identity for execution; existing clients need the documented header/startup integration, and signed-in browsers get a dashboard binding derived from their login session. [Repository configuration and annotation](REPO_CONFIG_PLAN.md) (4h) adds approved ingestion boundaries, tenant-local suppressions and local metadata annotation. [Ingestion transparency](INGEST_TRANSPARENCY_PLAN.md) (4i) adds durable tenant reports, commit reconciliation and fenced cumulative file metrics. [The code architecture audit](CODE_ARCHITECTURE_AUDIT.md) (4j) is complete: tests and benchmarks identify edit-validation, prompt-budget and allocation follow-ups, with conditional package prototypes in separate proposed PRs. Phase 4j.1 implements diff parsing and generated-edit validation. [Final-prompt input enforcement](PROMPT_BUDGETS.md) (4j.2) checks redacted provider envelopes, labels estimates and preserves batch tails; [measured allocation reductions](ALLOCATION_REDUCTIONS.md) (4j.3) index in-memory search and cut parser regexp work. [Edge evidence](EDGE_EVIDENCE.md) (4j.4) labels resolution edges typed, syntactic or inferred and records partial, truncated and omitted analysis; the optional SSA/VTA data-flow mode (4j.4b) links Go calls through function values. [Bootstrap-root separation](ROOT_BOOTSTRAP_PLAN.md) limits the setup account to governance; SSO follows the application roadmap. Development and production Kubernetes work remains deferred until these application phases are complete. See [the phase order](SAAS.md#phases).

Development and production Kubernetes manifests are deferred to the final deployment phase, after the application phases and SSO. Prepared deployment work remains on `feat/k8s-production` for later review and revalidation; it is not part of the active development phase.
