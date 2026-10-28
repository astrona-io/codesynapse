# Alerting

!!! info "In plain words"
    CodeSynapse can tell you when something is wrong with the installation itself: a database answering slowly or failing, jobs that no worker picks up, or metrics and logs being lost. A small program, vmalert, checks a set of rules every minute against the numbers CodeSynapse already collects and sends what it finds to an alert receiver you run, such as Alertmanager. It is off until you turn it on. Alerts never name repositories, files, people or anything an organisation stored.

Alerts are for whoever runs the installation. Warnings for people in an organisation stay in the dashboard's notification feed.

## How it works

- vmalert reads the rules in `deploy/alerts/*.yaml` and evaluates them against the internal VictoriaMetrics, once a minute.
- What fires goes to one Alertmanager-compatible receiver (Alertmanager itself, or anything that accepts its API). Grouping, silences, routing to chat or paging stay in that receiver.
- Every alert carries `severity` (`warning` or `critical`) and `component` (`telemetry`, `stores` or `jobs`) and links to the dashboard's Statistics page (`/stats` on your public URL). The Helm chart also adds `environment`.
- The receiver's address and credentials come only from the environment (Compose) or a Kubernetes Secret (Helm). They are never command-line flags or chart values, and vmalert's own log shows them as `secret`.
- Nothing connects to vmalert. In Compose it listens inside its container only; in Kubernetes a NetworkPolicy refuses every connection to it.

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

Store alerts add up every organisation's calls per store, so no organisation appears in them. Other alerts read installation-wide numbers. The only labels an alert carries besides `severity`, `component` and `environment` are the store (`db_system`), the job kind, the instrument name and a fixed reason.

More rules follow as the numbers they need are added: failing audit-log writes. The [telemetry plan](../TELEMETRY_PLAN.md#alerting) lists which phase each waits on.

## Change or add a rule

Rules are plain [vmalert rule files](https://docs.victoriametrics.com/vmalert/). Edit `deploy/alerts/*.yaml` and add a case to `deploy/alerts/tests/alerts.test.yaml`, then run:

```sh
just lint-alerts
```

It parses the rules with the vmalert release Compose ships (`-dryRun`), runs the unit tests with the matching vmalert-tool, and refuses a rule that groups by a label other than `metric`, `reason`, `db_system`, `le` or `kind`, uses `without`, or has no aggregation at all. CI runs it on every pull request as part of `just checks`.

The Helm chart packages the same files (`deploy/helm/codesynapse/alerts` links to `deploy/alerts`). To use rules of your own, keep alerting off in the chart and run vmalert or another evaluator against the metrics store yourself.
