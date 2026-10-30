# Text optimisation and external sources

Status: phase 1 (fetch and cache in the bridge) implemented (2026-10-29); phase 2 (tier 1 reduction) implemented (2026-10-30, see "As built (phase 2)" under pillar B); phase 5 (documentation prompts and the filler lint) implemented (2026-10-30); phase 6 (hook steering from `WebFetch` to `fetch_source`) implemented (2026-10-30); the other phases proposed. Phase 1 differs from this page: as the project owner asked, redirects are followed to any public https address, at most 5, each hop checked again at dial time (not only the same registrable domain, at most 3); the dial guard is shared in `internal/netguard`; and per-host TTLs in the user config are not there yet (the 7-day documentation hosts are a built-in list). It takes the project owner's "Text Optimization & External Source Engine" specification and fits it to what CodeSynapse already has and to the rules in `AGENTS.md`; where the two differ, this page says so and why.

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

**As built (phase 6).** `codesynapse hook webfetch` runs on `PreToolUse` for `WebFetch` (`cmd/codesynapse/webfetch.go`). It normalises the URL with `fetchsource.Normalise` and looks it up read-only with `fetchsource.Cached` (same SHA-256 key, the folder is never created and no entry is touched, so eviction order is unchanged; fresh and stale entries both count, since a stale one costs at most a conditional request). Only for a cached page, and only the first time in a session, the call is turned back with a pointer to `fetch_source` and the normalised URL, the advice to pass the question when the tool takes one, and "call WebFetch again" when `fetch_source` is not available. An uncached page, a URL `fetch_source` would refuse (not https, credentials, no host, unparsable), a second `WebFetch` of the same page, a session ID that is not a plain ID, a missing cache folder and any error go through with no output; the hook always exits 0. The session state keeps the cache key (a hash), never the URL, and the hook logs nothing about it and saves no hook activity for the event. Steers are reported at Stop as the workflow count `fetches_steered` (pseudo tool `usage.FetchSteered`, validated 0–10,000 like the other counts) and shown on Statistics → Agents. `CODESYNAPSE_FETCH_STEER=false` turns it off. Regression tests: `cmd/codesynapse/webfetch_test.go` (cached https, uncached, http, malformed, credentials, no host, cache folder missing, hook disabled, second fetch) and `internal/fetchsource/lookup_test.go`.

## Pillar B: reduction tiers

**Tier 1, CPU, deterministic, in the bridge.**

- HTML is parsed with `golang.org/x/net/html` (already in `go.mod` as an indirect dependency; making it direct is a decision below), not with regular expressions, which break on real pages. Drop `script`, `style`, `svg`, `nav`, `footer`, `header`, `aside`, forms, hidden elements and tracking images; keep headings, paragraphs, lists, tables and code blocks.
- Markdown: keep the sections whose headings or code match the agent's question, plus the page title and any signature blocks; collapse whitespace and repeated link lists.
- Bounded and fuzzed (`FuzzCleanHTML`, `FuzzPruneMarkdown`), like every parser here.

**Tier 2, local model, through the server.** When the tier-1 text is still over a threshold (default 6,000 characters), the bridge posts the question and the text to a new `POST /api/v1/repos/{repo}/source-extract` (Member, runtime session), which runs only on a new local-only task `source-extract` (added to `llm.LocalTasks`; 409 without a local model, and the bridge then returns the tier-1 text). The system prompt is constant; the question and page go in the user part, never interpolated into the system prompt. The answer is model output: thinking stripped, `redact.Free`, capped, and every code snippet in it must appear in the text that was sent, otherwise it is dropped.

**As built (phase 2).** In `internal/fetchsource` (`text.go`, `prune.go`), run by the bridge through `fetch_source`, which takes an optional `question` (at most 500 characters, used on the machine only: never sent, cached or logged; the MCP audit keeps only a digest of `question` should a call reach the server). Where it differs from the above:

