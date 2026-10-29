# Text optimisation and external sources

Status: proposed (2026-10-29). Nothing here is implemented yet. It takes the project owner's "Text Optimization & External Source Engine" specification and fits it to what CodeSynapse already has and to the rules in `AGENTS.md`; where the two differ, this page says so and why.

!!! tip "In plain words"
    Coding assistants spend many tokens reading web pages: API references, library docs, changelogs. Most of each page is menus, scripts and footers, and the same page is often read again in the next session. This plan has CodeSynapse fetch such pages on the developer's machine, strip them down, keep a copy until the page changes, and only hand the assistant the part that answers its question. It also tightens the prompts CodeSynapse's own models use to write documentation, and counts the tokens and money saved.

## Goal

Cut tokens, latency and cloud spend for text that is not the repository's code:

1. External sources (web pages, API references) are fetched once, cached until they change, and reduced before an agent reads them.
2. Reduction runs locally: deterministic CPU passes first, a local model only when a page is still large.
3. Documentation CodeSynapse generates is terse by construction.
4. Every saving is measured with the existing telemetry and priced with the existing price table.

## What already exists

The specification describes a new pipeline; about half of it is in place and is extended, not duplicated:

| Specification | Already in CodeSynapse | This plan adds |
|---|---|---|
| Middleware in a local MCP bridge | `codesynapse mcp bridge` answers local tools (`ask_local`, git tools, worktree answers) and reports each one to `POST /api/v1/usage/local` | A local `fetch_source` tool in the same bridge |
| Tier 2 local model | Local-only tasks (`llm.LocalTasks`: `local-answer`, `local-draft`, `compress`), refused on cloud providers, no cloud fallback | A `source-extract` local task |
| Prompt compression before the cloud | `services/api/internal/llm/compress.go`: a local model shortens prose of CodeSynapse's own AI tasks; constant `terseSuffix` | Documentation templates (pillar C) |
| Agent traffic through a local model | Agent gateway (`services/api/internal/api/gateway.go`), audited, counted | Nothing more: only prose people type is shortened, on by default and per organisation, never for excluded models (`gateway/shorten.go`); tool payloads stay byte exact |
| Token and cost ledger | Statistics → Models & savings, `stats_savings.go`, `pricing.go` with `price_source` | A "sources" savings line |
| OpenTelemetry | `internal/telemetry` (bounded instruments, OTLP export, trace rules) | New instruments under the same label rules |
| Storage | PostgreSQL accounts store; Milvus for vectors | Bridge-local cache files; a shared cache only after a decision (below) |

## Pillar A: external source cache

**Where it runs.** In the bridge, on the developer's machine. Fetching arbitrary URLs from the server would be a new SSRF surface inside the cluster and would let one tenant's request read what the server can reach; the developer's machine already reaches these pages through the agent's own web tool. Answers are reported to `POST /api/v1/usage/local` before they are given, like every other local answer, so they are counted and audited (`mcp.local_answer`). The report carries the tool, sizes and timing, never the URL.

**Fetching rules.**

- `https` only; no credentials, cookies or `Authorization` sent; no private, loopback or link-local addresses (the checks of `gitx.ValidateRemote`, applied at dial time like `sso.Client`).
- Redirects followed only to `https` on the same registrable domain, at most 3; the final address is checked again.
- Response capped (2 MiB before reduction), HTML and text types only, 15 s timeout.
- Conditional requests: `If-None-Match` with the stored ETag, `If-Modified-Since` with the stored `Last-Modified`; a `304` refreshes the entry's age and returns the stored reduction.

**Cache.** Files under the user cache folder (`~/Library/Caches/codesynapse/sources` on macOS, `$XDG_CACHE_HOME/codesynapse/sources` on Linux), 0600, one entry per SHA-256 of the normalised URL (scheme, lower-cased host, path, sorted query without tracking parameters). An entry holds the ETag, `Last-Modified`, fetch time, TTL, the cleaned text, the reduced text and both token counts. Total size is capped (default 200 MiB, oldest first). TTL defaults: 7 days for documentation hosts, 1 hour for anything else; configurable per host in the user config, never from a repository's `.codesynapse.yaml`.

**Untrusted content.** A fetched page is data, not instructions. Answers carry a notice like `memory.Notice`, the text passes `redact.Free` before it is cached or returned, and nothing from a page is written to the graph, the accounts store or a metric.

**Steering the agent.** The plugin's PreToolUse hook may point a `WebFetch` of a cached URL at `fetch_source` (never block on a first fetch). That is hook feedback, so PLAN item 18 applies: every wrong steer becomes a regression test.

## Pillar B: reduction tiers

**Tier 1, CPU, deterministic, in the bridge.**

