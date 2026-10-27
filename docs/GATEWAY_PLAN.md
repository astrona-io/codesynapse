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

## Identity (decided: option 1)

Every execution needs a registered runtime session (`X-CodeSynapse-Session`, equal parent). Most agent clients cannot send custom headers, and AGENTS.md says never to generate a fallback. Options:

1. Gateway key = session: an API key minted for the gateway, bound to one registered session by the dashboard (like `DashboardExecutionSession`), seven-day expiry, shown once.
2. Require the headers: works for clients that can set them (OpenCode, the Claude Code bridge), not Cursor.

Decision: option 1, as an explicit "gateway key" with its own audit trail.

## Checks before merging any phase

Route classified through `s.route`, limits on body and time, queue slot released on client disconnect, no secrets in logs, tests with a fake upstream (no real model), `docs/AGENT_GUIDE.md` updated.
