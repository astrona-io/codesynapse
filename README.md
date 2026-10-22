# CodeSynapse

A code graph for security reviews, code quality and AI coding assistants, backed by Neo4j.

CodeSynapse indexes a repository into one graph: Go code (type-checked, so calls and interface dispatch are exact), Python, JavaScript, TypeScript and Vue, Markdown docs, YAML (Kubernetes, compose, CI, Helm values), Go templates and Helm charts, git history, and Go/npm/PyPI dependencies with their known CVEs. Agents query it over MCP and get answers like "here is `ReplaceRepo`: signature, location, 3 callers, 2 tests that reach it, the doc section that describes it" in a few hundred tokens, instead of opening five files. On this repository the context an agent needs per question is reduced by a median 92% (measured by `codesynapse stats`).

The web dashboard shows the same graph to humans: an operator overview across all repositories, a graded security review, an interactive graph explorer, hotspots, metrics, docs and diff impact.

## Quick start (docker compose)

Requires [just](https://github.com/casey/just) and Docker or Podman (`podman machine start` plus `docker-compose` on the PATH). The justfile picks whichever is installed; set `COMPOSE="docker compose"` to force one.

```sh
just up                       # writes .env with random secrets, builds and starts everything
just ingest ~/src/my-service  # copies the checkout into ./sources and indexes it
just mcp                      # register the MCP server with your agents (asks per agent)
open http://localhost:3000
```

Or use the dashboard: **Clone by URL** adds a GitHub/GitLab repository, **Organisations** watches a whole GitHub organisation (`astrona-io/*`), **Sources** lists everything under `./sources`, and **Sync** on a repository fetches the chosen branch and re-indexes it.

| URL | What |
|---|---|
| http://localhost:3000 | Dashboard |
| http://localhost:8080 | API (bearer token: `CODESYNAPSE_API_TOKEN` in `.env`) |
| http://localhost:8090/mcp | MCP over streamable HTTP (bearer token: `CODESYNAPSE_MCP_TOKEN`) |
| http://localhost:7474 | Neo4j browser (user `neo4j`, password `NEO4J_PASSWORD`) |

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

Only module paths and versions are sent to osv.dev. Set `--vulns=false` (or `CODESYNAPSE_VULN_SCAN=false`) to stay offline.

### Dependency licences

`codesynapse licenses` (and the **Licences** page, and the `license_report` MCP tool) checks every Go, npm and PyPI dependency's licence against the repository's own and explains each verdict: GPL or AGPL in an Apache/MIT project is a conflict, MPL/LGPL or an unknown licence needs a review, development-only dependencies are flagged but not blocking. Licences are read from lockfiles, `vendor/`, the Go module cache, `node_modules` and `site-packages`; `--license-lookup` (or `CODESYNAPSE_LICENSE_LOOKUP=true`) asks deps.dev for the rest, sending only package names and versions.

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

## Code reviews

Ask your agent to review a change ("review my changes with CodeSynapse and submit the review", or the `code-review` MCP prompt). It analyses the change with the graph and calls `submit_review`; the review appears on the **Reviews** page with a verdict, a summary and comments on exact lines, each with severity, category, the symbol it points at and a suggested fix. Resolve comments in the UI, or let the agent mark them fixed with `update_review_comment`. These are the only MCP tools that write, and they write only to the review store (disable with `CODESYNAPSE_MCP_REVIEWS=false`).

Every comment needs a decision: **Accept** (agreed, to fix), **Reject** (disagree or false positive, with an optional reason), **Defer** (later) or **Mark fixed**. The Reviews page shows progress and tabs for To decide, To fix, Later and Done; agents record the same decisions with `update_review_comment` and read the to-do list with `get_review` status "accepted".

**Fix branches.** For an accepted comment, **Create fix branch & PR** asks the model routed to "Fixes" to rewrite the affected file, commits it to a new `codesynapse/fix-…` branch, pushes it and opens a draft pull request, so your CI/CD pipeline runs on it. You review and merge the PR, then mark the comment fixed. "Fix all on one branch" does the same for every accepted item. It is off by default:

```sh
# .env
CODESYNAPSE_ALLOW_PUSH=true
```

and needs a token (`just git-token`) with **Contents** and **Pull requests** read and write. Safety rails: work happens in a separate git worktree (the indexed checkout never changes); only files named in the comments may change; CI definitions (`.github/`, `.gitlab-ci.yml`, Jenkinsfile, …) and git config are never touched; pushes go only to new `codesynapse/*` branches and are never forced; answers that are empty, unchanged or wildly different in size are refused. The fix needs source, so route "Fixes" to a local model (remote providers need `send_source: true`). Workflows that run on every push run before anyone reviews the change; if that matters, limit them to pull requests.

## AI models (local or cloud)

CodeSynapse can ask a model to triage security findings, review changes and write briefings. Providers live in `config/llm.yaml` (copy `config/llm.example.yaml`; it is gitignored):

- `kind: openai` covers anything with an OpenAI-compatible `/v1/chat/completions`: MLX (`mlx_lm.server`), Ollama, vLLM, LM Studio, llama.cpp, OpenRouter. An API key is optional (`api_key_env` names the environment variable).
- `kind: anthropic` uses the official SDK; the key comes from `ANTHROPIC_API_KEY`.
- The compose API container reaches services on your Mac through `host.docker.internal` (for example `http://host.docker.internal:9080/v1`).
- `disable_thinking: true` turns off reasoning for Qwen-style models, which otherwise spend the whole output budget thinking.
- Set `context_window` to the model's real context (for MLX models it is `max_position_embeddings` in the model's `config.json`). CodeSynapse then gives each request everything up to the window minus `max_output_tokens`, and the amount of work per request grows with it: more findings per triage batch, more changed symbols and longer code excerpts per review. Without `context_window` it stays at a safe 32k in / 8k out. Lower `max_input_tokens` if answers get too slow; the timeout scales with the budget.

