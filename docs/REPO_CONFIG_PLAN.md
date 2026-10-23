# Repository configuration and asynchronous annotation

Status: implemented, SaaS phase 4h. CodeSynapse loads an optional repository-root `.codesynapse.yaml` for indexing exclusions, scanner controls and requests for local annotation. Repository content supplies requests; an authenticated administrator supplies approval.

This phase uses [repository permissions (4d)](ACCESS_CONTROL_PLAN.md), [retrieval isolation (4f)](RETRIEVAL_PLAN.md) and [immutable execution context (4g)](SESSION_CONTEXT_PLAN.md). Annotation metrics use the bounded [telemetry pipeline (4e)](TELEMETRY_PLAN.md). Detailed file-pruning dashboards and token-savings accounting remain phase 4i.

## Version 1 configuration

```yaml
version: 1

# Path exclusions for graph ingestion and vector embedding (saves tokens & memory)
ignore:
  paths:
    - "vendor/**"
    - "node_modules/**"
    - "dist/**"
    - "docs/generated/**"
    - "test/fixtures/**"

# Domain-specific scanner & tool controls
scanners:
  kubernetes:
    enabled: true
    # Exclude non-K8s YAML files from K8s rules and K8s agent tool outputs
    ignore_paths:
      - "ci/*.yaml"
      - "mkdocs.yml"
      - ".golangci.yml"
      - "config/app_config.yaml"
  security:
    ignore_rules:
      - rule_id: "sec-yaml-plain-secret"
        paths: ["test/mock_secrets.yaml"]

# Async AI Annotation Agent controls
annotation:
  enabled: true
  provider_task: "code-annotation"
  max_batch_size: 20
  ignore_paths:
    - "**/*_easyjson.go"
    - "pkg/pb/*.pb.go"
  taxonomy_hints:
    - "billing"
    - "auth"
    - "k8s-operator"
    - "telemetry"
```

`sec-yaml-plain-secret` is a compatibility alias for the existing catalogue rule `yaml-hardcoded-secret`. The parser normalizes it and returns a diagnostic. Unknown rules fail validation; stored finding IDs retain their catalogue spelling.

## Parser and defaults

`internal/config/repo.go` owns configuration loading and validation. It reuses the existing YAML dependency and provides the immutable policy used by ingestion and annotation. The generic YAML extractor remains in `internal/ingest/config`.

The loader reads through a root-confined file handle, rejects symlinks and non-regular files, and caps the configuration at 64 KiB. Exactly one document with `version: 1` is accepted. Duplicate or unknown keys, aliases, merge keys, custom tags, invalid types and unsupported versions fail validation. Parsing stops at 16 levels or 4,096 YAML nodes. Errors identify schema fields and line numbers without echoing arbitrary values.

An absent file preserves existing extractor defaults: Kubernetes scanning is enabled and annotation is disabled. Omitted booleans remain distinct from explicit `false`. Annotation defaults to task `code-annotation` and batch size 20; enabling it in the file still requires a matching tenant approval and a local provider. The normalized configuration has a SHA-256 digest, also used for approval matching. Invalid configuration fails ingestion before replacing the active graph.

| Source | Authority |
|---|---|
| Installation limits | Root confinement, bounded work, model routing, credentials and network policy always apply. |
| Trusted repository administrator | Approves the exact configuration digest, separately enabling exclusions, suppressions and annotation. |
| Repository file | Requests behavior within those limits. It cannot grant access or select credentials, provider URLs or session identity. |
| Extractor defaults | Built-in directory exclusions remain; version 1 cannot re-include them. |

The shared glob matcher accepts case-sensitive, repository-relative slash paths. `*` and `?` match within one component; a whole-component `**` matches zero or more components. `ci/*.yaml` therefore matches one directory level, while `**/*_easyjson.go` also matches a file at the repository root. Absolute paths, drive prefixes, backslashes, `..`, brace expansion and negation are rejected. Each pattern is capped at 512 bytes, with at most 256 patterns across the configuration. Directory pruning occurs only when all descendants match the exclusion.

