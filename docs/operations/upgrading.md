# Upgrading

Every release publishes the Helm chart and the images at the same version, so one number describes an installation:

```sh
helm upgrade codesynapse oci://ghcr.io/astrona-io/charts/codesynapse --version 1.4.0 \
  --kube-context <ctx> --namespace codesynapse --reuse-values
helm test codesynapse --kube-context <ctx> --namespace codesynapse
```

`image.tag` defaults to the chart version. Set it only to pin an image on purpose; an old tag with a new chart is untested.

Read the release notes first. A release that needs anything beyond `helm upgrade` says so there.

## What happens during an upgrade

- **Database migrations run on start.** The API and the workers (one binary) apply pending PostgreSQL migrations when they start, under an advisory lock, so pods starting together don't race. The MCP server uses the schema as it finds it. So far every migration adds tables, columns and indexes or loosens a constraint, so an older pod keeps working against the newer schema while the rollout finishes. A release that breaks this says so in its notes.
- **Background loops have one owner.** The scheduler and maintenance loops run in the API that holds the `api-loops` lease. The lease lasts a minute and is renewed every 20 seconds. When that pod stops, another API takes over within a minute. Schedules live in PostgreSQL, so none are lost.
- **Jobs survive the restart.** Every clone, sync, ingest and AI task is a row in PostgreSQL before it runs. A worker that stops leaves its jobs behind with a two-minute lease. When the lease runs out, a worker with the matching role claims the job and runs it again from the stored parameters, after checking the requester's session and permission again. A pod that is stopped (a rolling upgrade sends SIGTERM) leaves its running and queued jobs the same way, and so does a process that could not reach PostgreSQL to renew its leases for 90 seconds: it stops those jobs before another worker may claim them, so no job runs twice. A job starts at most three times. Fix jobs are the exception: an interrupted fix is marked failed and must be started again by hand, because half-pushed branches shouldn't be repeated automatically.
- **Graphs are not rebuilt.** Existing graphs stay as they are; the next sync of each repository uses the new extractor. When a release changes the graph schema, `EnsureSchema` adds the new properties on start, and the release notes say whether a re-index is worth it.

## Before you upgrade

Take a backup. The parts that matter:

| What | How |
|---|---|
| PostgreSQL (accounts, organisations, audit log, jobs, schedules, sealed tokens) | `kubectl --context <ctx> -n codesynapse exec codesynapse-postgresql-0 -- pg_dump -U codesynapse -Fc codesynapse > codesynapse.dump`, or your managed database's snapshots |
| NebulaGraph and Milvus volumes (graphs, reviews, memory, vectors) | Volume snapshots of the `codesynapse-nebula` and `codesynapse-milvus` StatefulSet volumes. Graphs can be rebuilt by re-indexing; reviews and agent memory can't |
| The Secret `codesynapse` | Keep `SECRET_KEY` safe. Organisation Git tokens and identity provider secrets sealed with a lost key have to be entered again |
| The audit log, for retention outside the cluster | `GET /api/v1/admin/audit?format=jsonl` as a platform admin; `GET /api/v1/admin/audit/verify` checks the hash chain |

Resource names assume the release is called `codesynapse`.

## Fix branches moved to their own worker

Fix branches (`features.allowPush`) are pushed by a separate worker, so the worker that reads untrusted repositories never holds the right to push. `worker.roles` is now `ingest,ai`. If you pushed fix branches before, set `fixWorker.enabled=true` along with `features.allowPush=true`; until then a fix job waits for a worker that can run it. In compose, `fix-worker` is a new service and `worker` no longer takes `CODESYNAPSE_WORKER_ROLES` from `.env`. Neither worker holds the API token or the administrator password any more.

## Rolling back

`helm rollback codesynapse <revision>` returns the Deployments to the previous images. Migrations are not reversed. For additive migrations that is safe: the older release ignores what it doesn't know. To return to the state before the upgrade instead, restore the PostgreSQL dump and the volume snapshots.

## Docker compose installs

Run `just init` (it adds missing keys such as `POSTGRES_PASSWORD`, `SECRET_KEY`, `NEBULA_PASSWORD` and `METRICS_PASSWORD` to `.env`), then `just up`.

- **PostgreSQL 17 to 18.** The compose stack runs PostgreSQL 18, which cannot open a data volume written by PostgreSQL 17. If your install predates the upgrade, dump the accounts database with the old image first (`docker compose exec postgres pg_dumpall -U codesynapse > accounts.sql`), remove the `postgres-data` volume, run `just up`, and restore (`docker compose exec -T postgres psql -U codesynapse -d codesynapse < accounts.sql`).
- **From HugeGraph.** HugeGraph was replaced by NebulaGraph and graphs are not migrated. Run `just init` to add `NEBULA_PASSWORD` (at most 24 characters), `just up`, and sync your repositories again. Reviews, curated records and agent memory were stored in the same graph, so they start empty; export what you need first. Recreate the Milvus volume if the collection was built for an older release (`docker volume rm codesynapse_milvus-data`). The old `hugegraph-data` volume stays until you delete it. In Kubernetes, put `NEBULA_PASSWORD` in the Secret, replace `hugegraph.*` values with `nebula.*` (`nebula.externalAddrs` for your own cluster) and delete the old `*-hugegraph` StatefulSet volume.
- **From Neo4j.** Neo4j support was removed and graphs are not migrated: after `just up`, sync your repositories again to rebuild them in NebulaGraph. The old data stays in the `neo4j-data` volume until you delete it (`docker volume rm codesynapse_neo4j-data`). `CODESYNAPSE_GRAPH_BACKEND`, `COMPOSE_PROFILES=hugegraph` and `NEO4J_*` lines in `.env` are no longer used; a CLI or service still configured with `CODESYNAPSE_GRAPH_BACKEND=neo4j` stops with an error that says so.
- **Module path.** The repository and Go module are `github.com/astrona-io/codesynapse`, and images are under `ghcr.io/astrona-io`. Update existing clones with `git remote set-url origin git@github.com:astrona-io/codesynapse.git`; consumers of the Go module must update their imports.

## Release candidates

A `vX.Y.Z-rcN` tag publishes the chart and images at `X.Y.Z-rcN` and runs the same checks. It never becomes `latest`. Try one on a throwaway cluster with `just chart-install-published X.Y.Z-rcN`.
