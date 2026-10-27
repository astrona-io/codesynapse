# Agent gateway plan

Goal: every model request an agent makes goes through CodeSynapse, waits its turn when the model is busy, and comes back before the agent continues. The per-provider queue (`max_concurrent`, `services/api/internal/llm/queue.go`) already exists for requests CodeSynapse makes itself. The gateway extends it to the agents' own chat traffic.

## Phase A: local-model gateway (OpenAI-compatible)

`POST /v1/chat/completions` and `GET /v1/models` on the API.

- A raw pass-through, not `Registry.Complete`: agents send full message arrays, tools and `stream: true`, which `llm.Request{System, User}` cannot hold. The handler takes a slot from the provider's queue (`slots.acquire`), forwards the body to the provider's `base_url`, and streams the answer back with flushes. The slot is held until the stream ends.
- Local providers only (`ProviderConfig.Local()`), chosen by the `model` field (provider name or served model) or the new `gateway` task route. No cloud fallback.
- Body cap, header allowlist, `GOPROXY`-style no-redirect client (the provider's existing `modelTransport`), provider key added server-side, never echoed.
- Audited like MCP requests: caller, key ID, session, provider, model, duration, sizes, token usage when reported. No prompt text.
- Prompt shortening does not apply: tool-call payloads must stay byte exact.

## Phase B: Claude Code through CodeSynapse (Anthropic-compatible)

`POST /v1/messages` (+ `count_tokens`), streaming, reached with `ANTHROPIC_BASE_URL`.

- The server forwards to Anthropic with the installation's or organisation's API key (`api_key_env`). A Claude subscription sign-in cannot be forwarded this way: it is a personal OAuth token and must not be stored on the server.
- This sends prompts and code to a cloud provider through the server. AGENTS.md forbids that except a diff the user asked to review, so it needs an explicit, per-provider switch (`gateway: true`, off by default), and an organisation-level opt-in. Decided: allowed as that opt-in only.
- Briefing: the plugin's `codesynapse hook brief` already adds a graph briefing to every Claude Code prompt; the gateway adds queueing, audit, usage and cost for the traffic itself.

## Routing: answer here or go to the cloud

Claude Code (`ANTHROPIC_BASE_URL` pointed at the gateway) sends every request to `POST /v1/messages`. The gateway holds it in the queue, decides, then either answers it with a local model or forwards it to Anthropic.

- The decision is rules first, no model call on the hot path: background calls Claude Code makes with its small model (titles, summaries, topic checks, short prompts without tools) go to the local model; turns of the main model, tool-using turns and long contexts go to the cloud. Rules are configuration (`gateway.rules`), and each decision is audited with the rule that chose it.
- A local answer needs the request translated from the Anthropic format (messages, system blocks, streaming events) to the OpenAI-compatible one and back. Requests with tools or images are never routed locally in the first version.
- A request the local model cannot take (busy past a wait limit, error, no local model) falls through to the cloud unless the caller asked for local only.
- Authorisation for the cloud leg: the caller's own `Authorization` / `x-api-key` header is passed through to Anthropic for that one request and never stored or logged, so a subscription sign-in works. A server-side key (`api_key_env`) is the alternative for shared installs.
- Opt-in as decided: off unless the provider has `gateway: true` and the organisation allows it.

## Identity (decided: option 1)

Every execution needs a registered runtime session (`X-CodeSynapse-Session`, equal parent). Most agent clients cannot send custom headers, and AGENTS.md says never to generate a fallback. Options:

1. Gateway key = session: an API key minted for the gateway, bound to one registered session by the dashboard (like `DashboardExecutionSession`), seven-day expiry, shown once.
2. Require the headers: works for clients that can set them (OpenCode, the Claude Code bridge), not Cursor.

Decision: option 1, as an explicit "gateway key" with its own audit trail.

## Local leg (built)

A provider with `gateway: true` (local only, explicit `model`) answers `/v1/messages` requests that name a haiku-class model, carry no tools, tool choice, thinking or non-text blocks, and are at most 256 KiB. The request is translated to `/chat/completions`, takes the provider's queue slot (shared with CodeSynapse's own calls), and the answer is translated back to Anthropic events. If the queue wait passes 20 s, the model errors or answers non-200, nothing has been written and the cloud takes the request. `X-CodeSynapse-Route: local|cloud` says which. A local answer needs no Anthropic credential. Still to do: the organisation-level opt-in, and the OpenAI-compatible endpoint for local agents.

## Credentials (decided)

The gateway key travels in `X-CodeSynapse-Key` (Claude Code: `ANTHROPIC_CUSTOM_HEADERS`). `Authorization` and `x-api-key` stay the caller's own Anthropic credential, passed to the cloud for that request only. The route is mounted only with `CODESYNAPSE_GATEWAY=true` and accounts. Requests are forwarded to `https://api.anthropic.com` only, without redirects, with a header allowlist and a 32 MiB body cap; the audit records the model, size and status, never text.

## Checks before merging any phase

Route classified through `s.route`, limits on body and time, queue slot released on client disconnect, no secrets in logs, tests with a fake upstream (no real model), `docs/AGENT_GUIDE.md` updated.

## OpenAI-compatible endpoints (built)

For local agents: `POST /v1/chat/completions` and `GET /v1/models`, behind `CODESYNAPSE_GATEWAY=true` like the Anthropic route. These paths accept the gateway key as `Authorization: Bearer csk_…` (OpenAI clients send nothing else). They are local-only: no cloud fallback, `model` is replaced by the local model's name, other fields (tools, stream) pass through. A request waits in the local provider's queue for as long as the agent keeps the connection open. With no `gateway: true` local provider they answer 503. Same key-bound, audited runtime session as `/v1/messages`.
