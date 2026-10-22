# Ingestion transparency and pruning reports

Status: planned, SaaS phase 4i. Extends [repository policy](REPO_CONFIG_PLAN.md) in phase 4h, [tenant access](ACCESS_CONTROL_PLAN.md), [telemetry](TELEMETRY_PLAN.md), [retrieval benchmarks](RETRIEVAL_PLAN.md) and [immutable execution context](SESSION_CONTEXT_PLAN.md). The counters, report schema and annotation behavior below are proposed contracts, not claims about the current ingest pipeline.

## File accounting

Create one bounded outcome ledger per logical ingestion run, keyed by the authenticated repository entry, source revision, run identity and normalized repository-relative file path. A fresh requested run receives its own job identity; a retry or worker restart retains the original run. Job identity never replaces the inherited primary session ID.

Record a file as scanned once when the walker encounters its entry. Count files, not directories, graph nodes, parser invocations or retry attempts. Do not follow symlinks. Metadata-only discovery may establish an entry without opening its contents. Report the discovery scope, including whether ignored directory contents were enumerated.

If the walker prunes an ignored directory before visiting its children, those children are unknown and cannot increment scanned or skipped counts. Record the pruned-directory count separately. An optional bounded metadata-only inventory can count permitted entries, but must obey the same filesystem restrictions and may not read ignored contents just to improve statistics. Reaching inventory limits makes discovery incomplete; do not extrapolate a complete file count.

Each encountered file has one final outcome:

| Outcome | Meaning |
|---|---|
| `indexed` | All required eligible extraction stages succeeded and their file contribution was committed to the selected graph backend |
| `skipped` | Deliberate whole-file exclusion under one of the reviewed reasons below; no index contribution was committed |
| `failed` | Extraction or graph persistence failed; include whether a partial contribution needs cleanup |
| `unprocessed` | Encountered but never completed, including cancellation, a limit or an unsupported file class |

During execution, files may be pending. At terminal completion, pending entries become failed or unprocessed as appropriate. The report must satisfy:

```text
total_files_scanned = files_indexed + files_skipped + files_failed + files_unprocessed
files_skipped = sum(skip_breakdown)
```

Only a complete, successful run with no failures or unprocessed entries can simplify this to scanned = indexed + skipped. Discovery-incomplete runs expose that status even if every encountered file has a terminal outcome. A successful parse alone does not count as an indexed file. A reused, unchanged contribution is indexed only after the backend confirms it belongs to the committed run snapshot; separately report actual writes and reused contributions so reuse is not presented as new database writes.

Maintain separate stage decisions when several parsers inspect the same file. Excluding a YAML document from Kubernetes detection does not skip the whole file if general YAML/config extraction still commits useful metadata. Mixed-document YAML with any accepted contribution is indexed; its rejected Kubernetes documents remain stage-level statistics. `non_k8s_yaml` is a file skip reason only when the effective policy excludes the entire file from every eligible indexing path.

## Requested metrics and skip reasons

Export these cumulative file counters through the phase 4e OTel/VictoriaMetrics path:

| Exported metric | Increment condition |
|---|---|
| `codesynapse_files_scanned_total` | First encounter of a distinct file entry in a logical run |
| `codesynapse_files_indexed_total` | First confirmed successful graph write of the file's eligible contribution in that run, to Neo4j or HugeGraph |
| `codesynapse_files_skipped_total` | First terminal whole-file skip decision in that run |

The report's indexed count includes confirmed reused contributions; this requested indexed-write counter counts actual successful writes only. Publish `files_written` and `files_reused` in the report, with `files_indexed = files_written + files_reused`, and label dashboards accordingly. Do not compare a write-only counter to the report's indexed total without including reuse.

Every instrument has authenticated `tenant_id` and server-resolved `repo`, plus the phase 4e `mcp_tool`, `llm_provider` and `llm_model` attributes. File accounting describes ingestion, so provider/model are `none`; the tool is an accepted originating tool or fixed `none` for API/scheduled work. The skipped counter additionally has exactly one `skip_reason` from this initial allowlist:

| `skip_reason` | Whole-file decision |
|---|---|
| `ignored_path` | An encountered file is excluded by an effective ignore policy |
| `non_k8s_yaml` | The policy permits only Kubernetes YAML for this indexing path and the file has no eligible contribution elsewhere |
| `generated_code` | The effective policy excludes a recognized generated file |
| `binary` | A bounded, approved classifier identifies a binary file that no enabled extractor indexes |

