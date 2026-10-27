# Agent gateway

!!! info "In plain words"
    The gateway is a doorway that your coding assistant (for example Claude Code) can talk through. Small, simple requests are answered by a model on your own machine, so they cost nothing and the text never leaves your premises. Everything else is passed straight on to Anthropic using your own credential. It is **on by default** and you can switch it off. Unknown word? See the [glossary](../GLOSSARY.md).

## What it does and why

Coding assistants send many small background requests, such as naming a chat or summarising a line. Paying a big cloud model for those wastes money and tokens. The gateway sits between the assistant and the cloud and does three things:

1. **Answers small requests locally.** A request qualifies when it asks for one of Claude's small (haiku-class) models, uses no tools, no thinking and no images, and is at most 256 KiB. A local model that you configured answers it. Nothing is sent to the cloud.
2. **Passes everything else on.** Other requests go to `api.anthropic.com` and nowhere else. The assistant's own Anthropic credential is used for that one request and is never stored.
3. **Falls back safely.** If the local model is busy for more than 20 seconds, fails, or answers with an error, nothing has been written yet and the request goes to the cloud instead.

Each answer carries a header, `X-CodeSynapse-Route: local` or `X-CodeSynapse-Route: cloud`, so you can see where it came from.

The audit log records the model, the size and the status of each request. It never records the text.

## Switching it on or off

The gateway is on by default, and it needs accounts (a PostgreSQL database). Two switches can turn it off:

| Switch | Who | Effect |
|---|---|---|
| `CODESYNAPSE_GATEWAY=false` | The operator, on the API | Off for the whole installation |
| **Settings → Organisation → Agent gateway** | An organisation owner or admin, signed in on the dashboard | Off for that organisation only. Its keys get `403` from the gateway |

The organisation switch is audited as `org.gateway.set`. It cannot be changed with an API key. If the operator switched the gateway off, the Settings page says so, and requests are refused even when the organisation switch is on.

## Set up the local model

Local answering needs a local provider that you opt in explicitly. In `config/llm.yaml` give that provider `gateway: true` and an explicit `model`:

```yaml
    gateway: true   # answer small tool-free requests (Claude haiku class) from this local model
```

See `config/llm.example.yaml` for the full provider block and [Local models](local-models.md) for choosing a model. Without such a provider the gateway still works, but every request goes to the cloud as a plain pass-through.

## Connect Claude Code

1. Create an API key (`csk_...`) in the dashboard and put it in the environment as `CODESYNAPSE_GATEWAY_KEY`.
2. Run `codesynapse mcp install --gateway`.

The command checks that the gateway answers, backs up `~/.claude/settings.json`, shows you the difference and then sets `ANTHROPIC_BASE_URL` and `ANTHROPIC_CUSTOM_HEADERS` there. To undo it, run `codesynapse mcp install --gateway --remove`. If the gateway cannot be reached when a session starts, you get a warning.

The key travels in the header `X-CodeSynapse-Key`. The headers `Authorization` and `x-api-key` stay your own Anthropic credential.

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
