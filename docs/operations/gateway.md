# Agent gateway

!!! info "In plain words"
    The gateway is a doorway that your coding assistant (for example Claude Code) can talk through. Every request is first offered to a model on your own machine, so what it can answer costs nothing and the text never leaves your premises. Whatever it cannot answer well is passed straight on to Anthropic using your own credential. It is **on by default** and you can switch it off. Unknown word? See the [glossary](../GLOSSARY.md).

## What it does and why

Coding assistants send many small background requests, such as naming a chat or summarising a line. Paying a big cloud model for those wastes money and tokens. The gateway sits between the assistant and the cloud and does three things:

1. **Answers locally first.** Every request is offered to the local model when it uses no thinking and no images (tools are fine) and fits the local model's context: the body size divided by four plus `max_tokens` must be at most the provider's `context_window`, and the body at most 4 MiB. When `context_window` is not known, the old limit of 256 KiB applies. When the request offers tools, the local model may call them and read their results; the call is checked first, and if the model gets it wrong (a tool that was not offered, broken arguments) the cloud answers instead. Tool data stays on your side. Nothing is sent to the cloud.
2. **Checks the answer for the main model.** A request for a larger model than haiku is buffered: if the local answer is empty, stopped at `max_tokens` or broke off, the cloud answers instead. Haiku requests keep streaming as they are written.
3. **Passes everything else on.** Other requests go to `api.anthropic.com` and nowhere else. The assistant's own Anthropic credential is used for that one request and is never stored.
4. **Falls back safely.** If no local slot is free within 60 seconds (`gateway_wait_seconds`), or the local model fails or answers with an error, nothing has been written yet and the request goes to the cloud instead. The wait covers only the queue for a free slot; once the local model has started, the provider's `timeout_seconds` bounds the answer.

**Every model is offered to the local model by default**, the main model (opus, sonnet) as well as haiku: local first is on for every organisation that has not switched it off, with no setup. An organisation whose local model is too small for its agents' main model turns **Answer with the local model first** off; then only haiku-class requests go to it.

To keep particular models away from the local model while the rest still go to it, give an **exclude list** (next section). A request for an excluded model always goes to the cloud, whether local first is on or off; the exclusion wins.

Each answer carries a header, `X-CodeSynapse-Route: local` or `X-CodeSynapse-Route: cloud`, so you can see where it came from.

The audit log records the model, the size and the status of each request. It never records the text.

## Switching it on or off

The gateway is on by default, and it needs accounts (a PostgreSQL database). Two switches can turn it off:

| Switch | Who | Effect |
|---|---|---|
| `CODESYNAPSE_GATEWAY=false` | The operator, on the API | Off for the whole installation |
| **Settings → Organisation → Agent gateway** | An organisation owner or admin, signed in on the dashboard | Off for that organisation only. Its keys get `403` from the gateway |

The same panel has **Answer with the local model first** (on by default; `local_first` on `PUT /api/v1/accounts/orgs/{org}/gateway`, which changes only the fields it is sent). It also has **Shorten typed prompts before they go to the cloud** (off by default; `shorten_prompts`, see below). The switches are audited, as `org.gateway.set`, `org.gateway.local_first.set` and `org.gateway.shorten_prompts.set`. None can be changed with an API key. If the operator switched the gateway off, the Settings page says so, and requests are refused even when the organisation switch is on.

## Keep some models away from the local model

**Settings → Organisation → Agent gateway → Never answer these models locally** takes one model per line:

- an exact model name, such as `claude-opus-5-5`, or
- a prefix ending in `*`, such as `claude-opus-*`, which matches every model whose name starts with `claude-opus-`.

Entries use `a-z`, `0-9`, `.`, `:`, `_` and `-`, at most 64 characters each, at most 32 entries. They are lower-cased, duplicates are dropped and the list is sorted; matching ignores case. A lone `*` is refused: to keep every model off the local model, turn local first off or remove `gateway: true` from the provider. An invalid entry refuses the whole change with `400` and nothing is saved.

Owners and admins change it from a signed-in session, never with an API key; each change is audited as `org.gateway.local_exclude.set` before it is written. Through the API:

```sh
curl -X PUT https://codesynapse.example.com/api/v1/accounts/orgs/<org>/gateway \
  -H 'Content-Type: application/json' -b cs_session=... \
  -d '{"local_exclude": ["claude-opus-*", "gpt-5"]}'
```