Choose one primary reason in deterministic order: ignored path, generated code, binary, then non-Kubernetes YAML. Evaluate only classifiers permitted by policy; a path decision must not trigger a content read. Record additional stage decisions only in the scoped report. Parse errors, store errors, missing permissions, cancellation and size limits are not successful skips. An unsupported text format remains unprocessed with a fixed diagnostic until an explicit skip category is designed; never mislabel it binary. Review any enum extension, rather than accepting arbitrary strings from configuration or a model.

The `repo` attribute is an explicit, narrow extension to the current bounded-label rules: an opaque, stable, server-owned repository-entry ID, never a repository name, URL, path, shared graph ID, session ID or model argument. Validate that it belongs to `tenant_id`. Retain display names in authorized API reports and UI lookup data only. Update `AGENTS.md` with this scoped exception when implementing it.

Opaque IDs still have cardinality. Admit repository series through a capped registry with per-tenant and installation-wide budgets, a bounded retention/churn policy and limits for the entire tool/provider/reason combination. Preserve tenant identity and never merge overflow into another tenant's series. If admission fails, retain the exact report, emit a bounded platform-only dropped-series signal and show metric coverage as incomplete. IDs must not rotate on each run. Session, job, revision, policy hash, filename and annotation text are never metric labels.

## Retries, persistence and reconciliation

Commit a ledger transition and its cumulative metric totals atomically in the job/report store, with a uniqueness guard on run/file/outcome. Graph writes use an idempotent file/run commit receipt or snapshot marker. After a crash between graph commit and ledger update, reconcile the receipt before marking the file indexed; an ambiguous backend acknowledgement is not proof of success. A batch failure must not mark every attempted file indexed. Retry counters for diagnostics are separate from file outcome counters.

Export persisted cumulative totals from one fenced owner per stream, preserving counter identity across restarts. Do not increment a second in-process counter by replaying ledger events. Validate counter start times, restart behavior and duplicate OTLP delivery against VictoriaMetrics. The ledger/report is the accounting authority; telemetry transport is not an exactly-once transaction with the graph. An exporter outage does not roll back ingestion, and the UI exposes telemetry lag or incomplete series coverage.

Keep the original report for each completed run; a new ingest does not overwrite historical counts. Label aggregate trends as file processing across runs, rather than unique files in the repository. Bound report/ledger retention and avoid storing raw source, YAML values, secrets or unredacted diagnostic bodies.

## End-of-ingest `Repository.metrics` report

Expose a logical `Repository.metrics` JSON object on the authorized organisation repository entry, backed by a tenant-owned ingestion report. The example below is illustrative arithmetic, not a measured CodeSynapse result:

```json
{
  "total_files_scanned": 1420,
  "files_indexed": 380,
  "files_skipped": 1040,
  "skip_breakdown": {
    "ignored_path": 850,
    "non_k8s_yaml": 120,
    "generated_code": 70
  },
  "estimated_tokens_saved_on_ingest": 1850000
}
```

This example assumes complete discovery, no failures or unprocessed entries, and a declared estimate with adequate baseline coverage. A zero binary count can be omitted from the compact breakdown. The full versioned report also carries files written/reused/failed/unprocessed, completion and discovery status, pruned-directory counts, run/revision, effective policy fingerprint, extractor versions, timestamps, graph-commit status and the token-estimate provenance below. Validate totals before publication. Preserve the last successful report separately from the most recent failed attempt.

The existing graph `Repository` node can be shared by several organisations. Do not put tenant-specific ignore rules, suppressions, annotations, session provenance or per-tenant counts into a shared `Repository.metrics` property. Store tenant reports under their organisation-owned repository entries in the accounts/report store. A shared graph aggregate is allowed only for the exact same source revision, effective extraction policy and extractor versions, and contains no tenant-specific metadata. Different effective policies require separate ingestion artifacts or a policy-aware isolation design before deployment; one tenant must never prune another tenant's shared graph.

Authorize report reads and MCP summaries through phase 4d repository `READ`; starting or changing ingestion requires `ADMIN`. Every lookup resolves the organisation entry rather than reading a report by shared graph ID alone. Apply the same scope to report history, failures, pagination and counts. A platform administrator without repository permission cannot see pruning paths or policy details. MCP report access remains read-only and bounded; define its tool/API shape and update `docs/AGENT_GUIDE.md` during implementation.

## Token estimate and evidence

