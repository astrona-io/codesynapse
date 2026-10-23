# Ingestion transparency and pruning reports

Status: implemented, SaaS phase 4i. This builds on [repository policy](REPO_CONFIG_PLAN.md), [repository permissions](ACCESS_CONTROL_PLAN.md), [telemetry](TELEMETRY_PLAN.md) and [execution identity](SESSION_CONTEXT_PLAN.md).

## File accounting

`internal/ingestreport` maintains one ledger per ingestion run. Its run ID is separate from the immutable runtime session ID. API jobs inherit and validate their original tenant, subject and execution session; the standalone CLI creates only a local accounting run ID.

Discovery reads directory entries and size metadata without following symlinks. It records at most 10,000 encountered files, with a maximum serialized report size of 16 MiB. Git metadata, nested repositories and policy-pruned directories are not enumerated. Their descendants are unknown: they increment neither scanned nor skipped totals. Reports expose `discovery_complete` and `pruned_directories`; limits and traversal errors make discovery incomplete.

Each terminal file has one outcome:

| Outcome | Evidence |
|---|---|
| `indexed` | Successful eligible extraction contributed to a confirmed committed graph snapshot |
| `skipped` | Effective policy excluded the entire encountered file |
| `failed` | A recorded extraction stage failed, or graph persistence is known to have failed |
| `unprocessed` | No completed contribution, including unsupported formats, cancellation, limits or an unknown commit |

Stage outcomes are recorded separately for Go, other languages, documentation, configuration, architecture, dependencies and Kubernetes scanning. Read errors, unavailable metadata and size limits on an eligible extraction stage are failures. A failed stage prevents a file being counted as successfully indexed even if other metadata reached the graph; `contribution` retains that distinction. Excluding Kubernetes detection does not skip general YAML extraction. Unsupported files are unprocessed, not inferred to be binary.

Terminal reports validate these identities before persistence:

```text
total_files_scanned = files_indexed + files_skipped + files_failed + files_unprocessed
files_skipped = sum(skip_breakdown)
files_indexed = files_written + files_reused
```

Current ingestion replaces a repository snapshot, so confirmed contributions are writes and `files_reused` is zero. The schema reserves reuse accounting for a future backend that can prove reuse. A successful run can still contain failed or unprocessed files; its success describes confirmed graph publication, not complete extraction of every encountered file.

## Skip reasons and estimates

The fixed reason vocabulary is `ignored_path`, `generated_code`, `binary`, `non_k8s_yaml`, in that precedence order. Current whole-file decisions emit `ignored_path`. The other reasons are reserved for explicit future classifiers or policies; this phase does not add content reads or new exclusions to manufacture those counts. Generated-file paths excluded by reviewed glob rules are therefore reported as `ignored_path`. Kubernetes-only exclusions appear as scanner-stage decisions.

`estimated_tokens_saved_on_ingest` is a hypothetical raw-context estimate, shown as **Estimated context tokens excluded**. It uses metadata bytes divided by four for excluded text candidates selected by extension, with a 10 MiB per-file size ceiling. Extensions identify candidates, not verified text. The report records estimator name/version, byte assumption, baseline, covered and unknown files, observed bytes and partial coverage. Binary classifications are excluded. No covered baseline produces `null`, not zero.

No ignored content is opened for the estimate. It does not extrapolate pruned directories or count scanner-only exclusions and reused contributions as saved context. It measures neither billed tokens nor cloud traffic. The earlier illustrative 1,850,000-token figure is not a measured result or a default. Actual retrieval reduction remains covered by the [phase 4f benchmarks](RETRIEVAL_PLAN.md).

## Durable reports and graph receipts

PostgreSQL migration `0009_ingest_reports.sql` adds tenant-owned runs, cumulative totals and a permanent metric-stream identity. The memory accounts adapter implements the same contract. A queued reservation, each checkpoint and final accounting update run under the existing policy transaction. Audit records `ingest.started` and `ingest.completed` commit with accounting. Starting work and publishing checkpoints require the original registered execution identity and current repository administrator permission; the API also rechecks the original credential.

The inventory is checkpointed before content parsing. A prepared checkpoint records file contributions before graph publication. The shared graph contains only `Repository.meta.ingest_receipt`, a hash of the organisation, graph and run identity. Tenant counts, file details and session provenance remain in accounts storage. The existing shared `Repository.metrics` code-quality data is unchanged; the ingestion API exposes its logical tenant-owned `metrics` object from the last successful report.

Publication reconciles prior receipts under the policy lock before replacing the graph. If the graph committed but the process died before its PostgreSQL transaction committed, recovery finalizes the prepared report from that receipt. It does not repeat the graph write or reactivate the originating user/session. Duplicate finalization cannot increment totals twice. An unknown acknowledgement alone cannot count a file as indexed.

Recovery checks every 15 seconds. API jobs have a one-hour lifetime; unmatched pending runs older than two hours close with an unknown commit. A graph read failure leaves the run pending for a later check. External CLI writers do not participate in the accounts publication lock: an overwritten receipt cannot prove a past write and remains unknown. Run through the API when durable tenant accounting is required.

