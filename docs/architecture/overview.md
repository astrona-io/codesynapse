# Architecture overview

## Services

Three services plus the graph database, all defined in `docker-compose.yml`.

| Service | Code | Port | Purpose |
|---|---|---|---|
| `mcp` | `services/mcp` | 8090 | MCP server for AI agents (stdio or streamable HTTP) |
| `api` | `services/api` | 8080 | JSON API for the dashboard; runs ingest, clone and sync jobs |
| `web` | `services/web` (Nuxt 4) | 3000 | Dashboard; proxies to `api` server-side |
| `nebula-graphd` (+ `nebula-metad`, `nebula-storaged`) | images | internal only | Graph store (nGQL; never published to the host) |
| `milvus` | image | internal only | Agent-memory vectors |

`cmd/codesynapse` is the CLI (`ingest`, `audit`, `stats`, `review`, `licenses`, `cleanup`, `mcp install`). It is not a service; it is for CI and local use.

```mermaid
flowchart TB
    subgraph clients[Clients]
        agent[AI agent<br/>Claude Code, Codex, Cursor...]
        browser[Browser]
        ci[CI job / developer]
    end

    subgraph services[CodeSynapse services]
        mcp[codesynapse-mcp]
        web[web - Nuxt 4]
        api[codesynapse-api]
        cli[codesynapse CLI]
    end

    subgraph shared[Shared packages]
        query[internal/query]
        agentctx[internal/agentctx]
        review[internal/review]
        security[internal/security]
        store[internal/store]
    end

    neo[(NebulaGraph)]
    pg[(PostgreSQL<br/>accounts)]
    vm[(VictoriaMetrics)]
    milvus[(Milvus<br/>vectors)]
    llm[Local or cloud LLM]

    agent -- MCP --> mcp
    browser --> web -- "/api/cg" --> api
    ci --> cli

    mcp --> agentctx
    api --> query
    cli --> review
    agentctx --> query
    query --> store
    review --> store
    security --> store
    store --> neo
    api --> pg
    mcp --> pg
    api --> llm
    api --> milvus
    api -. push metrics .-> vm
    mcp -. push metrics .-> vm
```

Every reader goes through `internal/query` (and `internal/agentctx` for the composite agent tools). MCP, API and CLI therefore return the same answers. `internal/store/memory` implements the same `store.Store` interface for tests and `--offline` mode.

## Request flow: an agent asks for symbol context

```mermaid
sequenceDiagram
    autonumber
    participant Agent
    participant MCP as mcp server
    participant Auth as policy + accounts
    participant Ctx as agentctx
    participant Store as store (scoped)
    participant DB as NebulaGraph

    Agent->>MCP: tools/call get_symbol_context {name, repo}
    MCP->>Auth: authenticate key, check tool policy and repo access
    Auth-->>MCP: caller + visible repositories
    MCP->>Ctx: SymbolContext(repo, name, verbosity)
    Ctx->>Store: ForRepository(repo) fence
    Ctx->>Store: resolve name / qualified_id
    Store->>DB: constant nGQL, values in parameters
    DB-->>Store: nodes and edges
    Store-->>Ctx: symbol, callers, callees, tests
    Ctx-->>MCP: compact JSON with file:line, risk score
    MCP-->>Agent: result (+ usage counters recorded)
```

If the name is ambiguous, `agentctx.Resolve` returns a `SuggestionError` listing `qualified_id` values, and the agent retries with one of them.

## Request flow: the dashboard

```mermaid
sequenceDiagram
    participant B as Browser
    participant W as web server routes
    participant A as api
    participant S as scope.go route policy
    participant Q as query / store

    B->>W: GET /api/cg/repos/x/security (cs_session cookie)
    W->>W: reject cross-origin mutations
    W->>A: forward cs_session and X-CodeSynapse-Org (never the API token)
    A->>S: s.route(pattern, repoRole, handler)
    S->>S: role on the repository's organisation?
    alt not visible
        S-->>W: 404
    else allowed
        S->>Q: handler runs with scope
        Q-->>A: result
        A-->>W: JSON
    end
    W-->>B: JSON
```

## Package map

```mermaid
flowchart LR
    model[internal/model<br/>kinds, IDs, Graph]
    ingest[internal/ingest<br/>golang, lang, docs, config, githistory]
    cleanup[internal/cleanup]
    vuln[internal/vuln]
    license[internal/license]
    config[internal/config]
    metrics[internal/metrics]
    security[internal/security]
    redact[internal/redact]
    gitx[internal/gitx]
    store[internal/store]
    query[internal/query]
    agentctx[internal/agentctx]
    review[internal/review]
    reviews[internal/reviews]
    memory[internal/memory]
    accounts[internal/accounts]

    ingest --> model
    ingest --> redact
    ingest --> gitx
    cleanup --> model
    vuln --> model
    license --> config
    metrics --> model
    security --> model
    store --> model
    query --> store
    agentctx --> query
    review --> query
    store --> reviews
    store --> memory
    query -. authz .-> accounts
```

See the [AGENTS.md layout section](https://github.com/astrona-io/codesynapse/blob/main/AGENTS.md) for the full directory list.
