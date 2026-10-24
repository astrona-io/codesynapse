# Cleanup and licences

Both reports are computed at ingest on the complete graph and stored, so they cover everything and never hit `store.MaxLimit`.

## Unused code

`internal/cleanup` computes reachability from real entry points.

```mermaid
flowchart TD
    subgraph roots[Entry points]
        m[main, init]
        ex[Exported API outside internal/ and cmd/]
        impl[Implicit stdlib methods<br/>String, Error, ServeHTTP...]
        pkg[Package-level initialisers]
        dec[Decorated / registered functions]
        vue[Vue setup bindings]
    end
    roots --> walk[Traverse CALLS, DISPATCHES_TO, REFERENCES]
    walk --> live[live]
    tests[Test functions] --> walk2[Traverse same edges]
    walk2 --> testonly[test-only]
    rest[Everything else] --> unreachable[unreachable]
```

Each function and module node carries a `liveness`. Packages only tests use are test support and not counted.

The report lists:

- functions with confidence and security relevance (security-relevant dead code first);
- files where nothing is live;
- unused direct Go modules, with the CVEs removing them eliminates;
- unused Helm values and template defines;
- a safe-removal checklist.

```sh
codesynapse cleanup .
```

Also available as the **Cleanup** page and the `unused_code` MCP tool (`security_only`, then `min_confidence: "high"`).

## Licences

`internal/license` checks every dependency's licence against the repository's own.

```mermaid
flowchart TD
    subgraph project[Project licence]
        p1[LICENSE text detection, SPDX headers] --> p2[package.json] --> p3[pyproject.toml]
    end
    subgraph dep[Dependency licence]
        d1[Lockfiles, vendor/, Go module cache,<br/>node_modules, site-packages METADATA] --> d2{Still unknown?}
        d2 -- yes, lookup enabled --> d3[deps.dev<br/>ecosystem, name, version only]
        d2 -- no --> d4
        d3 --> d4[Licence + source]
    end
    project --> cls
    d4 --> cls[Classify: public domain, permissive,<br/>weak / strong / network copyleft,<br/>restricted, unknown]
    cls --> cmp[Compare under distribution model<br/>SPDX OR = least restrictive, AND = most]
    cmp --> v{Verdict}
    v --> ok[ok]
    v --> review[review: weak copyleft, unknown]
    v --> conflict[conflict: strong or network copyleft<br/>in permissive project, GPL-2.0-only vs Apache-2.0]
```

Development-only dependencies turn conflicts into reviews. Every verdict carries its reason and the licence's source. This is guidance, not legal advice.

```sh
codesynapse licenses .                                      # Markdown
codesynapse licenses . --license-lookup --fail-on conflict  # CI gate
```

`--license-lookup` (or `CODESYNAPSE_LICENSE_LOOKUP=true`) is what sends package names and versions to deps.dev; without it, nothing leaves the machine.

## Dependency review page

**Dependency review** (per repository, under Dependencies and Licences) puts the packages that need a decision in one table: known advisories, licences to check and dependencies nothing imports. Each row shows the columns an agent-run review reports: dependency and version, how it was introduced, usage, licence, advisory, a proposed action and a verified-outcome column.

- **Introduced by** says "direct" for packages in your own manifest. For indirect packages it shows the shortest chain from a dependency you declared ("via a > b") and how many of your dependencies lead to it, so the fix is usually to upgrade the first link. Where no path is known the row says so and copies the command that finds it (`go mod why -m`, `npm explain`, `pipdeptree -r -p`).
- **Usage** comes from reachability for advisories (Go: your code calls a vulnerable symbol, imports the package, or only requires it; npm, PyPI and Cargo: a source file imports the package or none does; PHP, Ruby, .NET and Gradle: always "present, no import found", because their imports are not matched to packages) and from the unused-modules report.
- **Proposed action** is a suggestion from the data: remove an unused dependency, upgrade to the fixed version (marked urgent when your code calls the vulnerable symbol), replace or mitigate when no fix exists, ask about a licence conflict. Licence findings are flags for review, not legal findings.
- **Verified outcome** always starts as "Not verified": run the tests, build and scans after a change and Sync again.
- Below the table the page lists the package-manager commands for the ecosystems the repository uses, and the text to give an agent to run the full `dependency-review` prompt (any language, with smaller-alternative checks).

