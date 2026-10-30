# SARIF import of external security tools

Findings of external security linters join CodeSynapse's security report, and every finding names the tool that flagged it: tool name and version, the tool's own rule ID and a link to the rule's documentation when the tool gives one. CodeSynapse's own rules keep their "CodeSynapse built-in rule" label.

## Phase 1: parser, upload and storage (done)

- `internal/sarifimport`: a bounded SARIF 2.1.0 reader (32 MiB, 20 runs, 20,000 results, 20,000 rules per run) that decodes only the fields it maps, so snippets and code flows are never held. Paths are cleaned and repository-relative (no `..`, no other schemes); absolute paths are made relative through `originalUriBaseIds` or placed on the indexed file with the longest matching suffix (`Place`, bounded lookups). Messages pass `redact.Summary` (300 runes); names, versions and rule IDs are made printable and redacted. Severity comes from `security-severity`, else the SARIF level. `FuzzParse` checks the invariants.
- `PUT|GET|DELETE /api/v1/repos/{repo}/sarif` (`services/api/internal/api/sarif.go`): Member to upload and delete, Viewer to read, the organisation namespace from the authenticated caller (`memNamespace`). A tool's upload replaces its earlier one and keeps other tools'; limits are refused (413), never cut.
- Storage: `store.SARIFStore`, per namespace beside the graph like runtime facts. NebulaGraph keeps the set as a chunked gzipped document in the existing `runtime_head`/`runtime_chunk` tags under the key `sarif:<ns>` (no schema change); deleting the graph or an organisation's link removes it. Covered by `storetest.sarifContract`.
- How to use it: [operations/sarif-import.md](../operations/sarif-import.md).

## Phase 2: show them (planned)

1. **Security report.** `internal/security` gains external findings as a separate list (`external`), counted by severity and by tool, with `found_by`. They do not change the grade at first: a tool's severities are not calibrated against ours, and a noisy tool should not fail a repository. A later option lets an organisation count chosen tools in the grade.
2. **MCP.** `security_report` and `get_pr_context` add the external findings in the files they cover, each with `found_by`, rule ID and help link, read through a scoped helper like `store.RuntimeFactsOf` (namespace from the authenticated caller, refused for another graph or a fenced reader). Update `docs/AGENT_GUIDE.md` in the same change.
3. **Dashboard.** The Security page gets an "Other tools" section: per tool, version, commit, upload time and stale badge, and the findings with a "found by <tool>" label (`SevBadge` for severity), filtered by tool and severity, with a link to the rule help. Members can remove a tool's findings there.
4. **CLI.** `codesynapse sarif upload <file>…` (gzip, `--commit` defaults to HEAD, key from `CODESYNAPSE_API_TOKEN`), like `codesynapse runtime upload`, so CI needs no curl.
5. **Suppressions.** Honour the tool's own suppression comments (`nolint`, `nosec`, `noqa`, `eslint-disable`) as reported in SARIF (`suppressions`) as "suppressed by the tool, with reason"; the tenant-local approval overlay applies to external findings the same way as to built-in ones.

## Phase 3: run tools (to discuss)

Running licence-compatible scanners in a sandboxed worker (gitleaks, zizmor, kube-linter, Ruff `S` rules, ESLint security plugins, govulncheck) after ingest, so findings appear without CI changes. This needs a decision on the sandbox, the tool images and their licences before any work.
