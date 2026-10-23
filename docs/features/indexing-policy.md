# Indexing policy and reports

## Repository policy

An optional `.codesynapse.yaml` at a repository root requests path exclusions, Kubernetes scanner controls, security exceptions and local AI annotation. The file comes from the repository, so it is untrusted until someone approves it. Validate it first:

```sh
codesynapse config inspect /path/to/repository
# Review the normalized configuration and copy its exact digest:
codesynapse ingest /path/to/repository --approved-policy-digest '<reviewed-digest>'
```

With accounts, a repository administrator approves the configuration on **Indexing policy**, with a reason and optional expiry. Exclusions need approval from every organisation sharing the graph. Security exceptions apply only to the approving organisation; the raw findings remain available. Annotation also needs the local `code-annotation` model route and a registered runtime session.

See the [configuration reference](../REPO_CONFIG_PLAN.md) for the YAML schema and API workflow.

## Ingestion reports

**Ingestion reports** shows the latest attempt and the last successful run separately, with per-file outcomes, scanner exclusions and graph publication status. The read-only MCP tool `ingestion_report` returns the same summaries.

File metrics come from durable accounting, so restarts and recovery do not count a write twice. Savings are metadata-based estimates with coverage shown; unknown files beneath pruned directories are excluded. See [ingestion accounting](../INGEST_TRANSPARENCY_PLAN.md) for limits and recovery behaviour.
