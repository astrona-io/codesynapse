# Agent gateway plan

Goal: every model request an agent makes goes through CodeSynapse, waits its turn when the model is busy, and comes back before the agent continues. The per-provider queue (`max_concurrent`, `services/api/internal/llm/queue.go`) already exists for requests CodeSynapse makes itself. The gateway extends it to the agents' own chat traffic.

## Phase A: local-model gateway (OpenAI-compatible)

`POST /v1/chat/completions` and `GET /v1/models` on the API.

- A raw pass-through, not `Registry.Complete`: agents send full message arrays, tools and `stream: true`, which `llm.Request{System, User}` cannot hold. The handler takes a slot from the provider's queue (`slots.acquire`), forwards the body to the provider's `base_url`, and streams the answer back with flushes. The slot is held until the stream ends.
- Local providers only (`ProviderConfig.Local()`), chosen by the `model` field (provider name or served model) or the new `gateway` task route. No cloud fallback.
- Body cap, header allowlist, `GOPROXY`-style no-redirect client (the provider's existing `modelTransport`), provider key added server-side, never echoed.
- Audited like MCP requests: caller, key ID, session, provider, model, duration, sizes, token usage when reported. No prompt text.
- Prompt shortening does not apply here: tool-call payloads must stay byte exact. (The Anthropic cloud leg may shorten prose a person typed, never tool payloads; see "Prompt shortening on the cloud leg".)

## Phase B: Claude Code through CodeSynapse (Anthropic-compatible)

`POST /v1/messages` (+ `count_tokens`), streaming, reached with `ANTHROPIC_BASE_URL`.

- The server forwards to Anthropic with the installation's or organisation's API key (`api_key_env`). A Claude subscription sign-in cannot be forwarded this way: it is a personal OAuth token and must not be stored on the server.
- This sends prompts and code to a cloud provider through the server. AGENTS.md forbids that except a diff the user asked to review, so the cloud leg is a pass-through of the caller's own credential to `api.anthropic.com` only, and local answering needs an explicit per-provider switch (`gateway: true`). Decided (revised): the gateway is on by default and can be disabled for the installation or per organisation, because the project exists to lower cost, cut tokens and keep as much data on the premises as possible when people use tools such as Claude.
- Briefing: the plugin's `codesynapse hook brief` already adds a graph briefing to every Claude Code prompt; the gateway adds queueing, audit, usage and cost for the traffic itself.

## Routing: answer here or go to the cloud

Claude Code (`ANTHROPIC_BASE_URL` pointed at the gateway) sends every request to `POST /v1/messages`. The gateway holds it in the queue, decides, then either answers it with a local model or forwards it to Anthropic.

- The decision is rules first, no model call on the hot path: background calls Claude Code makes with its small model (titles, summaries, topic checks, short prompts without tools) go to the local model, with or without tools; with local first on (the default) turns of the main model do too, and contexts larger than the local model's go to the cloud. Rules are configuration (`gateway.rules`), and each decision is audited with the rule that chose it.
- A local answer needs the request translated from the Anthropic format (messages, system blocks, streaming events) to the OpenAI-compatible one and back. Tool-using requests are translated too (see below); requests with images are never routed locally.
- A request the local model cannot take (busy past a wait limit, error, no local model) falls through to the cloud unless the caller asked for local only.
- Authorisation for the cloud leg: the caller's own `Authorization` / `x-api-key` header is passed through to Anthropic for that one request and never stored or logged, so a subscription sign-in works. A server-side key (`api_key_env`) is the alternative for shared installs.
- On by default, can be disabled: the installation switch (`CODESYNAPSE_GATEWAY=false`) or the organisation setting turns it off. Local answering still needs a provider with `gateway: true`; without one every request goes to the cloud as a plain pass-through.

## Identity (decided: option 1)

Every execution needs a registered runtime session (`X-CodeSynapse-Session`, equal parent). Most agent clients cannot send custom headers, and AGENTS.md says never to generate a fallback. Options:

1. Gateway key = session: an API key minted for the gateway, bound to one registered session by the dashboard (like `DashboardExecutionSession`), seven-day expiry, shown once.
2. Require the headers: works for clients that can set them (OpenCode, the Claude Code bridge), not Cursor.

Decision: option 1, as an explicit "gateway key" with its own audit trail.

## Local leg (built)

A provider with `gateway: true` (local only, explicit `model`) answers `/v1/messages` requests that carry no thinking and no image or other non-text blocks, and that fit its context: body bytes / 4 + `max_tokens` <= the provider's `context_window`, with an absolute 4 MiB body cap (256 KiB when `context_window` is unknown). With local first on (the organisation default) that is any model; with it off, only haiku-class models. Tools are allowed (see below). The request is translated to `/chat/completions`, takes the provider's queue slot (shared with CodeSynapse's own calls), and the answer is translated back to Anthropic events. If the queue wait passes 60 s (`gateway_wait_seconds` on the provider, 1 to 300; it covers only the wait for a free slot, while the provider's `timeout_seconds` bounds the answer itself), the model errors or answers non-200, nothing has been written and the cloud takes the request. `X-CodeSynapse-Route: local|cloud` says which. A local answer needs no Anthropic credential.

### Local first (decided, on by default)

Every request is offered to the local model first, to keep as much as possible on the premises; an organisation whose local model is too small for its main model switches it off (`org_gateway.local_first`, migration 0029; no row or a row written only for `enabled` counts as on). `PUT /api/v1/accounts/orgs/{org}/gateway` takes `local_first` beside `enabled` and changes only the fields sent; owner or admin, signed-in session, audited as `org.gateway.local_first.set` before the write. The setting is read on every gateway request. A request whose context carries no choice at all is local first too (`gateway.WithLocalFirst`), so every model qualifies by default everywhere. A non-haiku request answered locally is buffered and checked before anything is written: an empty answer, `finish_reason: length` or a stream with no finish reason sends it to the cloud. Haiku requests keep streaming.

### Local exclude list (decided)

Decision: local first stays the default for every model, and a person keeps particular models away from the local model with an explicit exclude list rather than an allow list, so new models go local without anyone listing them.

- **Per organisation:** `org_gateway.local_exclude` (`text[] NOT NULL DEFAULT '{}'`, migration 0034), set with `local_exclude` on `PUT /api/v1/accounts/orgs/{org}/gateway` (only the fields sent change; `[]` clears it). Owner or admin, signed-in session, audited as `org.gateway.local_exclude.set` with the normalized list before the write. Read on every gateway request beside `local_first` and passed in the context (`gateway.WithExclude`).
- **Per installation:** `gateway_exclude` on the gateway provider in `config/llm.yaml`, normalized at load (a bad entry fails start-up), merged with the organisation's list (`Local.Exclude`).
- **Patterns** (`internal/modelpattern`): an exact name or a prefix ending in `*`; `[a-z0-9.:_-]`, at most 64 characters, at most 32 entries, lower-cased, deduplicated and sorted; a lone `*` is refused. Matching ignores case. The validator is fuzzed (`FuzzNormalize`).
- **Routing:** checked right after the request is parsed, before size and eligibility, so the exclusion wins over local first and over the small-model rule. The request goes to the cloud with fall-through reason `excluded`, which joins the fixed `gateway_fallthrough_total` reasons and the audit entry (`reason=excluded`); the model name never becomes a label.
- The local-only OpenAI-compatible paths ignore the list: they have no cloud leg.

### Local answers with tools (decided)

Decision: the local model also takes requests that use tools, because background and small-model turns often carry tool definitions and should stay on the premises.

- `tools[]` become OpenAI `tools` (`name`, `description`, `input_schema` as `parameters`). `tool_choice` maps `auto` to `auto`, `any` to `required`, `none` to `none` and `{tool, name}` to a named function; `disable_parallel_tool_use` becomes `parallel_tool_calls: false`. A `tool_choice` without tools, an unknown type, or a server tool (web search, code execution, which run at the provider) sends the request to the cloud.
- Assistant `tool_use` blocks become `tool_calls`; user `tool_result` blocks become `role: tool` messages (text only, `is_error` is prefixed `[error]`). Image blocks, also inside a result, keep the request ineligible.
- A tool answer is collected whole before anything is written, checked, then sent as Anthropic events (`tool_use` blocks with `input_json_delta`, `stop_reason: tool_use`) or one JSON message. If a call names a tool the request did not offer, its arguments are not a JSON object or the answer is over 1 MiB, nothing has been written and the cloud takes the request. Missing call ids get a generated `toolu_cs_…` id. Plain text answers to tool requests stream as before.
- Tool definitions, arguments and results go only to the local provider and are never logged; audit and telemetry keep their counts (the `tools` flag already exists).

## Credentials (decided)

The gateway key travels in `X-CodeSynapse-Key` (Claude Code: `ANTHROPIC_CUSTOM_HEADERS`). `Authorization` and `x-api-key` stay the caller's own Anthropic credential, passed to the cloud for that request only. The route is mounted with accounts unless `CODESYNAPSE_GATEWAY=false`. Requests are forwarded to `https://api.anthropic.com` only, without redirects, with a header allowlist and a 32 MiB body cap; the audit records the model, size and status, never text.

## Checks before merging any phase

Route classified through `s.route`, limits on body and time, queue slot released on client disconnect, no secrets in logs, tests with a fake upstream (no real model), `docs/AGENT_GUIDE.md` updated.

## OpenAI-compatible endpoints (built)

For local agents: `POST /v1/chat/completions` and `GET /v1/models`, behind the same `CODESYNAPSE_GATEWAY` switch (on by default) like the Anthropic route. These paths accept the gateway key as `Authorization: Bearer csk_…` (OpenAI clients send nothing else). They are local-only: no cloud fallback, `model` is replaced by the local model's name, other fields (tools, stream) pass through. A request waits in the local provider's queue for as long as the agent keeps the connection open. With no `gateway: true` local provider they answer 503. Same key-bound, audited runtime session as `/v1/messages`.

## Organisation setting (built, on by default)

The gateway is on by default: for cost, fewer tokens and keeping data on the premises. The operator can switch it off for the installation (`CODESYNAPSE_GATEWAY=false`), and each organisation can switch it off for itself (`org_gateway`, migration 0026; an organisation with no row counts as on, migration 0027 keeps the column default consistent). `GET/PUT /api/v1/accounts/orgs/{org}/gateway` reads (any member) and sets (owner or admin, signed-in session only, never an API key) the choice; the change is audited as `org.gateway.set` before it is stored, and a failed audit refuses it. Every `/v1/*` gateway request from a key of an organisation that switched it off answers 403, before a runtime session is registered. The GET answer also says `available`: whether the operator left the gateway on for the installation. Every other rail stays: a gateway key is required, each key gets its own audited runtime session, the caller's model credential is passed for one request and never stored, and the cloud leg goes only to `api.anthropic.com`.

## Prompt shortening on the cloud leg (built, on by default)

The local model shortens the prose people type before a `/v1/messages` request goes to Anthropic (`services/api/internal/gateway/shorten.go`), unless the organisation turned it off. It saves cloud input tokens on long typed prompts without touching anything the agent itself produced.

- **Every cloud leg:** routing stays local first. When the local model does not answer (any fall-through reason: `not_eligible`, `too_large`, `has_thinking`, `has_image`, `queue_timeout`, `answer_timeout`, `model_error`, `bad_status`, `empty_answer`, `truncated`, `bad_answer`, `no_local_model`), the request goes to the cloud with the shortened prose. The local model itself always gets the text as typed.
- **Excluded models are never touched:** a model on the organisation's or the installation's local exclude list goes to the cloud exactly as the agent sent it: no local answer and no shortening, also when another reason (too large, no local model) would have kept it from the local model.

- **What is touched:** only `text` blocks (or string content) of `user` messages. Tool-call payloads must stay byte exact: `tool_use`, `tool_result`, images, the system prompt, blocks that start with a tag (`<system-reminder>` and the like) or hold one, and blocks with a code fence pass byte for byte. `count_tokens`, the local leg and the OpenAI-compatible endpoints are never shortened. Blocks under 400 bytes or over 16 KiB are left alone, and `llm.ShortenProse` refuses anything `compressible` refuses (lists, diffs, JSON) and anything `redact.Free` would change, so a secret is never replaced by a placeholder on its way to the cloud.
- **Local only:** the rewrite runs on the local-only `compress` task through `services/api/internal/llm`, with the same constant system prompt and fact check as CodeSynapse's own tasks. The prompt asks for a terse, "caveman" style: no articles, filler, politeness, hedging or repetition, fragments where they stay clear, and every name, path, identifier, number, quote and negation kept exactly. A rewrite that loses a backticked or quoted span, a number, a path or identifier, or any negation or limit (not, never, no, only, except, without, nor), or saves under 10%, is discarded. With no local model routed to `compress`, or `compress_prompts: false`, requests go out unchanged.
- **Prompt cache:** Claude Code resends the whole conversation each turn and the provider caches it by its bytes. A rewrite is made once, when its block first appears in the newest user message, and kept in a per-runtime-session memo keyed by the SHA-256 of the original block (memory only, never persisted or logged; 512 entries per session, 256 sessions, 32 MiB, 24 hours). Every later request replays the same bytes for that block. Earlier blocks without an entry (after a restart, or on another API replica) go out as typed, so a turn is never rewritten twice in two different ways. Refusals are remembered too.
- **Bounded:** at most one local call per request, 8 s timeout; any error, timeout or failed check sends the original. Streaming is unaffected. The body is parsed only when it is valid JSON with no repeated key on the path walked, so the gateway and the provider cannot disagree on which value counts.
- **Setting:** `shorten_prompts` on `org_gateway` (migration 0033, `DEFAULT true`; an organisation without a row has it on). Owners and admins change it in a signed-in session (never an API key) on `PUT /api/v1/accounts/orgs/{org}/gateway`, audited as `org.gateway.shorten_prompts.set` before it is stored. The GET answer adds `shorten_available` (a local model can shorten).
- **Counting:** the bytes a rewrite made for that request removed are added to `llm_compressed_bytes_total` and `compressed_bytes`, attributed to the configured cloud Anthropic provider whose model is the one requested, else to the fixed label `gateway-cloud`/`anthropic` (the requested model is client text and never a label), which Statistics prices at the reference input price under "Saved by shorter prompts". Replays are not counted again: they mostly hit the prompt cache. The `gateway.request` audit entry adds `shortened=true removed=<bytes>`; no text is recorded anywhere.
