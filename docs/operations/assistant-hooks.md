# Codex and Cursor lifecycle integration

!!! info "In plain words"
    Codex and Cursor are coding assistants, like Claude Code. They can run small programs (**hooks**) at set moments, for example when a chat starts or ends. CodeSynapse installs hooks so they get the same rules, memory and review prompts. The hooks record only **metadata** (what kind of event, how long, how big), never your prompts, code or file names. The "Collection contract" section lists every recorded field.

CodeSynapse installs native lifecycle adapters for Codex and Cursor. The adapters load rules, recall chat memory, apply the existing structured-file read guard, request a final diff review, report retrieval usefulness, and record metadata about assistant activity. They never approve permission requests or execute commands received in hook input.

## Setup

Before you start:

1. Install a current `codesynapse` binary. A `go run` build is temporary and cannot be installed as a hook.
2. Create a personal organisation API key in Settings → API keys.
3. Set `CODESYNAPSE_API_TOKEN` to that key, using your secret manager or shell environment.

The installer stores the key in the existing user configuration token file, readable only by you (mode 0600). Hook files and command arguments contain no credentials.

To install hooks for one project:

```sh
codesynapse mcp hooks . --client codex
codesynapse mcp hooks . --client cursor
```

These commands write `.codex/hooks.json` and `.cursor/hooks.json` respectively. To install for your whole user account instead, together with the MCP connection settings:

```sh
codesynapse mcp install --yes --only codex,cursor
```

The combined installer configures detected assistants. It accepts `CODESYNAPSE_MCP_TOKEN` or the existing API token file/environment. Both commands accept `--api-url`; the combined installer also accepts `--url` for MCP. Settings are backed up as `.bak`; repeating installation replaces CodeSynapse adapter handlers while preserving other settings and hooks. Project hooks contain this machine's executable path and should normally stay local; teammates should run setup themselves. The existing `--client claude` default keeps the Claude memory hook installer.

After upgrading `codesynapse`, run `codesynapse doctor` to see which registrations are stale and `codesynapse doctor --fix` to write them again. Through the MCP bridge, an agent whose CLI is older than the server is told so in the server instructions, with the same upgrade and `doctor --fix` advice the Claude Code session start gives.

Restart the assistant. In Codex, open `/hooks`, inspect the exact definitions and trust them. Installation never bypasses trust. Project-local Codex configuration also requires a trusted project. In Cursor, inspect Customize → Hooks and the Hooks output channel. Runtime versions determine which events actually fire. Do not install project and global copies of the same adapters: both sources can run and duplicate observations.

