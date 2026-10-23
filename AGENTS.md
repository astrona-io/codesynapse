# AGENTS.md

Instructions for AI coding agents (Claude Code, Codex, Cursor, Copilot and others) working in this repository. Humans should start with `README.md`.

## What this project is

CodeSynapse indexes a repository's Go code, Markdown docs, YAML, Go templates/Helm charts, git history and dependencies (with CVEs) into an Apache HugeGraph property graph, then serves small, precise answers from it: symbol cards, file outlines, callers and callees, impact of a diff, a graded security review, metrics and hotspots. The point is that a coding assistant asks the graph instead of reading whole files, which cuts tokens and gives better review context.

Four services plus their stores, all in `docker-compose.yml`:

| Service | Code | Port | Purpose |
|---|---|---|---|
| `mcp` | `services/mcp` | 8090 | MCP server for AI agents (stdio or streamable HTTP) |
| `api` | `services/api` (image target `api`, no Go toolchain) | 8080 | JSON API for the dashboard; records jobs (`CODESYNAPSE_RUN_JOBS=false`) |
| `worker` | `services/api` (`codesynapse-api worker`, image target `worker`) | 8081 (health) | Runs clone, sync, index, AI and fix jobs of its roles (`--roles ingest,ai,fix`) |
| `web` | `services/web` (Nuxt 4) | 3000 | Dashboard; proxies to `api` server-side |
| `hugegraph` | image | internal only | Graph store (Gremlin; never published to the host) |
| `milvus` | image | internal only | Agent-memory vectors |
| `postgres` | image | internal only | Accounts, organisations, audit log |

With accounts, schedules and jobs live in PostgreSQL, and one API process at a time (the `api-loops` lease, `services/api/internal/api/singleton.go`) runs the scheduler and maintenance loops, so the API can run as several replicas.

`cmd/codesynapse` is the CLI (ingest, audit, stats, review, mcp install) for CI and local use. It is not a service.

## Use the graph before reading files

If the codesynapse MCP server is connected (`just mcp` installs it for Claude Code), use it on this repo too. Full guide with task prompts: `docs/AGENT_GUIDE.md`.

1. `minimal_context` with the task description first; read only the line ranges it returns.
2. `get_symbol_context` (verbosity "outline", then "full" before editing) for signature, risk, callers, callees, tests.
3. `get_implementations` for interfaces; never grep for interface calls. `get_call_graph` (depth 2) for deeper tracing.
4. `file_outline` before opening a file.
5. `get_pr_context` with your diff before finishing a change. Fix or justify anything it flags as untested, high risk or security-sensitive.
6. Ambiguous names return `qualified_id` suggestions: retry with one of them.
7. Before writing new code, look for what exists: `search` (and `search_all_repos`), then `duplicate_code`; reuse or extend before adding a second copy.

The graph reflects the last ingest. Files you edited in this session may differ.

When you change MCP tools, prompts or the server instructions, update `docs/AGENT_GUIDE.md` in the same change. A new tool goes into one toolset in `internal/toolsets` (`TestEveryToolInOneToolset`), and instruction text that names it goes into a clause tied to that toolset (`instructionRules` in `services/mcp/internal/mcpserver/toolsets.go`).

## Layout

