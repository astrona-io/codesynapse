# Statistics and telemetry

!!! tip "In plain words"
    CodeSynapse counts what it does: tool calls, model tokens and cost, database time, memory use and indexing. You see these numbers on the Statistics page. They never contain your code, prompts, file paths, repository names or anything that identifies a person.

CodeSynapse keeps two kinds of numbers:

- **Daily totals** per repository, tool and client, kept for 180 days. They answer "how much did agents use CodeSynapse this month, and how many tokens did it save?"
- **Live activity** by the minute and hour. Each service sends counters and timings every 15 seconds to an internal VictoriaMetrics, over OpenTelemetry (OTLP). They answer "what is slow or failing right now?"

Both appear on the **Statistics** page. Every number belongs to one organisation, and you only see your own organisations. Platform administrators also see a few installation-wide panels.

## What is measured today

| Area | What you can see |
|---|---|
| Agent tools | sessions, tool calls and failures, tool latency, tokens served to agents, MCP traffic in bytes |
| AI models | requests and failures per provider and model, input, output and cached tokens, estimated cost and cache savings, time to first token, read and write speed, how much of the context window requests used |
| Agent gateway | requests answered by the local model or the cloud, bytes, local tokens |
| Databases | latency (median, 95th and 99th percentile), requests, errors and timeouts, graph rows and vector matches |
| Agent memory | stored facts, saves, recalls, offloaded turns and tokens |
| Assistant hooks | lifecycle events and observed byte counts from Codex and Cursor |
| Indexing | files scanned, indexed and skipped per repository, annotation jobs |

Some numbers are estimates and are labelled so: cost depends on the prices you configured, and a call without reported usage or a known price shows as a gap, never as zero.

## What is never measured

No metric holds prompts, answers, source code, file paths, repository names, branch names, people, email addresses, API keys, session IDs or client addresses. Labels only take values from fixed lists, such as the names of CodeSynapse's own tools or the models you configured. The one exception is the per-repository file counts on the Ingestion page, which use an opaque ID, not the repository name.

## What is planned

Some parts of CodeSynapse have no live numbers yet. The [metrics coverage plan](../TELEMETRY_PLAN.md#metrics-coverage-plan) lists them and the order in which they will be added:

1. **Platform health** for operators: which replica runs the scheduler, process memory and CPU, database connections, and whether metrics themselves are being dropped or fail to export.
2. **Background jobs and indexing**: how long jobs wait and run, how many fail by kind, and indexing time by phase and language.
3. **Outside services and security events**: git clone and fetch, vulnerability and licence lookups, sign-in attempts and rate limits, and audit-log writes.
4. **Sharper existing numbers**: refused tool calls apart from errors, model timeouts and rate limits, gateway latency and why a request left the local model, and tool calls answered on your own machine by the Claude Code bridge.

The same privacy rules apply to everything planned.

## Related

- [Telemetry plan](../TELEMETRY_PLAN.md): every instrument, the dashboard queries, traces, logs and the rules behind them.
- [Indexing policy and reports](indexing-policy.md): the per-run file report behind the Ingestion page.