Reports retain the newest 50 terminal attempts plus the last successful report if older. Summary reads omit per-file payloads at the store; details are paginated separately. Unlinking a repository removes terminal report payloads and its latest-success pointer, while keeping replay tombstones and cumulative identities. A pending checkpoint stays internal until reconciliation and cannot authorize any work.

Permanent replay guards have installation/tenant/repository limits of 100,000 / 8,192 / 2,048 runs. Pending runs are limited to 16 globally, four per tenant and one per tenant repository entry. Capacity exhaustion refuses new ingestion; it never forgets a run to permit duplicate accounting. These are explicit operational limits, not an unlimited retention promise.

## Metrics and VictoriaMetrics

The exporter sends durable absolute totals without replaying increments into the ordinary in-process accumulator:

| Metric | Accounting point |
|---|---|
| `codesynapse_files_scanned_total` | Newly encountered entries in persisted checkpoints |
| `codesynapse_files_indexed_total` | Confirmed written contributions at finalization |
| `codesynapse_files_skipped_total` | Newly recorded whole-file exclusions in persisted checkpoints |

Counters describe processing across runs, not unique files currently in a repository. Each carries authenticated `tenant_id`, a stable server-owned opaque `repo` ID, and `mcp_tool`, `llm_provider`, `llm_model` set to `none`. Skips add a fixed `skip_reason`. Repository names, paths, run/session IDs, source revisions and model text never become labels.

Admission allows 800 repositories globally and 64 per tenant, with six series per repository. IDs and admission slots survive unlink/relink. Overflow preserves exact reports, marks `metric_coverage` false and emits a platform-only dropped-series count. The opaque `repo` label is limited to these three instruments; it is not a general exception to telemetry label rules.

One fenced exporter owns a database stream through the network call. Its persisted stream ID, counter start times and sample timestamps survive process restarts. It uses the configured OTLP/HTTP transport by default, or the existing explicit Prometheus push mode, never both for one stream. A failed export leaves accounting intact. Repeated snapshots and replacement exporters preserve the same cumulative series. The UI flags missing or stale telemetry; report totals remain authoritative.

`GET /api/v1/stats/series` adds fixed `ingest_files` and `ingest_skips` panels. Each requires a repository selector with current read permission, then passes both tenant and opaque repository ID as VictoriaMetrics `extra_filters`. Platform administration grants no repository panel access. The `annotation_jobs` panel is explicitly organisation-wide and uses the existing annotation outcome counters.

## API, MCP and dashboard

All report reads require repository `READ` permission in the selected organisation, including when organisations share one code graph:

- `GET /api/v1/repos/{repo}/ingestion`: latest attempt, last successful report, logical `metrics`, and history. History defaults to 10 entries, capped at 50, with a `before` run cursor.
- `GET /api/v1/repos/{repo}/ingestion/{run}`: one summary and file outcomes; default 100 files, capped at 500, with `offset` and `next_offset`.
- MCP `ingestion_report`: read-only latest five summaries and last successful report. It uses the existing authenticated tool policy, runtime session and repository scope. It omits file lists and originating session identity. See [the agent guide](AGENT_GUIDE.md).

The repository **Ingestion reports** page separates the latest attempt from the last successful run. It shows publication state, revision/policy evidence, outcome totals, exclusion reasons, discovery limits, estimate provenance, paginated files/history and processing trends. Annotation status links to **Indexing policy** and remains independent of deterministic file accounting. Local annotation continues to use the phase 4h registry, redaction, revision and permission checks; model output cannot alter file outcomes or counters.

The standalone CLI always logs an accounting summary. To save full local details, choose a new file:

```sh
codesynapse ingest /path/to/repository --ingest-report /tmp/ingestion-report.json
```

Existing output files are refused and new files use mode `0600`. Local reports do not create tenant counters or PostgreSQL history. A failed or ambiguous store write is reported conservatively; CLI reports have no background reconciliation.

## Validation

Shared accounts contracts exercise atomic checkpoints, duplicate completion, replay protection after retention, recovery after session revocation, unlink/relink identity, metric admission and exporter fencing. API/MCP tests cover tenant isolation, graph acknowledgement loss, revoked access and repository-scoped metric queries. Ledger tests cover pruned subtrees, symlinks, unsupported/generated/binary candidates, scanner exclusions, malformed content, missing stage evidence, cancellation and estimate bounds. VictoriaMetrics integration tests cover both transports, duplicate delivery, restart identity, cumulative increases and tenant/repository filters.

A local validation benchmark for 1,000 file entries measured 0.373 ms/op, 297,533 B/op and 26 allocations/op on an Apple M5 Pro. This measures report validation only; it is not an ingestion latency SLA.

Required checks are the Go race suite, lint, vet, web typecheck/build, PostgreSQL contracts and graph/metrics integration tests. No dependencies or Kubernetes manifests are added in this phase. The next application phase is [the code architecture audit](CODE_ARCHITECTURE_AUDIT_PLAN.md), followed by SSO; deployment work remains last.
