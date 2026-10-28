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
| Platform health (platform administrators) | API requests and latency, the job queue, which replica runs the scheduler and lease losses, goroutines, heap, CPU and garbage-collection pauses per service, database connection pool use and waits, dropped series and log lines, failed metric exports |

Some numbers are estimates and are labelled so: cost depends on the prices you configured, and a call without reported usage or a known price shows as a gap, never as zero.

## What is never measured

No metric holds prompts, answers, source code, file paths, repository names, branch names, people, email addresses, API keys, session IDs or client addresses. Labels only take values from fixed lists, such as the names of CodeSynapse's own tools or the models you configured. The one exception is the per-repository file counts on the Ingestion page, which use an opaque ID, not the repository name.

## What is planned

Some parts of CodeSynapse have no live numbers yet. The [metrics coverage plan](../TELEMETRY_PLAN.md#metrics-coverage-plan) lists them and the order in which they will be added:

1. **Platform health** for operators (done): see the table above and Administration → Overview.
2. **Alerts for operators**: an optional vmalert that checks rules against these numbers and sends alerts to the operator's own Alertmanager-compatible receiver. Alerts never name repositories, files or people.
3. **Background jobs and indexing**: how long jobs wait and run, how many fail by kind, and indexing time by phase and language.
4. **Outside services and security events**: git clone and fetch, vulnerability and licence lookups, sign-in attempts and rate limits, and audit-log writes.
5. **Sharper existing numbers**: refused tool calls apart from errors, model timeouts and rate limits, gateway latency and why a request left the local model, and tool calls answered on your own machine by the Claude Code bridge. The Statistics panels that count failures change in the same release, so a refused call is no longer shown as an error.
6. **The web server**: requests the dashboard server handles, including failures that never reach the API, such as the API being unreachable.

The same privacy rules apply to everything planned. The web server's numbers use page and route patterns, never the address you opened, and contain nothing you typed.

Operator alerts are for whoever runs the installation. Warnings for people in an organisation keep appearing in the dashboard's notification feed.

## Related

- [Telemetry plan](../TELEMETRY_PLAN.md): every instrument, the dashboard queries, traces, logs and the rules behind them.
- [Indexing policy and reports](indexing-policy.md): the per-run file report behind the Ingestion page.
