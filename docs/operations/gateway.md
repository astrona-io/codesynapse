# Agent gateway

!!! info "In plain words"
    The gateway is a doorway that your coding assistant (for example Claude Code) can talk through. Every request is first offered to a model on your own machine, so what it can answer costs nothing and the text never leaves your premises. Whatever it cannot answer well is passed straight on to Anthropic using your own credential. It is **on by default** and you can switch it off. Unknown word? See the [glossary](../GLOSSARY.md).

## What it does and why

Coding assistants send many small background requests, such as naming a chat or summarising a line. Paying a big cloud model for those wastes money and tokens. The gateway sits between the assistant and the cloud and does three things:

1. **Answers locally first.** Every request is offered to the local model when it uses no thinking and no images (tools are fine) and fits the local model's context: the body size divided by four plus `max_tokens` must be at most the provider's `context_window`, and the body at most 4 MiB. When `context_window` is not known, the old limit of 256 KiB applies. When the request offers tools, the local model may call them and read their results; the call is checked first, and if the model gets it wrong (a tool that was not offered, broken arguments) the cloud answers instead. Tool data stays on your side. Nothing is sent to the cloud.
2. **Checks the answer for the main model.** A request for a larger model than haiku is buffered: if the local answer is empty, stopped at `max_tokens` or broke off, the cloud answers instead. Haiku requests keep streaming as they are written.
3. **Passes everything else on.** Other requests go to `api.anthropic.com` and nowhere else. The assistant's own Anthropic credential is used for that one request and is never stored.
4. **Falls back safely.** If the local model is busy for more than 20 seconds, fails, or answers with an error, nothing has been written yet and the request goes to the cloud instead.

An organisation whose local model is too small for its agents' main model turns **Answer with the local model first** off; then only haiku-class requests go to it, as before.

Each answer carries a header, `X-CodeSynapse-Route: local` or `X-CodeSynapse-Route: cloud`, so you can see where it came from.

The audit log records the model, the size and the status of each request. It never records the text.

## Switching it on or off

The gateway is on by default, and it needs accounts (a PostgreSQL database). Two switches can turn it off:

| Switch | Who | Effect |
|---|---|---|
| `CODESYNAPSE_GATEWAY=false` | The operator, on the API | Off for the whole installation |
| **Settings → Organisation → Agent gateway** | An organisation owner or admin, signed in on the dashboard | Off for that organisation only. Its keys get `403` from the gateway |

The same panel has **Answer with the local model first** (on by default; `local_first` on `PUT /api/v1/accounts/orgs/{org}/gateway`, which changes only the fields it is sent). It also has **Shorten typed prompts before they go to the cloud** (off by default; `shorten_prompts`, see below). The switches are audited, as `org.gateway.set`, `org.gateway.local_first.set` and `org.gateway.shorten_prompts.set`. None can be changed with an API key. If the operator switched the gateway off, the Settings page says so, and requests are refused even when the organisation switch is on.

## Set up the local model

Local answering needs a local provider that you opt in explicitly. In `config/llm.yaml` give that provider `gateway: true` and an explicit `model`:

```yaml
    gateway: true   # answer gateway requests, tools included, from this local model
    context_window: 131072   # how much fits; requests that do not go to the cloud
```

See `config/llm.example.yaml` for the full provider block and [Local models](local-models.md) for choosing a model. Without such a provider the gateway still works, but every request goes to the cloud as a plain pass-through.

## Connect Claude Code

1. Create an API key (`csk_...`) in the dashboard and put it in the environment as `CODESYNAPSE_GATEWAY_KEY`.
2. Run `codesynapse mcp install --gateway`.

The command checks that the gateway answers, backs up `~/.claude/settings.json`, shows you the difference and then sets `ANTHROPIC_BASE_URL` and `ANTHROPIC_CUSTOM_HEADERS` there. To undo it, run `codesynapse mcp install --gateway --remove`. If the gateway cannot be reached when a session starts, you get a warning.

The key travels in the header `X-CodeSynapse-Key`. The headers `Authorization` and `x-api-key` stay your own Anthropic credential.

## Shorter prompts on the way to the cloud

When an organisation turns on **Shorten typed prompts before they go to the cloud**, the gateway has the local model rewrite the prose people type into a shorter version before a request goes to Anthropic. Everything else goes out byte for byte: tool calls and their results, images, files, code blocks, the system prompt and the reminders Claude Code adds. A rewrite that drops a name in backticks or a number is thrown away, and the original is sent.

To use it:

1. Configure a local model for the `compress` task (the first local provider takes it; see [Local models](local-models.md#queueing-and-shorter-prompts)). Leave `compress_prompts` unset or `true` in `config/llm.yaml`.
2. As an owner or admin, signed in, tick the box under **Settings → Organisation → Agent gateway** (or `PUT /api/v1/accounts/orgs/{org}/gateway` with `{"shorten_prompts": true}` from a signed-in session).
3. Connect Claude Code as described above, so `ANTHROPIC_BASE_URL` points at the gateway and `ANTHROPIC_CUSTOM_HEADERS` carries `X-CodeSynapse-Key`. Nothing else changes on the developer's side.

A prompt is rewritten once, when it is sent, and the same shorter text is reused for the rest of the chat, so the provider's prompt cache keeps matching. After an API restart, earlier prompts go out as typed. Prompts shorter than 400 bytes are left alone, and a request waits at most 8 seconds for the local model. The bytes removed show under Statistics → Models & savings, "Saved by shorter prompts". The audit entry of each request says `shortened=true` and how many bytes, never the text.

The operator turns it off for the whole installation with `compress_prompts: false` in `config/llm.yaml`, or by leaving no local model routed to `compress`.

## Local agents (OpenAI-compatible)

Agents that speak the OpenAI protocol can use two paths on the API:

- `POST /v1/chat/completions`
- `GET /v1/models`

They send the gateway key as `Authorization: Bearer csk_...`. These paths are **local only**: there is no cloud fallback, and the `model` field is replaced by the local model's name. A request waits in the local provider's queue while the connection stays open. With no local provider that has `gateway: true`, they answer `503`.

## Safety rules

- A gateway key is required, and each key gets its own audited runtime session.
- The cloud leg goes only to `https://api.anthropic.com`, follows no redirects, uses a header allowlist and caps the body at 32 MiB.
- Source never goes to a cloud model by way of CodeSynapse's own calls. The gateway only relays what your assistant itself sends, with your own credential.

The design and the decisions behind it are in the [gateway plan](../GATEWAY_PLAN.md).