CodeSynapse reads go.mod and the lockfiles of npm, PyPI, Cargo, Composer, Bundler, NuGet and Gradle only (see below); a clean page is not proof that a project is free of vulnerabilities or compliant with a licence policy.

### Dependency paths

At ingest, every indirect module gets `via` (the shortest chain of modules from a declared dependency) and `roots` (how many declared dependencies reach it).

| Ecosystem | Where the edges come from |
|---|---|
| Go | `go list -deps -test` per go.mod: each import that crosses from one module into another is an edge. This is what the build uses, it works offline once dependencies are in the module cache, and it needs the local Go toolchain. A module that cannot be listed contributes what loaded. |
| npm | `dependencies` and `optionalDependencies` of each `package-lock.json` entry (lockfile v2 and v3) |
| Python | `dependencies` of `uv.lock` and `[package.dependencies]` of `poetry.lock` |

Not covered yet: yarn and pnpm lockfiles, `requirements.txt`, Pipfile.lock and lockfile v1 carry no edges, so their indirect packages show no path. Paths are by package name: when npm installs two versions of a package they share one path. `security_report` returns `via` and `roots` for each vulnerability.

### Lockfiles read

| Ecosystem | Files | Declared dependencies come from | Paths |
|---|---|---|---|
| Go | `go.mod` | the `require` lines | yes |
| npm | `package-lock.json`, `yarn.lock`, `pnpm-lock.yaml` | `package.json` | `package-lock.json` only |
| Python | `uv.lock`, `poetry.lock`, `Pipfile.lock`, `requirements*.txt` | `pyproject.toml` | `uv.lock`, `poetry.lock` |
| Rust | `Cargo.lock` | `Cargo.toml` and workspace members (dev-dependencies mark dev) | yes |
| PHP | `composer.lock` | `composer.json` (`packages-dev` marks dev) | yes |
| Ruby | `Gemfile.lock` | the `DEPENDENCIES` section | yes |
| .NET | `packages.lock.json` | `type: Direct` | yes |
| Java and Kotlin | `gradle.lockfile` | coordinates named in `build.gradle(.kts)` next to it | no |

Advisories come from osv.dev for all of them. Not read: Maven `pom.xml` (it holds ranges and properties, not the resolved graph), Gradle without dependency locking, NuGet without `packages.lock.json`, Swift, Dart, Elixir, C/C++. For those the agent prompt gives the commands to run. Versions that are not semver (four-part Ruby or NuGet versions) are matched against advisories but the suggested "fixed in" version is the first one OSV lists, which may belong to another release branch: check it.

## SBOM export

**Download SBOM** on the Dependencies and Dependency review pages, `GET /api/v1/repos/{repo}/sbom` and `codesynapse sbom [path] --file sbom.cdx.json` produce a CycloneDX 1.5 JSON software bill of materials.

- **Components**: every module go.mod and the lockfiles resolved, with name, version, package URL (`pkg:golang/…`, `pkg:npm/%40scope/…`, `pkg:cargo/…`, `pkg:composer/…`, `pkg:gem/…`, `pkg:nuget/…`, `pkg:maven/…`, `pkg:pypi/…`), licence (an SPDX expression, a known identifier, or the name as written), scope (`excluded` for development dependencies) and properties for the ecosystem, whether the project declares it, and its dependency path.
- **Dependencies**: the project depends on the declared modules, and each module on the modules it requires, where the lockfile records that (Go, npm `package-lock.json`, uv, poetry, Cargo, Composer, Bundler, NuGet). Yarn, pnpm, Gradle and requirements files have no edges; the document says so in a property instead of claiming that those packages have no dependencies.
- **Vulnerabilities** (when the repository was synced with the vulnerability scan on): id, severity, fix and reachability, pointing at the affected component.
- The timestamp is the ingest time and the serial number is derived from the repository, commit and ingest time, so exporting the same ingest twice gives identical bytes. A list cut by a size limit (20,000 modules) is marked with a `codesynapse:truncated` property.

It lists what the lockfiles resolve, not what a build finally ships: a bundler may leave packages out, a container image adds its own, and vendored or copied code is not included. Treat it as the input to a build-time check, not a replacement for one. The CLI is offline by default; `--vulns` adds an osv.dev lookup (ecosystem, package names and versions only).
