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
