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
| Repository | head, branch, url (origin, no credentials), root, time (ingested at), metrics (JSON) |
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
| Module | path, version, indirect |
| Vulnerability | id, aliases (CVE/GHSA), severity, cwe, version, fixed_version, reachability, url |

Edges: `CONTAINS`, `DEFINES`, `HAS_METHOD`, `CALLS` (with `count`), `DISPATCHES_TO`, `IMPLEMENTS`, `IMPORTS`, `MENTIONS`, `MODIFIES` (with `count` = lines changed), `AUTHORED`, `HAS_FINDING`, `DEPENDS_ON`, `AFFECTS`, `INCLUDES`, `USES_VALUE`.

### Risk scoring

`review.Score` is a transparent, additive 0-100 score: direct callers (×3, max 25), transitive impact (max 20), exported (+10), complexity (+5/10/15), security tags (+15 high-risk / +8 other, max 30), and +15 when no test reaches the symbol. Levels: low < 20 ≤ medium < 40 ≤ high < 60 ≤ critical. Every point added comes with a reason string, so reviewers and agents can see why something scored high.

## Decisions

- **Neo4j as the backend**, with an in-memory twin behind the same interface. The contract test (`storetest.Run`) keeps them identical, which lets everything except the Cypher itself be tested without a database.
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
| System instructions | `Instructions` in `internal/mcpserver/server.go`, sent on MCP initialize |
| `get_symbol_context` (outline/full, risk score, package view) | `agentctx.SymbolContext` |
| `get_call_graph` (direction, depth ≤ 4, entry points) | `agentctx.CallGraph` |
| `get_implementations` | `agentctx.Implementations` |
| `get_pr_context` (diff / files / refs) | `agentctx.PRContext` on top of `review.Analyze` |
| `minimal_context` (`max_tokens`) | `agentctx.MinimalContext`: keyword extraction, ranking, greedy packing |
| `qualified_id` suggestions | `agentctx.Resolve` → `SuggestionError` |
| Prompts `refactor-analysis`, `pr-impact-review` (+ `package-onboarding`, `bug-investigation`) | `internal/mcpserver/prompts.go` |
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

## Fix guidance

Rules are static, human-readable files: `rules/code/*.yaml` and `rules/config/*.yaml`, one per rule (id, title, severity, CWE, languages, why, fix, references, vulnerable → fixed examples), validated against `rules/schema.json` and embedded with `go:embed`. Detectors only hold the matching logic and look rules up by id; `TestEveryDetectorHasARule` keeps both sides in sync. Changing a severity or wording is a YAML edit. It is served by `GET /api/v1/rules`, the `rule_guide` MCP tool and the **How to fix** panel on each finding.

## Code reviews over MCP

Agents submit structured reviews with `submit_review` (title, summary, verdict, comments anchored to file:line with severity, category, message, suggestion, optional security rule). CodeSynapse validates and redacts them (`internal/reviews`), anchors each comment to the symbol at its line, and stores them as `:Review` nodes apart from repository graphs, so re-indexing keeps them. The **Reviews** page lists reviews and groups open comments by file; people resolve or reopen comments there, agents with `update_review_comment`. The `code-review` prompt walks an agent through review with the graph tools. These two tools are the only MCP writes, scoped to the review store.

Follow-ups: link comments to the diff hunk they were made on and flag them "outdated" when that code changes; per-review export to Markdown or a GitHub PR review.

## Repository lifecycle

Repositories are added by URL (cloned into the workspace volume) or mounted under `./sources`. **Auto-sync** schedules (off, 15 min, 1 h, 6 h, daily; pausable) are stored in `workspace/.codesynapse/schedules.json` and run by a scheduler in the API; the next run and last result show in the UI. Removing a repository deletes its graph, schedule and optionally its clone. Private repositories use a server-side token set with `just git-token`; `GET /api/v1/settings` reports whether one is configured, never the value.

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
- Rule catalogue as static YAML files in `rules/` (39 rules), embedded and schema-validated
- Code reviews over MCP with a Reviews page; auto-sync schedules; repository removal; private-repo tokens
- UX pass: repository summary with health verdicts and next steps, plain-language labels and inline explanations, Kubernetes & Helm page, content detection for sources
- MCP server (18 read-only tools + 2 review write tools, 5 prompts, stdio + HTTP with bearer auth) and `codesynapse mcp install` for local agents
- API with dashboard, operator overview, graph explorer endpoints, ingest/clone/sync jobs
- Nuxt 4 dashboard in the Astrona style: overview, repositories, dashboard, security (clickable rules, fix examples), cleanup, graph, explorer, metrics, docs, packages, review
- docker compose (Docker or Podman), justfile, golangci-lint clean, race tests, fuzzers

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
