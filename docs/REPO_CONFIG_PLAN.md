# Repository configuration and asynchronous annotation

Status: planned, SaaS phase 4h. Add an optional repository-root `.codesynapse.yaml` for ingestion boundaries, scanner controls and annotation requests. This document describes future behavior; the current ingest pipeline does not load this file.

Implement the parser and ingestion filters first. Tenant-managed suppression and annotation depend on [access isolation (4d)](ACCESS_CONTROL_PLAN.md), [retrieval namespaces (4f)](RETRIEVAL_PLAN.md) and [execution context (4g)](SESSION_CONTEXT_PLAN.md). Report bounded operational metrics through [phase 4e](TELEMETRY_PLAN.md).

## Requested version 1 configuration

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

The requested `sec-yaml-plain-secret` identifier differs from the existing catalogue rule, `yaml-hardcoded-secret`. Version 1 will recognize the requested spelling as an explicit compatibility alias, normalize it to the catalogue ID and report that normalization. Unknown rule IDs fail validation. Do not rename stored findings or silently ignore an unknown rule.

## Parser, defaults and precedence

Add `internal/config/repo.go` for typed version 1 configuration, loading, validation and effective-policy construction. Keep YAML extraction in the existing `internal/ingest/config` package; use distinct import aliases where both packages are needed. Reuse the existing YAML dependency. A new glob library would require separate dependency approval.

Load the file once from the resolved checkout root before starting extractors. Read it through a root-confined file handle, reject symlinks and non-regular files, and apply a 64 KiB limit before decoding. Accept exactly one YAML document with `version: 1`. Reject duplicate keys, unknown fields, aliases, merge keys, custom tags, invalid types and unsupported versions. Bound document nesting and node count before conversion to typed values. Return field paths and line numbers in errors without echoing arbitrary values.

Keep explicit booleans distinct from omitted values. An absent file preserves current ingestion behavior, with Kubernetes scanning enabled and annotation disabled. With annotation explicitly enabled, default `provider_task` to `code-annotation` and `max_batch_size` to 20. Invalid configuration stops the new ingest before replacing the existing graph; expose the failed attempt and retain the last successful revision.

Effective policy follows these rules:

| Source | Effect |
|---|---|
| Installation safety limits | Always apply: root confinement, no symlink traversal, bounded work, approved model routing and network policy |
| Trusted organisation/repository settings | Authorize optional annotation, approve scanner exclusions and security suppressions, and cap resource use |
| Repository configuration | Requests exclusions and annotation within those permissions |
| Existing extractor defaults | Remain when no setting replaces them; built-in excluded directories cannot be re-included in version 1 |

Exclusions accumulate; there is no negation or re-inclusion syntax. A repository file cannot enable network access, select an arbitrary provider URL, alter credentials, grant permissions, set tenant/session identity, or override trusted limits. An explicit `annotation.enabled: false` prevents annotation even when the installation supports it. `enabled: true` requests the service, but does not authorize it.

Define one bounded glob matcher shared by ingestion and annotation. Patterns are case-sensitive, root-relative slash paths: `*` and `?` match within a component; a whole-component `**` matches zero or more components. Thus `ci/*.yaml` matches one directory level and `**/*_easyjson.go` also matches a file at the root. Reject absolute paths, drive prefixes, backslashes, `..` components, brace expansion and negation. Limit each pattern to 512 bytes and the entire configuration to 256 patterns; compile once with bounded matching work. Prune a directory only when the matcher proves every descendant is excluded.

Limit annotation batches to 1–100 items, taxonomy hints to 32 entries of at most 64 bytes, and task identifiers to the server's configured allowlist. Treat hints as untrusted descriptive data, redact them with `redact.Free`, and keep them out of system instructions and database labels.

## Ingestion and scanner boundaries