On the **AI models** page you pick a **global** model, the fallback for everything, and optionally a different one per area: code review, security analysis and briefings. The choice is saved in `workspace/.codesynapse/llm-routes.json`. Remote providers never receive source code beyond the diff you ask them to review; local providers also get short snippets around findings. Model answers are treated as untrusted text and redacted before they are stored.

**Background reviews:** in a repository's auto-sync settings, turn on "AI-review new commits" and "AI-triage new findings". After each successful sync (manual or scheduled) CodeSynapse reviews the commits that arrived and triages security findings of medium severity or above that are new since the previous sync. Results appear as reviews waiting for your decisions.

## Keeping repositories up to date

- **Sync** fetches the chosen branch (cloned repositories) or re-indexes the folder (mounted sources). The branch picker lists every branch of the remote (cloned) or of the checkout (mounted; only the checked-out one can be synced).
- **Auto-sync** runs **every 30 minutes by default** for every repository. The countdown starts when a sync finishes, and first runs are spread over the interval, so repositories do not all sync together. Per repository you can switch it off, or pick 5 or 15 minutes, hourly, every 6 hours or daily, with pause/resume. Status ("next in 3 min", "paused") shows on the repository card and summary, and the sidebar lists the most recently indexed repositories first and refreshes by itself. When the provider confirms (a clean 404 from its API, three scheduled syncs in a row) that a cloned repository no longer exists, its graph, schedule and clone are removed automatically. Anything unclear (network errors, rate limits, a refused request) never counts, and mounted folders are never removed.
- **Organisations:** on the **Organisations** page enter `astrona-io` (or `https://github.com/astrona-io/*`). GitHub (also Enterprise Server), **Forgejo/Gitea/Codeberg** and **GitLab** are supported: pick the provider and host, or paste a URL such as `https://codeberg.org/team/*` (github.com, gitlab.com and codeberg.org are recognised on their own). Repositories are named `owner/name` on github.com and `host/owner/name` elsewhere, so the same organisation and repository on two providers are two separate repositories. CodeSynapse lists every repository of that organisation or user, clones and indexes them, and rescans every 5 minutes to pick up new ones. Archived repositories and forks are skipped unless you tick them. With auto-index off it only lists, and you index repositories one by one. Stopping to watch keeps the repositories already indexed.
- **Private repositories:** `just git-token <host>` asks for a token (input hidden), stores it in `.env` as `CODESYNAPSE_GIT_TOKENS` (`host=token,host2=token2`) and restarts the API. Run it once per git host (`just git-token`, `just git-token codeberg.org`, ...). Each token is sent to its own host only. Use a read-only token (fine-grained GitHub token with *Contents* read). The single `CODESYNAPSE_GIT_TOKEN` + `CODESYNAPSE_GIT_TOKEN_HOST` pair still works. Hosts on a private network need `CODESYNAPSE_ALLOW_PRIVATE_GIT=true`; hosts with a port are not supported. Pull requests for fix branches are opened on github.com only; other hosts get the pushed branch.
- **Remove** a repository from its card or summary; a confirmation asks for the name and offers to delete the cloned files too.

