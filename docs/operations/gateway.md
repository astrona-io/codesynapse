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

The same panel has **Answer with the local model first** (on by default; `local_first` on `PUT /api/v1/accounts/orgs/{org}/gateway`, which changes only the fields it is sent). Both switches are audited, as `org.gateway.set` and `org.gateway.local_first.set`. Neither can be changed with an API key. If the operator switched the gateway off, the Settings page says so, and requests are refused even when the organisation switch is on.

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

## Local agents (OpenAI-compatible)

Agents that speak the OpenAI protocol can use two paths on the API:

- `POST /v1/chat/completions`
- `GET /v1/models`

They send the gateway key as `Authorization: Bearer csk_...`. These paths are **local only**: there is no cloud fallback, and the `model` field is replaced by the local model's name. A request waits in the local provider's queue while the connection stays open. With no local provider that has `gateway: true`, they answer `503`.

## What the audit log records

Every gateway request becomes one `gateway.request` entry in the hash-chained audit log. Its detail holds the route (`local`, `cloud` or `rejected`), the model, whether the request streamed or carried tools, the body size, the time taken and `shortened=true|false`. When the request carries user text, the entry also holds `prompt_bytes` and `prompt_sha256`: the size and SHA-256 of the newest user text as the assistant sent it. When the gateway shortened that text, `fwd_bytes` and `fwd_sha256` describe the text it forwarded. A request the local model could not answer adds `fallthrough=<reason>` (`not_eligible`, `too_large`, `busy`, `unavailable`, `no_first_chunk` or `bad_answer`). The audit log never holds the text itself.

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

The design and the decisions behind it are in the [gateway plan](../GATEWAY_PLAN.md).
