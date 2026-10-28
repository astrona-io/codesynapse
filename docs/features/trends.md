# Trends

!!! tip "In plain words"
    The latest numbers do not tell you whether things are getting better or worse. After every sync CodeSynapse keeps a small snapshot of the repository, so the Trends page can show what changed over time.

"Are we getting better or worse?" needs more than the latest numbers. After every sync CodeSynapse keeps a small snapshot of the repository, so the Trends page, the Overview and agents can say what changed.

## What is kept

A snapshot is about thirty numbers taken from the reports the ingest already stored, with the ingest time, commit and indexed branch:

| Group | Numbers |
|---|---|
| Security | grade and score, findings and advisories by severity, vulnerabilities your code calls |
| Dependencies | total and declared modules, licence conflicts and reviews |
| Code | functions, function lines, unused functions, unused lines, unused dependencies |
| Duplicates | redundant lines in exact and edited copies, their share of the function lines |
| Quality | share of code reached by tests, share of exported symbols documented |

No source, names or file paths. The latest 500 snapshots of a repository are kept; a snapshot with the same ingest time replaces the earlier one. History lives next to reviews, apart from the code graph: re-indexing does not erase it and deleting the repository does.

## What counts as worse

Each sync is compared with the previous sync of the same branch, so switching the indexed branch (say from `main` to `release`) is never reported as getting worse or better. Snapshots taken before the branch was recorded, and detached checkouts, count as one branch of their own. A change is reported when it is large enough not to be noise:

| Metric | Reported from |
|---|---|
| Security grade | any step down (a repository that stops being checkable counts) |
| Security score | 1 point |
| Vulnerabilities your code calls | 1 |
| High and critical findings, high and critical advisories | 1 |
| Licence conflicts | 1 |
| Duplicate code | 0.5 points of the function lines |
| Unused functions | 5 |
| Unused dependencies | 1 |
| Code reached by tests | 2 points |
| Documented symbols | 3 points |

Growth in size or in the number of dependencies is never a regression on its own.

## Where

- **Web**: the **Trends** page of each repository (what got worse and better since the previous sync, and six charts). The repository summary has a **Security trend** panel: the grade after each sync and the critical, high, medium and low findings over time, one branch at a time. On the Overview, a repository that got worse gets one item: high when the grade fell or vulnerabilities your code calls, serious findings or licence conflicts grew, low otherwise.
- **Notifications**: the bell alerts when a sync lowered the security grade or added critical or high findings or serious vulnerabilities, compared with the previous sync of the same branch. Like every notification it is shown only to people who can see the repository.
- **MCP**: `repo_trend` (optionally `limit`); `branches` holds the security trend per branch. An agent can check that its change did not make something worse after the next sync.
- **API**: `GET /api/v1/repos/{repo}/history?limit=`.

A repository starts with one point and gets its first trend after the second sync. `codesynapse ingest` records a snapshot too when it writes to the database.
