# Agent gateway

!!! info "In plain words"
    The gateway is a doorway that your coding assistant (for example Claude Code) can talk through. Every request is first offered to a model on your own machine, so what it can answer costs nothing and the text never leaves your premises. Whatever it cannot answer well is passed straight on to Anthropic using your own credential. It is **on by default** and you can switch it off. Unknown word? See the [glossary](../GLOSSARY.md).

## What it does and why

Coding assistants send many small background requests, such as naming a chat or summarising a line. Paying a big cloud model for those wastes money and tokens. The gateway sits between the assistant and the cloud and does three things:

1. **Answers locally first.** Every request is offered to the local model when it has no images (tools are fine) and fits the local model's context: the body size divided by four plus `max_tokens` must be at most the provider's `context_window`, and the body at most 4 MiB. When `context_window` is not known, the old limit of 256 KiB applies. When the request offers tools, the local model may call them and read their results; the call is checked first, and if the model gets it wrong (a tool that was not offered, broken arguments) the cloud answers instead. Tool data stays on your side. Nothing is sent to the cloud. A request that asks for thinking, as most agent turns do, is answered locally without thinking, and Claude's earlier thinking blocks are left out of what the local model sees (they are signed reasoning only Claude reads). If the local answer fails, the cloud gets the request unchanged, thinking blocks included.
2. **Checks the answer for the main model.** A request for a larger model than haiku is buffered: if the local answer is empty, stopped at `max_tokens` or broke off, the cloud answers instead. Haiku requests keep streaming as they are written.
3. **Passes everything else on.** Other requests go to `api.anthropic.com` and nowhere else. The assistant's own Anthropic credential is used for that one request and is never stored.
4. **Falls back safely.** If no local slot is free within 60 seconds (`gateway_wait_seconds`), or the local model fails or answers with an error, nothing has been written yet and the request goes to the cloud instead. The wait covers only the queue for a free slot; once the local model has started, the provider's `timeout_seconds` bounds the answer.

**Every model is offered to the local model by default**, the main model (opus, sonnet) as well as haiku: local first is on for every organisation that has not switched it off, with no setup. An organisation whose local model is too small for its agents' main model turns **Answer with the local model first** off; then only haiku-class requests go to it.

To keep particular models away from the local model while the rest still go to it, give an **exclude list** (next section). A request for an excluded model always goes to the cloud, whether local first is on or off; the exclusion wins.

Token counts (`POST /v1/messages/count_tokens`) always go to the cloud, also with local first on. The gateway does not estimate them: Claude Code sizes its context with the number and decides when to compact by it, only the model's own tokenizer gives it (Claude Opus 4.7 and later count about 30 percent more tokens for the same text than older models), and Anthropic does not charge for counting. Each one is recorded with the reason `count_tokens` (or `excluded` for a model on an exclude list), so it shows up under the reasons a request was not answered locally.

Each answer carries a header, `X-CodeSynapse-Route: local` or `X-CodeSynapse-Route: cloud`, so you can see where it came from.

The audit log records the model, the size and the status of each request. It never records the text.

## Switching it on or off

The gateway is on by default, and it needs accounts (a PostgreSQL database). Two switches can turn it off:

| Switch | Who | Effect |
|---|---|---|
| `CODESYNAPSE_GATEWAY=false` | The operator, on the API | Off for the whole installation |
| **Settings → Organisation → Agent gateway** | An organisation owner or admin, signed in on the dashboard | Off for that organisation only. Its keys get `403` from the gateway |

The same panel has **Answer with the local model first** (on by default; `local_first` on `PUT /api/v1/accounts/orgs/{org}/gateway`, which changes only the fields it is sent). It also has **Answer thinking requests with the local model first** (on by default; `local_thinking`; off sends every request that asks for thinking or carries thinking blocks straight to the cloud) and **Shorten typed prompts before they go to the cloud** (on by default; `shorten_prompts`, see below). The switches are audited, as `org.gateway.set`, `org.gateway.local_first.set`, `org.gateway.local_thinking.set` and `org.gateway.shorten_prompts.set`. None can be changed with an API key. If the operator switched the gateway off, the Settings page says so, and requests are refused even when the organisation switch is on.

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

### Ollama: thinking, context length and how an answer ends

Measured on Ollama 0.40.12 with `qwen3.8:27b` on `/v1/chat/completions` (synthetic prompts):