## Unused code

`codesynapse cleanup` (and the **Cleanup** page, and the `unused_code` MCP tool) computes reachability from real entry points over calls, interface dispatch and function references, and lists unreachable and test-only functions with a confidence level, files where nothing is live, unused go.mod modules (with the CVEs removing them eliminates), unused Helm values and template defines. Security-relevant dead code comes first. Test helpers and test-support packages are recognised and not counted.

## Connect an AI agent

**Install into your agents.** `just mcp` (`codesynapse mcp install`) finds Claude Code, Codex, Gemini CLI, OpenCode, Cursor and the project `.mcp.json`, and asks for each whether to add CodeSynapse. Codex, Gemini, OpenCode and Cursor read the token from `$CODESYNAPSE_MCP_TOKEN`; Claude Code stores it in its user config (you are told before it does).

**Or by hand.** The stack serves MCP at `http://localhost:8090/mcp` with the bearer token from `.env`:

```sh
just mcp-config      # prints a JSON client config for the running stack
claude mcp add --transport http codesynapse http://localhost:8090/mcp \
  --header "Authorization: Bearer $CODESYNAPSE_MCP_TOKEN"
```

**Without any database** the server can index one directory into memory at startup (stdio). Good for a single project, but it only sees that directory:

```sh
claude mcp add codesynapse -- go run github.com/parisnakitakejser/codesynapse/services/mcp/cmd/codesynapse-mcp@latest serve --offline .
```

This repository's own `.mcp.json` does that, so agents working on CodeSynapse use CodeSynapse. Connected to the **stack** instead, an agent sees every indexed repository, including whole organisations: call `repo_list` (or `search_all_repos`), then pass `repo` to the other tools. Repositories cloned by URL or through an organisation are named `owner/name` (for example `astrona-io/astrona-cli`), so equally named repositories of two organisations never clash; mounted folders and older indexes keep a plain name.

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

Tips: pass `diff` rather than refs whenever you have it, since it works even when the indexed clone is on another commit. Keep `limit` small on reports, and `min_severity` at medium or above for a first pass. The graph reflects the last ingest, so files you edited since may differ; the 30-minute auto-sync keeps it close (`repo_status` tells you when it is stale).

### MCP tools and prompts

`docs/AGENT_GUIDE.md` explains when to use which, with ready-made task prompts. Every tool is read-only except `submit_review` and `update_review_comment`, which write only to the review store (turn them off with `CODESYNAPSE_MCP_REVIEWS=false`), and the opt-in agent-memory tool below.

**Statistics.** The **Statistics** page shows MCP sessions and tool calls per day, the most used tools, clients and repositories, tokens served and an estimate of tokens avoided, agent memory use with a model of what offloading saves, and workspace-wide security, licence, update-automation, review, AI and sync figures. The MCP server records only daily counters (never arguments or results); turn it off with `CODESYNAPSE_MCP_USAGE=false`.

**Agent memory.** Agents can offload durable facts from their context window as triples (`upsert_graph_triples`, or `offload_turns` to let a local model extract them in the background) and recall them in later sessions (`search_graph_nodes`, `query_subgraph`, prompt `context-offload`). Facts are scoped to one indexed repository, merged by name and alias (`me`, `I` and `user` are the same person; `SAME_AS` joins two names), keep history when they change (`valid_from` / `valid_to`), carry a confidence, are redacted, size-limited and tagged with the agent that saved them, and are listed on the **Agent memory** page, where you can delete them. Turn it off with `CODESYNAPSE_ALLOW_MEMORY=false`.

#### Make agents use memory automatically

