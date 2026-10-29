# CodeSynapse

[![CI](https://github.com/astrona-io/codesynapse/actions/workflows/ci.yml/badge.svg)](https://github.com/astrona-io/codesynapse/actions/workflows/ci.yml)
[![Artifact Hub](https://img.shields.io/endpoint?url=https://artifacthub.io/badge/repository/codesynapse)](https://artifacthub.io/packages/search?repo=codesynapse)
[![License](https://img.shields.io/badge/license-Apache--2.0-blue)](LICENSE)

CodeSynapse reads your code once and keeps what it learned in a searchable map, called a graph. Your AI coding assistant then asks the map small, exact questions instead of opening whole files. That means fewer tokens (the unit AI models read and charge for), faster answers and better context for code review.

For example, ask for a function called `ReplaceRepo` and you get its signature, where it lives, its 3 callers, the 2 tests that reach it and the doc section that describes it. That is a few hundred tokens instead of several whole files. On this repository the context an assistant needs per question is a median 92% smaller (`codesynapse stats` measures it).

New to these words? See the [glossary](docs/GLOSSARY.md).

## What it does

- **Reads (indexes) your project.** Go is checked with the compiler's own type information, so calls and interface use are exact. Python, JavaScript, TypeScript, Vue, Java, Kotlin, Rust, C#, PHP and Ruby are read structurally. It also reads Markdown, YAML, Helm charts, git history and your dependencies, including their known security holes (CVEs).
- **Answers three kinds of users.** AI assistants connect over MCP, a standard way for an assistant to call outside tools. People use a web dashboard. Build servers (CI) use a command-line tool.
- **Reviews.** It grades security from A to F and says whether vulnerable code can actually be reached. It also checks licences, finds unused and duplicated code, and shows what a change to your code affects.
- **Runs on your own machine or cluster.** It uses NebulaGraph (the graph database) and Milvus (a store for searching by meaning), both Apache-2.0.

## Quick start

You need [just](https://github.com/casey/just) (a command runner) and Docker or Podman (they run the programs in containers).

```sh
just up                       # writes .env with random secrets, builds and starts everything
open http://localhost:3000    # dashboard: sign in as admin (password in .env), replace it, create your organisation
just ingest ~/src/my-service  # indexes a checkout (needs a git origin remote)
```

What these do:

1. `just up` starts the whole stack. The first run creates a file called `.env` with random passwords, so nothing ships with a default one.
2. The dashboard asks you to sign in as `admin`. The password is in `.env`. You then replace this account with your own and create your organisation (your team's private space).
3. `just ingest` indexes a project on your disk. It names the index after the checkout's `origin` remote.

To see that project in your organisation, sign in as an owner, open Repositories, choose Add and paste the origin's address. The dashboard links the index it already has instead of cloning it again. A private repository needs your organisation's Git token. Repositories you add by URL are cloned and indexed by the worker, so you do not need `just ingest` for them.

Then connect your assistant. For Claude Code, sign in, create a key (a long secret that lets a program act as you) under Settings → API keys, and run:

```sh
CODESYNAPSE_MCP_TOKEN=<key> just mcp
```

Other assistants and the session rules: [Agent context](docs/features/agent-context.md#connecting-an-agent). What to ask once connected: [Agent guide](docs/AGENT_GUIDE.md).

## CLI

These commands work on a folder directly. They need no running server:

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

Review every pull request automatically: changed functions and their impact, the tests to run, and the security findings the change introduces and fixes, posted as one comment that updates each time you push.

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

For a shared installation, use the Helm chart (the package for Kubernetes):

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

Read [AGENTS.md](AGENTS.md) before changing code. It has the conventions and the security rules every change must keep. Architecture starts at [docs/architecture/overview.md](docs/architecture/overview.md).

## Documentation

| | |
|---|---|
| [Glossary](docs/GLOSSARY.md) | What each term means |
| [Running the stack](docs/operations/running.md) | Compose, workers, sync, organisations, AI models, statistics |
| [Security review](docs/features/security.md) | Rules, languages, reachability, CI usage |
| [Reviews and fix branches](docs/features/reviews-fixes.md) | Agent reviews, decisions, draft PRs |
| [Agent memory](docs/features/memory.md) | Session and repository memory, Claude Code hooks |
| [Accounts and access](docs/operations/accounts-access.md) | Organisations, permissions, SSO, audit log |
| [Security model](docs/operations/security-model.md) | How untrusted repositories are handled |
| [Upgrading](docs/operations/upgrading.md) · [Releasing](docs/operations/releasing.md) | |

Read the [documentation site](https://codesynapse.astrona.io/), published with each stable release. The source is in [`docs/`](docs/index.md); `just docs` serves it locally.

Every docs page shows a **Draft** badge, and a pencil in the navigation, until a maintainer has read it and checked it against the code. To mark a page verified, add this front matter at the top of its Markdown file:

```markdown
---
verified: true
---
```

Leave it out, and new or regenerated pages stay drafts. The key is `verified`, not `status`, because the architecture catalog reads `status:` as an ADR's decision status. Any value other than `true` or `false`, or `status: draft`/`status: verified`, fails `just docs-build` (`docs/hooks/doc_status.py`).

## Contributing

Issues and pull requests are welcome. Run `just ci` before you open a PR, use [conventional commits](https://www.conventionalcommits.org/), and pin new GitHub actions by commit SHA.

## License

Copyright 2026 Astrona.io. Licensed under the [Apache License, Version 2.0](LICENSE); see [NOTICE](NOTICE) for attributions. All Go and production npm dependencies use permissive licences, and NebulaGraph and Milvus run as separate Apache-2.0 images.
