# CodeSynapse

A code graph for security reviews, code quality and AI coding assistants, backed by Apache HugeGraph (with Milvus for vector search).

CodeSynapse indexes a repository into one graph: Go code (type-checked, so calls and interface dispatch are exact), Python, JavaScript, TypeScript and Vue, Markdown docs, YAML (Kubernetes, compose, CI, Helm values), Go templates and Helm charts, git history, and Go/npm/PyPI dependencies with their known CVEs. Agents query it over MCP and get answers like "here is `ReplaceRepo`: signature, location, 3 callers, 2 tests that reach it, the doc section that describes it" in a few hundred tokens, instead of opening five files. On this repository the context an agent needs per question is reduced by a median 92% (measured by `codesynapse stats`).

The web dashboard shows the same graph to humans: an operator overview across all repositories, a graded security review, an interactive graph explorer, hotspots, metrics, docs and diff impact.

## Quick start (docker compose)

Requires [just](https://github.com/casey/just) and Docker or Podman (`podman machine start` plus `docker-compose` on the PATH). The justfile picks whichever is installed; set `COMPOSE="docker compose"` to force one.

```sh
just up                       # writes .env with random secrets, builds and starts everything
just ingest ~/src/my-service  # copies the checkout into ./sources and indexes it
just mcp                      # install client config; runtime session setup is also required
open http://localhost:3000
```

The dashboard supports sign-in, governance and repository browsing. Execution controls such as cloning, sync and model tasks now require a registered runtime session. The web proxy does not yet carry that trusted binding, so these controls are unavailable through the dashboard. Use the authenticated API with runtime metadata; offline CLI ingest remains available.

| URL | What |
|---|---|
| http://localhost:3000 | Dashboard |
| http://localhost:8080 | API (bearer token: `CODESYNAPSE_API_TOKEN` in `.env`) |
| http://localhost:8090/mcp | MCP over streamable HTTP (bearer token: `CODESYNAPSE_MCP_TOKEN`) |

All ports bind to 127.0.0.1.

## Security review

`codesynapse audit` (and the **Security** page, and the `security_report` MCP tool) grade a repository A–F and list:

- **Code findings** from type-aware rules: shell and command injection, SQL/Cypher built from strings, disabled TLS verification, old TLS versions, weak hashes and ciphers, hardcoded credentials, `template.HTML` from variables, HTTP servers and clients without timeouts, paths and URLs taken from requests, unbounded request bodies, world-writable files, unverified JWTs, `unsafe`.
- **Configuration findings** in YAML: privileged pods, privilege escalation, host namespaces, `hostPath`, root users, dangerous capabilities, unpinned images, literal secrets, GitHub Actions script injection, `pull_request_target` checkouts, unpinned third-party actions, privileged compose services.
- **Dependency update automation**: repositories with manifests but no Renovate or Dependabot config, and Renovate configs that cannot be parsed, are disabled, turn off vulnerability alerts, automerge without `minimumReleaseAge`, or leave actions and images unpinned by digest.
- **Vulnerable dependencies** from go.mod and the Go standard library, checked against osv.dev (Go vulnerability DB + GitHub advisories): CVE, severity, CWE, fixed version, and reachability: *called* (your code calls the vulnerable symbol, with call sites), *imported*, or *required*.

Each finding has a severity (info, low, medium, high, critical), its CWE, its location and owning function, whether it is **exposed** (reachable from `main`, an exported API or a handler) and whether a test reaches it. Priority combines all of these.

```sh
codesynapse audit .                  # Markdown report
codesynapse audit . --fail-on high   # CI gate: exit 1 on high/critical (reachable vulns only)
codesynapse audit . -o json
```

Every report says **what the rules checked** (code files per language, YAML files, Kubernetes resources, Helm templates, workflows, Compose files, dependencies) and **what they did not** (YAML that does not parse, Helm templates whose output is not YAML, and file kinds without rules yet such as Dockerfiles, Terraform or shell scripts), plus the commit it describes. Helm templates are checked on their literal values (`privileged: true`, `hostPath`, `image: x:latest`); values set through `{{ .Values }}` cannot be judged without rendering. A repository where nothing is covered gets **N/A** instead of a grade.

Only module paths and versions are sent to osv.dev. Set `--vulns=false` (or `CODESYNAPSE_VULN_SCAN=false`) to stay offline.

### Dependency licences

`codesynapse licenses` (and the **Licences** page, and the `license_report` MCP tool) checks every Go, npm and PyPI dependency's licence against the repository's own and explains each verdict: GPL or AGPL in an Apache/MIT project is a conflict, MPL/LGPL or an unknown licence needs a review, development-only dependencies are flagged but not blocking. Licences are read from lockfiles, `vendor/`, the Go module cache, `node_modules` and `site-packages`; `--license-lookup` (or `CODESYNAPSE_LICENSE_LOOKUP=true`) asks deps.dev for the rest, sending only package names and versions.

Go calls through function values (handler tables, injected functions) have no typed edge. `--dataflow` (or `CODESYNAPSE_INGEST_DATAFLOW=true`) runs an optional SSA/VTA analysis that links them, marked as `dataflow` evidence; it makes Go extraction about 40% slower and finds only targets declared in the repository. See [edge evidence](docs/EDGE_EVIDENCE.md).

```sh
codesynapse licenses .                                        # Markdown report
codesynapse licenses . --license-lookup --fail-on conflict    # CI gate
```

Every rule is a readable YAML file in [`rules/`](rules/README.md) with why it matters, how to fix it and vulnerable → fixed examples per language. They show up as **How to fix** on each finding, through the `rule_guide` MCP tool and at `GET /api/v1/rules`. `just rules` lists them; adding a rule is a YAML file plus a detector.

### Languages

| | Symbols & calls | Tests | Security rules | Dependency CVEs |
|---|---|---|---|---|
| Go | type-checked (go/types), interface dispatch | `_test.go` | ~20 rules | go.mod + stdlib, symbol-level reachability |
| Python | structural parser, imports, `self.` calls | pytest files/functions | shell, eval, pickle/yaml, SQL, TLS, JWT, secrets, debug | requirements*.txt, poetry.lock, uv.lock, Pipfile.lock |
| JavaScript / TypeScript / Vue | structural parser, ES imports/require, `this.` calls | `*.test.*`, `*.spec.*`, `__tests__` | child_process, eval, DOM XSS, `v-html`, SQL, TLS, JWT, secrets, redirects | package-lock.json, yarn.lock, pnpm-lock.yaml |
| YAML, Helm, Go templates | resources, key paths, `include`/`.Values` links | – | Kubernetes, compose, GitHub Actions, Renovate/Dependabot | – |

Non-Go call edges are name-based (imports, enclosing class, names unique in the repo), so they can miss dynamic dispatch; Go keeps exact type-checked edges. Parsers are pure Go: no cgo, static binaries.

## Repository indexing policy

An optional `.codesynapse.yaml` at a repository root requests path exclusions, Kubernetes scanner controls, security exceptions and local AI annotation. Validate it before approving it:

```sh
codesynapse config inspect /path/to/repository
# Review the normalized configuration and copy its exact digest:
codesynapse ingest /path/to/repository --approved-policy-digest '<reviewed-digest>'
```

With accounts, a repository administrator approves the configuration on **Indexing policy**, with a reason and optional expiry. Exclusions need approval from every organisation sharing the graph. Security exceptions apply only to the approving organisation; the raw findings remain available. Annotation additionally needs the local `code-annotation` model route and a registered runtime session. See the [configuration reference](docs/REPO_CONFIG_PLAN.md) for the YAML schema and API workflow.

**Ingestion reports** shows the latest attempt and last successful run separately, with per-file outcomes, scanner exclusions and graph publication status. The read-only MCP tool `ingestion_report` returns the same summaries. File metrics come from durable accounting, so restarts and recovery do not count a write twice. Savings are metadata-based estimates with coverage shown; unknown files beneath pruned directories are excluded. See [ingestion accounting](docs/INGEST_TRANSPARENCY_PLAN.md) for limits and recovery behavior.

The repository and Go module now use `github.com/astrona-io/codesynapse`; container image references use `ghcr.io/astrona-io`. Update existing clones with `git remote set-url origin git@github.com:astrona-io/codesynapse.git`. Consumers of the Go module must update their imports.

## Code reviews

Ask your agent to review a change ("review my changes with CodeSynapse and submit the review", or the `code-review` MCP prompt). It analyses the change with the graph and calls `submit_review`; the review appears on the **Reviews** page with a verdict, a summary and comments on exact lines, each with severity, category, the symbol it points at and a suggested fix. Resolve comments in the UI, or let the agent mark them fixed with `update_review_comment`. These review tools write only to the review store (disable with `CODESYNAPSE_MCP_REVIEWS=false`).

Every comment needs a decision: **Accept** (agreed, to fix), **Reject** (disagree or false positive, with an optional reason), **Defer** (later) or **Mark fixed**. The Reviews page shows progress and tabs for To decide, To fix, Later and Done; agents record the same decisions with `update_review_comment` and read the to-do list with `get_review` status "accepted".

**Prompt budgets.** Every completion checks its final redacted provider request before sending. Input counts are conservative estimates, separate from billed usage; oversized requests are refused. `input_budget_policy: require_exact` refuses calls until a verified model/template counter is available. See [configuration and limits](docs/PROMPT_BUDGETS.md).

**Fix branches.** For an accepted comment on a Go function or method, **Create fix branch & PR** asks the local model routed to "Fixes" to rewrite the affected file, commits it to a new `codesynapse/fix-…` branch, pushes it and opens a draft pull request, so your CI/CD pipeline runs on it. You review and merge the PR, then mark the comment fixed. "Fix all on one branch" does the same for every accepted item. It is off by default:

```sh
# .env
CODESYNAPSE_ALLOW_PUSH=true
```

and needs a token (`just git-token`) with **Contents** and **Pull requests** read and write. Safety rails: work happens in a separate git worktree (the indexed checkout never changes); only files named in the comments may change; CI definitions (`.github/`, `.gitlab-ci.yml`, Jenkinsfile, …) and git config are never touched; pushes go only to new `codesynapse/*` branches and are never forced; the review must pin the current commit, and the response must contain exactly one bounded Go code block. Go syntax is checked, and only the accepted comments’ function bodies may change; signatures, imports, headers and other bytes are preserved. Revision, comment, permission, file-hash and commit checks run before publication. Other edits need manual handling. Route "Fixes" to a local model; `send_source: true` does not permit cloud fixes. See [fix validation and retry rules](docs/FIX_VALIDATION.md). Workflows that run on every push run before anyone reviews the change; if that matters, limit them to pull requests.

## AI models (local or cloud)

CodeSynapse can ask a model to triage security findings, review changes and write briefings. Providers live in `config/llm.yaml` (copy `config/llm.example.yaml`; it is gitignored):

- `kind: openai` covers anything with an OpenAI-compatible `/v1/chat/completions`: MLX (`mlx_lm.server`), Ollama, vLLM, LM Studio, llama.cpp, OpenRouter. An API key is optional (`api_key_env` names the environment variable).
- `kind: anthropic` uses the official SDK; the key comes from `ANTHROPIC_API_KEY`.
- The compose API container reaches services on your Mac through `host.docker.internal` (for example `http://host.docker.internal:9080/v1`).
- `disable_thinking: true` turns off reasoning for Qwen-style models, which otherwise spend the whole output budget thinking.
- Set `context_window` to the model's real context (for MLX models it is `max_position_embeddings` in the model's `config.json`). CodeSynapse then gives each request everything up to the window minus `max_output_tokens`, and the amount of work per request grows with it: more findings per triage batch, more changed symbols and longer code excerpts per review. Without `context_window` it stays at a safe 32k in / 8k out. Lower `max_input_tokens` if answers get too slow; the timeout scales with the budget.

