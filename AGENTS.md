# AGENTS.md

Instructions for AI coding agents (Claude Code, Codex, Cursor, Copilot and others) working in this repository. Humans should start with `README.md`.

## What this project is

CodeSynapse indexes a repository's Go code, Markdown docs, YAML, Go templates/Helm charts, git history and dependencies (with CVEs) into a Neo4j property graph, then serves small, precise answers from it: symbol cards, file outlines, callers and callees, impact of a diff, a graded security review, metrics and hotspots. The point is that a coding assistant asks the graph instead of reading whole files, which cuts tokens and gives better review context.

Three services plus Neo4j, all in `docker-compose.yml`:

| Service | Code | Port | Purpose |
|---|---|---|---|
| `mcp` | `services/mcp` | 8090 | MCP server for AI agents (stdio or streamable HTTP) |
| `api` | `services/api` | 8080 | JSON API for the dashboard; runs ingest jobs |
| `web` | `services/web` (Nuxt 4) | 3000 | Dashboard; proxies to `api` server-side |
| `neo4j` | image | 7474/7687 | Graph store |

`cmd/codesynapse` is the CLI (ingest, audit, stats, review, mcp install) for CI and local use. It is not a service.

## Use the graph before reading files

If the codesynapse MCP server is connected (see `.mcp.json`), use it on this repo too. Full guide with task prompts: `docs/AGENT_GUIDE.md`.

1. `minimal_context` with the task description first; read only the line ranges it returns.
2. `get_symbol_context` (verbosity "outline", then "full" before editing) for signature, risk, callers, callees, tests.
3. `get_implementations` for interfaces; never grep for interface calls. `get_call_graph` (depth 2) for deeper tracing.
4. `file_outline` before opening a file.
5. `get_pr_context` with your diff before finishing a change. Fix or justify anything it flags as untested, high risk or security-sensitive.
6. Ambiguous names return `qualified_id` suggestions: retry with one of them.

The graph reflects the last ingest. Files you edited in this session may differ.

When you change MCP tools, prompts or the server instructions, update `docs/AGENT_GUIDE.md` in the same change.

## Layout

```
cmd/codesynapse/        CLI: ingest, audit, stats, review, mcp install, schema
services/api/           API service: cmd/codesynapse-api, Dockerfile, and packages only it uses
  internal/api/         HTTP handlers, jobs (ingest/clone/sync), auto-sync scheduler, reviews, dashboard and overview
  internal/llm/         Model providers (OpenAI-compatible, Anthropic SDK), per-task routing, usage
  internal/aitasks/     AI tasks (security triage, review, briefing) that produce reviews
  internal/fixes/       Fix branches for accepted review comments: worktree, model rewrite, push, draft PR
services/mcp/           MCP service: cmd/codesynapse-mcp, Dockerfile
  internal/mcpserver/   MCP tools, prompts (prompts.go) and server instructions
services/web/           Nuxt 4 dashboard (app/ pages+components, server/api/cg proxy, Dockerfile)
deploy/k8s/             Kubernetes manifests (docker-compose.yml stays at the repo root)
internal/model/         Node/Edge kinds, Graph, IDs. The schema lives here.
internal/ingest/        Pipeline: golang/ (go/packages + go/types; security rules in findings.go),
                        lang/ (Python, JS/TS, Vue: pure-Go structural parsers + rules),
                        docs/ (Markdown), config/ (YAML, Go templates, Helm; YAML security rules), githistory/
internal/cleanup/       Liveness (reachability from entry points), unused code/modules/config
internal/vuln/          go.mod and npm/PyPI lockfiles + OSV lookups, called/imported/required reachability
internal/security/      Security report: grading, exposure, prioritisation, Markdown; rule guides (guides.go)
internal/metrics/       Repository metrics and the token benchmark (computed at ingest)
internal/gitx/          Hardened git runner, diff/log parsers, clone/fetch with SSRF checks
internal/redact/        Secret redaction for every stored summary
internal/store/         Store interface; memory/ and neo4jstore/ implementations; storetest/ contract
internal/query/         Composed answers (Outline, Hotspots, Overview, Tests)
internal/agentctx/      Composite agent answers: get_symbol_context, get_call_graph, get_implementations,
                        get_pr_context, minimal_context; qualified_id resolution and suggestions
internal/review/        Diff → impact report, risk scoring, Markdown renderer
internal/agentcfg/      `codesynapse mcp install`: registers the MCP server with local agents
internal/reviews/       Code reviews: model, validation, redaction, comment decisions
config/                 llm.example.yaml (committed); llm.yaml is local and gitignored
internal/app/           Shared wiring: Neo4j flags, bearer auth, HTTP server, cobra CLI bootstrap
internal/logx/          slog logger carried in contexts
internal/testutil/      assert/require helpers for tests
docs/PLAN.md            Architecture, decisions, roadmap
docs/AGENT_GUIDE.md     How agents should use the MCP tools and prompts
rules/                  Security rule catalogue: one YAML per rule (code/, config/), embedded
```

## Commands

```sh
just            # list recipes
just test       # go test -race ./...
just lint       # golangci-lint (must report 0 issues)
just web-check  # nuxt typecheck + build
just ci         # all of the above
just up         # full stack in docker compose (creates .env with random secrets)
just db         # only Neo4j; then just dev-api / just dev-mcp / just dev-web
just test-integration   # store contract tests against the compose Neo4j
just review     # offline review pack for the current branch
just mcp        # register the MCP server with Claude Code, Codex, Gemini, OpenCode, Cursor
```