The PUT changes only the fields it is sent; `"local_exclude": []` clears the list. `GET` on the same path returns `local_exclude` and the operator's `installation_exclude`. The list is read on every gateway request, so a change applies to the next request.

An excluded request goes to the cloud with the fall-through reason `excluded`: that reason is counted in `gateway_fallthrough_total{reason="excluded"}` and written to the request's audit entry as `reason=excluded`. The model name is never a metric label; the audit entry already names the cleaned model.

The operator can add an installation-wide list with `gateway_exclude` on the gateway provider (next section). It applies to every organisation, beside the organisation's own list, and the Settings panel shows it.

The OpenAI-compatible paths (below) are local only and have no cloud to send a request to, so the exclude list does not apply to them.

## Set up the local model

Local answering needs a local provider that you opt in explicitly. In `config/llm.yaml` give that provider `gateway: true` and an explicit `model`:

```yaml
    gateway: true   # answer gateway requests, tools included, from this local model
    context_window: 131072   # how much fits; requests that do not go to the cloud
    # gateway_exclude: [claude-opus-*]   # models never answered locally, for every organisation
    # gateway_wait_seconds: 60           # wait for a free local slot (1-300) before the cloud takes over
    # timeout_seconds: 600               # how long one local answer may take once it has started
```

`gateway_exclude` follows the same rules as the organisation's list and is merged with it; an invalid entry stops the API at start-up with an error naming the provider. `gateway_wait_seconds` is how long a request waits in the provider's queue for a free slot; it does not limit the answer, which `timeout_seconds` bounds.

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

## Seeing how well the local model keeps up

**Statistics → Models** has a Gateway row for the selected window:

| Tile | What it shows |
|---|---|
| Gateway requests, Answered locally | All requests by route, and the share the local model answered |
| Local model timeouts | Requests that went to the cloud because the local model was too slow: they waited too long for its queue slot (`queue_timeout`) or for its answer (`answer_timeout`). The tile is tinted when there is at least one. A share above a few percent means the local model is overloaded or too slow for its traffic |
| Not answered locally | Every request the local model did not answer, by reason (below) |
| Response time, local / cloud | Average time from request to last byte by the route that answered, with the 95th percentile. The cloud figure includes any time first spent on the local model |
| Queue wait, local | How long requests waited for the local model's slot, average and 95th percentile, and how long its own answers took |

**Statistics → Activity → Model gateway** charts the same over time: response time (average and 95th percentile) by route, local model timeouts, queue wait and every fall-through by reason. Platform administrators see the same charts across organisations under **Administration → Statistics**, where a route or reason fewer than three organisations use is folded into "other". Yesterday's totals are also kept for 400 days in the daily platform statistics (`gateway` metric), so they outlive the metrics store.

The reasons a request was not answered locally are a fixed list:

| Reason | Meaning |
|---|---|
| `not_eligible` | The organisation answers only haiku-class requests locally, and this one asked for a larger model |
| `too_large` | The request does not fit the local model's context or the body limit |
| `has_thinking` | The request asks for thinking or sends thinking back |
| `has_image` | The request carries an image or a document, also inside a tool result |
| `excluded` | The model is on the installation's or organisation's exclude list |
| `no_local_model` | No local provider with `gateway: true` is configured |
| `queue_timeout` | No local slot was free within `gateway_wait_seconds` |
| `answer_timeout` | The local model started but did not finish within the provider's `timeout_seconds` |
| `cancelled` | The assistant gave up on the request before the local model answered |
| `model_error` | The local model could not be reached or failed |
| `bad_status` | The local model answered with an error status |
| `empty_answer` | The local model answered nothing |
| `truncated` | The answer stopped at `max_tokens` or broke off |
| `bad_answer` | The answer could not be read, or it called a tool that was not offered or with broken arguments |

On the OpenAI-compatible paths there is no cloud behind the local model, so their failures count with route `local`. Only the reason, the route and the timings are recorded; never the prompt or the answer.

## Safety rules

- A gateway key is required, and each key gets its own audited runtime session.
- The cloud leg goes only to `https://api.anthropic.com`, follows no redirects, uses a header allowlist and caps the body at 32 MiB.
- Source never goes to a cloud model by way of CodeSynapse's own calls. The gateway only relays what your assistant itself sends, with your own credential.

The design and the decisions behind it are in the [gateway plan](../GATEWAY_PLAN.md).