On the **AI models** page you pick a **global** model, the fallback for everything, and optionally a different one per area: code review, security analysis and briefings. The choice is saved in `workspace/.codesynapse/llm-routes.json`. Remote providers never receive source code beyond the diff you ask them to review; local providers also get short snippets around findings. Model answers are treated as untrusted text and redacted before they are stored.

**Background reviews:** in a repository's auto-sync settings, turn on "AI-review new commits" and "AI-triage new findings". After each successful sync (manual or scheduled) CodeSynapse reviews the commits that arrived and triages security findings of medium severity or above that are new since the previous sync. Results appear as reviews waiting for your decisions.

## What needs attention

The **Overview** page ranks what to fix across your repositories, worst first: vulnerable dependencies your code calls, critical and high security findings, licence conflicts, private repositories that lost access, failing syncs, repositories no security check covers, stale indexes and missing Renovate or Dependabot. Every item explains why it matters and links to where it is fixed; each repository's Summary shows its own list. The **Dependencies** page of a repository lists every package with its licence, known vulnerabilities (and whether your code calls them) and update automation.

## Live statistics (by the minute and hour)

The compose stack runs [VictoriaMetrics](https://victoriametrics.com/) (Apache-2.0). The API and MCP services push metrics to it every 15 seconds, and the **Statistics** page charts them per minute, 5 or 15 minutes, hour or 6 hours (`/stats?range=1h|6h|24h|7d|30d`):

- **Tokens and model cost:** provider-reported input, output and cached tokens, estimated spend and prompt-cache savings, and calls with missing usage or prices.
- **Network and cloud bandwidth:** MCP request/response payload bytes and cloud model request bytes. Avoided cloud traffic stays “Not measured” until an equivalent fallback provides a comparable baseline.
- **Database performance:** request volume, latency percentiles, errors/timeouts, graph rows returned and vector matches. Returned rows do not measure internal nodes traversed.
- **Agent memory and offloading:** current durable facts, recalls, saved-fact operations, offloaded turns and MCP activity. Fact counts refresh every 30 seconds, deduplicate API replicas and disappear from the chart after 90 seconds without a successful observation.

Ordinary panels show your organisation's activity, including when you are a platform administrator. The Administration area has separate platform-wide panels. Metrics never contain prompts, queries, file paths, repository names or user/session IDs. Missing model prices mean unknown cost; cached tokens may still be billed. Application payload bytes exclude HTTP headers, TLS and TCP overhead.

Metrics use OpenTelemetry cumulative OTLP/HTTP by default. Set `CODESYNAPSE_METRICS_URL` to the internal VictoriaMetrics base URL; the exporter appends `/opentelemetry/v1/metrics`. Authentication uses `CODESYNAPSE_METRICS_USER` and `CODESYNAPSE_METRICS_PASSWORD` from the environment. `CODESYNAPSE_ENVIRONMENT` optionally identifies the deployment environment, otherwise `unknown` is used. Each process has a separate opaque instance ID so replicas and restarts do not share a cumulative stream; this ID is unrelated to agent sessions.

`CODESYNAPSE_METRICS_EXPORTER=prometheus` selects the legacy Prometheus-text push path for rollback; the default is `otlp`. Restart the services when switching and use one exporter per process. No scrape endpoint is exposed. Failed exports do not block requests, and shutdown flushing has a five-second deadline. Upgrading: `just init` adds `METRICS_PASSWORD` to `.env` for the Compose stack. Without `CODESYNAPSE_METRICS_URL`, metrics export is disabled and the Statistics page explains how to enable it. Kubernetes configuration remains deferred to the final deployment phase.

## Keeping repositories up to date

- **Sync** fetches the chosen branch (cloned repositories) or re-indexes the folder (mounted sources). The branch picker lists every branch of the remote (cloned) or of the checkout (mounted; only the checked-out one can be synced).
- **Auto-sync** runs **every 30 minutes by default** for every repository. The countdown starts when a sync finishes, and first runs are spread over the interval, so repositories do not all sync together. Per repository you can switch it off, or pick 5 or 15 minutes, hourly, every 6 hours or daily, with pause/resume. Status ("next in 3 min", "paused") shows on the repository card and summary, and the sidebar lists the most recently indexed repositories first and refreshes by itself. When the provider confirms (a clean 404 from its API, three scheduled syncs in a row) that a cloned repository no longer exists, its graph, schedule and clone are removed automatically. Anything unclear (network errors, rate limits, a refused request) never counts, and mounted folders are never removed.
- **Organisations:** on the **Organisations** page enter `astrona-io` (or `https://github.com/astrona-io/*`). GitHub (also Enterprise Server), **Forgejo/Gitea/Codeberg** and **GitLab** are supported: pick the provider and host, or paste a URL such as `https://codeberg.org/team/*` (github.com, gitlab.com and codeberg.org are recognised on their own). Repositories are named `owner/name` on github.com and `host/owner/name` elsewhere, so the same organisation and repository on two providers are two separate repositories. CodeSynapse lists every repository of that organisation or user, clones and indexes them, and rescans every 5 minutes to pick up new ones. Archived repositories and forks are skipped unless you tick them. With auto-index off it only lists, and you index repositories one by one. Stopping to watch keeps the repositories already indexed.
- **Private repositories:** `just git-token <host>` asks for a token (input hidden), stores it in `.env` as `CODESYNAPSE_GIT_TOKENS` (`host=token,host2=token2`) and restarts the API. Run it once per git host (`just git-token`, `just git-token codeberg.org`, ...). Each token is sent to its own host only. Use a read-only token (fine-grained GitHub token with *Contents* read). The single `CODESYNAPSE_GIT_TOKEN` + `CODESYNAPSE_GIT_TOKEN_HOST` pair still works. Hosts on a private network need `CODESYNAPSE_ALLOW_PRIVATE_GIT=true`; hosts with a port are not supported. Pull requests for fix branches are opened on github.com only; other hosts get the pushed branch.
- **Remove** a repository from its card or summary; a confirmation asks for the name and offers to delete the cloned files too.

## Unused code

`codesynapse cleanup` (and the **Cleanup** page, and the `unused_code` MCP tool) computes reachability from real entry points over calls, interface dispatch and function references, and lists unreachable and test-only functions with a confidence level, files where nothing is live, unused go.mod modules (with the CVEs removing them eliminates), unused Helm values and template defines. Security-relevant dead code comes first. Test helpers and test-support packages are recognised and not counted.

## Connect an AI agent

MCP tool execution requires PostgreSQL accounts, a personal organisation API key and an immutable chat ID supplied by the calling runtime. The installation token alone cannot execute tools. Offline CLI indexing still works without accounts; MCP tools fail closed without the execution-session registry.

`just mcp` (`codesynapse mcp install`) installs connection settings for detected clients. It does not supply a chat identity or register a session. Before the first tool call, a runtime-aware launcher or client integration must:

1. Read the existing thread ID from its runtime and register it with `POST /api/v1/execution/sessions`, using the personal key and body `{"session_id":"<runtime-provided thread ID>"}`.
2. Send that same value in both `X-CodeSynapse-Session` and `X-CodeSynapse-Parent-Session` on every MCP HTTP request, together with `Authorization: Bearer <personal key>`.
3. Keep the identity unchanged across reconnects, compaction and delegated work. Each new chat needs its own runtime-provided ID. Do not generate an ID inside a tool, substitute an MCP transport ID or share one static value across chats.

The checked-in `.mcp.json` connects to `http://localhost:8090/mcp` and contains environment placeholders for `CODESYNAPSE_MCP_TOKEN`, `CODESYNAPSE_SESSION_ID` and `CODESYNAPSE_PARENT_SESSION_ID`. A launcher must supply these before the client reads its configuration, after registration. Placeholder syntax and per-chat headers vary by client; the template is not a verified integration for every client. It launches no local process or Python helper.

**Codex:** the installed CLI's `mcp add` supports the URL and bearer-token environment variable but has no HTTP header setter. Its installer therefore does not complete runtime-header setup. Configure headers through a supported client configuration or runtime integration and verify that they reach the server before using tools. See the [runtime contract and client limitations](docs/AGENT_GUIDE.md#runtime-session-setup).

**Stdio:** launch one MCP server process per chat, with `CODESYNAPSE_DATABASE_URL`, the personal key in `CODESYNAPSE_API_TOKEN`, and the already-registered primary and parent IDs in `CODESYNAPSE_SESSION_ID` and `CODESYNAPSE_PARENT_SESSION_ID`. Both IDs must match. A process cannot switch to another chat; the server rechecks credentials and registration on each tool call.

Once connected, call `repo_list` or `search_all_repos`, then pass an authorised `repo` to other tools. Cloned repositories use names such as `owner/name`; mounted folders and older indexes can have plain names.

### Working efficiently

The point is to ask the graph instead of reading files. A good order:

1. **Start:** `minimal_context` with the task in a sentence (`max_tokens: 1500`). Read only the line ranges it returns.
2. **Understand:** `get_symbol_context` with `verbosity: "outline"`; switch to `"full"` just before you edit. Use `file_outline` before opening a file, and `search` when you only have a name.
3. **Trace:** `get_call_graph` at `depth: 2` (raise it only if needed, max 4). Use `get_implementations` for interfaces instead of grepping for calls.
4. **Before finishing:** `get_pr_context` with your diff. Fix or justify anything it lists as untested, high risk or security-sensitive.
5. **Retry with an id:** an ambiguous name returns `qualified_id` suggestions; pass one back.

| I want to… | Use |
|---|---|
| Find where to start for a task | `minimal_context` |
| Know what breaks if I change a function | `get_symbol_context` (full), `get_call_graph` (callers) |
| Review what I changed | `get_pr_context` with `diff` (paste `git diff`), then `submit_review` |
| Review a repository that is checked out elsewhere | `get_pr_context` with `diff`; `base`/`head` refs only work for repositories CodeSynapse has cloned or mounted |
| See what is risky in a repository | `security_report` (`min_severity: "medium"`, `limit`), `security_symbols`, `hotspots` |
| Fix a security finding | `rule_guide` with the finding's rule and language |
| Clean up | `unused_code` (`security_only`, then `min_confidence: "high"`) |
| Get oriented in a repository | `repo_overview`, then `repo_metrics` |
| Check the graph is current | `repo_status` |
| Find which repository owns something | `search_all_repos` (`repo_prefix: "astrona-io/"`), then pass `repo` on |
| Read YAML or docs structure without opening files | `get_config_entries`, `get_doc_section` |
| Findings in one directory | `security_report` with `path` |
| Work through accepted review comments | `get_review` with `status: "accepted"`, then `update_review_comment` |

Tips: pass `diff` rather than refs whenever you have it, since it works even when the indexed clone is on another commit. Keep `limit` small on reports, and `min_severity` at medium or above for a first pass. The graph reflects the last ingest, so files you edited since may differ; a schedule with a valid delegated runtime session can refresh it (`repo_status` tells you when it is stale).

### MCP tools and prompts

`docs/AGENT_GUIDE.md` explains when to use which, with ready-made task prompts. Write tools are limited to reviews, explicitly requested durable memory, session offloading and catalog proposals/annotations. They do not edit source code. Review tools can be disabled with `CODESYNAPSE_MCP_REVIEWS=false`; memory is enabled by default and can be disabled with `CODESYNAPSE_ALLOW_MEMORY=false`.

**Statistics.** The **Statistics** page shows MCP sessions and tool calls per day, the most used tools, clients and repositories, tokens served and an estimate of tokens avoided, agent memory use with a model of what offloading saves, and workspace-wide security, licence, update-automation, review, AI and sync figures. The MCP server records only daily counters (never arguments or results); turn it off with `CODESYNAPSE_MCP_USAGE=false`.

**Agent memory has two scopes.** `recall_context` defaults to the current chat's private session memory, and `offload_turns` extracts older turns into that same scope using a local model. Both require the API and validated runtime metadata. Session recall never falls back to repository-wide memory. Sessions expire seven days after registration; reconnecting does not extend them. Revoking a session blocks further work and schedules deletion of its graph facts and vectors.

Durable repository knowledge is shared with authorised readers of that repository in the organisation. Use `upsert_graph_triples` only when the user explicitly asks to retain a fact beyond the chat. Read it with `search_graph_nodes`, `query_subgraph`, or `recall_context` with `memory_scope: "repository"`. The **Agent memory** page displays this durable scope. Existing facts stay in that scope; new writes record their originating session. Selected session facts can be explicitly promoted through the [session-memory API](docs/AGENT_GUIDE.md#session-memory-api).

#### Set up memory and hooks

1. Configure the runtime session binding above. Connected agents receive instructions to recall the current chat and offload older turns; those instructions do not supply authentication or runtime metadata.
2. Run `codesynapse mcp agents-md ~/src/your-repo` to add the same memory rules to AGENTS.md and, where applicable, CLAUDE.md. Re-running updates the marked block.
3. Supply a personal key in `CODESYNAPSE_API_TOKEN`, then run `codesynapse mcp hooks ~/src/your-repo` to install Claude Code hooks. The SessionStart and PreCompact hook payloads must contain the runtime's `session_id`. Hooks register that supplied ID and send both identity headers when recalling or offloading through the API. They report an error and skip work if the identity is missing or closed.
4. Route the `memory` task to a local provider for extraction and the `embeddings` task to a local provider with an `embedding_model`; see `config/llm.example.yaml`. Without embeddings, session recall can use lexical search through the API. There is no anonymous background embedding worker: session indexing follows authorised writes, and durable indexing uses `POST /api/v1/repos/{repo}/memory/index` with the caller's runtime context. MCP durable writes request this indexing when the API is configured; failed indexing leaves lexical retrieval available.

Hooks cannot change the environment or HTTP headers of an already-running MCP client. Installing them does not complete MCP runtime integration. They recall at session start and after compaction, and offload turns older than the last 10 exchanges before compaction. The installer also sets `CLAUDE_AUTOCOMPACT_PCT_OVERRIDE=30`; `--compact-at 0` leaves the client's setting unchanged. Hooks cannot remove messages or force compaction. Their configuration lives in `.claude/settings.local.json`, or `.claude/settings.json` with `--shared`.

| Tool | Returns |
|---|---|
| `minimal_context` | For a task description: the most relevant symbols, doc sections and config entries with line ranges, under a token budget |
| `get_symbol_context` | One symbol or package: signature, summary, risk, callers, callees, tests, implementations, findings (`verbosity` outline/full) |
| `get_call_graph` | Callers/callees up to depth 4 with edges and entry points |
| `get_implementations` | Interface ↔ implementation resolution via `IMPLEMENTS` / `DISPATCHES_TO` |
| `get_pr_context` | Diff, files or base/head → risk, blast radius, tests, findings, untested exported symbols |
| `file_outline` | Symbols, doc sections or YAML entries in a file with line ranges |
| `search` | Anything by name |
| `security_report` / `security_symbols` | Graded findings and vulnerable dependencies; functions calling sensitive APIs |
| `rule_guide` | Why a rule matters, the fix, and vulnerable → fixed code examples |
| `unused_code` | Unused functions, files, modules and Helm values to remove |
| `repo_list` / `repo_overview` / `repo_metrics` / `hotspots` / `file_history` | Orientation, metrics, churn |
| `repo_status` | How fresh the graph is: indexed commit and time, stale flag, and whether the checkout has newer commits or local changes |
| `search_all_repos` | Search every repository (or those with a `repo_prefix` such as `astrona-io/`) at once; a few hits each, with their `repo` |
| `get_config_entries` | Kubernetes, Helm, compose and CI YAML entries and template definitions by file and/or name, with line ranges |
| `get_doc_section` | Markdown sections by heading or file, with the symbols each mentions and line ranges |
| `submit_review` | Record a review (title, summary, verdict, comments on file:line); `kind: "report"` records a briefing |
| `update_review_comment` | Record a decision on a comment: accepted, rejected, deferred, resolved |
| `list_reviews` / `get_review` | Reviews of a repository; the open or accepted comments of one |

Prompts (`prompts/list`; clients show them as slash commands):

| Prompt | Does |
|---|---|
| `refactor-analysis` | Blast radius, callers and test coverage before you change a symbol |
| `pr-impact-review` | Risk-scored review pack for changed files |
| `package-onboarding` | How a package fits into the codebase |
| `bug-investigation` | Caller chain, interface dispatch and churn around a suspected bug |
| `dependency-review` | Recursive dependency, licence and vulnerability analysis with smaller-alternative checks, with commands for each language and package manager |
| `reuse-first` | Look for existing code (exact search, meaning, duplicates) before implementing, then reuse or extend it |
| `code-review` | Review a change or area and submit it so it appears in the UI |
| `security-triage` | Triage a repository's findings yourself and submit the result |
| `briefing` | Write a short repository briefing and submit it as a report |

**Use your own agent instead of an AI provider.** Security triage, reviews and briefings can also be run by the server with a configured model (see below). With `security-triage`, `code-review` and `briefing` your local agent does the work and submits it with `submit_review`, so no external provider is called and no key is needed. The result looks like any other review in the UI.

## CLI

```sh
go install github.com/astrona-io/codesynapse/cmd/codesynapse@latest

codesynapse audit .                      # security review (offline graph)
codesynapse cleanup .                    # unused code, files, modules (offline graph)
codesynapse stats .                      # metrics + token benchmark (offline graph)
codesynapse review . --base origin/main --offline   # review pack for the current branch
git diff | codesynapse review --diff - --offline -o json

# index into a HugeGraph you can reach (the compose one is internal only)
CODESYNAPSE_HUGEGRAPH_URL=https://graph.example CODESYNAPSE_HUGEGRAPH_USER=admin \
CODESYNAPSE_HUGEGRAPH_PASSWORD=... codesynapse ingest . --repo my-service --vulns
```

## Local development

```sh
just up        # the stack, including HugeGraph and Milvus
just dev-web   # Nuxt dev server on :3000 against the API of `just up`
just dev-mcp   # MCP on stdio with an in-memory graph of the checkout
just ci        # lint, race tests, web typecheck + build
```

HugeGraph runs Gremlin scripts and is never published to the host, so the API and MCP services run in compose; `just dev-mcp` and the `--offline` CLI commands need no database at all.

The code is split by service: `services/api`, `services/mcp` and `services/web`, with the shared core in `internal/` and the CLI in `cmd/codesynapse`.

## Container images and Kubernetes

```sh
just image-build v0.1.0     # api, mcp and web images (PLATFORM=linux/amd64, REGISTRY=ghcr.io/you)
just image-push v0.1.0
just k8s-secrets prod       # writes deploy/k8s/overlays/prod/secrets.env (git-ignored)
just k8s-render prod        # review the manifests
just k8s-apply prod
```

`deploy/k8s` holds Kustomize manifests: a `base` (api, mcp, web, network policies; outdated until HugeGraph and Milvus are added in phase 6a) and `dev` and `prod` overlays. Test on a throwaway kind cluster first (`just image-load-kind`). See `deploy/k8s/README.md`.

See `AGENTS.md` for conventions and security rules and `docs/PLAN.md` for architecture and roadmap.

## Security model, briefly

Indexing untrusted code is the normal case, so: `go list` runs without cgo, toolchain downloads or network; git runs with hooks, fsmonitor and diff drivers disabled, and clones only over HTTPS to public hosts; no source bodies or YAML values are stored and summaries are redacted for secrets; services require bearer tokens off loopback; MCP tools cannot edit source code; their approved writes are confined to reviews, memory and catalog records. Details in `AGENTS.md`.

## Graph database: HugeGraph + Milvus

The graph lives in [Apache HugeGraph](https://hugegraph.apache.org/), with [Milvus](https://milvus.io/) for agent-memory vectors. `just up` starts both; `just init` adds `HUGEGRAPH_PASSWORD` to `.env`.

**Upgrading from a Neo4j installation:** Neo4j support was removed. Graphs are not migrated: after `just up`, sync your repositories again to rebuild them in HugeGraph. The old data stays in the `neo4j-data` volume until you delete it (`docker volume rm codesynapse_neo4j-data`); `CODESYNAPSE_GRAPH_BACKEND`, `COMPOSE_PROFILES=hugegraph` and `NEO4J_*` lines in `.env` are no longer used. A CLI or service still configured with `CODESYNAPSE_GRAPH_BACKEND=neo4j` stops with an error that says so.

Indexing replaces a repository's graph in one HugeGraph transaction, so its old and new edges must fit the server's `edge.tx_capacity` and `vertex.tx_capacity`. HugeGraph's default of 10,000 is too small for most repositories; the compose service sets 1,000,000, the most HugeGraph accepts. If you run your own HugeGraph, set both options on the server and `CODESYNAPSE_HUGEGRAPH_TX_CAPACITY` to the same value for `api` and `mcp`. A replacement that does not fit is refused before anything is written, and the error names the setting. At 1,000,000 that is roughly a repository of 500,000 edges; larger ones cannot be indexed yet.

Both are Apache-2.0, scale out, and keep each organisation's memory and vectors apart in the data itself (tenant partitions in Milvus, `tenant_id` on every memory vertex). HugeGraph runs Groovy scripts, so it always has a password and is never published to the host. `just test-hugegraph` runs the store contract and a parity check against the in-memory reference on throwaway containers.

## Accounts, organisations and audit log

The web app has sign-in, a first-run setup (the first account is the installation's root account, which hands over to an organisation owner), Settings for your profile, API keys, members and Git tokens, an Agents page with client connection settings (runtime session integration is still required), and Help that explains every figure. It follows your system's light or dark mode, or the choice in the sidebar.

Repository access requires explicit organisation membership, including for platform administrators. A direct user grant (read, write, admin, or no access) overrides team grants; otherwise the strongest team grant applies, then the organisation's read-only/no-access default. Guests need a grant. Write permits reviews and memory updates; admin also permits sync, schedules and access management. Settings → Organisation controls the default and owner recovery; Settings → Teams and each repository's Access page manage grants. Changes are audited. See the [upgrade notes](docs/ACCESS_CONTROL_PLAN.md#implementation-and-upgrade) before upgrading an existing accounts database.

With PostgreSQL configured (`CODESYNAPSE_DATABASE_URL`; the compose stack does this), CodeSynapse has user accounts, organisations with roles (owner, admin, member, viewer) and personal API keys (`csk_…`) for agents and the CLI. An agent connected with a personal key and registered runtime session sees only its organisation's repositories and keeps its memory, reviews and statistics separate from other organisations. The first account becomes the installation's root account: it administers the platform (settings, health, people, the audit log) and never does organisation work. Its one organisation task is setup, which creates the first organisation and a separate owner account in one step, then signs root out; the owner signs in for everything else. Set `CODESYNAPSE_SIGNUP_OPEN=true` to let others sign up after setup. On an installation that predates this, the API identifies the root at start-up from its setup history; if that history is missing (for example, pruned by audit retention), platform administrators stay limited to administration until an operator runs `codesynapse-api designate-root --email <root>`. See [root bootstrap](docs/ROOT_BOOTSTRAP_PLAN.md). Every change and security event goes to a tamper-evident audit log (hash chain, `GET /api/v1/admin/audit/verify`), exportable as CSV or JSON Lines and also written to the logs for your SIEM, which is what NIS2-regulated organisations need. Platform admins get an Administration area: system health, every organisation and user, suspending accounts, granting platform admin, and the audit log with filters and export. The direction and remaining phases (privacy masking, hybrid search, Kubernetes production setup, single sign-on) are in [docs/SAAS.md](docs/SAAS.md).

Private repositories: an organisation admin stores a Git token per host (`PUT /api/v1/accounts/orgs/{org}/git-tokens/github.com`; the settings page comes with the new web app). Tokens are encrypted with `SECRET_KEY` from `.env` (keep it: tokens sealed with a lost key have to be set again). The organisation then adds private repositories it can read with its own token; CodeSynapse checks that access again at every sync and hides the repository from an organisation whose token stopped working.

Upgrading an existing install: run `just init` (adds missing keys such as `POSTGRES_PASSWORD` and `SECRET_KEY` to `.env`), then `just up`.

The compose stack runs PostgreSQL 18, which cannot open a data volume written by PostgreSQL 17. If your install predates the upgrade, dump the accounts database with the old image first (`docker compose exec postgres pg_dumpall -U codesynapse > accounts.sql`), remove the `postgres-data` volume, run `just up`, and restore (`docker compose exec -T postgres psql -U codesynapse -d codesynapse < accounts.sql`).

## License

Copyright 2026 Astrona.io. Licensed under the [Apache License, Version 2.0](LICENSE); see [NOTICE](NOTICE) for attributions.

All Go and production npm dependencies use permissive licences (Apache-2.0, MIT, BSD, ISC). Apache HugeGraph and Milvus, which the compose stack pulls as separate images, are Apache-2.0. Both are spoken to over HTTP with the Go standard library, so neither adds Go dependencies.
