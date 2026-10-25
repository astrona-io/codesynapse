# Security rules

One YAML file per rule. `code/` holds rules for source code (Go, Python, JavaScript, TypeScript, Vue); `config/` holds rules for Kubernetes manifests, compose files, GitHub Actions, Dockerfiles and dependency update automation.

These files are the single source of truth for everything a person reads about a rule: the title and severity in reports, the "why it matters" and "how to fix" text, and the vulnerable → fixed examples on the Security page, in `codesynapse audit`, and in the `rule_guide` MCP tool. They are embedded into the binaries at build time.

The detection logic lives in Go next to the parser for each language:

| Category | Detector |
|---|---|
| Go | `internal/ingest/golang/findings.go` |
| Python, JavaScript, TypeScript, Vue | `internal/ingest/lang/rules.go` |
| YAML (Kubernetes, compose, GitHub Actions) | `internal/ingest/config/yaml.go` |
| Dockerfile, Containerfile | `internal/ingest/config/dockerfile.go` (ids listed in `yaml.go`) |
| Dependency update automation (Renovate, Dependabot) | `internal/ingest/config/renovate.go` (ids listed in `yaml.go`) |

## Fields

```yaml
id: sql-concat                 # equals the file name
title: SQL query built from non-constant strings
severity: high                 # info | low | medium | high | critical
cwe: [CWE-89]
category: code                 # equals the directory: code | config
languages: [go, python, javascript, typescript]
why: What an attacker gains, in one or two sentences.
fix: What to do instead, in one sentence.
references:
  - https://cwe.mitre.org/data/definitions/89.html
examples:                      # at least one; one per affected language is best
  - language: go
    vulnerable: db.Query("SELECT ... '" + name + "'")
    fixed: db.Query("SELECT ... $1", name)
```

`schema.json` describes the format; editors with the YAML language server validate and autocomplete against it.

## Adding a rule

1. Write `rules/<category>/<id>.yaml`.
2. Add the detection to the detector for the language (see the table) and report findings with the same `id`.
3. Add a fixture that must trigger it and one that must not (false positive) to that detector's tests.
4. `just test`: `TestCatalog` validates every file, and `TestEveryDetectorHasARule` fails if a detector reports an id without a file or a file has no detector.

Changing a severity or wording needs only step 1 and a rebuild.
