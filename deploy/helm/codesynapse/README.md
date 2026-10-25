# CodeSynapse Helm chart

[![Artifact Hub](https://img.shields.io/endpoint?url=https://artifacthub.io/badge/repository/codesynapse)](https://artifacthub.io/packages/search?repo=codesynapse)

Installs CodeSynapse on Kubernetes: the dashboard (`web`), the API (`api`), job workers (`worker`) and the MCP server (`mcp`). The stores it needs are subcharts, all on by default, so a plain install runs everything: PostgreSQL, HugeGraph, Milvus, VictoriaMetrics and VictoriaTraces. For production, run them yourself and link them (see [Stores](#stores)). Every image is Apache-2.0, or PostgreSQL-licensed for the database.

## Install with the CLI

The quickest way, like `cilium install`: the `codesynapse` CLI creates the namespace and a Secret with random credentials, installs the chart at the CLI's own version and waits until every service answers. It uses the `helm` and `kubectl` on your machine and asks before it changes the cluster.

```sh
codesynapse install --local            # a laptop cluster: kind, k3d, Docker Desktop, minikube
codesynapse status                     # every component's ready replicas; --wait, --test
codesynapse uninstall                  # the release; --keep-data=false also deletes the namespace and its data
```

- **Target:** `--context` and `--namespace` pick it; the default is kubectl's current context, which it names and confirms first (`--yes` skips the question).
- **Version:** `--version` picks a chart version and `--chart deploy/helm/codesynapse` installs from a checkout. `-f values.yaml` and `--set key=value` pass values through, and `--dry-run` renders the chart without installing.
- **`--local`:** small resource requests and plain-HTTP cookies, for port-forwarding.
- **The Secret:** made once and never replaced, so a reinstall keeps the stores' passwords. Its credentials go to kubectl on standard input, never on a command line. Back it up: losing `SECRET_KEY` means re-entering sealed Git tokens and identity provider secrets.
- **`status`:** exits 1 when something is not ready, so scripts can use it.

The steps below do the same by hand.

## Install

1. Create the one Secret every credential comes from. The chart never invents passwords.

   ```sh
   kubectl --context <ctx> create namespace codesynapse
   kubectl --context <ctx> --namespace codesynapse create secret generic codesynapse \
     --from-literal=CODESYNAPSE_API_TOKEN="$(openssl rand -hex 24)" \
     --from-literal=CODESYNAPSE_MCP_TOKEN="$(openssl rand -hex 24)" \
     --from-literal=POSTGRES_PASSWORD="$(openssl rand -hex 24)" \
     --from-literal=HUGEGRAPH_PASSWORD="$(openssl rand -hex 24)" \
     --from-literal=METRICS_PASSWORD="$(openssl rand -hex 24)" \
     --from-literal=SECRET_KEY="$(openssl rand -base64 32)"
   ```

   Optional keys: `CODESYNAPSE_GIT_TOKENS` (installation Git tokens, `host=token,...`; keep them read-only), `CODESYNAPSE_GIT_PUSH_TOKENS` (same format, tokens that may push; only the fix worker and an API that runs jobs with push on get them; without it pushes use `CODESYNAPSE_GIT_TOKENS`), `ANTHROPIC_API_KEY`, `OPENROUTER_API_KEY`. With an external database, put its URL in `CODESYNAPSE_DATABASE_URL` and drop `POSTGRES_PASSWORD`. Keep `SECRET_KEY`: organisation Git tokens and identity provider secrets sealed with a lost key have to be entered again.

2. Install:

   ```sh
   helm upgrade --install codesynapse oci://ghcr.io/astrona-io/charts/codesynapse --version <X.Y.Z> \
     --kube-context <ctx> --namespace codesynapse \
     --set global.existingSecret=codesynapse \
     --set ingress.enabled=true --set ingress.host=codesynapse.example.com --set ingress.tlsSecretName=codesynapse-tls \
     --set publicURL=https://codesynapse.example.com --set publicMCPURL=https://codesynapse.example.com/mcp
   helm test codesynapse --kube-context <ctx> --namespace codesynapse
   ```

   Every release publishes the chart at its version, signed like the images (`cosign verify ghcr.io/astrona-io/charts/codesynapse:<X.Y.Z> ...`). To install from a checkout, use `deploy/helm/codesynapse` in place of the `oci://` reference. Upgrades and backups: [docs/operations/upgrading.md](https://codesynapse.astrona.io/operations/upgrading/).

3. Open the dashboard and sign in as `admin`. The chart generated its password into the Secret `<release>-admin` (kept across upgrades; `admin.existingSecret` brings your own, `admin.enabled=false` skips it):

   ```sh
   kubectl --namespace codesynapse get secret <release>-admin -o jsonpath='{.data.CODESYNAPSE_ADMIN_PASSWORD}' | base64 -d
   ```

   Replace that account with your own, then create the organisation owner. Locked out: `kubectl exec deploy/<release>-api -- codesynapse-api reset-admin --generate`.

## Values worth knowing

Every value is described and type-checked in `values.schema.json` (each store's in `charts/<store>/values.schema.json`), so `helm install` and `helm upgrade` refuse a misspelt key or a wrong type instead of ignoring it.

| Value | Default | Meaning |
|---|---|---|
| `global.existingSecret` | required | The Secret above |
| `image.tag` | chart appVersion | Image tag on `ghcr.io/astrona-io/codesynapse-{api,worker,mcp,web}` |
| `global.imagePullSecrets` | `[]` | Pull secrets for a private registry or mirror, e.g. `[{name: ghcr}]`; the stores use them too |
| `global.networkPolicies` | `true` | Each component and store accepts only the pods that need it |
| `worker.replicas`, `worker.roles` | `1`, `ingest,ai` | Clone, sync, index and AI jobs. It parses untrusted repositories, so it never holds the right to push, the API's service token or the first administrator's password |
| `fixWorker.enabled`, `fixWorker.replicas` | `false`, `1` | The only worker that pushes fix branches (`CODESYNAPSE_ALLOW_PUSH=true`). Needs `features.allowPush=true` and a Git token with write access |
| `worker.resources.limits.memory` | `6Gi` | Ingest type-checks whole modules; size it for your largest repository. `GOMEMLIMIT` is set to 90% |
| `workspace.accessModes` | `[ReadWriteOnce]` | `api`, `worker` and `mcp` share clones. Use a ReadWriteMany class on more than one node |
| `api.replicas`, `mcp.replicas`, `web.replicas` | `1` | Raise to 2 or more for availability; see [Scaling out](#scaling-out) |
| `global.kedaNamespace` | empty | With `worker.queueScaling.mode=keda`: the namespace of KEDA's operator, admitted by the PostgreSQL network policy |
| `<api,mcp,web>.autoscaling` | off | CPU autoscaling (`minReplicas`, `maxReplicas`, `targetCPU`); needs metrics-server |
| `mcp.stateless` | `true` | No MCP transport session between requests, so no sticky sessions are needed |
| `ingress.*` | off | One host: `/mcp` to MCP, the rest to the dashboard. The API is never exposed |
| `<store>.enabled`, `<store>.externalURL` | in-chart | See [Stores](#stores) |
| `features.allowPush` | `false` | Fix branches for accepted review comments |
| `traces.sample` | `1` | Share of new traces kept |
| `llmConfig` | empty | The content of `config/llm.yaml`; keys go in the Secret |

## Stores

Each store is a subchart in `charts/`, declared in `Chart.yaml` with `condition: <store>.enabled`. All are enabled and configured by default, which is right for trying CodeSynapse, a kind cluster or a small team. **For production, the recommended way is to run the stores yourself** (managed services or operators you already back up and monitor), disable the subcharts and link them:

| Store | Disable | Link instead |
|---|---|---|
| PostgreSQL (accounts, jobs, audit log) | `postgresql.enabled=false` | `CODESYNAPSE_DATABASE_URL` in the Secret; drop `POSTGRES_PASSWORD`. A managed database, or CloudNativePG with three instances (the `uri` of its `<cluster>-app` Secret); `just chart-failover-postgres` tests a primary failover |
| HugeGraph (code graph) | `hugegraph.enabled=false` | `hugegraph.externalURL` (required); `HUGEGRAPH_PASSWORD` stays in the Secret. Authentication must be on |
| Milvus (memory vectors) | `milvus.enabled=false` | `milvus.externalURL` (the proxy, `http(s)://host:19530`), and `MILVUS_TOKEN` in the Secret when authentication is on; empty turns memory vectors off. A Milvus cluster from the zilliztech chart with SeaweedFS for S3 and Woodpecker as the log is tested by `just chart-failover-milvus`; set `mq.type: woodpecker` before its first start |
| VictoriaMetrics (metrics) | `victoriametrics.enabled=false` | `victoriametrics.externalURL`; empty turns export off |
| VictoriaTraces (traces) | `victoriatraces.enabled=false` | `victoriatraces.externalURL`, the full OTLP/HTTP traces endpoint; empty turns export off |

Settings of an in-chart store go under its name, for example `--set hugegraph.size=50Gi`. Each subchart's `values.yaml` lists them: image, size, storageClass, resources, and retention for the Victoria stores. The subcharts take the Secret, pull secrets and network policies from `global`, and share the helpers in the `common` library chart.

```sh
# PostgreSQL and HugeGraph run elsewhere; the rest stays in the chart.
helm upgrade --install codesynapse oci://ghcr.io/astrona-io/charts/codesynapse --version <X.Y.Z> \
  --kube-context <ctx> --namespace codesynapse --set global.existingSecret=codesynapse \
  --set postgresql.enabled=false \
  --set hugegraph.enabled=false --set hugegraph.externalURL=https://hugegraph.internal:8080
```

## Scaling out

`api`, `mcp` and `web` keep nothing between requests that another replica would need:

- **API:** login limits, jobs, schedules and the audit chain live in PostgreSQL. One replica at a time holds the `api-loops` lease and runs the scheduler and maintenance loops; another takes over within a minute when it stops.
- **MCP:** runs stateless (`mcp.stateless`). Every request carries the personal key and the chat's runtime session and is checked on its own, so any replica answers it. The tools send no server-initiated messages, so nothing is lost.
- **Web:** forwards each visitor's session cookie to the API and keeps no state.

With more than one replica, or with autoscaling, a component gets a PodDisruptionBudget (`maxUnavailable: 1`). Its pods prefer different nodes and spread over zones where the cluster has them.

**Workers** scale by `worker.replicas`, or on queued jobs with `worker.queueScaling` (`mode: none|hpa|keda`, `minReplicas`, `maxReplicas`, `jobsPerWorker`). The chart installs neither tool, so pick what your cluster has: `hpa` makes a HorizontalPodAutoscaler on the external metric `metricName`, which an adapter must serve as the sum of `codesynapse_jobs_active` over the kinds `sync`, `clone`, `switch`, `ai` and `ai-auto` (see docs/TELEMETRY_PLAN.md); `keda` makes a KEDA ScaledObject that counts those jobs in PostgreSQL, so set `global.kedaNamespace` to the namespace of KEDA's operator, which the PostgreSQL network policy then admits (`just chart-scale-keda` checks this on kind). `replicas` is ignored when a mode is on, scale-downs take 15 minutes, and the fix worker is never scaled. Without a mode, workers scale by `worker.replicas`: a worker removed mid-job leaves the job to another worker once its two-minute lease runs out. Split roles over separate releases or Deployments to size ingest apart from AI work.

**Limits that remain:**

- `api`, `worker` and `mcp` mount the same workspace volume. Spread over several nodes, it needs a ReadWriteMany storage class.
- Status lookups for annotation jobs (`/annotations/jobs/{id}`) and memory extraction jobs (`/memory/jobs/{id}`) answer only on the API replica that took the job. The work itself is unaffected, and the dashboard doesn't poll them. A durable annotation queue is a follow-up.
- PostgreSQL, HugeGraph and Milvus run as single instances. Clustered stores are 6d in [docs/SAAS.md](https://codesynapse.astrona.io/SAAS/).

## Security

- **Pods:**
  - every CodeSynapse pod runs as non-root with a read-only root filesystem, no capabilities, no privilege escalation, the default seccomp profile and no service-account token;
  - PostgreSQL, Milvus and the Victoria stores run as non-root too;
  - HugeGraph's image rewrites its root-owned configuration at start, so it keeps root with capabilities limited to file ownership. Use an external, hardened HugeGraph in production.
- **Network policies:**
  - stores accept only `api`, `worker` and `mcp`;
  - the API accepts only `web`, `mcp` and the chart test;
  - workers, which parse untrusted repositories, accept nothing.
- **Linting:** `just chart-check` runs `helm lint`, kubeconform, kube-linter and kubesec. kubesec reports `SecretsAsEnvironmentVariables` for each workload: the services read credentials only from the environment. Mounting them as files is a follow-up.

## Try it locally

```sh
just chart-e2e    # lint, kind cluster "codesynapse-dev", build and load images, install, helm test
just kind-down    # delete the cluster
```

Every command names the `kind-codesynapse-dev` context. Never point these recipes at a shared or production cluster.
