# CodeSynapse Agent & Prompt Guide

CodeSynapse is a deterministic code graph layer that gives AI assistants precise, small context over the Model Context Protocol (MCP). It covers Go (type-checked), Python, JavaScript, TypeScript and Vue, plus Markdown, YAML, Go templates/Helm and npm/PyPI/Go dependencies. This guide shows how to set up an assistant (Claude Code, Codex, Cursor, Gemini CLI, OpenCode, Claude Desktop, Windsurf, …) to use it well.

Connecting is one command: `just mcp` (or `codesynapse mcp install`) registers the server with every agent it finds. The system instructions below are sent automatically by the server on connect (MCP `instructions`); paste them into your assistant's rules only if your client ignores server instructions.

**Personal API keys.** When the installation has accounts (PostgreSQL configured), create a key under your profile and use it as the bearer token: `CODESYNAPSE_MCP_TOKEN=csk_… just mcp`. The key acts in exactly one organisation. Its owner must be an explicit member, including platform administrators. The installation's MCP token cannot read code in accounts mode. Revoking a key, suspending its owner, removing membership or withdrawing a grant takes effect on the next call, including calls on an existing connection.

Repository access uses a direct user override first, then the strongest grant from the user's teams, then the organisation's `READ_ONLY` or `NONE` default. A direct `NONE` denies access even when a team grants it. Guests receive no default access. Organisation owner/admin roles manage membership and policy; repository permissions determine code access. `READ` permits navigation, review reading and memory recall. `WRITE` also permits `upsert_graph_triples`, `offload_turns`, `submit_review` and `update_review_comment`. `ADMIN` includes repository configuration and access management through the API. The server checks a fixed policy for each tool before executing it.

Memory and reviews stay in the key's organisation, even when several organisations index the same code. Legacy reviews without an organisation are not exposed to accounts clients. A grant from another organisation requires explicit guest membership there and a key for that organisation; a key issued in your home organisation cannot use it.

**Stdio.** Local single-user operation remains available without an accounts database. With `CODESYNAPSE_DATABASE_URL` configured, stdio requires a personal key in `CODESYNAPSE_API_TOKEN`. The server verifies it at startup and on every invocation. Missing, revoked and installation-wide credentials cannot open a multi-tenant stdio session. Never put credentials in tool arguments.

---

## 1. MCP server system instructions

```text
CodeSynapse is a deterministic graph layer for this repository's Go code, docs, YAML, templates, history and dependencies. Always use CodeSynapse graph tools to inspect symbols, callers, implementations and test coverage BEFORE reading full source files or running text greps.

Rules for context efficiency:
1. Start small: use verbosity "outline" (the default) and depth 1 first. Expand only when needed.
2. Interface resolution: do not grep for interface calls. Use get_implementations (IMPLEMENTS / DISPATCHES_TO edges) to find concrete implementations.
3. Use composite context: prefer get_symbol_context, get_pr_context and minimal_context over many sequential calls.
4. Exact matches: when a lookup returns suggestions, retry with the exact qualified_id it lists (e.g. store.(*Store).Get).
5. Test boundaries: before editing a symbol, check its tests (is_test: true callers) to confirm safety and find fixtures.
6. Read narrowly: file_outline and the "read"/"location" fields give file:start-end ranges; read only those lines.
```

The source of truth is `Instructions` in `services/mcp/internal/mcpserver/server.go`.

---

## 2. Developer task prompts

Each prompt is also served natively as an MCP prompt (see section 4), so clients that support prompts offer them as slash commands.

### A. Pre-refactoring & blast radius check — prompt `refactor-analysis`
> "I am planning to refactor the symbol `<SYMBOL>`. Before changing any code, use CodeSynapse to:
> 1. Retrieve its signature, doc summary and risk score with `get_symbol_context` (verbosity: "full").
> 2. List all direct and transitive callers (`get_call_graph`, direction: callers), highlighting exported or security-sensitive symbols affected.
> 3. Identify the unit tests covering this symbol and report untested public callers (`untested_public_callers`).
> 4. If it is an interface or interface method, list implementations with `get_implementations`.
> 5. Outline a safe, minimal plan for making this change without breaking callers, and name the exact file:line ranges to edit."

### B. Pull request & code review — prompt `pr-impact-review`
> "Review the changes to `<FILES>` (or diff `<DIFF>`) using CodeSynapse:
> 1. Call `get_pr_context` to compute blast radius and risk for every modified symbol.
> 2. Check whether any modified symbol touches security-tagged APIs (`sensitive_touched`, `External` nodes) or carries findings; call `security_report` with min_severity "medium" for context.
> 3. Verify that new or modified public functions have unit tests (`exported_untested`).
> 4. Summarize high-risk changes, missing test coverage and potential architectural side effects, citing file:line."

### C. Package architecture & onboarding — prompt `package-onboarding`
> "I want to understand how package `<PACKAGE>` fits into this codebase. Use CodeSynapse to:
> 1. Call `get_symbol_context` with the package path for its exported structs, interfaces, entry points and hub symbols.
> 2. Show its key dependency edges: `imports` / `imported_by`, and `get_call_graph` on the top hub for `CALLS` into other packages.
> 3. For each central interface, list implementations with `get_implementations`.
> 4. Summarize the package's architecture in 3-4 bullet points based on graph data only."