Annotation accepts 1–100 items per batch and up to 32 taxonomy hints of 64 bytes each. Hints are redacted descriptive data, never system instructions or graph labels. The only repository-selectable annotation task is `code-annotation`.

## Ingestion, scanner coverage and publication

The same policy reaches Go, other-language, Markdown, architecture-record, configuration and Helm extraction, plus module/manifest/lockfile discovery, licence inspection, security inventory and history output. Excluded paths are filtered before file reads where the extractor permits it, and the final graph removes excluded nodes and dangling relationships. `.codesynapse.yaml` itself produces no generic YAML findings or annotations.

Go extraction enumerates eligible packages and filters declarations, references and findings by canonical path. Type checking may still read an excluded package imported by eligible code. These exclusions bound workload and output; they do not provide a strict no-read sandbox for `go/packages`. Existing `CGO_ENABLED=0`, `GOTOOLCHAIN=local` and network defaults remain enforced.

Kubernetes eligibility is separate from ordinary YAML handling. `scanners.kubernetes.ignore_paths` removes Kubernetes classification, resource output and Kubernetes rules from matching files. Generic key indexing, secret detection and other applicable YAML scanners still run. The same eligibility applies to Helm resources. Disabling Kubernetes or excluding its paths requires exclusion approval and appears in coverage metadata.

`Repository.meta.repository_policy` records the normalized configuration, digest, diagnostics and coverage exclusions from the last successful ingest. The exclusion inventory examines at most 100,000 directory entries and flags incomplete inventory or unreadable entries. It does not claim complete coverage or measured token savings. Security reports retain coverage warnings for excluded paths and disabled scanners.

Repository replacement validates the complete graph before publication and caps it at 64 MiB. Neo4j writes new nodes, relationships and stale-row deletion in one transaction. HugeGraph uploads bounded staging chunks and publishes them in one transaction; interrupted uploads leave the active graph intact. Stages expire after two hours and later replacements prune abandoned stages. HugeGraph's synchronization assumes one graph-server process; this implementation does not claim distributed locking across a HugeGraph server cluster.

The API's `publishRepo` holds the accounts policy transaction while publishing the graph. Final checks revalidate execution identity, repository access, checkout commit and configuration digest. Attachments and approval changes use the same policy lock, so a new organisation cannot join between approval validation and publication. PostgreSQL uses the existing global policy advisory lock; governance writes therefore wait during graph publication. This is a consistency tradeoff, not a per-repository concurrency optimization.

There is no source-code vector index or annotation-summary embedding pipeline in this phase. Atomic graph replacement removes stale code retrieval data. Existing durable and session memory vectors retain their own namespaces and lifecycle; ingest exclusions do not delete them or convert them into code vectors.

## Approvals, suppression and shared graphs

Approvals live in the accounts store, separately from the shared graph. PostgreSQL migration `0008_repository_policy.sql` adds `repository_policy_approvals` and `repository_annotations`, with repository foreign keys and cascading cleanup on unlink. The memory accounts adapter follows the same contract.

A repository administrator using a signed-in browser session can approve or revoke a configuration. API keys cannot manage approvals. Each approval records the exact normalized configuration and digest, three independent permissions (`allow_exclusions`, `allow_suppressions`, `allow_annotation`), a redacted reason, actor, timestamp and optional expiry. Approval changes and their `repo.config.approve` or `repo.config.revoke` audit event commit together. A changed digest or expired approval is inactive.

Security suppression affects the authorised tenant's report and grade. Findings remain stored, and reports show suppressed findings, counts and approval reasons separately. Unapproved or expired suppression requests leave findings active. Suppression of findings does not authorize ingestion exclusions; coverage loss requires its own explicit approval. The configuration file alone cannot make an incomplete scan appear complete.

