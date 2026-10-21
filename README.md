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

Or use the dashboard: **Clone by URL** adds a GitHub/GitLab repository, **Sources** lists everything under `./sources`, and **Sync** on a repository fetches the chosen branch and re-indexes it.

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
- **Vulnerable dependencies** from go.mod and the Go standard library, checked against osv.dev (Go vulnerability DB + GitHub advisories): CVE, severity, CWE, fixed version, and reachability: *called* (your code calls the vulnerable symbol, with call sites), *imported*, or *required*.

Each finding has a severity (info, low, medium, high, critical), its CWE, its location and owning function, whether it is **exposed** (reachable from `main`, an exported API or a handler) and whether a test reaches it. Priority combines all of these.

```sh
codesynapse audit .                  # Markdown report
codesynapse audit . --fail-on high   # CI gate: exit 1 on high/critical (reachable vulns only)
codesynapse audit . -o json
```

Only module paths and versions are sent to osv.dev. Set `--vulns=false` (or `CODESYNAPSE_VULN_SCAN=false`) to stay offline.

Every rule is a readable YAML file in [`rules/`](rules/README.md) with why it matters, how to fix it and vulnerable → fixed examples per language. They show up as **How to fix** on each finding, through the `rule_guide` MCP tool and at `GET /api/v1/rules`. `just rules` lists them; adding a rule is a YAML file plus a detector.

### Languages

| | Symbols & calls | Tests | Security rules | Dependency CVEs |
|---|---|---|---|---|
| Go | type-checked (go/types), interface dispatch | `_test.go` | ~20 rules | go.mod + stdlib, symbol-level reachability |
| Python | structural parser, imports, `self.` calls | pytest files/functions | shell, eval, pickle/yaml, SQL, TLS, JWT, secrets, debug | requirements*.txt, poetry.lock, uv.lock, Pipfile.lock |
| JavaScript / TypeScript / Vue | structural parser, ES imports/require, `this.` calls | `*.test.*`, `*.spec.*`, `__tests__` | child_process, eval, DOM XSS, `v-html`, SQL, TLS, JWT, secrets, redirects | package-lock.json, yarn.lock, pnpm-lock.yaml |
| YAML, Helm, Go templates | resources, key paths, `include`/`.Values` links | – | Kubernetes, compose, GitHub Actions | – |

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

- **Sync** fetches the chosen branch (cloned repositories) or re-indexes the folder (mounted sources).
- **Auto-sync** per repository: off, every 15 minutes, hourly, every 6 hours or daily, with pause/resume. Status ("next in 23 min", "paused") shows on the repository card and summary.
- **Private repositories:** `just git-token` asks for a token (input hidden), stores it in `.env` and restarts the API. Use a fine-grained GitHub token with read-only *Contents* access. The UI shows whether a token is configured, never the token.
- **Remove** a repository from its card or summary; a confirmation asks for the name and offers to delete the cloned files too.

## Unused code

`codesynapse cleanup` (and the **Cleanup** page, and the `unused_code` MCP tool) computes reachability from real entry points over calls, interface dispatch and function references, and lists unreachable and test-only functions with a confidence level, files where nothing is live, unused go.mod modules (with the CVEs removing them eliminates), unused Helm values and template defines. Security-relevant dead code comes first. Test helpers and test-support packages are recognised and not counted.

## Connect an AI agent

`just mcp` (`codesynapse mcp install`) finds Claude Code, Codex, Gemini CLI, OpenCode, Cursor and the project `.mcp.json`, and asks for each whether to add CodeSynapse. Codex, Gemini, OpenCode and Cursor read the token from `$CODESYNAPSE_MCP_TOKEN`; Claude Code stores it in its user config (you are told before it does).

Without any database, the server can index a directory into memory at startup:

```sh
claude mcp add codesynapse -- go run github.com/parisnakitakejser/codesynapse/services/mcp/cmd/codesynapse-mcp@latest serve --offline .
```

This repository's own `.mcp.json` does that, so agents working on CodeSynapse use CodeSynapse.

### MCP tools and prompts

All tools are read-only. `docs/AGENT_GUIDE.md` explains when to use which, with ready-made task prompts.

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

Prompts (`prompts/list`): `refactor-analysis`, `pr-impact-review`, `package-onboarding`, `bug-investigation`.

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

See `AGENTS.md` for conventions and security rules and `docs/PLAN.md` for architecture and roadmap.

## Security model, briefly

Indexing untrusted code is the normal case, so: `go list` runs without cgo, toolchain downloads or network; git runs with hooks, fsmonitor and diff drivers disabled, and clones only over HTTPS to public hosts; no source bodies or YAML values are stored and summaries are redacted for secrets; services require bearer tokens off loopback; MCP tools cannot write. Details in `AGENTS.md`.