```
cmd/codesynapse/        CLI: ingest, audit, stats, review, mcp install, schema
services/api/           API service: cmd/codesynapse-api, Dockerfile, and packages only it uses
  internal/api/         HTTP handlers, jobs (ingest/clone/sync), auto-sync scheduler, reviews, dashboard and overview
  internal/llm/         Model providers (OpenAI-compatible, Anthropic SDK), per-task routing, usage
  internal/annotation/  Bounded local metadata annotation with session/policy/revision checks
  internal/aitasks/     AI tasks (security triage, review, briefing) that produce reviews
  internal/fixes/       Fix branches for accepted review comments: worktree, model rewrite, push, draft PR
services/mcp/           MCP service: cmd/codesynapse-mcp, Dockerfile
  internal/mcpserver/   MCP tools, prompts (prompts.go) and server instructions
services/web/           Nuxt 4 dashboard (app/ pages+components, server/api/cg proxy, Dockerfile)
deploy/helm/codesynapse/ Helm chart; stores are subcharts in charts/ (on by default, disable to link external ones), common/ holds shared helpers
                        values.schema.json (one per chart) validates every value; keep it in step with values.yaml
internal/ingestreport/  Bounded file ledger, commit-confirmed outcomes and metadata-only context estimates
internal/config/        Strict repository .codesynapse.yaml policy, digest and shared path matcher
internal/model/         Node/Edge kinds, Graph, IDs. The schema lives here.
internal/ingest/        Pipeline: golang/ (go/packages + go/types; security rules in findings.go),
                        lang/ (Python, JS/TS, Vue: pure-Go structural parsers + rules),
                        docs/ (Markdown), config/ (YAML, Go templates, Helm; YAML security rules), githistory/
internal/curation/       Proposals and links added to the architecture catalog by people and agents (never decisions); validation and redaction
internal/arch/           Reads architecture records (ADR, RFC, standard, policy, overview, runbook, diagram) kept as files into Record and Component nodes
internal/catalog/        The one service the API and MCP tools use to list, read and find the records that apply to a task
internal/history/        Snapshot and Trend: what a repository looked like after each ingest, and what got worse or better
internal/snapshot/       Takes a snapshot from the stored reports and records it after each ingest
internal/refactorplan/   How to merge one duplicate group (name, home, callers, tests, risks, steps); plans only
internal/cleanupplan/   Ranked cleanup plan from the stored reports (dead code, dependencies, duplicates, hotspots)
internal/cleanup/       Liveness (reachability from entry points), unused code/modules/config
internal/sbom/          CycloneDX 1.5 SBOM from the dependency graph (store.DependencyGraph)
internal/vuln/          go.mod and lockfiles (npm, PyPI, Cargo, Composer, Bundler, NuGet, Gradle) + OSV lookups, called/imported/required reachability
internal/security/      Security report: grading, exposure, prioritisation, Markdown; rule guides (guides.go)
internal/metrics/       Repository metrics and the token benchmark (computed at ingest)
internal/forge/         Organisation listing for GitHub, Forgejo/Gitea and GitLab (one host per client) for the Organisations feature
internal/gitx/          Hardened git runner, diff/log parsers, clone/fetch with SSRF checks
internal/redact/        Secret redaction for every stored summary; personal data masking for free text (personal.go)
internal/store/         Store interface; memory/ and hugegraphstore/ implementations; storetest/ contract;
                        nodeprops/ (node <-> property map, shared); StatsBuilder (shared Stats rules)
internal/vector/        Memory-embedding index: Flat (in-process), milvus/ (Milvus REST v2); vectortest/ contract
internal/query/         Composed answers (Outline, Hotspots, Overview, Tests)
internal/agentctx/      Composite agent answers: get_symbol_context, get_call_graph, get_implementations,
                        get_pr_context, minimal_context; qualified_id resolution and suggestions
internal/review/        Diff → impact report, risk scoring, Markdown renderer
internal/agentcfg/      `codesynapse mcp install`: registers the MCP server with local agents
internal/reviews/       Code reviews: model, validation, redaction, comment decisions
internal/accounts/      Users, organisations, roles, sessions, API keys, audit log (memstore/, pgstore/ with
                        embedded migrations, accountstest/ contract)
config/                 llm.example.yaml (committed); llm.yaml is local and gitignored
internal/app/           Shared wiring: graph store flags (HugeGraph, Milvus), bearer auth, HTTP server, cobra CLI bootstrap
internal/logx/          slog logger carried in contexts
internal/telemetry/     Bounded metrics, OTel/OTLP export and VictoriaMetrics queries
internal/testutil/      assert/require helpers for tests
docs/PLAN.md            Architecture, decisions, roadmap
docs/AGENT_GUIDE.md     How agents should use the MCP tools and prompts
docs/adr/               Architecture decision records (0001: search stays inside the stores)
rules/                  Security rule catalogue: one YAML per rule (code/, config/), embedded
```

## Commands

```sh
just            # list recipes
just test       # go test -race ./...
just lint       # golangci-lint (must report 0 issues)
just web-check  # nuxt typecheck + build
just checks     # actionlint, hadolint, shellcheck, yamllint, gitleaks, Trivy, govulncheck
just ci         # all of the above plus the licence check: what CI runs on a pull request
just up         # full stack in docker compose (creates .env with random secrets)
just dev-mcp    # MCP on stdio with an in-memory graph of the checkout (no database)
just dev-web    # Nuxt dev server against the API of `just up`
just test-integration   # test-hugegraph + test-postgres on throwaway containers
just test-postgres      # accounts contract against a throwaway PostgreSQL container
just test-hugegraph     # HugeGraph/Milvus store contract and parity on throwaway containers
just test-metrics       # telemetry push and tenant-filtered queries on a throwaway VictoriaMetrics
just review     # offline review pack for the current branch
just eval       # score minimal_context and search on past commits against docs/benchmarks/task-eval (needs full history)
just image-build  # api, worker, mcp and web images
just chart-check  # helm lint, kubeconform, kube-linter, kubesec
just chart-e2e    # chart on a throwaway kind cluster: build, load, install, helm test (just kind-down)
just chart-failover-postgres   # after chart-e2e: CloudNativePG with three instances, kill the primary under writes
just chart-failover-milvus     # after chart-e2e: Milvus cluster (SeaweedFS, Woodpecker), kill nodes under writes
just chart-install-published X.Y.Z   # a released chart and images on the kind cluster (release check)
just mcp        # register the MCP server with Claude Code, Codex, Gemini, OpenCode, Cursor
```