`estimated_tokens_saved_on_ingest` estimates context tokens excluded from a declared hypothetical raw-text ingestion/context baseline. It is not billed tokens, provider-cache savings, runtime model consumption or cloud bytes avoided. Many ingestion stages consume no model tokens at all. Use the UI label “Estimated context tokens excluded” and link to the baseline definition.

Record the estimator name/version, intended tokenizer or bytes-per-token assumption, baseline definition, observed metadata bytes, eligible file classes, covered-file count, unknown-file count and uncertainty/coverage. For safe text candidates whose size metadata is already available, a documented bytes-based estimator can approximate excluded text without opening it. Treat it as an estimate, not a tokenizer measurement. Exclude binary files and classes for which text size is not a defensible baseline. Never traverse pruned trees, read ignored secrets, open huge payloads, follow symlinks or send content to a model to manufacture savings.

Compute the estimate once per excluded file and sum only comparable, covered estimates. Reused index contributions and scanner-only Kubernetes exclusions are not excluded context. An unknown baseline is `null` with an explanation, not zero. Partial coverage produces a clearly labeled partial estimate; do not scale it to the whole repository. The illustrative 1,850,000 figure requires its own declared assumptions and must not become a default value or a product claim. Use phase 4f's controlled workload when comparing actual returned context and answer quality.

## Annotation jobs and context propagation

Make the report's counts deterministic before optional annotation. An annotation job can explain pruning decisions or summarize existing metadata, but cannot edit counters, choose skip reasons, relax ignore policy or silently mark a failed file indexed. Keep annotation status and failure separate from ingestion status. Treat configuration descriptions and model output as untrusted data; validate output shape and repository-relative paths and redact free text before persistence.

Queue envelopes inherit the authenticated tenant, subject, repository entry, source revision, effective policy, immutable `session_id` and matching `parent_session_id` from phases 4g/4h. The runtime supplies that identity, including for scheduled work; missing parent context rejects queue insertion and execution. Record job/attempt IDs separately. Recheck session validity, repository permission and private-repository access before reading inputs and before storing results. A stale revision or changed policy must cancel or explicitly supersede the annotation rather than attaching it to a newer report.

Use only the existing LLM registry, redaction and local-provider restrictions. Do not send repository source to a cloud model for annotations. Persist bounded, redacted provenance: producing component/model, input report version, source/policy identity, timestamps and scoped execution references. Provenance is access-controlled report/audit data, never VictoriaMetrics labels. Retry annotation writes idempotently and keep conversation turns transient. Phase 4g session-memory storage remains separate from repository-level ingest reports; omit session details from reports served to unrelated repository readers.

## Dashboard and implementation sequence

Add an ingestion detail panel showing scanned, indexed, written/reused, skipped by reason, failed and unprocessed files, discovery coverage, last successful run and the latest attempt. Link every displayed total to its report and effective policy. Use fixed MetricsQL templates through the existing tenant-filtered series endpoint for trends; authorize any repository selector before binding its opaque ID. VictoriaMetrics `extra_filters` must still enforce tenant scope. A latest-run report is the source for exact totals; counter increases show activity across runs and cannot reconstruct one run without a report.

1. Define the outcome ledger/report schema and effective-policy identity with phase 4h. Inventory current parser decisions, including scanner-only exclusions and unsupported file types, before enabling the initial reason enum.
2. Implement bounded discovery and stage accounting, graph commit reconciliation, exact report publication and retention. Keep policy-specific data isolated from shared graph nodes.
3. Add the three requested counters, admitted repository-ID labels and cumulative exporter recovery under phase 4e. Document exact exported names, units and coverage behavior.
4. Add the optional annotation workflow, scoped API/MCP report summaries and dashboard. Carry phase 4g context throughout and keep annotation failures independent of ingest accounting.
5. Test mixed YAML, ignored subtrees, symlinks, generated text, binary files, classifier overlap, size limits, reused snapshots, partial writes, cancellation, duplicate deliveries, worker restarts and backend acknowledgement loss. Test two tenants sharing a graph with different policies, revoked access mid-job, a foreign report ID, missing/forged session context and metric-series overflow. Verify no ignored contents are read for estimates and unavailable baselines remain unavailable.
6. Run `just test`, `just lint`, graph/store contracts, PostgreSQL report contracts and VictoriaMetrics integration checks; run `just web-check` for the UI. Measure ledger, inventory and telemetry overhead on representative repositories. Update `AGENTS.md` and `docs/AGENT_GUIDE.md` with implemented contracts before rollout.