- HTML is parsed with `golang.org/x/net/html` (already in `go.mod` as an indirect dependency; making it direct is a decision below), not with regular expressions, which break on real pages. Drop `script`, `style`, `svg`, `nav`, `footer`, `header`, `aside`, forms, hidden elements and tracking images; keep headings, paragraphs, lists, tables and code blocks.
- Markdown: keep the sections whose headings or code match the agent's question, plus the page title and any signature blocks; collapse whitespace and repeated link lists.
- Bounded and fuzzed (`FuzzCleanHTML`, `FuzzPruneMarkdown`), like every parser here.

**Tier 2, local model, through the server.** When the tier-1 text is still over a threshold (default 6,000 characters), the bridge posts the question and the text to a new `POST /api/v1/repos/{repo}/source-extract` (Member, runtime session), which runs only on a new local-only task `source-extract` (added to `llm.LocalTasks`; 409 without a local model, and the bridge then returns the tier-1 text). The system prompt is constant; the question and page go in the user part, never interpolated into the system prompt. The answer is model output: thinking stripped, `redact.Free`, capped, and every code snippet in it must appear in the text that was sent, otherwise it is dropped.

**What is not taken from the specification:** a fixed model name in code (the route picks the model), the 120-line keyword window (it cuts sections mid-way; tier 1 keeps whole sections), and `LOCAL_GPU_ENDPOINT` (local providers are configured in `config/llm.yaml`).

## Pillar C: documentation prompts

Applies to CodeSynapse's own generation tasks (briefings, PR summaries, cleanup briefings, the auto-generated docs pages), never to agent prompts passing through the gateway (those are only shortened, not templated, when an organisation turns on prompt shortening).

- A constant style block appended to those tasks' system prompts: no greetings or sign-offs; one-sentence description, then a table of parameters and return values, then one minimal example; imperative phrasing ("Returns the user", not "This function will return the user"); no filler ("Note that", "In order to", "As you can see").
- Constant, so provider prompt caches still match (`AGENTS.md`: system prompts stay constant).
- A check in the task tests that generated text with filler phrases or greetings fails a lint, so the rule is enforced by tests, not by hope.
- Auto-generated docs pages keep the draft status until a person verifies them.

## Pillar D: measurement

New instruments in `internal/telemetry/metrics.go`, with the tenant attributes every tenant instrument carries (`tenant_id`, `mcp_tool`, `llm_provider`, `llm_model`) and nothing else that comes from a request: no URL, host, path or question.

| Instrument | Type | Extra attribute |
|---|---|---|
| `codesynapse_source_fetches_total` | counter | `result`: `hit`, `revalidated`, `miss`, `refused`, `error` |
| `codesynapse_source_tokens_total` | counter | `stage`: `raw`, `cleaned`, `extracted` |
| `codesynapse_source_reduce_seconds` | histogram | `tier`: `cpu`, `local_model` |

The hit ratio is computed in the query, not kept as a gauge. Spans carry the tier, result and sizes, never the URL (trace rules in `docs/TELEMETRY_PLAN.md#traces`).

**Money.** Saved tokens = raw − returned. They are priced at the input price of the agent's model from `pricing.go`, with its `price_source`, and shown as their own "External sources" line on Models & savings beside graph answers and the local cache. The specification's output-token term is left out: reducing what an agent reads does not change what it writes by a measurable amount, and a figure we cannot measure is shown as unknown, not estimated.

## Phases

1. **Fetch and cache in the bridge**: fetching rules, cache files, conditional requests, `fetch_source` in toolset `core` answered by the bridge (registered on the server with an error for direct calls, like the git tools), usage report, tests with an `httptest` TLS server for 200/304/redirect/private-address cases.
2. **Tier 1 reduction**: HTML cleaning and section pruning, fuzz tests, the token counts in the answer.
3. **Measurement**: instruments, the savings line, `docs/AGENT_GUIDE.md` and the tool's entry in `internal/toolsets`.
4. **Tier 2**: the `source-extract` local task and endpoint, defensive parsing, snippet check.
5. **Documentation prompts** (pillar C) and the filler lint.
6. **Hook steering** from `WebFetch` to `fetch_source`.

Each phase is one pull request.

## Decisions needed

- **Shared cache.** The specification keeps the cache in a database (`external_source_cache`). A server-side cache shared by an organisation would save more but means server-side fetching or storing pages fetched by one member for others to read (cache poisoning by a member, pages that differ per viewer). Proposal: start with the bridge-local cache; decide on a per-organisation PostgreSQL cache of public pages after phase 3 shows the hit rate.
- **New dependency.** `golang.org/x/net/html` as a direct dependency (BSD-3-Clause, compatible with Apache-2.0).
- **New MCP tool.** `fetch_source` reads the network, which no tool does today. It is read-only and answered on the developer's machine, but `AGENTS.md` asks for tools beyond the current set to be discussed first.
