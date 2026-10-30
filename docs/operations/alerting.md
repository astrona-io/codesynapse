# Alerting

!!! info "In plain words"
    CodeSynapse can tell you when something is wrong with the installation itself: a database answering slowly or failing, jobs that no worker picks up, or metrics and logs being lost. A small program, vmalert, checks a set of rules every minute against the numbers CodeSynapse already collects and sends what it finds to an alert receiver you run, such as Alertmanager. It is off until you turn it on. Alerts never name repositories, files, people or anything an organisation stored.

Alerts are for whoever runs the installation. Warnings for people in an organisation stay in the dashboard's notification feed.

## How it works

- vmalert reads the rules in `deploy/alerts/*.yaml` and evaluates them against the internal VictoriaMetrics, once a minute.
- What fires goes to one Alertmanager-compatible receiver (Alertmanager itself, or anything that accepts its API). Grouping, silences, routing to chat or paging stay in that receiver.
- Every alert carries `severity` (`warning` or `critical`) and `component` (`telemetry`, `stores` or `jobs`) and links to the dashboard's Statistics page (`/stats` on your public URL). The Helm chart also adds `environment`.
- The receiver's address and credentials come only from the environment (Compose) or a Kubernetes Secret (Helm). They are never command-line flags or chart values, and vmalert's own log shows them as `secret`.
- Only the API connects to vmalert, and only to read what fires for the dashboard (see [below](#see-alerts-in-the-dashboard)). In Compose it listens on port 8880 of the internal network, which is never published to the host; in Kubernetes a NetworkPolicy admits API pods on that port and refuses everything else.

## Turn it on with Docker Compose

1. Put the receiver in `.env`:

    ```sh
    ALERTMANAGER_URL=https://alertmanager.example.com
    # Optional, one of:
    ALERTMANAGER_USERNAME=codesynapse
    ALERTMANAGER_PASSWORD=...
    ALERTMANAGER_BEARER_TOKEN=...
    ```

2. Start the stack with the `alerting` profile:

    ```sh
    COMPOSE_PROFILES=alerting just up
    ```

    Without the profile, `just up` starts everything else exactly as before. vmalert refuses to start when `ALERTMANAGER_URL` is empty, so a missing receiver shows up at once (`docker compose logs vmalert`).

## Turn it on with Helm

1. Add the receiver to the chart's Secret (`global.existingSecret`), or to a Secret of its own named in `alerting.existingSecret`:

    | Key | Required | Meaning |
    |---|---|---|
    | `ALERTMANAGER_URL` | yes | Base URL of the receiver |
    | `ALERTMANAGER_USERNAME`, `ALERTMANAGER_PASSWORD` | no | Basic authentication |
    | `ALERTMANAGER_BEARER_TOKEN` | no | Bearer token instead |

    ```sh
    kubectl create secret generic codesynapse-alerting \
      --from-literal=ALERTMANAGER_URL=https://alertmanager.example.com \
      --from-literal=ALERTMANAGER_BEARER_TOKEN="$TOKEN"
    ```

2. Turn it on:

    ```yaml
    alerting:
      enabled: true
      existingSecret: codesynapse-alerting   # empty: global.existingSecret
      evaluationInterval: 1m
    ```

The chart refuses to render alerting without a metrics store (`victoriametrics.enabled`, or `victoriametrics.externalURL` for one you run). vmalert reads it with `METRICS_PASSWORD` from the chart's Secret, like the services. It runs as one replica: a second would send every alert twice.

## See alerts in the dashboard

Platform administrators see what vmalert evaluates under Administration → Statistics → Alerts: alerts firing and pending with their value and since when, and every alerting rule with its group, health and last evaluation. The API reads vmalert's `/api/v1/alerts` and `/api/v1/rules` when the page asks and passes on only names, states, severity, value and times; other labels and annotations stay in vmalert.

The API finds vmalert through `CODESYNAPSE_ALERTS_URL`, read from the environment only. It must be https to any host, or http to an internal service name (`vmalert`, `name.namespace.svc`, `name.namespace.svc.cluster.local`), with no credentials, path, query or fragment; the API refuses to start otherwise. It follows no redirects, reads at most 1 MiB and gives up after 5 seconds.

- **Compose:** set `CODESYNAPSE_ALERTS_URL=http://vmalert:8880` in `.env` beside the `alerting` profile.
- **Helm:** with `alerting.enabled` the chart sets it to the in-chart vmalert Service. For a vmalert you run yourself, set `alerting.externalURL` (for example `http://vmalert.monitoring.svc:8880`) and keep `alerting.enabled` off.

Unset, the tab says alerts are off and nothing else changes.

## The rules

| Alert | Severity | Fires when |
|---|---|---|
| `CodeSynapseStoreSlow` | warning | The 95th percentile of NebulaGraph, Milvus or PostgreSQL round trips is above 1 s for 10 minutes |
| `CodeSynapseStoreErrors` | critical | More than 5% of a store's round trips fail for 10 minutes, with at least one call a minute |
| `CodeSynapseJobsNotClaimed` | warning | Jobs of a kind have waited 15 minutes with none of that kind running: no worker with that role is up |
| `CodeSynapseSchedulerLeaseLost` | warning | An API replica lost the scheduler lease in the last 15 minutes |
| `CodeSynapseNoSchedulerHolder` | critical | No API replica has held the scheduler lease for 10 minutes, so scheduled syncs, watches and audit sealing stop |
| `CodeSynapseMetricsExportFailing` | warning | Metric exports of an exporter (otlp, prometheus, ingest) failed for 15 minutes |
| `CodeSynapseJobFailureRate` | warning | More than 20% of a kind's jobs failed in the last 30 minutes, with at least 10 jobs |
| `CodeSynapseSeriesCapOverflow` | warning | An instrument reached its series cap in the last hour, so its panels undercount |
| `CodeSynapseLogShippingDropping` | warning | Log lines have not reached the log store for 15 minutes |
| `CodeSynapseIngestMetricsRejected` | warning | Indexing file totals were refused before export in the last hour |
| `CodeSynapseIngestMetricsCapacity` | warning | Indexing file totals reached their repository limit in the last hour |
| `CodeSynapseGitFailing` | warning | More than 20% of a git operation's calls (clone, fetch, ls_remote, push) failed or timed out in the last 30 minutes, with at least 10 calls |
| `CodeSynapseLookupsRateLimited` | warning | osv.dev, deps.dev or a git host's API has rate limited lookups for 15 minutes |
| `CodeSynapseSignInBurst` | warning | More than 30 sign-ins of one method (password, api_key, session, sso) failed or were rate limited in 10 minutes |
| `CodeSynapseAuditWriteFailing` | critical | Audit entries from the API, the MCP server or background work could not be written in the last 10 minutes, so those requests were refused |
| `CodeSynapseAuditSealFailing` | warning | Sealing the audit log into the archive failed in the last 25 hours |
| `CodeSynapseAuditNotArchived` | warning | Two retention runs found sealed audit files but no archive configured, so old entries are not pruned |
| `CodeSynapseDashboardAPIUnreachable` | critical | More than 5% of the dashboard's API calls find the API unreachable or time out for 10 minutes, with at least one call a minute |

Store alerts add up every organisation's calls per store, so no organisation appears in them. Git, lookup, sign-in and audit alerts add up every organisation's numbers too. Other alerts read installation-wide numbers. The only labels an alert carries besides `severity`, `component` and `environment` are the store (`db_system`), the job kind, the instrument name, a fixed reason, and the fixed git operation, lookup service, sign-in method or audit source.

More rules follow as the numbers they need are added. The [telemetry plan](../plans/TELEMETRY_PLAN.md#alerting) lists which phase each waits on.

## Change or add a rule

Rules are plain [vmalert rule files](https://docs.victoriametrics.com/vmalert/). Edit `deploy/alerts/*.yaml` and add a case to `deploy/alerts/tests/alerts.test.yaml`, then run:

```sh
just lint-alerts
```

It parses the rules with the vmalert release Compose ships (`-dryRun`), runs the unit tests with the matching vmalert-tool, and refuses a rule that groups by a label other than `metric`, `reason`, `db_system`, `le` or `kind`, uses `without`, or has no aggregation at all. CI runs it on every pull request as part of `just checks`.

The Helm chart packages the same files (`deploy/helm/codesynapse/alerts` links to `deploy/alerts`). To use rules of your own, keep alerting off in the chart and run vmalert or another evaluator against the metrics store yourself.