1. **Nothing to do for the basics.** Memory is on by default, and every agent connected to the MCP server (`just mcp`) receives instructions to recall at the start of a task and save decisions and preferences as they come up.
2. **Repository instructions** (for agents that follow AGENTS.md more closely than server instructions): `codesynapse mcp agents-md ~/src/your-repo` adds a marked memory block to AGENTS.md (and CLAUDE.md if it does not import AGENTS.md). Re-running updates the block; commit it.
3. **Claude Code hooks** (fully automatic recall and offloading): in the CodeSynapse folder run `export CODESYNAPSE_API_TOKEN=$(sed -n 's/^CODESYNAPSE_API_TOKEN=//p' .env)`, then `codesynapse mcp hooks ~/src/your-repo`. At session start the saved facts are put into context; before Claude Code compacts, turns older than the last 10 exchanges go to background extraction. Hooks go to `.claude/settings.local.json` (use `--shared` for the committed `settings.json`); remove them there to turn them off.
4. **Background extraction and recall by meaning need local models:** route the "memory" task to a local provider (Ollama, MLX, LM Studio) on the **AI models** page, and the "embeddings" task to a local provider with an `embedding_model` (e.g. Ollama's `nomic-embed-text`; see `config/llm.example.yaml`). Without them, `offload_turns` explains what is missing, agents save triples themselves, and `recall_context` matches by name only.

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
| `code-review` | Review a change or area and submit it so it appears in the UI |
| `security-triage` | Triage a repository's findings yourself and submit the result |
| `briefing` | Write a short repository briefing and submit it as a report |

**Use your own agent instead of an AI provider.** Security triage, reviews and briefings can also be run by the server with a configured model (see below). With `security-triage`, `code-review` and `briefing` your local agent does the work and submits it with `submit_review`, so no external provider is called and no key is needed. The result looks like any other review in the UI.

## CLI

```sh
go install github.com/parisnakitakejser/codesynapse/cmd/codesynapse@latest

codesynapse audit .                      # security review (offline graph)
codesynapse cleanup .                    # unused code, files, modules (offline graph)
codesynapse stats .                      # metrics + token benchmark (offline graph)
codesynapse review . --base origin/main --offline   # review pack for the current branch
git diff | codesynapse review --diff - --offline -o json

export CODESYNAPSE_NEO4J_PASSWORD=...
codesynapse ingest . --repo my-service --vulns      # index into Neo4j
```

## Local development

```sh
just db        # Neo4j only
just dev-api   # API on :8080 against ./sources
just dev-web   # Nuxt dev server on :3000
just ci        # lint, race tests, web typecheck + build
```

The code is split by service: `services/api`, `services/mcp` and `services/web`, with the shared core in `internal/` and the CLI in `cmd/codesynapse`.

## Container images and Kubernetes

```sh
just image-build v0.1.0     # api, mcp and web images (PLATFORM=linux/amd64, REGISTRY=ghcr.io/you)
just image-push v0.1.0
just k8s-secrets prod       # writes deploy/k8s/overlays/prod/secrets.env (git-ignored)
just k8s-render prod        # review the manifests
just k8s-apply prod
```

`deploy/k8s` holds Kustomize manifests: a `base` (Neo4j StatefulSet, api, mcp, web, network policies) and `dev` and `prod` overlays. Test on a throwaway kind cluster first (`just image-load-kind`). See `deploy/k8s/README.md`.

See `AGENTS.md` for conventions and security rules and `docs/PLAN.md` for architecture and roadmap.

## Security model, briefly

Indexing untrusted code is the normal case, so: `go list` runs without cgo, toolchain downloads or network; git runs with hooks, fsmonitor and diff drivers disabled, and clones only over HTTPS to public hosts; no source bodies or YAML values are stored and summaries are redacted for secrets; services require bearer tokens off loopback; MCP tools cannot change code or the graph (only the review store). Details in `AGENTS.md`.

## License

Copyright 2026 Astrona.io. Licensed under the [Apache License, Version 2.0](LICENSE); see [NOTICE](NOTICE) for attributions.

All Go and production npm dependencies use permissive licences (Apache-2.0, MIT, BSD, ISC). Neo4j Community Edition, which the compose stack and Kubernetes manifests pull as a separate image, is licensed under GPL-3.0 and is not part of this distribution.