Today `internal/ingest/ingest.go` coordinates separate walkers and extractors. Docs, configuration and language walkers each skip built-in directories. Go extraction uses `go/packages` with a default `./...` pattern. Pass the same immutable effective policy to these paths, dependency discovery, security coverage inventory, history enrichment and future vector indexing.

Apply `ignore.paths` before opening files or collecting walk entries wherever possible. Excluded files must produce no code/configuration nodes, vectors, annotation jobs, history-derived file nodes or dangling relationships. Honor exclusions when discovering modules and manifests; report excluded dependency coverage rather than presenting an incomplete inventory as complete. Exclude the configuration file itself from generic YAML finding generation and annotation, while retaining its validated digest and policy diagnostics.

Go loading needs a separate boundary: enumerate permitted root packages and filter emitted declarations, references and findings by canonical file path. Type checking can still read an excluded package imported by permitted code. An ingestion ignore is therefore an output and workload filter, not a promise that `go/packages` never reads a file. Preserve `CGO_ENABLED=0`, `GOTOOLCHAIN=local` and the existing network defaults. If a trusted policy requires a strict no-read boundary, reject affected loads or use a separately designed confined input snapshot; do not claim that post-load filtering provides that guarantee.

Kubernetes detection currently treats YAML mappings with both `apiVersion` and `kind` as resources. Keep that classification and add an eligibility check before resource nodes, Kubernetes rules and Kubernetes-specific tool results. `scanners.kubernetes.ignore_paths` suppresses that domain classification only; ordinary YAML key indexing, generic secret detection, Compose/workflow rules and other applicable scanners still run. Apply the same eligibility decision to Helm scanning and generated resource metadata. `kubernetes.enabled: false` requests disabling that scanner, subject to trusted policy, and must appear as disabled coverage.

Reconcile existing graph and vector data after a successful filtered ingest. An excluded path must disappear from all retrieval channels, including stale vectors and cached outlines. Stage the new revision and switch active metadata only when graph replacement and vector invalidation agree; until then, gate retrieval against the active ingest revision and filter out obsolete hits.

## Security exclusions and shared repositories

Treat `scanners.security.ignore_rules` as suppression requests. A repository author cannot approve their own exception merely by committing this file. Require a trusted organisation repository administrator to approve the canonical rule, path patterns and configuration digest, with a reason, optional expiry, actor and audit event. Changed patterns or rule IDs require new approval. CLI/local use needs an explicit trusted policy input outside the scanned repository.

Keep detected findings and suppression provenance available to authorized reviewers. Apply approved suppression when presenting and grading a tenant's report; show suppressed counts and reasons separately. Pending, rejected or expired requests leave findings active. Mandatory security coverage cannot be disabled through a general ingestion ignore: either scan the relevant paths in a separate security pass or require an audited exclusion approval. Always mark unscanned paths and scanner-disabled coverage; an exclusion must never turn missing coverage into a clean bill of health.

Code graphs can be shared by several organisations. Store the repository's parsed configuration digest with its commit/ingest revision, but keep approvals and tenant-specific annotation in the organisation's namespace. One tenant's approval must not remove another tenant's findings or rewrite the shared graph. If trusted ingestion policy changes the actual graph shape between tenants, introduce an explicit policy-specific graph variant before supporting that combination; do not reuse a shared graph under mismatched policy digests.

## Asynchronous annotation

After successful ingestion, enqueue eligible changed symbols for a dedicated annotation task under `services/api/internal/aitasks`, with model calls through `services/api/internal/llm`. Add `code-annotation` to the configured task allowlist. Repository configuration selects that approved task, never a provider endpoint or credentials.

Source bodies may be read transiently from the pinned checkout for local annotation only. Require the selected provider's existing `Local()` policy and reject a cloud route or fallback when source is included. The existing exception for a user-requested diff review does not authorize background cloud annotation. Redact model inputs through the registry, keep system prompts constant, and supply taxonomy hints as delimited user data. Do not store source bodies or raw prompts in graph, vectors or durable job records.

