# CodeSynapse Helm chart

Installs CodeSynapse on Kubernetes: the dashboard (`web`), the API (`api`), job workers (`worker`), the MCP server (`mcp`), and, unless you point it elsewhere, PostgreSQL, HugeGraph, Milvus, VictoriaMetrics and VictoriaTraces. Every image is Apache-2.0, or PostgreSQL-licensed for the database.

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

   Optional keys: `CODESYNAPSE_GIT_TOKENS` (installation Git tokens, `host=token,...`), `ANTHROPIC_API_KEY`, `OPENROUTER_API_KEY`. With an external database, put its URL in `CODESYNAPSE_DATABASE_URL` and drop `POSTGRES_PASSWORD`. Keep `SECRET_KEY`: organisation Git tokens and identity provider secrets sealed with a lost key have to be entered again.

2. Install:

   ```sh
   helm upgrade --install codesynapse oci://ghcr.io/astrona-io/charts/codesynapse --version <X.Y.Z> \
     --kube-context <ctx> --namespace codesynapse \
     --set secrets.existingSecret=codesynapse \
     --set ingress.enabled=true --set ingress.host=codesynapse.example.com --set ingress.tlsSecretName=codesynapse-tls \
     --set publicURL=https://codesynapse.example.com --set publicMCPURL=https://codesynapse.example.com/mcp
   helm test codesynapse --kube-context <ctx> --namespace codesynapse
   ```

   Every release publishes the chart at its version, signed like the images (`cosign verify ghcr.io/astrona-io/charts/codesynapse:<X.Y.Z> ...`). To install from a checkout, use `deploy/helm/codesynapse` in place of the `oci://` reference. Upgrades and backups: [docs/operations/upgrading.md](../../../docs/operations/upgrading.md).

3. Open the dashboard. The first account becomes the installation's root account and hands over to an organisation owner.

## Values worth knowing

| Value | Default | Meaning |
|---|---|---|
| `secrets.existingSecret` | required | The Secret above |
| `image.tag` | chart appVersion | Image tag on `ghcr.io/astrona-io/codesynapse-{api,worker,mcp,web}` |
| `image.pullSecrets` | `[]` | Pull secrets for a private registry, e.g. `[{name: ghcr}]` |
| `worker.replicas`, `worker.roles` | `1`, `ingest,ai,fix` | Job workers. Split roles across Deployments to scale them apart |
| `worker.resources.limits.memory` | `6Gi` | Ingest type-checks whole modules; size it for your largest repository. `GOMEMLIMIT` is set to 90% |
| `workspace.accessModes` | `[ReadWriteOnce]` | `api`, `worker` and `mcp` share clones. Use a ReadWriteMany class on more than one node |
| `api.replicas`, `mcp.replicas`, `web.replicas` | `1` | Raise to 2 or more for availability; see [Scaling out](#scaling-out) |
| `<api,mcp,web>.autoscaling` | off | CPU autoscaling (`minReplicas`, `maxReplicas`, `targetCPU`); needs metrics-server |
| `mcp.stateless` | `true` | No MCP transport session between requests, so no sticky sessions are needed |
| `ingress.*` | off | One host: `/mcp` to MCP, the rest to the dashboard. The API is never exposed |
| `<store>.enabled`, `<store>.externalURL` | in-chart | For production prefer managed or operator-run PostgreSQL, and a hardened HugeGraph |
| `features.allowPush` | `false` | Fix branches for accepted review comments |
| `traces.sample` | `1` | Share of new traces kept |
| `llmConfig` | empty | The content of `config/llm.yaml`; keys go in the Secret |

## Scaling out

`api`, `mcp` and `web` keep nothing between requests that another replica would need:

- **API:** login limits, jobs, schedules and the audit chain live in PostgreSQL. One replica at a time holds the `api-loops` lease and runs the scheduler and maintenance loops; another takes over within a minute when it stops.
- **MCP:** runs stateless (`mcp.stateless`). Every request carries the personal key and the chat's runtime session and is checked on its own, so any replica answers it. The tools send no server-initiated messages, so nothing is lost.
- **Web:** forwards each visitor's session cookie to the API and keeps no state.

With more than one replica, or with autoscaling, a component gets a PodDisruptionBudget (`maxUnavailable: 1`). Its pods prefer different nodes and spread over zones where the cluster has them.

**Workers** scale by `worker.replicas`, not by autoscaling: a worker removed mid-job leaves the job to another worker once its two-minute lease runs out. Split roles over separate releases or Deployments to size ingest apart from AI work.

**Limits that remain:**

- `api`, `worker` and `mcp` mount the same workspace volume. Spread over several nodes, it needs a ReadWriteMany storage class.
- Status lookups for annotation jobs (`/annotations/jobs/{id}`) and memory extraction jobs (`/memory/jobs/{id}`) answer only on the API replica that took the job. The work itself is unaffected, and the dashboard doesn't poll them. A durable annotation queue is a follow-up.
- PostgreSQL, HugeGraph and Milvus run as single instances. Clustered stores are 6d in [docs/SAAS.md](../../../docs/SAAS.md).

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