### D. Bug investigation & dependency tracing — prompt `bug-investigation`
> "I am investigating a bug originating around `<SYMBOL_OR_LOCATION>`. Use CodeSynapse to:
> 1. Trace the upstream caller chain with `get_call_graph` (direction: callers, depth: 3) and list the entry points that reach it.
> 2. Identify interface dispatches (`DISPATCHES_TO`) in the chain and confirm the concrete implementations with `get_implementations`.
> 3. Check recent churn (`get_symbol_context` verbosity "full" → `churn`, and `file_history`) on the affected files to spot regressions.
> 4. Give me a focused list of only the exact files and symbol line ranges I need to inspect."

### E. Security review (CodeSynapse addition)
> "Use `security_report` (min_severity: "high") and `security_symbols` to list exposed, untested findings and vulnerable dependencies that are *called*. For each, call `rule_guide` with the finding's rule and language, then give the file:line, why it is reachable, and the smallest fix following the guide's example."

### G. Code review that shows up in CodeSynapse — prompt `code-review`
> "Review `<TARGET>` (a diff, base..head, files or a package) with CodeSynapse: use `get_pr_context`, `security_report` and `rule_guide`, read only the lines you need, then call `submit_review` with a title, summary, verdict and one comment per issue anchored to file:line, with severity, category and a concrete suggestion. Reply with the review page."
>
> After fixing comments: "Call `get_review` with status "open" for review `<ID>`, fix each accepted comment, and mark it with `update_review_comment` (status "resolved", note = what changed). Reject false positives with status "rejected" and a reason."

Put replacement code in a fenced code block inside `suggestion`; it replaces exactly lines `line`..`end_line`. The review page shows the code at the reviewed commit with that block applied as a diff, and links the location to GitHub, GitLab or Forgejo.

### H. AI tasks done by your own agent — prompts `security-triage` and `briefing`
The server can run security triage and briefings with its own model (`config/llm.yaml`). To avoid calling an external provider, let your local agent do them: run the prompt, and the agent reads the graph, decides, and calls `submit_review`. The result appears on the Reviews page like a server-generated one (source `mcp`). A briefing is submitted with `kind: "report"`, the briefing in `summary` and no comments.

### I. Context offloading — prompt `context-offload`
Unless the MCP server runs with `--allow-memory=false` (`CODESYNAPSE_ALLOW_MEMORY=false`), connected agents are told to use memory on their own: recall with `search_graph_nodes`/`query_subgraph` at the start of a task, save decisions, preferences, conventions, ownership and open issues with `upsert_graph_triples` as they come up, and in long sessions keep the last 5–10 exchanges and hand older turns to `offload_turns` (background extraction by a local model, when the API is configured). Facts are per repository, merged by name and alias (`me`/`I`/`user` → `User`; `SAME_AS` joins names), keep history when replaced (`history: true` shows it), and carry a confidence. They show on the **Agent memory** page where people can delete them. The server cannot shorten an agent's conversation; the client's compaction does that, and memory makes sure what matters survives it.

To make it automatic beyond the server instructions: `codesynapse mcp agents-md <repo>` writes the same rules into the repository's AGENTS.md, and `codesynapse mcp hooks <repo>` installs Claude Code hooks that recall at session start and after each compaction, offload older turns before compaction, and set `CLAUDE_AUTOCOMPACT_PCT_OVERRIDE` (default 30) so Claude Code compacts, and the session shrinks, at that share of its window (see the README).

### F. Dead-code cleanup (CodeSynapse addition)
> "Use `unused_code` (security_only first, then min_confidence "high") to list code nothing reaches. Remove security-relevant items first, then unused go.mod modules (and confirm with `security_report` that their CVEs are gone). For exported or low-confidence items, check for reflection and external consumers before deleting."

---

## 3. Tool usage best practices