Test the Helm chart only on the throwaway kind cluster of `just chart-e2e` (`kind-codesynapse-dev`), with `--context`/`--kube-context` set explicitly; never on a shared or production cluster. Run `just chart-check` for any chart change.

Every value lives in the chart's `values.schema.json` (and a store's in `charts/<store>/values.schema.json`): Helm checks installs against it and Artifact Hub shows it as the chart's settings. When you add, rename, remove or retype a value, change `values.yaml` and its schema in the same change, with a description and strict types (`additionalProperties: false`, enums, patterns); a value missing from the schema makes every install fail. `just chart-check` renders the chart with valid values and makes sure bad ones are refused; add a case there for a new rule.

Run `just test` and `just lint` before you call a Go change done, and `just web-check` for anything under `services/web/`. CI (`.github/workflows/ci.yml`) runs `just ci` on every pull request and merge, plus the integration contracts when stores or accounts change; run `just checks` before touching workflows, Dockerfiles, scripts or YAML. Accepted findings go in `.gitleaks.toml`/`.gitleaksignore`, `.trivyignore` or `.hadolint.yaml`, each with a reason. Pin new GitHub actions by commit SHA.

Defer both development and production Kubernetes manifest work to the final deployment phase, after application features and SSO.

## Go conventions

- Go 1.27, module `github.com/astrona-io/codesynapse`.
- Standard library first. Current dependencies: cobra (CLI), pgx (PostgreSQL), the MCP Go SDK, golang.org/x/tools and x/mod. Ask before adding any other dependency.
- Tests use the in-repo helpers `internal/testutil/assert` (continue on failure) and `internal/testutil/require` (stop on failure). Do not add testify.
- Logging is `log/slog`, carried through contexts with `internal/logx`.
- Binaries use `app.NewSubCommand` runners (`SetupFlags/Complete/Validate/Run`) from `internal/app/cli.go`; see `cmd/codesynapse/main.go`.
- `context.Context` first on anything that blocks. Wrap errors with `%w` and context. No panics outside programmer errors.
- Table-driven tests with subtests. Parsers get fuzz tests (`FuzzParseDiff`, `FuzzText`).
- Comments say why, not what. Match the density of the surrounding code.

## Adding a security rule

Rules are static files: `rules/code/<id>.yaml` and `rules/config/<id>.yaml` (see `rules/README.md` and `rules/schema.json`). They hold everything humans read (title, severity, CWE, why, fix, vulnerable → fixed examples) and are embedded with `go:embed`.

1. Write the YAML file.
2. Add detection to the detector for the language (`internal/ingest/golang/findings.go`, `internal/ingest/lang/rules.go` or `internal/ingest/config/yaml.go`), list the id in that detector's `RuleIDs`, and report findings with it.
3. Add a fixture that triggers it and one that must not to the detector's tests.
4. `just test`: `rules.TestCatalog` validates the files and `TestEveryDetectorHasARule` keeps detectors and files in sync.

## Changing the graph schema

1. Add the kind to `internal/model/model.go` and to `AllNodeKinds`/`AllEdgeKinds`. Those lists are the label allowlist for Gremlin.
2. New node fields need entries in `internal/store/nodeprops` (both directions, `TestPropsRoundTrip`) and a type in `hugegraphstore/schema.go` (`TestSchemaCoversNodeProps` fails otherwise). `EnsureSchema` appends new HugeGraph properties to existing labels.
3. New `store.Reader` methods must be implemented in `memory` and `hugegraphstore` and covered in `storetest.Run`. HugeGraph publishes incrementally (`delta.go`): node vertices carry `content_hash`/`edges_hash`, and `ReplaceRepo` writes only what changed, falling back to a full write; `successiveContract` keeps the result identical to the in-memory reference. Keep the graph an ingest produces deterministic (sort what comes out of maps), or every publish rewrites the nodes that differ by chance. The stores must behave identically; the contract test (`just test-hugegraph`) and `TestParityWithReference` are how we know.
4. Update `docs/PLAN.md` (schema section) and `services/web/app/utils/types.ts` if the API shape changes.

## Security rules (non-negotiable)

This tool reads untrusted repositories and serves their contents to LLMs. Keep it that way:

