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
| Agent tools | sessions, tool calls, failures and refusals (a call the server turned down is counted apart from one that failed), calls answered on your own machine by the Claude Code bridge, tool latency, tokens served to agents, MCP traffic in bytes |
| AI models | requests and failures per provider and model, and why requests failed (timeout, rate limit, refused or other error), input, output and cached tokens, estimated cost and cache savings, time to first token, read and write speed, how much of the context window requests used |
| Agent gateway | requests answered by the local model or the cloud, response time per route, why a request left the local model, bytes, local tokens |
| Databases | latency (median, 95th and 99th percentile), requests, errors and timeouts, graph rows and vector matches |
| Agent memory | stored facts, saves, recalls, offloaded turns and tokens |
| Assistant hooks | lifecycle events and observed byte counts from Codex and Cursor |
| Indexing | files scanned, indexed and skipped per repository, annotation jobs; indexing runs and time, time per phase (fetching, extraction, storing the graph) and extraction time per language |
| Background jobs | finished jobs by outcome, failures by kind, how long jobs waited for a worker and how long they ran (Statistics → Sync) |
| Platform statistics (platform administrators) | organisations, people, active people, memberships by role, seats, teams, repository entries, single sign-on, watches, browser and runtime sessions and API keys, across every organisation (Administration → Statistics). Counted in PostgreSQL, not in the metrics store, and every view is audited. |
| Outside services | remote clones, fetches, access checks and pushes by outcome and time (Statistics → Sync); vulnerability, licence and organisation-listing lookups by outcome and latency, and how long audit-log writes take (Statistics → Activity) |
| Platform health (platform administrators) | API requests and latency, the job queue, which replica runs the scheduler and lease losses, goroutines, heap, CPU and garbage-collection pauses per service, database connection pool use and waits, dropped series and log lines, failed metric exports, sign-in attempts by method and outcome, failed audit-log writes and archiving runs |
| Web server (platform administrators) | requests the dashboard server answers by status, its latency, and dashboard calls that never reached the API (unreachable, timed out or refused), all labelled by page or route pattern, never the address you opened |

Some numbers are estimates and are labelled so: cost depends on the prices you configured, and a call without reported usage or a known price shows as a gap, never as zero.

### Token savings

Agents re-send their whole context every turn, and most of it comes back from the provider's prompt cache. CodeSynapse neither causes nor removes those cache reads, so they are left out of the savings figures:

- **Work tokens** are the agent's fresh input, cache writes and output.
- **Share of agent work avoided** is avoided tokens ÷ (work tokens + avoided tokens). It reads "unknown", not 0, when a turn did not report its cache fields.
- Answers about lines the session already had count for nothing, and agents whose hooks cannot tell those apart yet (older CLIs) are left out of the share and the saved cost.
- An **upper figure** also counts the avoided context the agent would have re-sent from its cache. It is shown apart, labelled, and never used in tiles or totals.
- The saved cost values each avoided token once, at the cache-write price for providers that cache by default and the input price otherwise. The provider's own prompt-cache saving on CodeSynapse's model calls is shown apart and never added to it.

## What is never measured

No metric holds prompts, answers, source code, file paths, repository names, branch names, people, email addresses, API keys, session IDs or client addresses. Labels only take values from fixed lists, such as the names of CodeSynapse's own tools or the models you configured. The one exception is the per-repository file counts on the Ingestion page, which use an opaque ID, not the repository name.

## Alerts for operators

Whoever runs the installation can turn on alerts: an optional vmalert checks rules against these numbers every minute and sends what fires to an Alertmanager-compatible receiver they run. The rules cover slow or failing databases, jobs no worker picks up, a dashboard that cannot reach the API, metrics or logs being lost, failing remote clones and fetches, rate-limited lookups, bursts of failed sign-ins, and failing audit-log writes or archiving. Alerts add up organisations and never name repositories, files or people. [Alerting](../operations/alerting.md) explains how to turn it on.

## What is planned

Some parts of CodeSynapse have no live numbers yet. The [metrics coverage plan](../TELEMETRY_PLAN.md#metrics-coverage-plan) lists them and the order in which they will be added:

1. **Platform health** for operators (done): see the table above and Administration → Overview.
2. **More operator alerts** (done): each later step adds the alerts for its numbers.
3. **Background jobs and indexing** (available now, see the table above).
4. **Outside services and security events** (available now, see the table above).
5. **Sharper existing numbers** (available now, see the table above): refused tool calls apart from errors, model timeouts and rate limits, gateway latency and why a request left the local model, and tool calls answered on your own machine by the Claude Code bridge. A refused tool call is no longer shown as an error, and the model failure panels count timeouts, rate limits and refusals as failures.
6. **The web server** (available now, see the table above).

The same privacy rules apply to everything planned.

Operator alerts are for whoever runs the installation. Warnings for people in an organisation keep appearing in the dashboard's notification feed.

## Related

- [Telemetry plan](../TELEMETRY_PLAN.md): every instrument, the dashboard queries, traces, logs and the rules behind them.
- [Indexing policy and reports](indexing-policy.md): the per-run file report behind the Ingestion page.