Hook-enabled plugins require manual installation in Codex desktop and are not eligible for its public plugin directory. Direct hook installation above does not require packaging a plugin. See the official [Codex hooks documentation](https://learn.chatgpt.com/docs/hooks), [OpenAI plugin packaging guide](https://developers.openai.com/plugins/build/plugins), and [Cursor hook reference](https://cursor.com/docs/hooks).

## Runtime identity and MCP

Codex supplies `session_id`; Cursor supplies `conversation_id` (also `session_id` at session boundaries). If both Cursor fields are supplied, they must agree. Each invocation registers that existing ID idempotently with `POST /api/v1/execution/sessions`, then sends both execution headers when recording activity or recalling memory. Missing, mismatched, expired or closed identities cannot store activity. A subagent keeps the primary runtime identity; a conflicting parent is refused. No substitute session ID is generated.

The installer configures local stdio bridges and the Codex/Cursor lifecycle hooks. Claude Code uses its existing plugin or project hooks for lifecycle behavior; the plugin also runs this adapter (`--client claude`) for the events its other hooks do not cover (see the plugin page). Use one setup command for Claude Code, Codex and Cursor:

```sh
CODESYNAPSE_MCP_TOKEN='<personal organisation key>' codesynapse mcp install --yes --only claude,codex,cursor
```

After the one-time setup, launch the CLI you want:

```sh
codesynapse codex
codesynapse claude
codesynapse cursor
```

`codesynapse codex` and `codesynapse claude` forward native arguments directly, including their normal resume options. For example, `codesynapse codex --model <model>` and `codesynapse claude --continue`. They use the already-installed bridge and hooks; they do not install clients or replace their login. Desktop apps use their normal launchers and the same configuration.

The personal key is stored in the private user configuration file, never in assistant settings or command arguments. Restart clients after installation. Codex CLI and desktop share `~/.codex/config.toml`; Cursor CLI and desktop share `~/.cursor/mcp.json`. Install the official CLIs before setup. Codex still requires hook review/trust; Cursor hooks must stay enabled. This code requires a release after v0.0.4.

| Client | Automatic runtime identity |
|---|---|
| Codex CLI and desktop | Native MCP `_meta.threadId` or `x-codex-turn-metadata.thread_id`; conflicting IDs are refused. Subagent lineage comes from the trusted `SubagentStart` hook. Ephemeral execution `session_id` is never a substitute. |
| Cursor desktop | Native MCP string `_meta.progressToken` matched to the `tool_use_id` and `conversation_id` in its pre-MCP hook. Requires an editor version exposing matching IDs. |
| Cursor CLI | Launch with `codesynapse cursor`. It runs official `cursor-agent create-chat`, then `cursor-agent --resume` with that runtime-created ID. The child process receives the binding; a private live launcher socket also supplies it when Cursor filters the MCP environment. Resume an existing chat with `codesynapse cursor --resume <existing-chat-id>`; pass other native arguments after `--`. |

Cursor CLI currently drops per-call MCP IDs, so launching plain `cursor-agent` cannot provide this integration. Keep one chat per launcher process; relaunch to change chats. If hooks observe a different chat, subsequent MCP calls from that launcher are refused. Separate launcher processes remain independent. Desktop remains independent of the CLI process environment. MCP child processes find the live launcher through their OS parent chain, at most 32 ancestors; the socket requires the installed personal key's fingerprint. The socket contains no chat ID on disk, stops with the launcher, and cannot be selected through tool arguments. This launcher supports macOS and Linux with `/bin/ps`.

The bridge registers the actual runtime ID and attaches both execution headers to every upstream tool call. Each chat has its own upstream MCP transport, including when the client shares a single native MCP process across chats. Discovery can list capabilities without a chat identity; it cannot execute tools. There is no model-selected identity or global “current chat” file. Each bridge permits at most 128 chats before requiring a restart.

Local correlation receipts contain only opaque call/thread IDs, their primary chat and expiry. They live in a private cache partitioned by API URL, credential hash and client; files are mode 0600. Call receipts expire after 30 minutes and are consumed once. Thread lineage and CLI switch markers expire after seven days. Each cache partition is bounded to 4096 receipts. They are never OTel attributes or conversation archives.

Concurrency tests exercise two interleaved chats through stateful and stateless transports for both adapters, refusal of model-supplied IDs, and receipt credential isolation. Cursor desktop correlation is covered by wire fixtures; an actual installed desktop client has not been verified here. Verify a real tool call in two independent chats after installation, especially after client upgrades. Missing runtime metadata produces an explicit error instead of guessing an identity. See [runtime session setup](../AGENT_GUIDE.md#runtime-session-setup).

Session-end hooks record an observation without permanently revoking the execution binding: closing an assistant process may be followed by resuming that chat. Explicit session deletion/revocation still closes the binding permanently. Registration does not extend its seven-day lifetime.

## Event coverage and behavior

All 12 documented Codex event names are installed:

| Events | Behavior beyond recording activity |
|---|---|
| `SessionStart` | Load rules, baseline the working tree once, recall chat memory when available |
| `UserPromptSubmit` | Refresh rules on each prompt |
| `PreToolUse` | Track first lookup; guard structured file reads |
| `PostToolUse` | Track CodeSynapse answer ranges and successful structured reads |
| `PermissionRequest` | Observe only; leave approval to Codex and the user |
| `PreCompact` | Flush retrieval counts |
| `PostCompact` | Restore rules |
| `SubagentStart` | Supply rules to the subagent |
| `SubagentStop`, `Interrupt`, `SessionEnd` | Flush retrieval counts |
| `Stop` | Flush retrieval counts; request review once per changed working tree |

All 21 Cursor event names are installed: `sessionStart`, `sessionEnd`, `beforeSubmitPrompt`, `preToolUse`, `postToolUse`, `postToolUseFailure`, `subagentStart`, `subagentStop`, `beforeShellExecution`, `afterShellExecution`, `beforeMCPExecution`, `afterMCPExecution`, `beforeReadFile`, `afterFileEdit`, `preCompact`, `stop`, `afterAgentResponse`, `afterAgentThought`, `beforeTabFileRead`, `afterTabFileEdit`, and `workspaceOpen`.

Cursor session start loads rules and chat memory; prompt submission records activity on every prompt. Generic tool events apply the shared lookup/read guard and retrieval tracking; `beforeReadFile` also guards structured reads. `stop` uses Cursor's native follow-up response and respects its loop count. Compaction, subagent completion and session end flush retrieval counts. Other events are observational. Specialized and generic tool hooks may describe the same operation; these are separate observations, not unique call counts. Codex shell commands do not expose structured read ranges, so their contents are not parsed into file reads.

Cursor `workspaceOpen` has no conversation identity. It returns a valid empty response and stores no session activity. Tab events without a runtime chat identity behave the same way. This preserves session isolation rather than attaching activity to whichever chat happened to run last. Compaction hooks record metadata; they do not parse or offload Codex/Cursor transcripts. Transcript formats are not stable contracts and the existing Claude transcript parser is not reused.

## Collection contract

Only the following allowlisted fields leave the machine. Counts are observations supplied by the client, not proof of server execution or billing.

| Data | Stored meaning |
|---|---|
| Client and event | `codex`/`cursor` and the native event enum |
| Runtime identity | Existing session ID bound to the authenticated user and organisation |
| Received time and sequence | Server timestamp and audit event ID; no claim of precise client execution ordering |
| Tool category | `none`, `read`, `write`, `search`, `shell`, `mcp`, `subagent`, or `other` |
| CodeSynapse tool | Known tool name, when recognized; other raw tool names are discarded |
| Outcome | Reported completion/failure/interruption, or `unknown` when absent |
| Failure and execution mode | Allowlisted timeout/error/permission-denied category, native sandbox flag and Codex permission mode when provided; no error text |
| Source | Allowlisted start/compaction source such as `resume`, `compact`, `auto`, `manual` |
| Duration | Native `duration_ms`, or Cursor's millisecond `duration` field; missing values remain unknown |
| Byte counts | Tool input/output JSON bytes (including Cursor's `result_json`), shell-command, prompt/text and file-content UTF-8 bytes; content is discarded |
| Context | Native context token count, window limit, usage percentage, messages to compact and first-compaction flag when present |
| Session/subagent counts | Message count, tool-call count, continuation loop count and background flag when provided |

The API schema also accepts optional reported `input_tokens`, `output_tokens` and `cache_tokens` for integrations that can supply them. These native adapters currently leave them unknown: they do not estimate token counts, cloud cost or savings from byte sizes.

No prompts, responses, reasoning text, source, command arguments, file paths, repository names, raw error text, transcripts, tool arguments/results, credentials, user emails or model names are included in activity measurements. Payload contents exist only briefly in the local adapter while their sizes are measured. Existing account audit records separately retain their normal actor/request context (including actor identity, IP and user agent according to audit policy); the session activity endpoint returns only the allowlisted measurements, sequence and timestamp. That account context is never added to OTel attributes.

## Persistence, retrieval and OTel

`POST /api/v1/execution/activity` accepts one observation with the personal key and both registered execution headers. The server rechecks the current participant and membership inside the accounts policy transaction before appending. Unknown fields/enums, negative/oversized values, malformed JSON and bodies above 4 KiB are refused. Measurements must fit the existing 500-byte audit detail bound without truncation. Admission is capped at 600 observations per session per minute using the shared accounts rate limiter.

Activity uses the existing hash-chained account audit store: `agent.hook.<client>.<event>` actions, JSON measurements in `detail`, and runtime context in the existing `mcp` envelope with `method=agent/hook`. These entries are not MCP calls and do not increment MCP call counters. PostgreSQL persists them across API restarts; the in-memory development store is not durable. Existing audit retention applies (400 days by default). Deleting a session does not remove its audit history.

The original participant can retrieve a bounded timeline, including after closure:

```text
GET /api/v1/execution/sessions/{session}/activity?after_id=0&limit=100
```

The response is `{"events":[{"id":123,"at":"…","client":"codex","event":"PostToolUse","category":"shell","outcome":"unknown"}]}`. Use the last ID as `after_id` to page forward; maximum page size is 1000. Other users/organisations receive 404. Organisation admins can use the existing audit export filters `action=agent.hook.&session=<id>` for JSON, JSONL and CSV views.

Accepted observations feed the existing bounded registry and its configured OTLP/HTTP exporter:

| Metric | Additional labels |
|---|---|
| `agent_hook_events_total` | `client`, `event`, `category`, `outcome` |
| `agent_hook_duration_seconds` | `client`, `event` |
| `agent_hook_bytes_total` | `client`, `event`, `kind` (`tool_input`, `tool_output`, `prompt`, `text`, `file_content`) |

Every series retains authenticated `tenant_id` and fixed `none` for MCP tool/provider/model. There are no session, user, repository, path or content labels. The Statistics series API exposes `agent_hook_events` and `agent_hook_bytes` through fixed tenant-filtered queries. External OTel backends can use the same streams. Byte totals count observations and can count content more than once.

Each accepted observation also produces an `agent.hook` span when tracing is enabled/sampled. It includes fixed client/event/category/outcome, allowlisted source/tool/failure/permission mode, measured counts, context usage, reported duration and boolean flags when present, the opaque session ID and audit activity ID. Its duration describes the observation inside the API, not the reported operation duration. Session identity is a span attribute, never a resource or metric attribute.

Delivery is synchronous and best effort with an 8 MiB local hook-input limit and a ten-second budget (2.5 seconds for Codex `SessionEnd`/`Interrupt`). Network/storage failure logs a content-free warning and leaves the assistant running. There is no offline spool, automatic retry or exactly-once delivery guarantee; process death, disabled/untrusted hooks and unavailable APIs can lose observations. Metrics are live accepted observations and are not replayed from audit after restart. Existing metric stream identity/reset behavior applies. Hooks are useful observations, not a complete enforcement or billing boundary.
