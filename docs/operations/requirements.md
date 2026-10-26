# Requirements

What you need to run CodeSynapse on a laptop, on a single-replica Kubernetes install, and on a highly available one. The figures come from the compose file and the Helm chart's defaults, plus a measured local stack. A local AI model needs memory on top of all of this; see [Local models](local-models.md).

## At a glance

| | Local (compose) | Kubernetes, no HA | Kubernetes, HA with failover |
|---|---|---|---|
| Machines | One laptop or workstation | One node is enough | Three or more nodes, ideally in three zones |
| CPU | 4 cores | 2 cores requested, 4 recommended | 2 cores per node for the app, plus your stores |
| Memory for CodeSynapse | 8 GB free | 5 GiB requested, 20 GiB at the limits | 5 GiB requested for the app at two replicas each, plus your stores |
| Disk | 30 GB plus your clones | 75 GiB of volumes (defaults) | Workspace on ReadWriteMany storage, stores on their own |
| Stores | Bundled containers | Bundled single-instance subcharts | External, clustered stores you run |
| A failure means | Restart the container | Kubernetes restarts the pod; minutes of downtime | Another replica or the store's standby takes over |

## Local with docker or podman compose

**Software:** Docker Desktop, or podman 5 with `podman compose`. `just up` creates `.env` with random secrets and starts everything; `just` is optional.

**Measured:** the whole stack used about 1 GB of memory at rest (`podman stats`, 12 containers). Memory grows while a large repository is indexed, because the worker loads Go packages with their types. The chart gives the worker up to 6 GiB for that reason. Give Docker Desktop or the podman machine 8 GB, and more if you index very large repositories.

**Ports:** only 127.0.0.1 is bound. The dashboard is on 3000, the MCP server on 8090, the API on 8080 and trace queries on 10428. The stores are not published to the host.

**Disk:** named volumes hold NebulaGraph, Milvus, PostgreSQL, metrics and traces; `workspace/` holds the clones. 30 GB covers the stores for a few dozen repositories; add the size of what you clone.

