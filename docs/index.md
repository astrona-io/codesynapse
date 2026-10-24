# CodeSynapse

CodeSynapse indexes a repository into one property graph and answers small, precise questions from it. A coding assistant asks the graph ("who calls `ReplaceRepo`?", "what does this diff break?") instead of opening whole files. That cuts tokens and gives better review context.

On this repository the context an agent needs per question drops by a median 92%, measured by `codesynapse stats`.

## What goes into the graph

| Source | What is extracted |
|---|---|
| Go | Packages, files, functions, methods, types, exact calls and interface dispatch (`go/types`) |
| Python, JS/TS, Vue, Java, Kotlin, Rust, C# | Structural symbols, imports, name-based call edges, tests |
| Markdown | Sections with line ranges; `MENTIONS` edges to exported symbols |
| YAML, Helm, Go templates | Kubernetes resources, key paths, `{{define}}` blocks, `.Values` links |
| Git history | Commits, authors (e-mail hashed), per-file churn |
| Dependencies | Go, npm, PyPI, Rust, PHP, Ruby, .NET and Gradle modules with licences and CVEs (via osv.dev) |
| Security rules | Findings per rule, linked to the owning function |

## Big picture

```mermaid
flowchart LR
    subgraph Sources
        R[Repository checkout]
        G[Git remote]
    end
    subgraph Ingest
        P[Pipeline<br/>Go, lang, docs, config, git]
        V[Vuln and licence lookup]
    end
    DB[(Graph store<br/>HugeGraph + Milvus)]
    subgraph Readers
        MCP[MCP server<br/>:8090]
        API[JSON API<br/>:8080]
        CLI[codesynapse CLI]
    end
    A[AI agents]
    W[Web dashboard<br/>:3000]
    CI[CI / developer shell]

    R --> P
    G --> P
    V --> P
    P --> DB
    DB --> MCP --> A
    DB --> API --> W
    DB --> CLI --> CI
```

## Where to start

- **I want to understand the system:** [Architecture overview](architecture/overview.md).
- **I want to know what is stored:** [Graph schema](architecture/graph-schema.md).
- **I am wiring up an agent:** [Agent context (MCP)](features/agent-context.md), then the [Agent guide](AGENT_GUIDE.md). For Claude Code, start with the [Claude Code bridge](operations/claude-code-bridge.md).
- **I am reviewing security posture:** [Security review](features/security.md) and [Security model](operations/security-model.md).
- **I am deploying it:** [Running the stack](operations/running.md).

## Design principles

1. **Deterministic answers.** Graph tools never call an LLM; the agent using them is the LLM.
2. **Read-only by default.** Only review tools and opt-in agent memory write, and only to their own stores.
3. **No source bodies in the graph.** Hashes, signatures, key paths, line ranges and redacted summaries only.
4. **Untrusted input everywhere.** Repositories, model output and agent text are treated as hostile.
5. **Tenant isolation in the data.** Every API route and MCP tool is scoped to the caller's organisation.