- **Ingestion accounting:** exact reports live in tenant-owned accounts records, never in shared graph metrics. `Repository.meta.ingest_receipt` is an opaque commit receipt only. Checkpoints precede extraction/publication; recovery counts indexed files only after matching a receipt under the publication lock. Preserve run tombstones, cumulative totals and stable repository metric IDs across retries, restarts and unlink/relink. The three `codesynapse_files_*_total` metrics alone may carry `repo`, an admitted server-owned 24-hex repository-entry ID verified against its tenant, never a display name or graph ID. Export persisted totals through the fenced ingestion exporter, never replay them into Default counters. Scoped series use both tenant and repository extra filters. See `docs/INGEST_TRANSPARENCY_PLAN.md`.
- **Repository policy:** `.codesynapse.yaml` is untrusted. Global/Kubernetes exclusions require a matching reviewed digest (every linked organisation in the API, explicit `--approved-policy-digest` offline). Finding suppression is a tenant-local approval overlay; never delete raw findings. Annotation uses only the local `code-annotation` route, validated symbol metadata and inherited execution identity. Graph and annotation publication must retain the accounts policy transaction and final credential/revision checks. See `docs/REPO_CONFIG_PLAN.md`.
- **Gremlin (HugeGraph):** scripts are constants in `hugegraphstore`; values, labels and IDs travel only in `bindings`, never in script text. Gremlin-groovy executes code on the server, so HugeGraph always runs with a password (`HUGEGRAPH_PASSWORD`; the compose service refuses to start without one), is never published to the host, and there is no raw-Gremlin tool. Tenant-owned data (agent memory) carries `tenant_id` and every traversal over it starts at `has('tenant_id', …).has('ns', …)`.
- **Vectors (Milvus):** filters are expression templates with values in `exprParams`, never in the expression. Every item carries its tenant (`tenant_id` is the partition key), every search names one (`vector.ErrNoTenant` otherwise), and `vector.Isolate` drops any hit of another tenant or namespace even if the backend's filter was wrong. Only entity names are embedded, by a local model.
- **Telemetry:** tenant instruments always carry `tenant_id`, `mcp_tool`, `llm_provider` and `llm_model`. Use the context-aware recording methods: the tenant comes from authenticated `telemetry.WithTenant`, tools from accepted `WithTool`/`AllowTools`, and models from configured `AllowModels`/`WithModel`; positional or client-supplied attributes cannot override them. API/background work uses `none` where no tool/model applies. Work without a trusted tenant goes to separate `platform_*` metrics with no tenant attribute. Never label prompts, queries, paths, repository names, user/key/session IDs or other request content. Each instrument caps its series before aggregation, including histogram buckets; overflow is a separate platform counter. The existing bounded registry supplies an OTel SDK `Producer` and periodic OTLP/HTTP exporter; do not record the same observation into a second SDK accumulator. `CODESYNAPSE_METRICS_EXPORTER=otlp` is the default; `prometheus` selects the legacy push format, never both for one stream. Services use the internal VictoriaMetrics URL and environment-only credentials; nothing listens for scrapes. Resource identity distinguishes service/version/environment and process instance, never a chat session. `GET /api/v1/stats/series` uses fixed MetricsQL and authenticated organisations as VictoriaMetrics `extra_filters`; ordinary panels stay scoped even for platform admins, and only explicit platform panels can query across tenants. Durable memory counts use the newest observation per tenant, deduplicate replicas and expire stale samples; never sum replica gauges. Cost without reported usage or configured prices is unknown; local execution alone proves no cloud-byte savings. New instruments go in `internal/telemetry/metrics.go`. **Traces** (`internal/telemetry/tracing.go`, docs/TELEMETRY_PLAN.md#traces): spans carry route patterns, kinds, opaque IDs, provider/model and timings, never paths, repository names, people, prompts, code, arguments, scripts or bindings. Continue an incoming `traceparent` only from trusted proxies (never in MCP). Jobs carry the queueing request's trace in `jobs.trace`. With a valid span, the request ID is the trace ID.
- **git:** only through `gitx.Run`. It disables fsmonitor, hooks, external diff and textconv, ignores global config, and never uses a shell. Validate user revisions with `gitx.ValidateRev`.
- **go list:** extraction runs with `CGO_ENABLED=0`, `GOTOOLCHAIN=local`, `GOPROXY=off` unless the user opts into network. Do not loosen these defaults.
- **Filesystem:** API ingest is confined to `CODESYNAPSE_SOURCES_ROOT` after symlink resolution (`insideSources`). Docs walking never follows symlinks.
- **No source bodies or YAML values in the graph.** Store hashes, signatures, key paths, line ranges and short summaries. Every summary passes through `redact.Summary`. Author e-mails are stored only as hashes. Security findings describe the problem, never the secret.
- **Remote git:** only `gitx.Clone`/`FetchCheckout`/`RemoteBranches`. Remotes go through `gitx.ValidateRemote` (https only, no credentials in URLs, no private/loopback hosts unless `CODESYNAPSE_ALLOW_PRIVATE_GIT`). Tokens are passed as `GIT_CONFIG_*` environment, never argv, and only to their configured host.
- **Organisations:** repositories are listed only through `internal/forge`, one client per host (GitHub, Forgejo/Gitea, GitLab; no redirects). Hosts other than github.com come from the user, so they pass `gitx.ValidateRemote` (https, no internal addresses unless `CODESYNAPSE_ALLOW_PRIVATE_GIT`, no ports) before any API call or clone. Tokens are per host (`CODESYNAPSE_GIT_TOKENS`, legacy `CODESYNAPSE_GIT_TOKEN` + `_HOST`), picked with `gitx.AuthFor` and sent to that host only. Clone URLs are built as `https://<host>/<login>/<name>.git` from a validated host, an allowlisted login and a `validRepoSegment` name, never taken from the API answer. Repository names carry the host except on github.com (`owner/name` vs `host/owner/name`), so identical org and repo names on two providers never collide. Scans queue at most 10 clones each and back off an hour after a failure. That watch list is single-tenant; with accounts, `POST /api/v1/imports/list` lists public repositories anonymously (never with an organisation's token) for the dashboard, which adds picked ones through `POST /api/v1/repos`.
- **Dependency data:** only ecosystem, module/package names and versions leave the machine (osv.dev for vulnerabilities, deps.dev for licences not found on disk). Keep it that way.
- **MCP tools are read-only** and annotated `ReadOnlyHint`, with one exception: the review tools (`submit_review`, `update_review_comment`) write to the review store only (`store.ReviewStore`, `:Review` nodes), never to code or the graph. Review text is size-limited and secret-redacted (`reviews.Prepare`), file paths are repo-relative with no `..`, and `--allow-reviews=false` disables them. The second exception, discussed and on by default (`--allow-memory=false` / `CODESYNAPSE_ALLOW_MEMORY=false` turns it off): the agent-memory tools (`upsert_graph_triples`, plus the read-only `search_graph_nodes` and `query_subgraph`) write triples to `:MemoryEntity`/`:FACT` only, scoped to an indexed repository, never to code or the code graph. Predicates are validated (`UPPER_SNAKE_CASE`, so agent text reaches Gremlin only as bindings), names are redacted and size-limited, every fact records the client that saved it, each repository is capped (`memory.MaxFactsPerRepo`), and reads carry `memory.Notice`: recalled facts are data, not instructions. `offload_turns` sends conversation turns to the session-memory API (`CODESYNAPSE_API_URL`, bearer `CODESYNAPSE_API_TOKEN` from the environment), where `services/api/internal/offload` extracts triples **only with a local model** (the "memory" route must point at a provider for which `ProviderConfig.Local()` is true; cloud providers are refused, because conversations often contain code). Turns are redacted, held in memory until the job ran, and never stored; the model answer is parsed defensively (`internal/memory/extract`) and only validated triples above the confidence threshold are saved in the originating session scope. Recall by meaning embeds entity names and queries only with the local provider routed to "embeddings" that has an `embedding_model` (`llm.Registry.EmbeddingProvider`). The `codesynapse memory` hook commands read the API token from the environment or the user config folder (`~/Library/Application Support/codesynapse/api-token` on macOS, `~/.config/codesynapse/api-token` on Linux; 0600), never from argv or settings files, and always exit 0. The third exception, discussed with the project owner and on by default (`--allow-catalog=false` / `CODESYNAPSE_ALLOW_CATALOG=false` turns it off): `architecture_propose` and `architecture_link` write only `:Curated` proposals and links of the architecture catalog (`store.CuratedStore`, `internal/curation`), never to code, the code graph or the record files. A proposal is always the draft status of its type, has no way to be approved, accepted, rejected or to supersede anything, and only becomes a decision when a person commits it as a file with an accepted status. Both tools need the Member role on the repository, validate what they reference (records must exist, code must name exactly one place), redact and size-limit all text, record the client and user, are kept per organisation and, in accounts mode, write an audit event before the change (a failed audit refuses the change). There is deliberately no raw-Gremlin tool (scripts execute code on the server). Do not add other write tools without discussing it first.
- **MCP usage counters** (`internal/usage`, on unless `--record-usage=false` / `CODESYNAPSE_MCP_USAGE=false`): the MCP server adds daily aggregates per repository, tool and client (calls, errors, response size, latency) to `:McpUsage` nodes for the Statistics page. Only the `repo` argument is read; other arguments and results are measured, never stored in the counters (the audit log keeps redacted arguments, see below). Client names are cleaned and capped, buckets between flushes are bounded, and data older than 180 days is pruned.
- **Destructive API actions** (delete repository, delete review) are confirmed in the UI; the one automatic removal is `reapIfGone` (scheduler): a cloned repository goes only after the provider API answers a clean 404 three scheduled syncs in a row; deleting cloned files is limited to a single directory inside the workspace, and mounted sources are never deleted.
- **Network services** refuse to bind a non-loopback address without a bearer token (`app.ErrTokenRequired`). The web proxy (`services/web/server/api/cg`) rejects cross-origin mutating requests and, with accounts, forwards only the browser's `cs_session` cookie (and the `cs_org` slug as `X-CodeSynapse-Org`), never the API token: a visitor is exactly who they signed in as. Only single-tenant installs get the server-side token, and an unreadable mode fails closed. The API takes the client address from `X-Forwarded-For` only from `CODESYNAPSE_TRUSTED_PROXIES` (login limits and audit IPs are per user, not per proxy). `/api/v1/settings` shows the installation's Git token previews to platform admins only.
- **Limits everywhere:** `store.MaxLimit`, `store.MaxDepth`, request body caps, query timeouts. New endpoints keep them.
- **Pushing:** only `services/api/internal/fixes` pushes, only when `CODESYNAPSE_ALLOW_PUSH=true`, only through `gitx.PushBranch` (new `codesynapse/*` branches, never forced) from a separate worktree, and never edits CI or git configuration (`fixes.protected`). Keep those rails.
- **LLM calls:** only through `services/api/internal/llm`. Model output is untrusted: parse it defensively, validate enums and file paths, redact before storing. Prompts and embedding inputs pass `redact.Free` (secrets and personal data) in the registry, so no caller can skip it. Free text people or agents write (memory, offloaded turns, reviews) also goes through `redact.Free` before it is stored; new free-text stores do the same. Keep system prompts constant: the Anthropic provider caches them as the prompt prefix, and anything per-request belongs in the user part. Never send source to a non-local provider except a diff the user asked to review. API keys come from `api_key_env`, never from `config/llm.yaml` itself.
- **Accounts** (`internal/accounts`, PostgreSQL via `CODESYNAPSE_DATABASE_URL`, env only): passwords are argon2id; session tokens and API keys (`csk_…`) are stored only as SHA-256 hashes; sessions are HTTP-only `SameSite=Lax` cookies; logins are rate limited through PostgreSQL (`TakeAttempt`, hashed keys), so every API replica shares the count, and unknown emails take as long as wrong passwords; API keys act in one organisation and stop working when the member is removed; cookie-authenticated changes must be same-origin. Never return a password hash or key hash from an endpoint.
- **Tenant scoping:** every API route is registered with `s.route(pattern, policy, handler)` (`services/api/internal/api/scope.go`): `repoRole`/`reviewRole` (a role on the repository's organisation; others' repositories answer 404), `visibleOnly` (the handler filters with `visibleRepos`/`scopeFrom(ctx).sees`), `orgRole` (a role in the selected organisation, `X-CodeSynapse-Org` or the API key's), `anyone`, or `platformOnly`. Pick the narrowest; a new list endpoint must filter. Organisations add public repositories only (cloned without the installation's tokens); an already indexed graph is linked, not cloned again; deleting removes the organisation's link and the graph only with the last link.
- **Organisation Git tokens and private repositories** (`services/api/internal/api/orgtokens.go`, `internal/accounts/gittokens.go`): tokens are sealed with AES-256-GCM (`accounts.Sealer`, key from `CODESYNAPSE_SECRET_KEY`, env only; the ciphertext is bound to organisation and host), set and removed by owners and admins only (never with an API key), shown as previews, opened only to clone, fetch or push, and never logged, returned or audited in clear. Organisations never use the installation's tokens: a public repository is cloned anonymously, a private one only after the organisation's own token read it (`proveAccess`, one `ls-remote`). An existing graph is linked only for the same remote (`sameRemote`), so a name cannot open someone else's graph. Every sync re-checks each organisation's private access (`verifyAccess`): `gitx.ErrNoAccess` or a missing token marks the entry `lost` (hidden from API and MCP scope, audited `repo.access_lost`), network errors change nothing, and a working token restores it. Fix branches of organisation members push with the organisation's token (`pushAuth`).
- **Repository permissions** (`internal/accounts/authz.go`, `teams.go`): PostgreSQL memberships and relation tuples are authoritative. Every subject must be an active, explicit member of the owning organisation; platform administration gives no code access. A direct user grant (`admin`, `editor`, `viewer`, or explicit `none`) overrides team grants; otherwise the strongest same-organisation team grant wins, then the organisation's `READ_ONLY`/`NONE` default. Guests receive no default. Organisation roles manage membership and policy without granting repository permissions. API keys stay in their selected organisation and never mutate access. `AccessOf` uses bounded bulk reads; overflow or invalid grants fail closed. `Access.Scope` excludes unselected and ambiguous tenant entries. Memory/review writes need editor; sync, schedules, settings and grants need repository admin. Creating or attaching an organisation repository requires a signed-in session and records an initial explicit admin grant atomically. Voluntary access changes preserve an active repository admin; security revocation and suspension still apply immediately. An explicit organisation owner can recover administration through an audited action. External grants remain inactive without membership.
- **Access projections** (`internal/model/access.go`, `internal/query/access.go`, store adapters): `Organization`, `User`, `Team` and `AccessRepository` are separate from code `Node` vertices. Access relationships never attach to packages/functions. Versioned snapshots contain tenant IDs, opaque subject IDs and permissions, never credentials or user emails. PostgreSQL policy mutations commit audit and outbox records together. A bounded worker rebuilds projections; missing, conflicting or stale projections cannot authorize requests. Keep Gremlin scripts constant, with values only in bindings. The shared store contract tests permission parity and revision handling.
- **Execution identity:** every MCP tool and API execution action requires accounts and a registered runtime session (`internal/execution`, `internal/accounts/execution.go`). HTTP carries `X-CodeSynapse-Session` and `X-CodeSynapse-Parent-Session`; stdio uses the corresponding `CODESYNAPSE_SESSION_ID` and `CODESYNAPSE_PARENT_SESSION_ID` startup values. Primary and parent are equal, immutable, supplied by the runtime and bound to one tenant/user. Never generate a fallback or select scope from tool arguments. The one server-side binding is the dashboard's: a request authenticated by a browser session cookie that names no runtime session gets `accounts.DashboardExecutionSession`, derived from the login session's hash, the selected organisation and a seven-day window and registered like any other; API keys and the service token never get one, and explicit headers are never replaced by it. Jobs, model calls and subprocesses inherit the validated identity and recheck permissions before work/persistence. Accountsless automation fails closed. Claude Code connects through `codesynapse mcp bridge` (stdio, one per chat), which takes the ID Claude Code supplies in `CLAUDE_CODE_SESSION_ID`, registers it and adds the headers; it never generates one. Register with `POST /api/v1/execution/sessions`; seven-day expiry and revocation retain permanent tombstones. Session IDs never become metric labels. See `docs/SESSION_CONTEXT_PLAN.md` for client compatibility and limits. Jobs are durable with accounts (`internal/accounts/jobs.go`, `services/api/internal/api/jobs_durable.go`): every new job needs `JobOptions.Durable` (stored parameters, never credentials), and a resumed job is rebuilt only through the function that built it, after `jobContext` rechecked session, account, key, membership and permission. Fix jobs are never resumed. A job stopped by its process stopping or by lost leases (`errLeaseLost`, renewal failing for 90 s) is never marked failed: its record stays for another process (`leftBehind`).
- **Session memory:** `offload_turns` writes only session-private `SessionMemoryEntity`/`SESSION_FACT` data; `recall_context` defaults to the tenant/session/namespace, with explicit `memory_scope: "repository"` for durable recall. Session vector collections are separate and filter tenant/session/namespace/expiry with `exprParams`; foreign hits are discarded before graph hydration. Existing triple tools remain durable repository memory. New durable facts record `origin_session_id`; legacy facts are not assigned invented sessions. Promotion is an explicit authorised API action. Clearing or expiring session memory retains replay tombstones and removes its vectors. No anonymous background embedding: refresh requires the originating session. Raw turns are redacted, bounded, transient and extracted only by a local model.
- **MCP tenant scoping:** with `CODESYNAPSE_DATABASE_URL` set, HTTP and stdio require personal API keys. The installation token and an absent `Caller` grant no content access. Authentication and permissions are checked on every tool invocation, including resumed sessions. Every tool appears in the fixed policy table; unknown tools fail closed. `h.repo` and `visibleRepos` limit graph selection; `store.ForRepository` fences node IDs, graph traversals and contextual answers. Memory uses the `org|graph` namespace; reviews require the exact organisation (legacy organisation-less reviews are unavailable in accounts mode). Calls to the API forward the caller's own key. Background work carries actor and organisation identity and rechecks credentials and permission before execution and writes. Schedules require explicit delegated identity and current repository admin. Explicit offline/single-tenant mode remains available without accounts.
- **Audit log (NIS2):** every changing request and every security event is recorded with actor, how they authenticated, client address, user agent, request ID, target, status and outcome, in an append-only, hash-chained table (`events`, tip in `audit_head`, appended under a row lock) with `GET /api/v1/admin/audit/verify`, export as JSON, JSON Lines or CSV (formula-neutralised), and a structured `audit` log line for SIEMs. New endpoints are covered automatically by the HTTP middleware; new security-relevant actions in services also call `Service.Record` with a semantic action. Retention is `CODESYNAPSE_AUDIT_RETENTION_DAYS` (default 400, minimum 30). **MCP requests** are audited too (`services/mcp/internal/mcpserver/audit.go`, docs/SAAS.md 5c). Every request except `ping` and notifications becomes an `mcp.<method>` entry, accepted or refused. It records the caller, the API key ID (never the key or its hash), the runtime and transport session, the client, the tool or prompt, the repository, duration, response size, a SHA-256 digest of the canonical arguments, and the arguments themselves: values pass `redact.Free` and are capped at `accounts.MaxMCPArgValue`, while bulk inputs (`diff`, `code`, `turns`, …) and longer values keep only a digest and size. Results are never stored. If the entry cannot be written, the result is withheld. MCP details are an optional `Event.MCP` block that joins the hash chain only when present, so older entries still verify. The client address comes from `RequestInfoHTTP`, which sets `X-CodeSynapse-Client-Addr`/`-Request-Id` itself; client-sent copies are overwritten. The MCP service trusts no proxy unless `CODESYNAPSE_MCP_TRUSTED_PROXIES` is set in compose.
- **Bootstrap root** (`internal/accounts/bootstrap.go`, `services/api/internal/api/root.go`, migration 0010): the first account is the installation's immutable root (one per installation, never cleared; independent of the platform-admin flag). Root does governance only; `businessAllowed` refuses it in `accessOf`, `RoleIn`, API-key authentication, org creation and execution sessions, so memberships, grants, keys, jobs and schedules cannot bypass it. Every API route is registered through `s.route` (policy, `governance(...)` for root-allowed routes) or `s.account` (public/governance/business); `TestEveryRouteIsClassified` keeps it that way, and new routes default to business. Its only tenant change is `CompleteSetup` (organisation + distinct standard owner + completion + session end in one transaction). Existing installations are classified once at API start-up from setup history; an ambiguous one limits every platform admin until `codesynapse-api designate-root`. Single-tenant mode has no accounts and no root.
- **Single sign-on** (`internal/sso`, `internal/accounts/sso.go`, `services/api/internal/api/sso.go`, migration 0011; see `docs/SSO_PLAN.md`):
  - **Providers:** one OpenID Connect provider per organisation, set by owners and admins in a signed-in session. The issuer passes discovery before it is stored. The client secret is sealed with `accounts.Sealer` bound to purpose, organisation and issuer, and only its preview is shown.
  - **Network:** all provider traffic goes through `sso.Client`, which refuses internal addresses at dial time unless `CODESYNAPSE_SSO_ALLOW_PRIVATE`, follows no redirects, caps responses at 1 MiB and needs https endpoints.
  - **Sign-in:** PKCE S256, a nonce, a single-use state stored as a hash and bound to the browser's `cs_sso` cookie, and verified ID tokens only. No step redirects through the API.
  - **Accounts:** a new subject needs a verified email. It links an existing account only if that account is already a member of the provider's organisation, and gets a passwordless account only when the provider grants a role.
  - **Membership:** the provider's groups manage only the memberships it created (`idp_memberships`) and mapped teams. Hand-added memberships are untouched. The last owner is never removed or demoted. Root and suspended accounts never sign in this way.
  - **Audit:** every step is audited.
- **Platform administration** (`/api/v1/admin/*`, platform admins only; suspending and the platform admin flag need a signed-in session, never an API key): suspending an account (`users.disabled_at`) ends its sessions and refuses its logins and keys at the next request; the service keeps at least one active platform admin (`ErrLastAdmin`) and refuses suspending yourself. Admin user lists never carry password hashes.
- Secrets come from the environment (`CODESYNAPSE_HUGEGRAPH_PASSWORD`, `*_TOKEN`), never flags, never logs.

## Web (Nuxt 4)

Code identity and reuse: `code_identities` reads permanent symbol IDs and
revision-specific evidence; `code_records` reads attributed annotations and
explicit lineage. `code_annotate` and `code_record_reuse` are approved narrow
catalog writes under the existing `--allow-catalog` switch: `Curated` records
only, organisation-scoped, write permission required and audited before writes
in accounts mode. They never edit code, approve decisions, resolve identities
or propagate changes. Free text is redacted and bounded. Reuse pins original
and destination IDs/revisions/fingerprints and the author's licence,
attribution, compatibility and validation assessment. Reads flag changed or
unavailable evidence without rewriting history. Uncertain matches are data for
review, never verified lineage. Provenance may name one source repository in
the same selected organisation: creation requires source read access and
destination write access. Every read rechecks source access and hides the
whole record if it is lost, including free text and source identifiers.
An authorised but unavailable source is flagged, never treated as clean.
See `docs/AGENT_GUIDE.md` for contracts and limits.

- `services/web/app/` is the source dir. Pages under `app/pages/repos/[repo]/`.
- All data goes through `useApiGet` / `apiPost` in `app/composables/useApi.ts`, which hit `/api/cg/*`. Never call the Go API from the browser directly.
- No UI or chart libraries; the graph canvas, bars and backdrop are plain canvas/SVG/CSS.
- The design is a calm product UI: neutral surfaces, one brand accent (indigo, `--accent`), status colours only for meaning (`--success`, `--warning`, `--danger`, `--info`), light and dark themes (`<html data-theme>`, from the `cs_theme` cookie; "Auto" follows the system), Inter and JetBrains Mono, plain-language labels. All tokens are CSS variables at the top of `app/assets/css/main.css`, defined for both themes; use them, never raw colours. The old accent names (`--orange`, `--cyan`, …) are aliases for pages not yet reworked. Chart colours (`--s-1`…`--s-4`) are validated per theme with the dataviz palette checks.
- Sign-in, the organisation switcher and route guarding live in `useSession` and `middleware/auth.global.ts`; the API enforces every rule itself, the UI only routes people. Hide operator-only controls (installation tokens, mounted folders) from organisation users.
- Severity colours: `SevBadge` / `.sev--critical|high|medium|low|info`.
- TypeScript is pinned to 5.x because vue-tsc does not support newer releases yet.

## Commits

Conventional commits (`feat(api): ...`, `fix(gitx): ...`). Don't commit `.env`, `sources/`, `bin/` or `services/web/.output`.

After completing each phase, commit the changes, push the branch and open a pull request for that phase.
