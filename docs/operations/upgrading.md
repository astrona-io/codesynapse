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

- **Database migrations run on start.** The API, the workers and the MCP server apply pending PostgreSQL migrations when they start, under an advisory lock, so pods starting together don't race. So far every migration adds tables, columns and indexes or loosens a constraint, so an older pod keeps working against the newer schema while the rollout finishes. A release that breaks this says so in its notes.
- **Background loops have one owner.** The scheduler and maintenance loops run in the API that holds the `api-loops` lease. The lease lasts a minute and is renewed every 20 seconds. When that pod stops, another API takes over within a minute. Schedules live in PostgreSQL, so none are lost.
- **Jobs survive the restart.** Every clone, sync, ingest and AI task is a row in PostgreSQL before it runs. A worker that stops leaves its jobs behind with a two-minute lease. When the lease runs out, a worker with the matching role claims the job and runs it again from the stored parameters, after checking the requester's session and permission again. A job starts at most three times. Fix jobs are the exception: an interrupted fix is marked failed and must be started again by hand, because half-pushed branches shouldn't be repeated automatically.
- **Graphs are not rebuilt.** Existing graphs stay as they are; the next sync of each repository uses the new extractor. When a release changes the graph schema, `EnsureSchema` adds the new properties on start, and the release notes say whether a re-index is worth it.

## Before you upgrade

Take a backup. The parts that matter:

| What | How |
|---|---|
| PostgreSQL (accounts, organisations, audit log, jobs, schedules, sealed tokens) | `kubectl --context <ctx> -n codesynapse exec codesynapse-postgresql-0 -- pg_dump -U codesynapse -Fc codesynapse > codesynapse.dump`, or your managed database's snapshots |
| HugeGraph and Milvus volumes (graphs, reviews, memory, vectors) | Volume snapshots of the `codesynapse-hugegraph` and `codesynapse-milvus` StatefulSet volumes. Graphs can be rebuilt by re-indexing; reviews and agent memory can't |
| The Secret `codesynapse` | Keep `SECRET_KEY` safe. Organisation Git tokens and identity provider secrets sealed with a lost key have to be entered again |
| The audit log, for retention outside the cluster | `GET /api/v1/admin/audit?format=jsonl` as a platform admin; `GET /api/v1/admin/audit/verify` checks the hash chain |

Resource names assume the release is called `codesynapse`.

## Rolling back

`helm rollback codesynapse <revision>` returns the Deployments to the previous images. Migrations are not reversed. For additive migrations that is safe: the older release ignores what it doesn't know. To return to the state before the upgrade instead, restore the PostgreSQL dump and the volume snapshots.

## Release candidates

A `vX.Y.Z-rcN` tag publishes the chart and images at `X.Y.Z-rcN` and runs the same checks. It never becomes `latest`. Try one on a throwaway cluster with `just chart-install-published X.Y.Z-rcN`.