Run `just test` and `just lint` before you call a Go change done, and `just web-check` for anything under `services/web/`.

## Go conventions

- Go 1.27, module `github.com/parisnakitakejser/codesynapse`.
- Standard library first. Current dependencies: cobra (CLI), the Neo4j driver, the MCP Go SDK, golang.org/x/tools and x/mod. Ask before adding any other dependency.
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

1. Add the kind to `internal/model/model.go` and to `AllNodeKinds`/`AllEdgeKinds`. Those lists are the label allowlist for Cypher.
2. New node fields need entries in `neo4jstore/props.go` (both directions) and `TestPropsRoundTrip`.
3. New `store.Reader` methods must be implemented in `memory` and `neo4jstore` and covered in `storetest.Run`. The two stores must behave identically; the contract test is how we know.
4. Update `docs/PLAN.md` (schema section) and `services/web/app/utils/types.ts` if the API shape changes.

## Security rules (non-negotiable)

This tool reads untrusted repositories and serves their contents to LLMs. Keep it that way:

- **Cypher:** values only through parameters. Labels, relationship types, depth and ORDER BY may be interpolated only from `model` allowlists, `store.ClampDepth`, or the `metricCypher` table. Never build Cypher from request strings.
- **git:** only through `gitx.Run`. It disables fsmonitor, hooks, external diff and textconv, ignores global config, and never uses a shell. Validate user revisions with `gitx.ValidateRev`.
- **go list:** extraction runs with `CGO_ENABLED=0`, `GOTOOLCHAIN=local`, `GOPROXY=off` unless the user opts into network. Do not loosen these defaults.
- **Filesystem:** API ingest is confined to `CODESYNAPSE_SOURCES_ROOT` after symlink resolution (`insideSources`). Docs walking never follows symlinks.
- **No source bodies or YAML values in the graph.** Store hashes, signatures, key paths, line ranges and short summaries. Every summary passes through `redact.Summary`. Author e-mails are stored only as hashes. Security findings describe the problem, never the secret.
- **Remote git:** only `gitx.Clone`/`FetchCheckout`/`RemoteBranches`. Remotes go through `gitx.ValidateRemote` (https only, no credentials in URLs, no private/loopback hosts unless `CODESYNAPSE_ALLOW_PRIVATE_GIT`). Tokens are passed as `GIT_CONFIG_*` environment, never argv, and only to their configured host.
- **Vulnerability data:** only module paths and versions leave the machine (osv.dev). Keep it that way.
- **MCP tools are read-only** and annotated `ReadOnlyHint`, with one exception: the review tools (`submit_review`, `update_review_comment`) write to the review store only (`store.ReviewStore`, `:Review` nodes), never to code or the graph. Review text is size-limited and secret-redacted (`reviews.Prepare`), file paths are repo-relative with no `..`, and `--allow-reviews=false` disables them. There is deliberately no raw-Cypher tool (read transactions still allow `LOAD CSV`, an SSRF and exfiltration path). Do not add other write tools without discussing it first.
- **Destructive API actions** (delete repository, delete review) are confirmed in the UI; deleting cloned files is limited to a single directory inside the workspace, and mounted sources are never deleted.
- **Network services** refuse to bind a non-loopback address without a bearer token (`app.ErrTokenRequired`). The web app keeps the API token server-side, and its proxy rejects cross-origin mutating requests.
- **Limits everywhere:** `store.MaxLimit`, `store.MaxDepth`, request body caps, query timeouts. New endpoints keep them.
- **Pushing:** only `services/api/internal/fixes` pushes, only when `CODESYNAPSE_ALLOW_PUSH=true`, only through `gitx.PushBranch` (new `codesynapse/*` branches, never forced) from a separate worktree, and never edits CI or git configuration (`fixes.protected`). Keep those rails.
- **LLM calls:** only through `services/api/internal/llm`. Model output is untrusted: parse it defensively, validate enums and file paths, redact before storing. Never send source to a non-local provider except a diff the user asked to review. API keys come from `api_key_env`, never from `config/llm.yaml` itself.
- Secrets come from the environment (`CODESYNAPSE_NEO4J_PASSWORD`, `*_TOKEN`), never flags, never logs.

## Web (Nuxt 4)

- `services/web/app/` is the source dir. Pages under `app/pages/repos/[repo]/`.
- All data goes through `useApiGet` / `apiPost` in `app/composables/useApi.ts`, which hit `/api/cg/*`. Never call the Go API from the browser directly.
- No UI or chart libraries; the graph canvas, bars and backdrop are plain canvas/SVG/CSS.
- The design follows Astrona's "mission control" style: dark violet-black only, Space Grotesk + JetBrains Mono, glass sidebar and topbars (`PageHeader`), neon accents with one job each (orange = primary/action, green = success, cyan = info, pink = error/critical, amber = warning, violet = labels), mono uppercase micro-labels (`.eyebrow`, `th`), dashed row and section rules. All tokens are CSS variables at the top of `app/assets/css/main.css`; use them, never raw colours.
- Severity colours: `SevBadge` / `.sev--critical|high|medium|low|info`.
- TypeScript is pinned to 5.x because vue-tsc does not support newer releases yet.

## Commits

Conventional commits (`feat(api): ...`, `fix(gitx): ...`). Don't commit `.env`, `sources/`, `bin/` or `services/web/.output`.