**Limits:** compose sets no CPU or memory limits, and every store runs once. Back up the volumes as described in [Upgrading](upgrading.md#before-you-upgrade).

With a local model on the same machine, add its memory: about 18 GB for `qwen3.8:27b`, so a 32 GB machine is the practical minimum and 48 GB is comfortable.

## Kubernetes without HA

The chart's defaults: every component once, the stores as bundled subcharts, each a StatefulSet with one pod.

| Component | Requests | Limit | Volume |
|---|---|---|---|
| api | 250m, 256Mi | 1Gi | |
| worker | 500m, 1Gi | 6Gi | |
| mcp | 100m, 128Mi | 1Gi | |
| web | 50m, 128Mi | 512Mi | |
| fix worker (off by default, needs `features.allowPush`) | 250m, 512Mi | 2Gi | |
| PostgreSQL | 100m, 256Mi | 1Gi | 5Gi |
| NebulaGraph (metad, storaged and graphd in one pod) | 500m, 2Gi | 4Gi | 20Gi |
| Milvus standalone | 250m, 1Gi | 4Gi | 10Gi |
| VictoriaMetrics | 50m, 128Mi | 1Gi | 10Gi |
| VictoriaTraces | 50m, 128Mi | 1Gi | 10Gi |
| workspace (shared by api, worker, mcp) | | | 20Gi |
| **Total** | **1.85 CPU, 5 GiB** | **19.5 GiB** | **75 GiB** |

The fix worker adds 250m, 512Mi and a 2 GiB limit when you turn it on. Size the worker's limit for your largest repository.

A single node with 4 CPU and 16 GB runs this with room to spare. On several nodes the workspace volume needs a ReadWriteMany storage class, because api, worker and mcp mount it together; with ReadWriteOnce they must land on one node.

**What a failure costs:** Kubernetes restarts the pod, and the service is down until it is back. A running job is picked up by a worker again after its two-minute lease runs out. Losing a store's volume loses its data, so snapshot the volumes.

Install and checks: [deploy/helm/codesynapse/README.md](https://github.com/astrona-io/codesynapse/blob/main/deploy/helm/codesynapse/README.md). Try it on kind first with `just chart-e2e`.

## Kubernetes with HA and failover

The application tier scales out on its own. The bundled stores do not: each subchart is one pod with no replica setting. For HA, turn the subcharts off and link stores that you run clustered (`<store>.enabled=false` plus the external address; see [Stores](https://github.com/astrona-io/codesynapse/blob/main/deploy/helm/codesynapse/README.md#stores)).

### Application tier

| Component | Set | What happens on failure |
|---|---|---|
| api | `api.replicas: 2` or more, or `api.autoscaling` (2 to 6 on CPU) | Requests go to the other replica. One replica holds the `api-loops` lease and runs the scheduler and maintenance; another takes over within a minute. |
| mcp | `mcp.replicas: 2` or more; it runs stateless | Any replica answers any request; chats keep working. |
| web | `web.replicas: 2` or more | Visitors keep their session; it lives in PostgreSQL. |
| worker | `worker.replicas: 2`, or `worker.queueScaling` with HPA or KEDA (1 to 4) | A job on a lost worker is taken over after its two-minute lease. Scale-downs wait 15 minutes. |
| fix worker | one replica, when pushing is on | It is never scaled; a push it did not finish is not resumed. |

With more than one replica, each component gets a PodDisruptionBudget (`maxUnavailable: 1`), its pods prefer different nodes and spread over zones. The workspace volume must be ReadWriteMany.

If PostgreSQL is unreachable for 90 seconds, workers stop their jobs instead of failing them, and the jobs resume once the database is back.

### Stores

| Store | Run it as | Tested here |
|---|---|---|
| PostgreSQL (accounts, jobs, audit log) | A managed database, or CloudNativePG with three instances; set `postgresql.enabled=false` and `CODESYNAPSE_DATABASE_URL` in the Secret | `just chart-failover-postgres`: three CloudNativePG instances, the primary force-killed while the API writes. The standby took over in about 25 seconds and a few writes were refused in between. |
| Milvus (memory vectors) | Milvus cluster with Woodpecker as the message queue; set `milvus.enabled=false`, `milvus.externalURL` and `MILVUS_TOKEN` | `just chart-failover-milvus`: etcd ×3, mixCoordinator ×2 (active and standby), proxy ×2, queryNode ×2, streamingNode ×2, dataNode ×1, with a query node, a streaming node and the coordinator killed under load. Set `mq.type: woodpecker` before Milvus first starts; it cannot be changed later. The test's S3 store (SeaweedFS) is a single replica; in production use S3 that is itself highly available. |
| NebulaGraph (code graph, memory) | A NebulaGraph cluster you run (three metad, three or more storaged, two or more graphd); set `nebula.externalAddrs` to the graphd addresses | Not tested here. See the gap below. |
| VictoriaMetrics, VictoriaTraces (statistics, traces) | VictoriaMetrics cluster or a managed service; set `victoriametrics.externalURL` and `victoriatraces.externalURL` | Not tested. Losing them loses statistics and traces, not code or accounts. |

**Gap: NebulaGraph replication.** The graph space is created with `CODESYNAPSE_NEBULA_REPLICAS` copies of each partition (default 1), and a space keeps the replica count it was created with. The chart has no value for it yet, so an external NebulaGraph cluster still gets a space with one copy, and losing one storaged host loses part of the graph until it is back. Until the chart can set it, an HA install's graph is not replicated. The code graph can be rebuilt by indexing again; agent memory lives there too and cannot.

### Also

- Back up the Secret. Losing `CODESYNAPSE_SECRET_KEY` means re-entering every sealed organisation token.
- The NebulaGraph password is at most 24 characters (`openssl rand -hex 12`).
- Network policies let only api, worker and mcp reach the stores. With KEDA, set `global.kedaNamespace`.
- Two things are not shared between API replicas: the status of an annotation job and of a memory extraction job answers only on the replica that took it. The work itself is unaffected.

## Related

- [Running the stack](running.md): compose, daily development, Kubernetes install.
- [Upgrading](upgrading.md): backups before an upgrade.
- [Stores and backends](../architecture/stores.md): what each store holds.
- [Local models](local-models.md): memory and setup for a model on your own machine.