Every linked organisation must actively approve the same digest with `allow_exclusions` before a shared graph can be filtered. A new organisation cannot attach to an already filtered graph without a matching approval. Because an approval belongs to an existing repository entry, the supported onboarding path is to publish an unfiltered graph, attach the organisation, obtain every owner's approval, then ingest the exclusions. Alternatively, use a separate unfiltered graph. Automatic policy-specific graph variants are not implemented.

Revoking an exclusion approval prevents further filtered ingestion; it does not reconstruct previously excluded code. Restoring coverage requires a successful ingest of a configuration without those exclusions. Suppression revocation immediately restores active finding presentation, and annotation revocation blocks new result persistence. Existing annotation provenance remains stored.

For trusted local CLI work, inspect the requests before approving their digest:

```sh
codesynapse config inspect /path/to/repo
codesynapse ingest /path/to/repo --approved-policy-digest <reviewed-digest>
```

The digest flag approves ingestion exclusions for that invocation. It is not an API approval, a permission grant, or permission to run cloud annotation. Commands that extract the repository expose the same flag. CLI inspections and local overrides do not replace the accounts workflow for a shared installation.

## Local annotation jobs

`services/api/internal/annotation` runs a dedicated bounded queue through `llm.Registry.Complete`, using the configured `code-annotation` route. It accepts only a provider satisfying the existing `Local()` policy. Cloud routes are refused and there is no cloud fallback.

Inputs are indexed symbol IDs, names, signatures, existing redacted summaries and fingerprints. The runner does not read source files. The system prompt is constant; taxonomy hints travel as JSON user data. Raw prompts and model responses are transient. No source bodies, prompts or annotations are inserted into the shared code graph or vector collections.

Successful API ingestion queues eligible functions, methods and types under the originating execution context. Annotation failures do not roll back a completed ingest. Unchanged symbols with the same fingerprint and policy digest reuse their stored annotation, retaining the original commit, revision, job ID and `origin_session_id`. Reads validate the fingerprint against the current graph before returning that annotation; they do not rewrite its history to the latest revision.

Jobs record tenant, subject, immutable primary/parent session, graph, commit, ingest revision, policy digest, selected symbols and a deterministic idempotency key. Authentication, session validity, repository write permission and current symbol evidence are checked before model work and before persistence. Persistence uses the shared publication lock and an accounts transaction to check live approval/session state and save the batch atomically. Changes to commit, policy or symbol fingerprint invalidate pending results. Schedules inherit their registered runtime context; they cannot manufacture a replacement session after expiry.

| Limit | Value |
|---|---:|
| Active jobs per API process | 64 |
| Active jobs per tenant per process | 8 |
| Concurrent consumers per process | 2 |
| Retained job statuses per process | 256 |
| Job deadline | 2 minutes, also subject to provider timeout |
| Symbols per batch | Configured ceiling, at most 100 |
| Serialized batch input | 24 KiB |
| Model answer | 64 KiB; at most 8,192 output tokens or the provider limit |
| Summary | 1,024 UTF-8 bytes |
| Taxonomy per result | At most 8 distinct supplied hints |
| Stored annotations per tenant/repository | 5,000 |

The worker uses a conservative byte-based check against the configured input-token budget, including prompt overhead. This is a guard, not an exact tokenizer measurement. A batch that exceeds the provider budget fails rather than sending an oversized prompt.

Model output must contain exactly one item per selected symbol. Unknown fields, duplicate JSON keys, foreign or repeated symbols, unknown taxonomy values, trailing output and confidence outside 0–1 are rejected. Stored free text is redacted and bounded again. Annotation storage is tenant-owned PostgreSQL metadata; no new code-node properties, graph relationship kinds or vector schema are required.

