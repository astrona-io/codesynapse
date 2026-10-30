# Findings of other security tools (SARIF import)

CodeSynapse has its own security rules. Many teams also run other scanners in CI: gitleaks for secrets, Semgrep, gosec, zizmor for GitHub workflows, kube-linter for manifests. Most of them can write SARIF 2.1.0. Upload that file and CodeSynapse keeps the findings beside its own, each labelled with the tool that found it.

This page covers phase 1: the upload, what is kept and how to read it back through the API. Showing the findings in the security report, the dashboard and MCP answers is planned in [the SARIF import plan](../plans/SARIF_IMPORT_PLAN.md).

## Upload from CI

```sh
gitleaks detect --report-format sarif --report-path gitleaks.sarif
gzip -c gitleaks.sarif | curl --fail-with-body -X PUT \
  -H "Authorization: Bearer $CODESYNAPSE_API_TOKEN" \
  -H "Content-Type: application/sarif+json" \
  -H "Content-Encoding: gzip" \
  --data-binary @- \
  "$CODESYNAPSE_URL/api/v1/repos/$REPO/sarif?commit=$(git rev-parse HEAD)"
```

- `PUT /api/v1/repos/{repo}/sarif?commit=<sha>` needs the Member role on the repository. The key must be a personal API key of a member; it acts in its own organisation.
- The body is the SARIF log, `application/sarif+json` or `application/json`, plain or gzip. Both the body as sent and the log inside it are capped at 32 MiB.
- `commit` is the hexadecimal commit the tool scanned. Findings of another commit than the indexed one are marked `stale`.
- A log may hold several runs (tools). Each tool's upload replaces that tool's earlier findings and keeps the other tools'. Runs of the same tool name, compared without case, are joined.
- Limits: 20 runs per log and 20 tools per repository, 20,000 results, 20,000 rules and 50,000 artifacts per log, and 50,000 findings per repository. Over a limit the request is refused with 413, never cut. The log is read as a stream, one result at a time, so memory follows what is kept, not what the log repeats. Messages are cut to 2 KiB before arguments are filled in and before redaction.
- The upload is recorded in the audit log like every changing request.

## What is kept

Only what CodeSynapse maps from each result, never the log itself:

| Field | From |
|---|---|
| Tool and version | `tool.driver.name`, `semanticVersion` or `version`. A run without a tool name is refused, because every finding must say who found it. |
| Rule | `ruleId` (or the rule a `ruleIndex` points at), and the rule's `helpUri` when it is a plain https link |
| Severity | GitHub's `security-severity` property when the result or rule has one (9 and up critical, 7 high, 4 medium, above 0 low, 0 info), else the SARIF level: `error` high, `warning` medium, `note` low, `none` info. The level falls back to the rule's default, then to `warning`. |
| Location | The first physical location: a repository-relative path and line range. Absolute `file://` paths are made relative to the log's `originalUriBaseIds`, else matched against the indexed files by their longest suffix (a CI runner's checkout path is not known). |
| Message | `message.text`, or the rule's message string with its arguments, or its short description; passed through secret redaction and cut to 300 characters |
| Suppressed | Set when the tool reported an accepted suppression |

Left out and counted per tool: results with no location in the repository (a URL, a path with `..`, an absolute path no indexed file matches), results of kind `pass` or `notApplicable`, and results whose baseline state is `absent`. Snippets, code flows, fixes and any other source text are never read. Identical results are kept once.

Findings live per organisation beside the code graph, like uploaded test runs: organisations that share a graph each see only their own uploads, a re-index keeps them, and they leave with the organisation's link or the graph.

## Read and remove

- `GET /api/v1/repos/{repo}/sarif` (Viewer) lists the tools (version, commit, upload time, findings by severity, suppressed, dropped, unmatched, stale) and the findings, most severe first. Each finding carries `tool` and `found_by` (tool and version). Filters: `tool`, `min_severity`, `limit` (default 100, at most 500); `total` is the number that matched.
- `DELETE /api/v1/repos/{repo}/sarif?tool=<name>` (Member) removes one tool's findings; without `tool` it removes all of them.