- **Thinking.** Without `reasoning_effort`, the model thinks first. Its thinking arrives in a separate `reasoning` field of each delta (vLLM calls it `reasoning_content`), never in `content`, and it counts towards `max_tokens`. With `max_tokens: 60`, all 60 tokens went to thinking, `content` stayed empty and the stream ended with `finish_reason: "length"`. The gateway reports that as `truncated`. With `effort: none` in the provider block (sent as `reasoning_effort: "none"`), no `reasoning` field appears and the answer starts at once. Set `effort: none` for a reasoning model on Ollama; `disable_thinking: true` is the vLLM equivalent.
- **Context length.** The OpenAI-compatible endpoint ignores `num_ctx` and every other Ollama `options`, so the gateway cannot set the context. Ollama loads the model with `OLLAMA_CONTEXT_LENGTH` (environment of `ollama serve`) or the model's own default. Check what was loaded with `curl -s localhost:11434/api/ps` (`context_length`) and set `context_window` in `config/llm.yaml` to that number or less. When the loaded context is smaller than `context_window`, a long prompt can be cut by Ollama itself, without an error.
- **How a stream ends.** A whole answer ends with a chunk that has `finish_reason` `stop`, `length` or `tool_calls`, then a usage chunk and `data: [DONE]`. A tool call arrives whole in one delta. If generation fails after Ollama has sent the 200, its chat endpoint does not pass the error on: the stream just stops, with no `finish_reason` and no `[DONE]`. vLLM and llama.cpp send `data: {"error": ...}` instead.
- **`max_tokens`** from the request (Claude Code often asks for 32000 or more) is passed as is. Ollama uses it as the token limit of the answer, and the gateway counts it against `context_window` when it decides whether a request fits.

The gateway reads a stream like this: a held answer that ended with `length` (or with `[DONE]` but no `finish_reason`) is `truncated`, one with nothing in it is `empty_answer`, and a stream that stopped without `[DONE]` and without a `finish_reason`, or carried an error, is `model_error`. Before 2026-10-30 that last case was counted as `truncated`.

See `config/llm.example.yaml` for the full provider block and [Local models](local-models.md) for choosing a model. Without such a provider the gateway still works, but every request goes to the cloud as a plain pass-through.

## Connect Claude Code

1. Create an API key (`csk_...`) in the dashboard and put it in the environment as `CODESYNAPSE_GATEWAY_KEY`.
2. Run `codesynapse mcp install --gateway`.

The command checks that the gateway answers, backs up `~/.claude/settings.json`, shows you the difference and then sets `ANTHROPIC_BASE_URL` and `ANTHROPIC_CUSTOM_HEADERS` there. To undo it, run `codesynapse mcp install --gateway --remove`. If the gateway cannot be reached when a session starts, you get a warning.

The key travels in the header `X-CodeSynapse-Key`. The headers `Authorization` and `x-api-key` stay your own Anthropic credential.

## Shorter prompts on the way to the cloud

Unless an organisation turns off **Shorten typed prompts before they go to the cloud**, the gateway has the local model rewrite the prose people type into a terse version before a request goes to Anthropic: no articles, filler, politeness or hedging, but every name, path, identifier, number, quote and negation (not, never, no, only, except) kept exactly. Everything else goes out byte for byte: tool calls and their results, images, files, code blocks, the system prompt and the reminders Claude Code adds. A rewrite that drops a name, number, path, quote or negation, or saves less than 10%, is thrown away, and the original is sent.

This applies to every request that goes to the cloud, also one the local model was offered first and did not answer (it timed out, failed, gave an empty or cut-off answer, or could not take the request). The local model always gets the text as typed. A model on the organisation's or the installation's exclude list is never shortened: it goes to the cloud exactly as sent.

To use it:

1. Configure a local model for the `compress` task (the first local provider takes it; see [Local models](local-models.md#queueing-and-shorter-prompts)). Leave `compress_prompts` unset or `true` in `config/llm.yaml`.
2. Nothing to switch on: it is on for every organisation. To turn it off, an owner or admin, signed in, clears the box under **Settings → Organisation → Agent gateway** (or sends `PUT /api/v1/accounts/orgs/{org}/gateway` with `{"shorten_prompts": false}` from a signed-in session). The change is audited.
3. Connect Claude Code as described above, so `ANTHROPIC_BASE_URL` points at the gateway and `ANTHROPIC_CUSTOM_HEADERS` carries `X-CodeSynapse-Key`. Nothing else changes on the developer's side.

A prompt is rewritten once, when it is sent, and the same shorter text is reused for the rest of the chat, so the provider's prompt cache keeps matching. After an API restart, earlier prompts go out as typed. Prompts shorter than 400 bytes are left alone, and a request waits at most 8 seconds for the local model. The bytes removed show under Statistics → Models & savings, "Saved by shorter prompts". The audit entry of each request says `shortened=true` and how many bytes, never the text.

The operator turns it off for the whole installation with `compress_prompts: false` in `config/llm.yaml`, or by leaving no local model routed to `compress`.

## Local agents (OpenAI-compatible)

Agents that speak the OpenAI protocol can use two paths on the API:

- `POST /v1/chat/completions`
- `GET /v1/models`

They send the gateway key as `Authorization: Bearer csk_...`. These paths are **local only**: there is no cloud fallback, and the `model` field is replaced by the local model's name. A request waits in the local provider's queue while the connection stays open. With no local provider that has `gateway: true`, they answer `503`.

## Seeing how well the local model keeps up

**Statistics → Gateway** shows the gateway on its own page, with the same window chips as the other Statistics pages. Its tiles cover the selected window:

| Tile | What it shows |
|---|---|
| Gateway requests, Answered locally | All requests by route, and the share the local model answered |
| Local model timeouts | Requests that went to the cloud because the local model was too slow: they waited too long for its queue slot (`queue_timeout`) or for its answer (`answer_timeout`). The tile is tinted when there is at least one. A share above a few percent means the local model is overloaded or too slow for its traffic |
| Not answered locally | Every request the local model did not answer, by reason (below) |
| Response time, local / cloud | Average time from request to last byte by the route that answered, with the 95th percentile. The cloud figure includes any time first spent on the local model |
| Queue wait, local | How long requests waited for the local model's slot, average and 95th percentile, and how long its own answers took |
| Bytes in / out | Request and response sizes through the gateway |

What gateway answers saved stays on **Statistics → Models & savings** as "Saved by gateway answers", because it is part of the total saved there.

Below the tiles, the page charts the same over time from the metrics store, which keeps 30 days (a 90-day window shows the last 30):

- **Response time, local model** and **Response time, cloud**: the 50th, 75th, 95th and 99th percentile as lines, one chart per route. The two charts share one y axis by default, so the route whose lines sit lower is the faster one; "Own scale" gives each its own. The percentiles are `histogram_quantile` estimates from the request-duration histogram, whose buckets end at 5, 10, 25, 50, 100, 250 and 500 ms and 1, 2.5, 5, 10, 30, 60, 120 and 300 s, so a value is only as precise as the bucket it falls in.
- **Gateway response time (average)** by route, **Local model queue wait** and **Local model timeouts**, as lines.
- **Not answered locally, by reason**: the fall-through reasons for the window, as bars, largest first.

Platform administrators see the response-time percentiles across organisations under **Administration → Statistics**. A route fewer than three organisations used in the window is hidden there, and a reason fewer than three organisations share is folded into "other". Yesterday's totals are also kept for 400 days in the daily platform statistics (`gateway` metric), so they outlive the metrics store.

The reasons a request was not answered locally are a fixed list:

| Reason | Meaning |
|---|---|
| `not_eligible` | The organisation answers only haiku-class requests locally, and this one asked for a larger model |
| `bad_request` | The gateway could not read the request: broken JSON, no messages, a role other than `user` or `assistant`, or content that is neither text nor blocks. The cloud answers it, usually with its own error |
| `system_not_text` | The system prompt is neither a string nor a list of text blocks |
| `unsupported_block` | A message holds a block the local model cannot take, such as `server_tool_use`, `web_search_tool_result` or an unknown block type, also inside a tool result. Text, `tool_use`, `tool_result`, `search_result` and `tool_reference` blocks are converted, and `cache_control` and citation settings are dropped |
| `unsupported_tool` | The request offers a tool the local model cannot run (a server tool such as `web_search`, `web_fetch`, `code_execution` or `tool_search_tool_*`, or an Anthropic-defined client tool such as `bash_*` or `text_editor_*`), or a `tool_choice` it cannot follow. Such a request stays in the cloud, because answering without the tool would change what the request asked for. Deferred tools (`defer_loading: true`) are not a reason: the local model is offered only those a `tool_reference` loaded, as the cloud does |
| `too_large` | The request does not fit the local model's context or the body limit |
| `has_thinking` | The request asks for thinking or sends thinking back, and the organisation turned `local_thinking` off |
| `has_image` | The request carries an image or a document, also inside a tool result |
| `excluded` | The model is on the installation's or organisation's exclude list |
| `no_local_model` | No local provider with `gateway: true` is configured |
| `count_tokens` | A token count (`/v1/messages/count_tokens`): always counted by the cloud, never estimated locally |
| `queue_timeout` | No local slot was free within `gateway_wait_seconds` |
| `answer_timeout` | The local model started but did not finish within the provider's `timeout_seconds` |
| `cancelled` | The assistant gave up on the request before the local model answered |
| `model_error` | The local model could not be reached or failed, also when its stream stopped without `[DONE]` and without a finish, or carried an error |
| `bad_status` | The local model answered with an error status |
| `empty_answer` | The local model answered nothing |
| `truncated` | The answer stopped at `max_tokens` (thinking counts towards it; see [Ollama](#ollama-thinking-context-length-and-how-an-answer-ends)) or ended with `[DONE]` but no finish. A held answer cut off by a `max_tokens` of 64 or less that the client set itself (a quota check asks for one token) is kept with `stop_reason: "max_tokens"`, because the cloud would stop the same way |
| `bad_answer` | The answer could not be read, or it called a tool that was not offered or with broken arguments |

On the OpenAI-compatible paths there is no cloud behind the local model, so their failures count with route `local`. Only the reason, the route and the timings are recorded; never the prompt or the answer.

## What the audit log records

Every gateway request becomes one `gateway.request` entry in the hash-chained audit log. Its detail holds the route (`local`, `cloud` or `rejected`), the model, whether the request streamed or carried tools, the body size, the time taken and `shortened=true|false`. When the request carries user text, the entry also holds `prompt_bytes` and `prompt_sha256`: the size and SHA-256 of the newest user text as the assistant sent it. When the gateway shortened that text, `fwd_bytes` and `fwd_sha256` describe the text it forwarded. A request the local model could not answer adds `reason=<reason>`, one of the reasons listed under [Seeing how well the local model keeps up](#seeing-how-well-the-local-model-keeps-up). The audit log never holds the text itself.

## Prompt log (opt-in, off by default)

An organisation can choose to keep the text as well, in a prompt log kept apart from the audit log. Turn it on in **Settings → Prompt log**, or from a signed-in owner or admin session:

```sh
PUT /api/v1/accounts/orgs/{org}/gateway/prompt-log/settings
{"enabled": true, "retention_days": 30}
```

Each row holds the audit entry it belongs to, the time, the person, the API key ID, the runtime session, the model and the route, plus:

- the original prompt (the newest user text);
- the shortened text, when the gateway shortened it;
- the local model's answer, only when the local model answered.

Each text passes the secret and personal data redaction and is cut at 64 KiB. Tool results, file contents and system prompts are never kept. Every row carries the same `prompt_sha256` as its audit entry, so it can be checked against the chain.

| Setting | Default | Range |
|---|---|---|
| `enabled` | `false` | |
| `retention_days` | 30 | 1 to 400 |

Rows past the retention are deleted every hour by the API process that holds the `api-loops` lease. With an audit archive set (`CODESYNAPSE_AUDIT_ARCHIVE_DIR` or the S3 settings), rows older than `CODESYNAPSE_AUDIT_HOT_DAYS` are first sealed into `promptlog-<first>-<last>.jsonl.gz` files, one organisation per file. The file's SHA-256 is recorded before the rows leave PostgreSQL, and the file is deleted when the organisation's retention has passed.

Who can do what:

| Action | Endpoint | Who | Audit action |
|---|---|---|---|
| See the setting | `GET …/gateway/prompt-log/settings` | any member | |
| Change it | `PUT …/gateway/prompt-log/settings` | owner or admin, signed-in session | `org.gateway.prompt_log.set`, written before the change |
| Read the log (filters: `user`, `session`, `model`, `route`, `shortened`, `fallthrough`, `since`, `until`) | `GET …/gateway/prompt-log` | owner or admin, signed-in session | `gateway.prompt_log.read` |
| Export it as JSON Lines or CSV (`format=jsonl|csv`, `since` required, at most 20,000 rows per call; `X-CodeSynapse-Next-After-Id` names where to go on) | `GET …/gateway/prompt-log/export` | owner or admin, signed-in session | `gateway.prompt_log.export` |

API keys are refused (403). Another organisation's members get 404. CSV cells that start with `=`, `+`, `-`, `@`, a tab or a carriage return are prefixed with `'`. No other endpoint returns the texts, and they never reach the service logs, metrics or traces.

## Safety rules

- A gateway key is required, and each key gets its own audited runtime session.
- The cloud leg goes only to `https://api.anthropic.com`, follows no redirects, uses a header allowlist and caps the body at 32 MiB.
- Source never goes to a cloud model by way of CodeSynapse's own calls. The gateway only relays what your assistant itself sends, with your own credential.

The design and the decisions behind it are in the [gateway plan](../plans/GATEWAY_PLAN.md).