The queue is transient and does not replay across process restarts. Shutdown drops pending metadata and callbacks. Queued and running jobs can be cancelled by the originating runtime. There are no automatic retries. Repeating an identical submission returns its retained job, including a failed or cancelled state; completed persistence is idempotent by job ID. Queue saturation returns a deferred-symbol count. Queue limits are per process, while persisted annotation quotas are enforced by the accounts store.

`codesynapse_annotation_jobs_total` counts fixed completion outcomes and `codesynapse_annotation_duration_seconds` measures acceptance-to-completion latency. Both use the existing bounded tenant/tool/provider/model attributes. Repository names, paths, taxonomy hints, digests and session IDs are never metric labels. Provider usage is already recorded by the shared LLM registry.

## API and dashboard

All paths below are under `/api/v1/repos/{repo}` and enforce the selected tenant's repository permissions.

| Method and suffix | Behavior and access |
|---|---|
| `GET /policy` | Read indexed requests, approval and whether its digest/expiry matches; repository READ. |
| `POST /policy/preview` | Validate supplied `config` YAML and return normalized config, digest and diagnostics; READ. |
| `PUT /policy` | Approve the supplied YAML and matching digest with permission flags, reason and optional expiry; repository ADMIN and signed-in session. |
| `DELETE /policy` | Revoke approval; ADMIN and signed-in session. |
| `GET /annotations` | Read current validated annotations; READ. Supports `limit` and `after`, returning `next` and `validated_revision`. |
| `POST /annotations` | Queue metadata annotation; WRITE plus registered execution headers. Optional `symbol_ids` selects at most 100 indexed node IDs. |
| `GET /annotations/jobs/{job}` | Read a job belonging to the same runtime; READ plus execution headers. |
| `DELETE /annotations/jobs/{job}` | Cancel a job belonging to the same runtime; WRITE plus execution headers. |

Without explicit symbol IDs, manual annotation examines the first 500 eligible symbols by name and reports `limited` when that bound is reached. Use explicit IDs to select later symbols. Submission responses distinguish queued, deferred, skipped and retained work. Annotation reads default to 100 results, cap each page at 500 results or examined records, and use a ten-second deadline. The accounts read itself is bounded by the 5,000-record repository quota.

The repository's **Indexing policy & annotations** page shows indexed configuration, coverage exclusions, approval state and current annotations. Administrators can preview YAML, approve selected requests with a reason/expiry, and revoke approval. Jobs are started and cancelled through the API or an agent; the page shows their results. The page initially displays the first annotation page; the API exposes continuation cursors for larger result sets.

## Upgrade and verification

Existing repositories without `.codesynapse.yaml` retain their scanner defaults. Existing graphs without policy metadata remain readable, with no invented configuration approval or annotation provenance. A file requesting exclusions now stops ingest until a matching trusted approval exists; fix or approve the request before retrying. A failed parse, rejected approval, cancelled stage or backend write failure leaves the prior graph active when the graph transaction has not committed. As with other remote writes, a lost acknowledgement after commit requires checking the active revision before retrying.

The tests cover strict parsing and glob matching, malformed input fuzzing, ignored imports and nested modules, Helm/Kubernetes eligibility, secret findings outside Kubernetes classification, dependency exclusions, history boundaries and stale graph removal. Accounts contracts exercise tenant isolation, audit rollback, annotation quotas, session revocation and unlink cleanup. Worker/API tests cover cloud refusal, invalid model output, changed revision/policy, duplicate delivery, cancellation and queue limits. Concurrent publication tests cover attachment serialization and final authority checks. Real Neo4j/HugeGraph tests exercise atomic replacement and retained old state after rejected writes; PostgreSQL runs the same accounts contract as memory storage.

Run `just test`, `just lint`, `go vet ./...`, applicable database contracts and `just web-check` before release. Quantified token or latency savings require fixed-revision measurements; phase 4h publishes no assumed savings percentage. Development and production Kubernetes work remains deferred to the final deployment phase.