- Cleaning walks the tree `html.Parse` builds (the HTML5 algorithm, so broken markup is repaired as a browser would) instead of the token stream phase 1 used, so hidden elements are dropped with everything inside them. Besides the listed elements it drops `noscript`, `template`, `iframe`, `object`, `canvas`, media, `dialog` and elements with a navigation, search, menu, toolbar or dialog role. A `header` (or `role="banner"`) is dropped only outside `main` and `article`, where it usually holds the page's own title. Hidden means the `hidden` attribute, `aria-hidden="true"`, or `display:none`/`visibility:hidden` in `style`. Tracking images are not told apart from others: every image is dropped, alt text included.
- Kept structure is Markdown-like: `#` headings, `- ` list items (nested lists indented, at most 4 levels), `| a | b |` table rows with a `| --- |` separator after a header row (blocks inside a cell stay on its row), fenced `pre` blocks with the language of a `language-*`/`lang-*` class, and backticks around inline code.
- Bounds: input capped at 2 MiB, the walk stops 256 levels deep; a page nested deeper than the parser takes (512 elements) falls back to its text without structure. Pruning reads at most 8 MiB, splits at most 4,096 sections and matches at most 16 terms.
- "Repeated link lists" are runs of two or more list items that repeat an earlier run verbatim; they are kept once, outside code fences, in HTML, Markdown and plain-text pages (raw Markdown is often served as `text/plain`).
- Pruning splits at ATX headings outside code fences. A section matches when its heading or code names a question word (lower case, at least two characters, common words left out; words of four or more characters may continue, so `client` matches `ClientConn`). A matched section keeps its subsections and the headings above it; the text before the first heading (the title) is always kept. "Signature blocks" are fenced blocks of at most 3 lines whose first line declares something (`func`, `type`, `def`, `class`, ...), at most 32, kept with their heading from unmatched sections. Two fallbacks the plan did not name: with no heading or code match, sections whose prose names a word are kept; with none at all, the whole cleaned page is returned. JSON is never pruned.
- The cache stores the cleaned page (with a cleaner version; entries from another version are fetched again, not revalidated) and pruning runs per answer. Everything returned passes `redact.Free` again and carries `Notice`.
- The answer reports `raw_*`, `cleaned_*` and `text_*` (returned) bytes and tokens with `telemetry.EstimateTokens` (bytes / 4), and `sections`, `sections_kept` and `pruned`, for phase 3 to measure. `FuzzCleanHTML` and `FuzzPruneMarkdown` check UTF-8, size bounds and that pruning only returns lines of the page.

**What is not taken from the specification:** a fixed model name in code (the route picks the model), the 120-line keyword window (it cuts sections mid-way; tier 1 keeps whole sections), and `LOCAL_GPU_ENDPOINT` (local providers are configured in `config/llm.yaml`).

## Pillar C: documentation prompts

Applies to CodeSynapse's own generation tasks (briefings, PR summaries, cleanup briefings, the auto-generated docs pages), never to agent prompts passing through the gateway (those are only shortened, not templated, when an organisation turns on prompt shortening).

- A constant style block appended to those tasks' system prompts: no greetings or sign-offs; one-sentence description, then a table of parameters and return values, then one minimal example; imperative phrasing ("Returns the user", not "This function will return the user"); no filler ("Note that", "In order to", "As you can see").
- Constant, so provider prompt caches still match (`AGENTS.md`: system prompts stay constant).
- A check in the task tests that generated text with filler phrases or greetings fails a lint, so the rule is enforced by tests, not by hope.
- Auto-generated docs pages keep the draft status until a person verifies them.

**As built (phase 5).** `internal/prose` holds the constant `prose.Style` block, appended to the system prompts of `briefing`, `pr-summary` and `cleanup-briefing`, and its no-filler part `prose.NoFiller`, appended to `code-annotation` (symbol summaries), whose JSON answer has no room for tables or examples; the review and triage tasks answer in JSON and are unchanged. The lint (`prose.Lint`, a bounded phrase list matched case-insensitively at word boundaries, fuzzed) fails fixtures and golden outputs in the task tests (`services/api/internal/aitasks/testdata`) and, at run time, only logs per-task counts (`ai task prose lint`, no text); model output is never rejected or rewritten for it. Tests check that every system prompt is a package constant carrying the block once. The generated documentation that has a status is the architecture proposal, which is always its type's draft word and cannot be set otherwise (`TestProposalStaysDraft`); symbol annotations have no status and are shown as attributed model output.

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