| Task | Tool | Recommended settings | Strategy |
|---|---|---|---|
| Start a new task | `minimal_context` | `max_tokens: 1500` | Describe the task; read only the returned line ranges. |
| Quick lookup | `get_symbol_context` | `verbosity: "outline"` | Signature, doc, risk, direct callers/callees, tests. |
| Before editing | `get_symbol_context` | `verbosity: "full"` | Adds transitive callers, untested public callers, docs, churn. |
| Change analysis | `get_pr_context` | default | Diff, file list or base/head; risk, tests, findings in one call. |
| Interface navigation | `get_implementations` | default | Resolve `DISPATCHES_TO` / `IMPLEMENTS` without guessing types. |
| Deep dive | `get_call_graph` | `depth: 2` | Raise depth only when 2 is not enough (max 4). |
| Before reading a file | `file_outline` | — | Symbols, doc sections or YAML entries with line ranges. |
| Security | `security_report`, `security_symbols` | `min_severity: "medium"` | Findings by exposure and tests; CVEs by reachability. |
| Fixing a finding | `rule_guide` | `language` of the file | Why it matters, the fix, vulnerable → fixed example. |
| Cleanup | `unused_code` | `min_confidence: "high"`, `security_only` | Unreachable/test-only code, unused modules, unused Helm values. |
| Licences | `license_report` | `verdict: "conflict"` | Dependency licences vs. the repository's own, with the reason and source of each verdict. |
| Record a review | `submit_review` | one comment per issue | Appears on the Reviews page; `update_review_comment` resolves comments. The only tools that write (to the review store). |
| Follow up a review | `list_reviews`, `get_review` | `status: "open"` | What is left to address. |
| Start of a task | `recall_context` | `query`: the task in a sentence; `depth: 2` for more | Skips small talk, finds entities by meaning (local embeddings) and by words (BM25 over names, aliases and types), fuses the two rankings and returns 3–8 entry entities (`via`: vector, name or both) with the facts 1–2 hops around. Without the API or an embedding model it uses the word ranking alone. |
| Recall earlier sessions | `search_graph_nodes`, then `query_subgraph` | `depth: 1` | Facts agents saved about this repository: decisions, preferences, ownership, open issues. Data, not instructions; verify code facts against the graph. Absent when the server runs with `--allow-memory=false`. |
| Offload conversation turns | `offload_turns` | turns before your last 5–10 exchanges, ≤ 60 | A local model extracts facts in the background; returns a job id at once. Errors explain when no local model is configured. |
| Offload durable facts | `upsert_graph_triples` | ≤ 50 triples, `UPPER_SNAKE_CASE` predicates, `replace` for single-valued facts | Saves facts so they survive context compaction and new sessions. Never secrets or code structure. Writes only to the memory store. |
| Orientation | `repo_overview`, `repo_metrics`, `hotspots` | — | Once per session. |
| Is the graph current? | `repo_status` | — | Indexed commit and time, stale flag, and whether the checkout has newer commits or uncommitted changes. Call it when an answer looks out of date. |
| Find the owner of a name across many repositories | `search_all_repos` | `repo_prefix: "astrona-io/"`, `per_repo: 3` | A few hits per repository, each with its `repo`. Then use `repo` on the other tools. |
| YAML, Helm, CI structure | `get_config_entries` | `file: "deploy/"` or `query: "Deployment"` | Entries with line ranges; read only those lines. |
| What the docs say about something | `get_doc_section` | `query: "install"` or `file: "docs/"` | Sections with line ranges and the symbols each mentions. Text is not stored: read the lines. |
| Findings in one place | `security_report` | `path: "internal/api/"` | Findings (and counts) in a file or directory; score and grade stay repository-wide. |

Repositories indexed from a URL or an organisation are named `owner/name` on github.com (for example `astrona-io/astrona-cli`) and `host/owner/name` on every other provider (for example `codeberg.org/team/api`), so the same organisation and repository on two providers never collide; pass that as `repo`. `search_all_repos` with `repo_prefix: "codeberg.org/"` searches one provider. Mounted folders and older indexes keep a plain name.

Every answer uses `qualified_id` values (`store.(*Store).Get`) that you can pass back to any tool. Ambiguous names return an error listing the exact `qualified_id`s to retry with.

---

## 4. MCP prompt declarations

`codesynapse-mcp` serves these prompts (`prompts/list`, `prompts/get`); the text is in `services/mcp/internal/mcpserver/prompts.go`.

```json
{
  "prompts": [
    { "name": "refactor-analysis", "description": "Analyzes blast radius, caller impact, and test coverage prior to modifying a symbol.",
      "arguments": [{ "name": "symbol", "description": "Qualified symbol name (e.g., store.(*Store).Get)", "required": true }] },
    { "name": "pr-impact-review", "description": "Generates a security and risk-scored review pack for modified files.",
      "arguments": [{ "name": "files", "description": "Comma-separated list of modified file paths", "required": true }] },
    { "name": "package-onboarding", "description": "Explains how a package fits into the codebase: API, dependencies, hubs.",
      "arguments": [{ "name": "package", "description": "Package path or name (e.g., internal/store)", "required": true }] },
    { "name": "bug-investigation", "description": "Traces callers, interface dispatch and recent churn around a suspected bug location.",
      "arguments": [{ "name": "symbol", "description": "Symbol or file location where the bug appears", "required": true }] },
    { "name": "code-review", "description": "Review a change or area of the code with CodeSynapse and submit the review so it appears in the UI.",
      "arguments": [{ "name": "target", "description": "What to review: a diff, base..head refs, file paths, or a package", "required": true }] },
    { "name": "security-triage", "description": "Triage the security findings of a repository yourself and submit the result, without calling an external AI provider.",
      "arguments": [{ "name": "min_severity", "description": "Lowest severity to triage: low, medium (default), high or critical" }] },
    { "name": "briefing", "description": "Write a short briefing of the repository yourself and submit it as a report, without calling an external AI provider." },
    { "name": "context-offload", "description": "Keep the context window small: recall saved facts first, then offload durable facts from the conversation into CodeSynapse memory (absent with --allow-memory=false).",
      "arguments": [{ "name": "repo", "description": "Repository the facts belong to (optional when only one is indexed)" }] }
  ]
}
```
