# CodeSynapse

[![CI](https://github.com/astrona-io/codesynapse/actions/workflows/ci.yml/badge.svg)](https://github.com/astrona-io/codesynapse/actions/workflows/ci.yml)
[![Artifact Hub](https://img.shields.io/endpoint?url=https://artifacthub.io/badge/repository/codesynapse)](https://artifacthub.io/packages/search?repo=codesynapse)
[![License](https://img.shields.io/badge/license-Apache--2.0-blue)](LICENSE)

CodeSynapse indexes a repository into a graph and answers small, precise questions from it, so your coding agent asks the graph instead of opening whole files.

Ask for `ReplaceRepo` and you get its signature, location, 3 callers, the 2 tests that reach it and the doc section that describes it, in a few hundred tokens. On this repository that cuts the context an agent needs per question by a median 92% (`codesynapse stats`).

- **Indexes** Go (type-checked, exact calls and interface dispatch), Python, JavaScript, TypeScript, Vue, Java, Kotlin, Rust, C#, PHP, Ruby, Markdown, YAML, Helm charts, git history and dependencies with their CVEs.
- **Serves** agents over MCP, people through a web dashboard, and CI through a CLI.
- **Reviews** security (graded A–F, with reachability), licences, unused code, duplicates and the impact of a diff.
- **Runs** on NebulaGraph and Milvus, all Apache-2.0, on your own machine or cluster.

## Quick start

You need [just](https://github.com/casey/just) and Docker or Podman.

```sh
just up                       # writes .env with random secrets, builds and starts everything
open http://localhost:3000    # dashboard: sign in as admin (password in .env), replace it, create your organisation
just ingest ~/src/my-service  # indexes a checkout (needs a git origin remote)
```

`just ingest` indexes the checkout through the worker, named after its `origin` remote. To see it in your organisation, sign in as an owner, open Repositories, choose Add and paste the origin's address: the dashboard links the index instead of cloning (a private repository needs your organisation's Git token). Repositories you add by URL are cloned and indexed by the worker without `just ingest`.

Then connect your agent. For Claude Code: sign in, create a key under Settings → API keys, and run

```sh
CODESYNAPSE_MCP_TOKEN=<key> just mcp
```

Other clients and the session contract: [Agent context](docs/features/agent-context.md#connecting-an-agent). What to ask once connected: [Agent guide](docs/AGENT_GUIDE.md).

## CLI

No server needed for the offline commands:

```sh
brew install astrona-io/tap/codesynapse
# or: go install github.com/astrona-io/codesynapse/cmd/codesynapse@latest

codesynapse audit .                                  # security review
codesynapse audit . --fail-on high                   # CI gate
codesynapse audit . --changed --fail-on high         # pre-commit: staged files only
codesynapse licenses .                               # dependency licences
codesynapse cleanup .                                # unused code, files, modules
codesynapse upgrade-impact golang.org/x/net@v0.40.0  # what a Go module upgrade changes for you
codesynapse review . --base origin/main --offline    # impact of the current branch
codesynapse review . --offline --fail-on-new high    # CI gate on what the branch adds
```

## GitHub Action

Review every pull request: changed symbols and their impact, the tests to run, and the security findings the change introduces and fixes, posted as one comment that updates on each push.

```yaml
on: pull_request
permissions:
  contents: read
  pull-requests: write
jobs:
  codesynapse:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v7
        with:
          fetch-depth: 0
      - uses: astrona-io/codesynapse@vX.Y.Z   # a release tag, or its commit SHA
        with:
          fail-on-new: high               # fail on new high or critical findings only
          # coverage: cover.out           # measured coverage of the changed code
          # junit: report.xml             # flaky and failing tests that reach it
```

The action builds the CLI from its own source and never runs the code it reviews. Pull requests from forks get the report in the job summary; their token cannot comment.

## Kubernetes

```sh
helm install codesynapse oci://ghcr.io/astrona-io/charts/codesynapse --version X.Y.Z
```

Images and chart are signed with cosign. See the [chart README](deploy/helm/codesynapse/README.md).

## Development

```sh
just            # list recipes
just ci         # what CI runs: lint, race tests, web build, licences, checks
just dev-web    # Nuxt dev server against the API of `just up`
```

| Path | What |
|---|---|
| `cmd/codesynapse` | CLI |
| `services/api`, `services/mcp`, `services/web` | API and workers, MCP server, Nuxt dashboard |
| `internal/` | Shared core: ingest, graph model, stores, security, review |
| `rules/` | Security rules, one YAML file each |
| `deploy/helm/codesynapse` | Helm chart |

Read [AGENTS.md](AGENTS.md) before changing code: it has the conventions and the security rules every change must keep. Architecture starts at [docs/architecture/overview.md](docs/architecture/overview.md).

## Documentation

| | |
|---|---|
| [Running the stack](docs/operations/running.md) | Compose, workers, sync, organisations, AI models, statistics |
| [Security review](docs/features/security.md) | Rules, languages, reachability, CI usage |
| [Reviews and fix branches](docs/features/reviews-fixes.md) | Agent reviews, decisions, draft PRs |
| [Agent memory](docs/features/memory.md) | Session and repository memory, Claude Code hooks |
| [Accounts and access](docs/operations/accounts-access.md) | Organisations, permissions, SSO, audit log |
| [Security model](docs/operations/security-model.md) | How untrusted repositories are handled |
| [Upgrading](docs/operations/upgrading.md) · [Releasing](docs/operations/releasing.md) | |

Read the [documentation site](https://codesynapse.astrona.io/), published with each stable release. The source is in [`docs/`](docs/index.md); `just docs` serves it locally.

## Contributing

Issues and pull requests are welcome. Run `just ci` before you open a PR, use [conventional commits](https://www.conventionalcommits.org/), and pin new GitHub actions by commit SHA.

## License

Copyright 2026 Astrona.io. Licensed under the [Apache License, Version 2.0](LICENSE); see [NOTICE](NOTICE) for attributions. All Go and production npm dependencies use permissive licences, and NebulaGraph and Milvus run as separate Apache-2.0 images.