Each job records authenticated tenant, resolved graph/repository namespace, actor, immutable parent session context, commit, ingest revision, policy digest, symbol IDs and an idempotency key. Follow phase 4g for `session_id` and `parent_session_id` inheritance; retries and detached workers retain the original identity. Reject jobs without valid context before queueing, and recheck access and session validity before model work and before persistence. Scheduled indexing needs a separately defined trusted non-chat execution context; it must not invent a chat session ID to satisfy this contract.

Set per-tenant queue limits, worker concurrency, batch token/byte budgets, model deadlines and bounded retries. `max_batch_size` is an item ceiling, not permission to exceed the token budget. Support cancellation and backpressure. A revoked permission, changed commit, deleted session or changed policy invalidates pending results. Worker replay must not resurrect removed repositories or annotations.

Validate model output against a fixed annotation structure: existing symbol ID, bounded summary, approved taxonomy values and optional bounded confidence. Confirm every symbol belongs to the authorized ingest revision. Redact stored text, reject unknown fields and relationship kinds, and use only allowlisted model node/edge kinds. Store tenant-owned annotations separately from source-derived shared symbol metadata. Do not convert taxonomy hints into Cypher labels, Gremlin scripts or arbitrary edge predicates.

Link annotation references to the phase 4f retrieval model. Only approved entity names and aliases enter embeddings under the existing local-embedding policy. New text embedding behavior requires an explicit policy/schema change. Session chat memory continues to require phase 4g tenant/session/namespace filters; durable annotation is repository knowledge with originating session provenance, not chat history.

## Delivery and acceptance

1. Add the versioned parser, strict validation, deterministic policy digest and bounded glob matcher. Document omitted values and the rule-ID compatibility alias. Verify an absent file preserves current behavior.
2. Thread policy through CLI/API ingestion and every discovery path. Add coverage reporting and staged removal of stale graph/vector results. Keep the previous successful revision visible after failure.
3. Add tenant-scoped suppression approval records, expiry and semantic audit events in the accounts/control plane. Protect management through phase 4d repository ADMIN authorization; READ can inspect permitted effective policy, and WRITE alone cannot change scanner policy.
4. Add the bounded annotation queue, local task route and validated annotation persistence. Extend model allowlists, `nodeprops`, HugeGraph schema coverage, store contracts and vector contracts for any new fields or kinds. Use additive migrations and explicit revision backfills; never fabricate legacy session provenance.
5. Expose effective policy, rejected requests and annotation status through scoped API/dashboard views. Record bounded outcomes and latency through phase 4e; paths, patterns, taxonomy hints, digests, repository names and session IDs must not become metric labels.
6. Update the configuration reference, CLI/API documentation, `docs/PLAN.md`, schema notes and web API types. Update `docs/AGENT_GUIDE.md` whenever MCP tool output or behavior changes. Publish a migration note explaining the difference between ingestion exclusion, scanner exclusion and approved finding suppression.

Parser tests must cover duplicate/unknown keys, unsupported versions, oversized input, aliases, symlinks, malformed globs, root escapes, omitted/explicit booleans, rule aliases and unknown rule IDs. Fuzz decoding and path matching with cancellation and resource bounds. Ingestion fixtures must include nested modules, excluded imported Go code, Helm templates, non-Kubernetes YAML, general secrets in Kubernetes-excluded files, lockfiles, renamed/deleted paths and stale vector hits.

Exercise two tenants sharing a graph with different suppression decisions; an untrusted config requesting cloud annotation; annotation output referencing a foreign symbol; revoked permission/session during a job; changed commit/config during inference; duplicate delivery; queue exhaustion; and interrupted graph/vector replacement. Check store behavior across memory, Neo4j and HugeGraph implementations. Run `just test`, `just lint`, applicable store/PostgreSQL integration suites and `just web-check` for web changes. Measure avoided file reads, graph/vector sizes and annotation latency on fixed repository revisions before publishing savings claims.
