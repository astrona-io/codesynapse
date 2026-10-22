# Plan and architecture

## Goal

Make coding assistants cheaper and better on real repositories. An agent that greps and reads whole files spends most of its context on code it doesn't need. A graph that already knows every symbol's location, signature, summary, callers, tests and docs can answer most navigation and review questions in a fraction of the tokens, and with better precision than text search.

Second goal: give humans the same picture in a dashboard, so code quality work (hotspots, untested public API, risky changes) is visible.

Inspired by [code-review-graph](https://github.com/tirth8205/code-review-graph). What we do differently:

| | code-review-graph | CodeSynapse |
|---|---|---|
| Call resolution | tree-sitter AST, heuristic + "inferred" edges | go/types: exact static calls, interface dispatch via `DISPATCHES_TO` |
| Store | single SQLite file | Neo4j, multi-repo, shared by services |
| HTTP auth | Host/Origin checks | bearer tokens, refuses unauthenticated non-loopback binds |
| Write tools | `apply_refactor` writes files | none; all tools read-only |
| Secrets | names sanitised | summaries redacted, no source bodies, hashed author e-mails |
| Security review | – | graded review: type-aware code rules, YAML rules, CVEs with reachability, exposure and test context |
| Languages | 40+ | Go (type-checked), Python, JS/TS/Vue, Markdown, YAML, Go templates/Helm |
| Dependencies | – | Go, npm and PyPI with CVEs; symbol-level reachability for Go |
| Dead code | – | reachability-based liveness, unused modules/config, security-first ordering |
| Fix guidance | – | per-rule guide with vulnerable → fixed examples per language |

## Architecture

```
             ┌──────────────────┐  MCP (stdio/http)  ┌───────────┐
 agents ───▶ │ codesynapse-mcp  │ ─────────────────▶ │           │
             └──────────────────┘                    │  Neo4j 5  │
 browser ──▶ web (Nuxt 4) ─▶ /api/cg ─▶ codesynapse-api ─▶ │           │
                                                     └───────────┘
 CI / dev ─▶ codesynapse CLI (ingest, audit, stats, review --offline) ─▲
```

Every reader goes through `internal/query` (and `internal/agentctx` for the composite agent tools), so MCP, API and CLI return the same answers. `internal/store/memory` implements the same `store.Store` interface for tests and `--offline` mode.

### Ingest pipeline

1. **Go** (`internal/ingest/golang`): `go/packages` loads all packages with tests. Pass 1 handles primary packages; pass 2 adds `_test.go` files from test variants. For each declaration it records the location, signature, doc summary, cyclomatic complexity, a body hash, and calls resolved through `types.Info.Uses`. Calls to sensitive external APIs become `External` nodes with security tags. Interface satisfaction is computed with `types.Implements` across in-repo types.
2. **Docs** (`internal/ingest/docs`): Markdown files are split into heading sections with line ranges and summaries. Inline code spans that name an exported symbol become `MENTIONS` edges (ambiguous short names are skipped).
3. **History** (`internal/ingest/githistory`): the last N commits with numstat give `Commit`/`Author` nodes, `MODIFIES` edges and per-file churn.
4. **Persist**: batched `UNWIND … MERGE`, tagged with a run ID, then stale nodes and edges for the repo are deleted in bounded batches.

### Schema

Every node has the label `Node` plus its kind, and the properties `id`, `repo`, `kind` and `name`. IDs are `repo|Kind|local`.

| Node | Key properties |
|---|---|
| Repository | head, branch, url (origin, no credentials), root, time (ingested at), metrics (JSON), license, license_source, licenses (JSON report) |
| Package | qualified_name, summary |
| File | file, loc, churn, is_test |
| Function / Method | qualified_name, file, start_line, end_line, signature, summary, complexity, loc, body_hash, exported, is_test, security_tags |
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

Edges: `CONTAINS`, `DEFINES`, `HAS_METHOD`, `CALLS` (with `count`), `DISPATCHES_TO`, `IMPLEMENTS`, `IMPORTS`, `MENTIONS`, `MODIFIES` (with `count` = lines changed), `AUTHORED`, `HAS_FINDING`, `DEPENDS_ON`, `AFFECTS`, `INCLUDES`, `USES_VALUE`.

### Risk scoring

`review.Score` is a transparent, additive 0-100 score: direct callers (×3, max 25), transitive impact (max 20), exported (+10), complexity (+5/10/15), security tags (+15 high-risk / +8 other, max 30), and +15 when no test reaches the symbol. Levels: low < 20 ≤ medium < 40 ≤ high < 60 ≤ critical. Every point added comes with a reason string, so reviewers and agents can see why something scored high.

## Decisions

- **Pluggable graph backend** (`CODESYNAPSE_GRAPH_BACKEND`): Neo4j (default) or Apache HugeGraph with Milvus for memory vectors, plus an in-memory twin. HugeGraph and Milvus are Apache-2.0 (Neo4j Community is GPL-3.0), scale out (HugeGraph on distributed storage, Milvus with tenant partitions), and keep tenant isolation in the data itself. Both are spoken over HTTP with the standard library: the Milvus Go SDK pulls in ~470 packages and a cgo CPU probe that crashes `-race` tests on macOS. HugeGraph specifics: hashed vertex IDs (128-byte cap), size-bounded write batches (Gremlin's 64 KB request default), read-before-write scripts (no index lookups after uncommitted writes). Statistics rules live once in `store.StatsBuilder`; HugeGraph aggregates groups server-side and feeds them through it.
- **Neo4j as the default backend**, with an in-memory twin behind the same interface. The contract test (`storetest.Run`) keeps them identical, which lets everything except the Cypher itself be tested without a database.
- **No raw Cypher over MCP.** Read transactions still permit `LOAD CSV` and procedure calls. A curated tool set is safer and cheaper for agents anyway.
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
- **Dependencies:** npm and PyPI lockfiles feed the same OSV scan; reachability is imported/required (advisories rarely list symbols).

Known limits: no type inference for Python/JS, so dynamic dispatch and higher-order calls are under-reported; tree-sitter (roadmap) would give exact parsing and more languages.

## Unused code (cleanup)

`internal/cleanup` runs at ingest on the complete graph. Roots are `main`/`init`, exported API outside `internal/`/`cmd/`, methods called implicitly by the standard library (`String`, `Error`, `ServeHTTP`, …), package-level initialisers, decorated or registered functions, and Vue setup bindings. Everything reachable over `CALLS`, `DISPATCHES_TO` and `REFERENCES` (functions used as values, a new edge) is live; code reachable only from tests is test-only; the rest is unreachable. Packages only tests use are test support and not counted. The report lists functions with confidence and security relevance, files with nothing live, unused direct Go modules with removable CVEs, unused Helm values and template defines, plus a safe-removal checklist. Every function and module node carries `liveness`.

Running it on CodeSynapse itself found and removed three dead helpers.

## Security coverage

A grade only means something next to what was checked. At ingest, `security.CoverageOf` counts what the rules read (code files per language, YAML files and unparseable ones, Kubernetes resources, workflows, Compose files, Helm templates scanned, dependencies) and `security.Inventory` counts file kinds without rules (Dockerfile, Terraform, shell, Java, ...); the result is stored on the Repository node (`coverage`). Helm templates are scanned by `config.neutralize`: lines holding only template actions become blank and inline actions become a placeholder the rules treat as templated, keeping line numbers; the Kubernetes and secret rules then run on literal values. `security.Build` reports coverage as checked / not checked, the indexed head and time, counts every finding through `FindingCounts` (List stops at `store.MaxLimit`; findings beyond it count at full weight), and grades `N/A` when nothing was scannable.

## Dependency licences

`internal/license` checks every dependency's licence against the repository's own. The project licence comes from licence files at the root (text detection, SPDX headers), then `package.json`, then `pyproject.toml`. Dependency licences come from disk first: `package-lock.json` entries, `node_modules/*/package.json`, `vendor/` and the Go module cache, and installed packages' `METADATA` in `site-packages`. What is still unknown is looked up on deps.dev when `CODESYNAPSE_LICENSE_LOOKUP` / `--license-lookup` is on (ecosystem, name and version only). Licences are classified (public domain, permissive, weak/strong/network copyleft, restricted, unknown; SPDX `OR` takes the least restrictive choice, `AND` the most) and compared with the project's under a distribution model: strong or network copyleft and non-open-source licences conflict with permissive or proprietary projects, weak copyleft and unknown licences need a review, GPL-2.0-only rejects Apache-2.0 and GPL-3.0. Development dependencies turn conflicts into reviews. Every verdict carries its reason and the licence's source. The report is computed at ingest and stored on the Repository node, so it covers every dependency (not just `store.MaxLimit`); it is served by `GET /api/v1/repos/{repo}/licenses`, the **Licences** page, the `license_report` MCP tool and `codesynapse licenses` (`--fail-on conflict|review` for CI). Guidance, not legal advice.

## Agent memory

Agents lose detail when their context is compacted and start each session from nothing. The MCP server (unless started with `--allow-memory=false`) stores durable facts per repository as `(:MemoryEntity)-[:FACT]->(:MemoryEntity)` (`internal/memory`, `store.MemoryStore`), apart from the code graph so re-indexing keeps them. Entities merge by case-folded name; facts by (subject, predicate, object), where saving again bumps `confirmations`; `replace` makes a predicate single-valued. The predicate is a relationship property validated as `UPPER_SNAKE_CASE`, so the relationship type stays fixed and agent text only reaches Cypher as parameters. Names pass `redact`, are capped at 120 characters, and a repository holds at most 10,000 facts and 5,000 entities. Every fact records the MCP client that saved it; reads carry a notice that facts are data, not instructions (they may come from agents that read untrusted code). Tools: `upsert_graph_triples`, `search_graph_nodes`, `query_subgraph`, prompt `context-offload`; the **Agent memory** page lists and deletes facts (`GET`/`DELETE /api/v1/repos/{repo}/memory`). Context pruning itself stays with the client.


**Hybrid memory.** Short-term context stays with the client: the prompt and instructions tell agents to keep the last 5–10 exchanges verbatim. Older turns go to long-term memory either as triples the agent writes, or through `offload_turns` / the PreCompact hook to `POST /api/v1/repos/{repo}/memory/offload`, which queues a job (`services/api/internal/offload`, non-blocking, bounded queue, one worker, last 200 jobs at `GET /api/v1/memory/jobs`). The job builds a prompt with the known entity names (`internal/memory/extract`), runs it on the provider routed to the "memory" task only if it is local, parses the JSON defensively, drops triples below confidence 0.6, and upserts the rest with `source` "<client> (extracted by <model>)". Upserts go through one planner shared by both stores (`memory.Batch.Resolve/Needs/Plan`): names resolve through aliases (speaker names map to `User`; `SAME_AS` adds an alias and merges an existing duplicate entity, facts included), repeated facts gain confirmations and keep the highest confidence, `replace` closes older values with `valid_to` (history, still counted towards the limit), and saving a closed fact again reopens it. Reads return current facts unless `history` is set. Claude Code integration: hooks cannot compact or trim a session, but Claude Code compacts earlier when `CLAUDE_AUTOCOMPACT_PCT_OVERRIDE` is lowered, and runs PreCompact before and SessionStart (`source: compact`) after each compaction; `codesynapse mcp hooks` sets that variable (default 30%, `--compact-at`) and installs SessionStart (`codesynapse memory recall`, facts as additional context) and PreCompact (`codesynapse memory offload`, turns before the window, once per session) hooks; `codesynapse mcp agents-md` writes the AGENTS.md block. Recall (`services/api/internal/recall`, `recall_context`, `POST /api/v1/repos/{repo}/memory/recall`) is hybrid: `memory.RecallGate` skips small talk and queries without words; entry entities come from two legs fused by reciprocal rank (`memory.FuseRRF`, k = 60, deduplicated, at most 8): a vector search (query embedded by the local provider routed to "embeddings", `MemoryNearest`, cosine mapped to 0..1, kept from 0.7, 3–10 hits) and BM25 over the namespace's entity names, aliases and types (`MemoryLexical` → `memory.RankBM25`, camelCase and `-`/`_` split, prefixes from three letters at half weight); then 1–2 hops. Why search stays in the stores instead of a separate engine: `docs/adr/0001-search.md`. A background embedder (every 30 s, and after extraction jobs) embeds entity names that have no vector from the current model (`MemoryEmbeddingTodo`/`SetMemoryEmbeddings`); in Neo4j they live in `embedding`/`embedding_model` on `MemoryEntity` with a cosine vector index created on first use and recreated when the length changes; the in-memory store searches exactly. Without an embedding model, recall uses the BM25 leg only and says so (`mode`). `GET /api/v1/memory/status` reports which memory features have a local model.
## Statistics

**Live activity (by the minute and hour).** `internal/telemetry` keeps Prometheus-style counters and histograms in each service and pushes them every 15 seconds to VictoriaMetrics (no scrape listener; VictoriaMetrics stays internal and password protected). Every series carries `tenant_id` from the authenticated caller. Recorded: MCP sessions, tool calls, latency and response tokens; LLM requests, input, output and cached ("saved") tokens, estimated cost from configured prices, request time and time to first token (streaming providers); database round trips by `db_system` (neo4j, hugegraph, milvus, postgres) and operation; graph rows and vector matches; memory facts saved, turns offloaded, recalls and embeddings; API requests by route pattern; indexing runs. A series' first push adds a 0 sample at its creation time, because VictoriaMetrics' `increase()` otherwise takes a large first value (one call returning 37k tokens) for a counter that predates collection and counts nothing. `GET /api/v1/stats/series?panel=…&range=1h|6h|24h|7d|30d` answers fixed panels at 1-minute to 6-hour steps; percentiles use `increase()` because `rate()` reads 0 for series born inside the window. The Statistics page charts them with an in-house SVG line chart (four validated series colours, never cycled; the rest folds into "other").

`GET /api/v1/stats?days=7|30|90` and the **Statistics** page combine everything measured across the workspace. Agent activity comes from MCP usage counters (`internal/usage`): a receiving middleware counts sessions (`initialize`, or `server/discover` for sessionless clients), tool calls and prompt requests per day, repository, tool and client, with errors, response tokens (bytes/4) and latency, and flushes aggregates every 30 seconds to `:McpUsage` nodes (180 days kept). Tokens avoided are estimated for navigation tools from each repository's ingest-time token benchmark (file-reading baseline ÷ graph answer). Context offloading shows stored facts, saves, recalls and measured tokens per recall, and a turn-by-turn model of one session with and without offloading (client compaction vs. offloading at a target fraction of the window) whose assumptions are editable on the page. The rest aggregates existing reports: security grades, findings and vulnerabilities by severity, the most widespread rules, licence verdicts and repository licences, update automation and Renovate issues, review decisions and fix branches, AI provider usage and sync health.

## Fix guidance

Rules are static, human-readable files: `rules/code/*.yaml` and `rules/config/*.yaml`, one per rule (id, title, severity, CWE, languages, why, fix, references, vulnerable → fixed examples), validated against `rules/schema.json` and embedded with `go:embed`. Detectors only hold the matching logic and look rules up by id; `TestEveryDetectorHasARule` keeps both sides in sync. Changing a severity or wording is a YAML edit. It is served by `GET /api/v1/rules`, the `rule_guide` MCP tool and the **How to fix** panel on each finding.

## Code reviews over MCP

Agents submit structured reviews with `submit_review` (title, summary, verdict, comments anchored to file:line with severity, category, message, suggestion, optional security rule). CodeSynapse validates and redacts them (`internal/reviews`), anchors each comment to the symbol at its line, and stores them as `:Review` nodes apart from repository graphs, so re-indexing keeps them. The **Reviews** page lists reviews and groups open comments by file; people resolve or reopen comments there, agents with `update_review_comment`. The `code-review` prompt walks an agent through review with the graph tools. These two tools are the only MCP writes, scoped to the review store.

Each comment card shows the code around its lines: `GET /api/v1/reviews/{id}/code` reads them on request with `git cat-file` at the reviewed commit (falling back to the working tree, e.g. after a shallow fetch), redacts each line and returns at most 80 lines per comment; nothing is stored. When the suggestion has a fenced code block, the page applies it to `line..end_line` and shows a before/after line diff. Locations link to the lines on GitHub, GitLab or Forgejo at that commit (provider from the host or a configured organisation).

Follow-ups: link comments to the diff hunk they were made on and flag them "outdated" when that code changes; per-review export to Markdown or a GitHub PR review.

## AI providers and background work

`services/api/internal/llm` holds providers (`openai`-compatible HTTP and the Anthropic SDK), a registry with per-task routing (global default plus `review`, `security-triage`, `briefing`; editable through `PUT /api/v1/ai/routing`, persisted in the workspace) and usage counters. `services/api/internal/aitasks` turns tasks into review documents: triage in batches sized from the output budget (6–40 findings; up to 200 per run, partial results kept); all task sizes scale with the provider's input budget, which defaults to `context_window` minus the output budget, review of a diff (comments only on files in the diff), and briefings (`kind: report`). After a successful sync, `Server.automation` compares the old and new head and finding IDs and, if the schedule asks for it, runs one background job that triages new medium+ findings and reviews `old..new`. Comment decisions are open, accepted, rejected, deferred and resolved.

Decisions: model output is untrusted (redacted, size-limited, validated against known files and enums); remote providers get no source beyond a requested diff; budgets are per request and small by default because local hardware is slow; jobs share one semaphore with ingest so a laptop is never running two heavy things at once.

Fix branches (`services/api/internal/fixes`, opt-in with `CODESYNAPSE_ALLOW_PUSH`): for accepted comments the "fix" model rewrites each affected file whole (it must fit the output budget) in a temporary worktree at the indexed head; CodeSynapse validates the answer, commits only those files, pushes a new `codesynapse/fix-<review>-<rand>` branch and opens a draft PR through the GitHub API (falling back to a normal PR, then to a compare link). Comment fix state (running, pushed, failed, branch, PR) is stored on the comment; the comment stays accepted until a person marks it fixed. Follow-up: detect merged fix branches on sync and mark comments fixed automatically; GitLab merge requests.

Next: Postgres for reviews, decisions with an audit trail, a persistent job queue, finding/score history and AI usage; an in-process TTL cache for overview/security/metrics invalidated on ingest; container security (Dockerfile rules and opt-in Trivy image scans).

## Repository lifecycle

Repositories are added by URL (cloned into the workspace volume) or mounted under `./sources`. **Auto-sync** schedules (off, 5/15/30 min, 1 h, 6 h, daily; default 30 min, counted from the end of the previous sync; pausable) are stored in `workspace/.codesynapse/schedules.json` and run by a scheduler in the API; the next run and last result show in the UI. Removing a repository deletes its graph, schedule and optionally its clone. Private repositories use a server-side token set with `just git-token`; `GET /api/v1/settings` reports the hosts with a token and a masked preview (first and last five characters; tokens under 20 characters show only `***`), never the full value.

## Operator overview

`GET /api/v1/overview` and the **Overview** page aggregate every indexed repository: totals by node and edge kind, LOC, grade distribution, findings and CVEs by severity, called CVEs, vulnerable modules, functions per sensitive API, index freshness (stale after 7 days), the workspace-wide context reduction, and ingest job health (success/failure, durations, recent errors). Repositories are listed worst security score first.

## Status

Done:
- Go extraction with tests, interfaces, security tags, complexity, doc summaries and ~20 type-aware security rules
- Markdown docs, YAML (Kubernetes, compose, GitHub Actions, Helm values) and Go templates/Helm charts, with YAML security rules
- git history; dependencies from go.mod with OSV vulnerabilities and called/imported/required reachability (stdlib checked at the building Go version)
- Neo4j and memory stores with shared contract tests (verified against Neo4j 5.26)
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

1. **Incremental ingest.** Re-extract only packages whose file hashes changed; update the graph per package; `watch` mode and a git post-commit hook.
2. **More ecosystems for CVEs:** container base images, Maven, Cargo, Composer. Grype could back this as a second scanner.
3. **Compact IDs** relative to the module path, to cut repeated prefixes from answers.
4. **tree-sitter parsers** for exact Python/JS/TS syntax and more languages (Java, Rust, C#, PHP), via a WASM runtime to stay cgo-free; type inference for TypeScript (tsc) and Python (pyright) as optional enrichers.
5. **Unused npm/PyPI dependencies**, with tooling-aware exceptions (bundler plugins, CLIs, type packages).
6. **Semantic search** with Neo4j vector indexes over summaries and signatures (would also improve `minimal_context`), local embeddings by default.
7. **GitHub Action** running `codesynapse audit --fail-on high` and `review --offline`, posting the result as a PR comment.
8. **Scheduled sync** of registered repositories and webhooks from the forge.
9. **Read-only Neo4j role** for the MCP and API services, separate from the ingest writer.

## Security roadmap

What would put security more in front of developers, ordered by value for the effort:

1. **New vs. fixed findings per change.** Fingerprint findings (rule + owner + normalised location) and compare against the base branch, so `get_pr_context`, `review` and the PR comment say "this change introduces 1 high, fixes 2" instead of listing the whole backlog. This is what makes developers act.
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

## Organisations

`internal/forge` lists the repositories of an organisation or user on GitHub (also Enterprise Server), Forgejo/Gitea/Codeberg or GitLab; one client per host, API address derived from the host, token sent to that host only, no redirects. The API keeps watched organisations in `<workspace>/.codesynapse/orgs.json` (`services/api/internal/api/orgs.go`), each with a provider and host (entries saved earlier default to GitHub). An organisation's key is `login` on github.com and `host/login` elsewhere, and repositories are named the same way (`owner/name` / `host/owner/name`), so the same organisation and repository on two providers never collide; clones live in `<workspace>/<name>`. Hosts other than github.com pass the SSRF check of `gitx.ValidateRemote` and ports are not supported. Tokens are per host (`CODESYNAPSE_GIT_TOKENS=host=token,...`). Each organisation is rescanned every 5 minutes and, with `auto_index` on, clone + index jobs are queued for repositories that are not indexed yet (archived repositories and forks only when asked for). Cloned repositories then follow the default 30-minute auto-sync. Endpoints: `GET/POST /api/v1/orgs` (body: `provider`, `host`, `org`), `PUT/DELETE /api/v1/orgs/{key}`, `POST /api/v1/orgs/{key}/scan`, `POST /api/v1/orgs/{key}/repos/{name}/index` (`{key}` URL-encoded). Stopping to watch an organisation keeps its indexed repositories. The dashboard page is `/orgs`.
