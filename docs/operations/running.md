# Running the stack

## Quick start

Requires [just](https://github.com/casey/just) and Docker or Podman.

```sh
just up                       # writes .env with random secrets, builds and starts everything
just ingest ~/src/my-service  # copies the checkout into ./sources and indexes it
just mcp                      # register the MCP server with your agents
open http://localhost:3000
```

| URL | What |
|---|---|
| http://localhost:3000 | Dashboard |
| http://localhost:8080 | API (bearer `CODESYNAPSE_API_TOKEN`) |
| http://localhost:8090/mcp | MCP over streamable HTTP (bearer `CODESYNAPSE_MCP_TOKEN`) |
| http://localhost:7474 | Neo4j browser |

All ports bind to 127.0.0.1.

## Deployment topology

```mermaid
flowchart TB
    subgraph host[Docker compose network]
        web[web :3000]
        api[api :8080]
        mcp[mcp :8090]
        neo[(neo4j :7687)]
        pg[(postgres)]
        vm[(victoriametrics)]
        hg[(hugegraph)]
        mv[(milvus)]
    end
    user[Browser] --> web
    agent[Agent] --> mcp
    web --> api
    api --> neo
    mcp --> neo
    api --> pg
    mcp --> pg
    api -.-> hg
    api -.-> mv
    api --> vm
    mcp --> vm
    api -- host.docker.internal --> llm[Local model on host]
    api -- https --> forge[GitHub, GitLab, Forgejo]
    api -- names and versions --> osv[osv.dev / deps.dev]
```

Dashed boxes are the optional HugeGraph/Milvus backend (`docker-compose.hugegraph.yml`). The compose API container reaches model servers on the host through `host.docker.internal`.

## Daily development

| Recipe | Does |
|---|---|
| `just test` | `go test -race ./...` |
| `just lint` | golangci-lint, must report 0 issues |
| `just web-check` | Nuxt typecheck and build |
| `just ci` | All of the above |
| `just db` | Only Neo4j; then `just dev-api`, `just dev-mcp`, `just dev-web` |
| `just test-integration` | Store contract against compose Neo4j |
| `just test-postgres` | Accounts contract on a throwaway PostgreSQL |
| `just test-hugegraph` | HugeGraph/Milvus contract and parity |
| `just test-metrics` | Telemetry on a throwaway VictoriaMetrics |
| `just review` | Offline review pack for the current branch |

Run `just test` and `just lint` before calling a Go change done, and `just web-check` for anything under `services/web/`.

## Sync and scheduling

```mermaid
stateDiagram-v2
    [*] --> Idle
    Idle --> Running: manual Sync or timer fires
    Running --> Fetch: cloned repo
    Running --> Reindex: mounted folder
    Fetch --> Reindex: FetchCheckout ok
    Fetch --> Failed: error
    Reindex --> Review: AI review / triage enabled
    Reindex --> Idle: done, timer restarts
    Review --> Idle
    Failed --> Idle: backoff
```

Auto-sync runs every 30 minutes by default. The countdown starts when a sync finishes, and first runs are spread over the interval so repositories do not sync together. Schedules require an explicit delegated identity and current repository admin.

## Organisations

```mermaid
flowchart LR
    enter[Enter org, e.g. astrona-io] --> list[internal/forge lists repositories<br/>GitHub, Forgejo/Gitea, GitLab]
    list --> filter[Skip archived and forks unless ticked]
    filter --> queue[Queue at most 10 clones]
    queue --> clone[Clone and ingest]
    clone --> rescan[Rescan every 5 minutes for new repos]
    clone -- failure --> back[Back off one hour]
```

Repository names are `owner/name` on github.com and `host/owner/name` elsewhere, so identical names on two providers never collide.

## Kubernetes

Manifests live in `deploy/k8s` (Kustomize base + dev/prod overlays). Test them only on a throwaway kind cluster with `--context` set explicitly, never on a shared or production cluster. See `deploy/k8s/README.md`.

## AI models

Providers are configured in `config/llm.yaml` (copy `config/llm.example.yaml`; it is gitignored).

```mermaid
flowchart LR
    task{Task} --> route[Per-task route<br/>llm-routes.json]
    route --> global[Global fallback]
    route --> p1[openai-compatible<br/>MLX, Ollama, vLLM, LM Studio, OpenRouter]
    route --> p2[anthropic SDK]
    p1 --> local{Local?}
    local -- yes --> src[May receive snippets and source]
    local -- no --> diff[Only the diff the user asked to review]
```

Set `context_window` to the model's real context so budgets scale; without it, the safe default is 32k in / 8k out.

## Statistics

The API and MCP push metrics to VictoriaMetrics every 15 seconds; the **Statistics** page charts agent sessions, tool calls, latency, tokens, AI cost, database latency and memory use per organisation. Without `CODESYNAPSE_METRICS_URL` the page explains how to enable it.

## Building this documentation

```sh
pip install -r docs/requirements.txt
mkdocs serve        # http://127.0.0.1:8000
mkdocs build --strict
```
