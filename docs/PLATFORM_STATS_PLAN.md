# Platform statistics for administrators

Status: proposed. Phases 1 (access, tenants, sessions), 2 (realistic token savings), 3 (usage and traffic), 5 (models and cost) and 8 (the daily rollup, and the 90d and 365d ranges of the summary, tenants and sessions sections) are implemented; the rest is not, and the other sections that read the rollup come with their phases. It builds on [platform administration](operations/accounts-access.md#platform-administration), [root separation](ROOT_BOOTSTRAP_PLAN.md), [repository permissions](ACCESS_CONTROL_PLAN.md), [telemetry](TELEMETRY_PLAN.md) and its [metrics coverage plan](TELEMETRY_PLAN.md#metrics-coverage-plan), and [ingestion transparency](INGEST_TRANSPARENCY_PLAN.md).

!!! tip "In plain words"
    Platform administrators run the installation. Today they see a health check, a list of organisations and users, the audit log and four charts. This plan adds a Statistics area under Administration: how the platform grows, how it is used, how jobs, indexing, models and stores behave, and what security and audit look like across all organisations. It shows counts and timings only. It never shows code, repository names, paths, prompts or e-mail addresses in cross-tenant figures.

## Goal

Answer the questions an operator has about the whole installation, from one place, without giving the operator access to any organisation's code:

- Is the platform growing, and who is using it?
- Is it healthy: are jobs waiting, failing or lost, are the stores slow or full, are alerts firing?
- What does it cost: model calls, tokens and estimated spend, and how much runs on local models?
- Is it safe: how do security grades and findings develop across organisations, do sign-ins fail in bursts, does the audit chain verify?

## What exists today

The plan extends these pieces. It does not replace them.

| Piece | Where | What it gives | Limits for this plan |
|---|---|---|---|
| Admin overview | `services/web/app/pages/admin/overview.vue`, `GET /api/v1/admin/health` (`admin.go`) | Store checks, user, admin, suspended, organisation and graph counts, audit chain check, job totals, unowned graphs; platform health charts; the four former platform usage charts moved to Statistics → Usage in phase 3 | Totals only, no trends, no per-organisation table |
| Admin organisations and users | `GET /api/v1/admin/orgs`, `GET /api/v1/admin/users` | Members, roles, repositories, lost access, teams; memberships, active keys, last key use | Lists of up to 1,000, no activity or growth |
| Audit log | `GET /api/v1/admin/audit`, `.../verify`, export (`audit.go`) | Every changing request and security event, hash chained | Rows, not rates |
| Series panels | `GET /api/v1/stats/series` and `/batch` (`series.go`), `GET /api/v1/stats/models` (`stats_models.go`) | Fixed MetricsQL panels, five ranges (1h to 30d), tenant scope as `extra_filters`; `platform: true` panels cross tenants | Platform panels accept API keys; tenant series are labelled by opaque ID |
| Workspace statistics | `GET /api/v1/stats` (`stats.go`), Statistics pages under `services/web/app/pages/stats/` | Per-caller MCP usage, memory, licences, reviews, AI, jobs, repositories | Always scoped to the caller's visible repositories; not a platform view |
| Usage buckets | `internal/usage`, `:McpUsage` nodes | Daily calls, errors, response tokens, latency per organisation, repository, tool and client; workflow, Bash, retrieval and unindexed pseudo tools; 180 days | Keyed by repository name; unindexed buckets carry checkout names |
| Telemetry | `internal/telemetry/metrics.go`, VictoriaMetrics, at most 30 days | Tenant instruments with `tenant_id`; `platform_*` for work without a tenant | Phase 1 (platform health), phase 2 (vmalert) and phase 3 (jobs and indexing) of the coverage plan are being built separately |
| Jobs | `internal/accounts/jobs.go` (`JobRecord`, `JobQueue`) | Durable jobs with kind, organisation, state, attempts, times | `Repo` and `Error` are tenant content |
| Ingest reports | `internal/ingestreport`, `accounts.IngestRun` | Exact file accounting per run, skip reasons, status | Per repository and tenant |
| Trends | `internal/history`, `internal/snapshot` | Per-repository snapshot after each ingest: grade, findings and vulnerabilities by severity, licences, dead code, duplicates, coverage | Per repository |
| Model prices | `services/api/internal/llm/pricing.go` | `price_source` and `price_checked` per provider, optional OpenRouter feed | Cost is unknown when prices or usage are missing |

## Audience and access

**Decisions.**

1. **Platform administrators only, from a signed-in browser session.** API keys and the installation token are refused, as for suspending users and the platform admin flag (`accounts.requirePlatform`). An API key acts in one organisation; cross-tenant figures do not belong to one. There is no MCP tool for these figures.
2. **Root sees them.** The bootstrap root is created with platform administration and is limited to governance. [ROOT_BOOTSTRAP_PLAN.md](ROOT_BOOTSTRAP_PLAN.md) lists telemetry, health and audit as governance, and these statistics hold no repository content. So the routes are governance routes (`s.account(..., governanceRoute, ...)`). If an installation is classified `ambiguous`, every platform admin is limited to governance and still sees them. Root sees organisation names in the admin tables, as it already does under Administration → Organisations (decided).
3. **No code access follows from it.** Platform administration gives no repository permissions. Nothing here links into an organisation's repositories, reviews, memory or findings. Drill-down stops at the organisation.
4. **Every view is audited.** `GET` requests are not covered by the HTTP audit middleware, which records changing requests. Each statistics request therefore records a semantic event with `Service.Record` before it answers: `admin.stats.view` with the section and range as target, or `admin.stats.export` with section, range and format. If the event cannot be written, the request answers 503 and returns no figures, as MCP withholds a result it cannot audit. To keep the log readable, the page loads one tab with one batch request, so one tab view is one event, and automatic refresh is off by default (at most once a minute when switched on, each refresh audited).
5. **The existing admin reads follow the same rule (decided).** `GET /api/v1/admin/users`, `/admin/orgs`, `/admin/health` and the platform panels on `/api/v1/stats/series` also become signed-in only and audited on view (`admin.users.view`, `admin.orgs.view`, `admin.health.view`, `admin.stats.view`). They refuse API keys. **This breaks scripts that read them with a key.** The change ships in phase 1, and its release notes and `docs/operations/accounts-access.md` say so.
6. **Single-tenant installs.** Without accounts there are no platform admins. The Statistics pages there stay the workspace pages they are today.

**Enforcement.** One helper, `accounts.Service.PlatformStats(ctx, p, section)`, reuses `requirePlatform` (session only, fresh principal, platform admin flag read from the store) and writes the audit event. Every handler calls it first. A suspended account fails at authentication, as today.

## What to show

Each group lists its figures, the source (see the next section for the rules), the finest step and how long the figures are kept. "Org" means the figure is also shown per organisation in the tenants table or on the organisation drill-down.

### 1. Tenants and growth

How people are counted (decided):

- There is one root account, and there are organisation admins and members. Root is never a member of an organisation. An ordinary account can be admin in one organisation and a member in others.
- Platform totals count each distinct user once, never once per membership.
- Per-organisation figures count that organisation's memberships. Their sum over organisations is therefore larger than the platform total when people belong to several organisations, and the page says so next to the total.
- Root is never counted as a user, a member, an active user or a seat. It appears only as the root line of the health view.
- A seat is an active non-guest member: a membership with a role other than guest whose account is not suspended. Seats are a count, not a quota.

| Figure | Source | Step | Kept |
|---|---|---|---|
| Organisations: total, created per period, with no members left, with SSO | PostgreSQL (`orgs`, `sso_providers`) | day | rollup 400 days |
| Users: distinct accounts other than root, created per period, suspended, platform admins, accounts that are guests everywhere, accounts in more than one organisation | PostgreSQL (`users`, `memberships`) | day | rollup 400 days |
| Active users: distinct accounts other than root with a successful sign-in, a session or key use, or a new execution session in the window (1, 7, 30 days) | PostgreSQL (`events` with `user.login` success, `api_keys.last_used_at`, `execution_sessions.created_at`) | day | rollup 400 days |
| Memberships per organisation, by role (each person once per organisation); teams per organisation (org) | PostgreSQL | now | current only |
| Repository entries: organisation and personal, public and private, `lost`; distinct graphs (a public graph linked by several organisations counts once) (org) | PostgreSQL (`repositories`), graph `Repos` | day | rollup 400 days |
| Watches and imports: active watches, repositories added by watches per period | PostgreSQL (`org_watches`, `events` `watch.repo_add`) | day | rollup 400 days |
| Seats: active non-guest members per organisation (org), and distinct seated people on the platform | PostgreSQL | day | rollup 400 days |

There is no licence or seat limit in CodeSynapse, and this plan adds none.

### 2. Usage

| Figure | Source | Step | Kept |
|---|---|---|---|
| MCP calls, errors and sessions per period; by tool (org) | VictoriaMetrics `mcp_tool_calls_total`, `mcp_sessions_total` | 1 min to 6 h | 30 days |
| The same per day for longer ranges, by tool and by client | usage buckets, summed over repositories | day | 180 days, rollup 400 days |
| Tool latency p50 and p95, per tool for the top tools | `mcp_tool_duration_seconds` | 1 min to 6 h | 30 days |
| Tokens served to agents, tokens avoided, and the share of agent work tokens avoided (without prompt-cache reads) | `mcp_response_tokens_total`; usage buckets; see [token savings without the prompt cache](#token-savings-without-the-prompt-cache) | day | as above |
| Answered on the developer's machine versus server | usage buckets (`usage/local`); later `mcp_local_answers_total` (coverage plan item 17) | day | as above |
| Agent workflow: sessions that began with CodeSynapse, reads turned back, Bash classes | usage pseudo tools (`usage.IsWorkflow`, `usage.BashClasses`) | day | as above |
| Unindexed checkouts reported: count of distinct names per organisation, never the names | usage pseudo tool `usage.Unindexed` | day | as above |
| API traffic: requests by route pattern and status class, p50 and p95 by route | `http_requests_total`, `http_request_duration_seconds` (recorded, no panel today) | 1 min to 6 h | 30 days |
| Gateway: requests by route (local, cloud, rejected) and status class | `gateway_requests_total` | 1 min to 6 h | 30 days |
| Gateway: response time by route, queue wait, fall-throughs by reason (timeouts apart) | `gateway_request_duration_seconds`, `gateway_queue_wait_seconds`, `gateway_fallthrough_total`; daily totals in the `gateway` rollup metric | 1 min to 6 h | 30 days; 400 days as daily totals |

### 3. Jobs and indexing

The instruments are those of telemetry coverage phase 1 and phase 3. This plan adds panels, not metrics, except the worker roles gauge below.

| Figure | Source | Step | Kept |
|---|---|---|---|
| Queue: waiting and active per kind, oldest waiting job age | `platform_codesynapse_jobs_waiting`, `platform_codesynapse_jobs_active`; oldest age from PostgreSQL `jobs` (`created_at` of the oldest queued) | 1 min; now | 30 days |
| Finished jobs by kind and outcome (`ok`, `error`, `left_behind`, `cancelled`) (org) | `codesynapse_jobs_finished_total` (phase 3) | 1 min to 6 h | 30 days, rollup 400 days |
| Wait and run time p50 and p95 per kind | `codesynapse_job_wait_seconds`, `codesynapse_job_duration_seconds` (phase 3) | 1 min to 6 h | 30 days |
| Lease events and the scheduler holder | `platform_codesynapse_lease_events_total`, `platform_codesynapse_loops_lease_held` (phase 1) | 1 min | 30 days |
| Processes per service and worker role | phase 1 runtime gauges counted per `job` and `instance`; a new `platform_codesynapse_worker_roles{role}` gauge (`ingest`, `ai`, `fix`), 1 per role a worker process runs | 1 min | 30 days |
| Indexing time by phase and by language | `codesynapse_ingest_phase_*`, `codesynapse_ingest_extract_seconds_total` (phase 3) | 1 h | 30 days |

Job counts per organisation come from `codesynapse_jobs_finished_total` grouped by `tenant_id`, never from job rows. Job rows hold the repository name and the error text, which are tenant content, so no cross-tenant view lists them. This matches `handleAdminHealth`, which already clears `RecentFailures`.

### 4. Ingest

| Figure | Source | Step | Kept |
|---|---|---|---|
| Runs by status (succeeded, failed, cancelled) per period (org) | PostgreSQL ingest runs (`accounts.IngestRun`), status only; `ingest_runs_total` | day; 1 min | rollup 400 days; 30 days |
| Files scanned, indexed, skipped, failed, unprocessed per period; skips by reason | ingest run reports summed (the exact source); `codesynapse_files_*_total` without the `repo` label for charts | day | rollup 400 days |
| Indexed files by language or format across the platform | graph `Stats.Formats` summed per graph, graphs counted once | day | rollup 400 days |
| Incomplete discovery: runs with `discovery_complete` false | ingest run reports | day | rollup 400 days |

The `repo` label of the three `codesynapse_files_*_total` metrics is never shown or grouped by here. Platform panels sum it away.

### 5. Models

| Figure | Source | Step | Kept |
|---|---|---|---|
| Requests and failures by provider, model and task (org) | `llm_requests_total` | 1 min to 6 h | 30 days, rollup 400 days |
| Input, output and cached tokens by provider and model (org) | `llm_tokens_*_total` | as above | as above |
| Estimated cost by provider and model, with price source and date (org) | `llm_cost_estimated_dollars`; `price_source` and `price_checked` from the provider config | as above | as above |
| Calls with unknown cost: usage not reported, prices missing | `llm_usage_unknown_total`, `llm_pricing_missing_total` | as above | as above |
| Local versus cloud share of requests and tokens | the same series split by `llm_provider`, with `ProviderConfig.Local()` applied in the API, as `stats_models.go` does | as above | as above |
| Cloud request bytes; the provider's prompt-cache saving on CodeSynapse's own calls, shown apart and never added to CodeSynapse savings | `cloud_bytes_sent_total`, `llm_cost_saved_estimated_dollars_total` | as above | as above |
| Time to first token and request time p95 per model | `llm_time_to_first_token_seconds`, `llm_request_duration_seconds` | as above | 30 days |
| Embeddings and memory extraction | `memory_entities_embedded_total`, `memory_offload_turns_total` | as above | 30 days |

### 6. Storage

| Figure | Source | Step | Kept |
|---|---|---|---|
| NebulaGraph: vertices per tag and edges per type across the space; live graphs | new platform gauges sampled by the scheduler (see below) | day | rollup 400 days |
| Milvus: entities per collection | new platform gauge, Milvus REST statistics per collection | day | rollup 400 days |
| PostgreSQL: database size, size of the largest tables, audit hot rows and sealed segments | new platform gauges from `pg_database_size` and `pg_total_relation_size` over a fixed table list; `audit_segments` | day | rollup 400 days |
| Health and latency | `GET /api/v1/admin/health` checks; `db_query_duration_seconds`, `db_query_errors_total`, `db_query_timeouts_total` summed over tenants per `db_system`; phase 1 pool gauge | 1 min | 30 days |

The sizes are sampled once a day by the process that holds the `api-loops` lease, with constant statements only (nGQL constants in `nebulastore`, fixed SQL, fixed Milvus collection names), and recorded as `platform_codesynapse_store_rows{db_system,kind}` and `platform_codesynapse_store_bytes{db_system,kind}`. `kind` is a tag, edge type, collection or table name from the code's own allowlists (`AllNodeKinds`, `AllEdgeKinds`, the collection constants, a fixed table list), never a repository or tenant. NebulaGraph reports counts, not bytes, through nGQL, so its panel says rows. The NebulaGraph count needs a `STATS` job; it runs at most once a day and is skipped while an ingest holds the replace lease.

### 7. Security posture across tenants

Counts only. Never finding text, rule matches per repository, file paths or symbol names.

| Figure | Source | Step | Kept |
|---|---|---|---|
| Graphs by grade (A to F, N/A) (org) | latest `history.Snapshot` per graph, graphs counted once on the platform, per entry for an organisation | day | rollup 400 days |
| Open findings by severity; vulnerable dependencies by severity, and the called share (org) | latest snapshots | day | rollup 400 days |
| Graphs whose grade got worse since the previous snapshot of the same branch | `history.GradeWorse` | day | rollup 400 days |
| Most common rule IDs across the platform | latest snapshots; rule IDs come from the rule catalogue, not from tenants | day | rollup 400 days |
| Suppressed findings (approval overlay) | accounts suppression records, count only | day | rollup 400 days |

Rule IDs are shown only at the platform level and only when at least three organisations have the rule (see privacy rules). Per organisation the page shows grade and severity counts, not rule IDs.

### 8. Audit and sign-in

| Figure | Source | Step | Kept |
|---|---|---|---|
| Events per period by action family (`user.*`, `http ...`, `mcp.*`, `repo.*`, `git_token.*`, ...) and outcome | PostgreSQL `events`, grouped by the action's prefix | hour | rollup 400 days |
| Failed sign-ins by cause (wrong password, unknown login, rate limited, suspended) | `events` `user.login` failures by their constant `Detail`; later `codesynapse_auth_attempts_total` (coverage item 12) | hour | rollup 400 days |
| Suspensions and resumptions, platform admin grants, `root.denied`, `auth.denied` | `events` | day | rollup 400 days |
| Chain verification: last result, entries checked, when | `accounts.VerifyAudit`, run by the scheduler once a day and on demand; the result is kept, not recomputed per view | day | latest and 400 days of results |
| Archive: sealed segments, oldest hot entry, last seal outcome | `audit_segments`; `platform_codesynapse_audit_seals_total` (coverage item 15) | day | rollup 400 days |
| Audit write failures | `codesynapse_audit_writes_total{outcome="error"}` (coverage item 13) | 1 min | 30 days |

Counting rows in `events` over a long window is expensive. The page reads the daily rollup for anything longer than 24 hours, and the 24-hour figures use an index on `(at, action)`. The phase that adds them checks that index exists and adds it in a migration if not.

### 9. Sessions

| Figure | Source | Step | Kept |
|---|---|---|---|
| Browser sessions: active now, created per day | PostgreSQL `sessions` (`created_at`, `expires_at`) | day | rollup 400 days |
| Execution sessions: active, created per day, revoked, dashboard bindings versus runtime sessions (org) | PostgreSQL `execution_sessions` | day | rollup 400 days |
| MCP sessions per period (org) | `mcp_sessions_total` | 1 min to 6 h | 30 days |
| Active API keys, keys used in the window, keys expiring in 14 days | PostgreSQL `api_keys` | day | rollup 400 days |

No session ID, key ID or user agent is shown.

### 10. Alerts

| Figure | Source | Step | Kept |
|---|---|---|---|
| Rules firing and pending, with since when | vmalert `GET /api/v1/alerts` on the internal URL (telemetry coverage phase 2) | now | none (vmalert holds state) |
| Rule groups and their last evaluation errors | vmalert `GET /api/v1/rules` | now | none |
| Firing time per rule per day | `ALERTS` and `ALERTS_FOR_STATE` series that vmalert writes back to VictoriaMetrics, if remote write is on | 1 h | 30 days |

Alert labels and annotations are constant text by the rules in [TELEMETRY_PLAN.md](TELEMETRY_PLAN.md#alerting). The API passes on the rule name, state, severity, the value and the times, and drops any other label. When vmalert is off, the tab says so and shows nothing else.

## Where each number comes from

| Source | Used for | Rule |
|---|---|---|
| VictoriaMetrics | Minute-to-hour activity, latencies, the last 30 days | Fixed MetricsQL in a server table, as `seriesPanels`. The client names a panel and a range; it never sends query text. The only value put into a query is the step window. Platform panels pass `tenants = nil`. An organisation drill-down passes `tenants = []string{orgID}` with the ID read from PostgreSQL, through `extra_filters`. |
| PostgreSQL | Organisations, users, memberships, entries, jobs, ingest runs, sessions, keys, audit | Fixed SQL in `pgstore` (and the same behaviour in `memstore`), bounded windows, aggregates only. New `accounts.Store` methods are covered by the accounts contract (`accountstest`). |
| Graph | Distinct graphs, formats per graph, latest snapshots, store row counts | Existing `store.Reader` methods (`Repos`, `Stats`, snapshot reads) or new constant nGQL, implemented in `memory` and `nebulastore` and covered by `storetest`. |
| Usage buckets | MCP usage beyond 30 days, workflow, Bash, local answers | `store.UsageStore.Usage`, summed over repositories in the API before anything leaves the server. |
| Daily rollup | Everything kept longer than its source | A new PostgreSQL table, below. |

**Daily rollup.** A new table `platform_stats_daily (day, metric, dimension, org_id, value)` holds one row per day, metric and dimension, with `org_id` empty for platform totals. `metric` and `dimension` come from a fixed list in the code; a value outside it is refused by a check in the store. The process holding `api-loops` writes yesterday's rows shortly after midnight UTC with an upsert, so a retry or a second writer changes nothing. Rows are kept 400 days (decided), the same as the default audit retention, and independent of it. Deleting an organisation keeps its rows, because they hold only counts and an opaque ID; its name then shows as "deleted organisation". The rollup is the source for the 90-day and 365-day ranges. Two sources outside PostgreSQL add to a day: the MCP usage buckets of the graph store (`mcp_*` metrics), and the gateway's traffic from VictoriaMetrics (`gateway`: requests by route, fall-throughs and the two timeouts, and millisecond sums with counts behind the average response time and queue wait). When VictoriaMetrics does not answer, the day is written with the gateway rows at zero and `gateway`/`measured` 0, so the accounts figures are not held back.

As built (phase 8): the metric and dimension list is `statMetrics` in `internal/accounts/platformstats_daily.go`. Only `memberships`, `seats`, `repo_entries`, `jobs_finished`, `execution_sessions` and `api_keys` have organisation rows; the others are platform totals. `PreparePlatformStats` refuses any other metric, dimension, organisation row, negative count or duplicate, and the table's checks keep the columns to short identifiers. Every platform row is written each day, zeros included, which marks the day as written; organisation rows only when not zero. A write replaces the whole day under a per-day advisory lock, so a second writer leaves the same rows. The writer (`services/api/internal/api/statsrollup.go`) runs at 00:05 UTC and on start-up, writes yesterday if it is not written yet and prunes days older than 400; a failed write is retried after 10 minutes. It writes only yesterday: a day missed while no API process held `api-loops` stays missing rather than being reconstructed. Counts of state (suspended users, active sessions and keys) are taken when the writer runs, just after the day ends. Jobs count by their kind (unknown kinds as `other`) and final state (`succeeded`, `failed`); a job of a deleted organisation counts only in the platform total. Watches, imports, the audit and ingest families, usage buckets, models, storage and security posture are not in the rollup yet; the phases that add those sections add their metrics to the list. `accounts.Service.PlatformStatsHistory` reads the 90d or 365d range for a platform admin (it does not write the view event: the section handler's `PlatformStats` call does).

As wired (`services/api/internal/api/admin_stats_daily.go`): `summary`, `tenants`, `sessions` and their exports accept `90d` and `365d`. The range is the last 90 or 365 complete UTC days, up to yesterday. State figures (organisations, people, memberships, seats, entries, what is open or active now) stay live, read with an empty window so no long window is scanned. Window figures are summed over the rollup's days: organisations and people created, browser sessions created, runtime sessions created by kind and revoked (per organisation too). Figures that count distinct people or keys over the window cannot be summed from days: active people are the rollup's 30-day figure on the latest written day (`active_window: "30d"`), and per-organisation active members, keys used and watch additions are listed in `rollup.unavailable` (they read 0 in JSON, empty in CSV, "–" on the page). Every long-range answer carries `rollup` with the range's days, the days the rollup holds and the latest one, so days before the writer ran are visible as missing. A read that reaches `MaxPlatformStatRows` answers 422 rather than short sums. The series panels stay 1h to 30d.

## Privacy rules

These hold for every figure, endpoint, export and alert in this plan. They follow the telemetry, tenant scoping, platform administration and root rules in `AGENTS.md`.

**May cross tenants (shown to platform admins):**

- Counts, sums, rates, percentiles and sizes.
- Organisation names and slugs, in tables and drill-downs. They come from PostgreSQL by organisation ID after the query ran. They are never metric labels; series carry only `tenant_id`, which the API maps to a name. The admin organisation list already shows them.
- Tool names (the `AllowTools` set), route patterns, job kinds, model providers and configured model names, tasks, `db_system`, node and edge kinds, rule IDs from the catalogue, skip reasons, languages and formats, grade letters and severities.
- Client names, already cleaned and capped by `internal/usage`, as a top 10 with the rest in "other".

**Never crosses tenants:**

- Repository names, graph IDs and repository-entry IDs, file paths, branch names, revisions, remote URLs and hosts, forge logins.
- Job error text, ingest report file lists, finding text, symbol names, review text, memory, prompts and model answers.
- E-mail addresses, user IDs, user names, key IDs, session IDs and client addresses in any statistic. The existing admin user list keeps showing e-mail addresses, as today; statistics do not join to it.
- Checkout names from the unindexed buckets.

**Limits.**

- **Top N.** Every breakdown shows at most the top 10 values plus "other", and every organisation table shows at most 50 rows per page, sorted by the chosen column. Charts keep the existing rule: four named series and "other".
- **Small groups.** A breakdown that does not name an organisation (rule IDs, languages, clients, tools) shows a value only when at least three organisations contribute to it in the window (decided); otherwise it is folded into "other". This stops a cross-tenant breakdown from pointing at one organisation's stack. Organisation rows themselves are not folded, because the admin needs them, but they show only the counts listed above.
- **Windows.** Ranges are 1h, 6h, 24h, 7d and 30d from VictoriaMetrics, and 90d and 365d from the rollup. No other value is accepted.
- **Size.** Responses are capped (one section at most 1 MiB), exports at 10,000 rows, queries at the existing timeouts. The batch endpoint takes at most 20 panels.
- **Cardinality.** The new instruments (worker roles, store rows and bytes) are platform gauges with fixed label sets of at most a few dozen values. They go in `internal/telemetry/metrics.go` and get a label-set test, like the coverage plan's instruments.

**Cost honesty.**

- Cost is an estimate from configured prices and is labelled so. Each provider row names its `price_source` and `price_checked`.
- Calls without reported usage or without prices are counted and shown next to the cost ("412 calls with unknown cost"). When every call in the window lacks a price, the cost reads "unknown", never "$0".
- Local providers show tokens and requests, and no cost. Local execution alone is never shown as cloud savings; the cloud-bytes-avoided figure appears only with a measured fallback baseline, as on the existing panel.
- Small amounts show as `<$0.001`, not `$0.000`.

## Token savings without the prompt cache

Cloud agents re-send their whole context on every turn, and the provider serves most of it from its prompt cache. In a long Claude Code session, cache reads are often more than 90% of all input tokens. They are cheap, and CodeSynapse neither causes nor removes them. A percentage with cache reads in its denominator looks tiny. A percentage that counts every later re-send of an avoided file as "saved" looks huge. Neither is realistic. This section fixes one definition for the admin view and the per-organisation Statistics pages.

### How it is computed today

- **Avoided tokens.**
  - Navigation tools: each successful call is counted. The tools are listed in `navigationTools` (`services/api/internal/api/stats.go:24-31`). The formula is avoided = response tokens × success share × (ratio − 1) (`stats.go:499-509`).
  - The ratio: each repository's token benchmark from ingest, in baseline tokens per graph token (`stats.go:392-404`).
  - The baseline: "defining file + files of direct callers, callees and reaching tests; tokens ≈ bytes/4" (`internal/metrics/metrics.go:509-510`). That file reading is the "normal agent" CodeSynapse compares with.
  - ask_local: adds what the local model read, minus the answer (`stats.go:453-456`; method text at `stats.go:414`).
- **The current percentage.**
  - `ContextSavedPct` = avoided ÷ (served + avoided) (`stats.go:552-555`; test `token_stats_test.go:33-40`).
  - It compares a graph answer with the files it replaces. It does not involve the agent's own tokens or the cache, so this section does not change it.
  - It is renamed "Smaller than reading the files", so it is not read as a share of the agent's tokens.
- **Agent tokens.** The hooks read the agent's transcript.
  - Claude: input = uncached input + cache reads + cache writes, and `CachedInput` = cache reads (`cmd/codesynapse/hook_tokens.go:62-67`). Cache writes are folded into input and not kept apart.
  - Codex: `cached_input_tokens` is the cached part of input (`hook_tokens.go:78-80`).
  - Both are stored as `usage.TokenCounts`, where `CachedInput` is a subset of `Input` (`internal/usage/tokens.go:15-22`).
  - The total is input + output, with cache reads included (`stats.go:543`).
- **Where the cache leaks in.**
  - The Models page puts `tokens.total` ("Cloud agents") into the token pool. "Kept off cloud models" = (graph avoided + local tokens) ÷ pool (`services/web/app/pages/stats/models.vue:22-31`). Cache reads dominate that denominator.
  - The Agents page shows cached input as a tile next to the savings (`services/web/app/pages/stats/agents.vue:98`).
- **Cost.**
  - `llm.Cost` prices fresh input, cache reads and cache writes separately (`services/api/internal/llm/llm.go:650-664`).
  - The page values graph-avoided tokens at the full input price (`models.vue:39-46`).
  - `PriceReference` has no cache-write price (`services/api/internal/api/stats_models.go:116-131`).
  - `llm_cost_saved_estimated_dollars_total` is the provider's prompt-cache saving on CodeSynapse's own model calls (`llm.go:701-703`; panel `llm_savings` in `series.go:115-116`). It is not a CodeSynapse saving.

### The definition

For a window and a scope (one organisation, or the platform):

- **W, the agent's work tokens** = fresh input + cache writes + output = `Input − CachedInput + Output`. Cache reads are left out.
- **A, avoided tokens** = the existing figure (graph answers and ask_local). Each answer counts once, as the turn on which the agent would have read the files.
- **Share avoided** = A ÷ (W + A). This is the share of work tokens the agent would have spent on top of W had it read the files itself. MCP answers are already inside W, because they reach the agent as input.

Rules:

1. **Cache reads never count.** They are not in W, not in A and not in the denominator. Adding cache reads to a fixture must not change the share.
2. **Re-sends earn no credit in the main figure.** An avoided file would have entered the context once, as fresh input or a cache write. It would then have been re-sent as cache reads on every later turn. Only the first entry counts in the main figure; the re-sends appear only in the upper figure below.
3. **Answers about content already in context earn nothing.**
   - If the agent already had those files in its context earlier in the session, reading them again would have been a cache hit. The server cannot see that.
   - The plugin hook already keeps the session's answered and read `file:start-end` ranges in its local state file. It adds one count, `avoided_repeat_ranges`: answered ranges that overlap a range the session already answered or read.
   - The server scales each bucket's avoided figure down by that overlap share.
   - Until a client reports the count, its avoided tokens are left out of the share and out of the saved cost (decided). They are still shown as "tokens avoided" with a note that this client cannot yet tell repeats apart.
4. **Unknown means unknown, not 0 and not raw.**
   - The share is "unknown" when the window has no hook token counts, or when any counted turn did not report its cache fields.
   - The hook records a new daily counter, `(tokens:cache-known-turns)`, for turns whose usage carried the cache fields.
   - The share is computed only when that counter equals `(tokens:turns)` for every bucket in the scope. It never falls back to `Input + Output`.
5. **An upper figure, labelled, beside the main one (decided).**
   - The hook reports `avoided_resend_tokens`: for each counted answer, its avoided tokens times the turns the session ran after it. Repeats (rule 3) are left out here too.
   - Upper share = (A + R) ÷ (W + C + A + R), where R is the re-sent tokens and C is the agent's own cache reads. Both sides then include cache reads.
   - Upper saving = the main saving + R at the cache-read price.
   - It is labelled "Upper figure: also counts the avoided context the agent would have re-sent from its prompt cache". The main figure is always the larger, first number on the page. It is the only one used in tiles, exports and the admin summary.
6. **The raw figure is secondary.** A ÷ (Input + Output + A), with cache reads included, appears only in the method note, labelled "including prompt-cache reads, which CodeSynapse does not affect". It is never a tile and never an export headline.

**Cost uses the same rule.**

- Avoided tokens are valued once, at the price the reference cloud model would have charged for their first entry. That is the cache-write price when the agent's provider caches by default (Anthropic), and the input price otherwise. When the reference has no cache-write price, the input price is used instead of leaving the figure unknown.
- Tokens avoided by clients without `avoided_repeat_ranges` stay out of the share, but they are valued apart at the input price (`saved.unaware_dollars`; their repeats cannot be taken out). `saved.total_dollars` adds both parts and is what the money total uses; the tile names how many tokens came from such clients. Before this, a window where every avoided token came from such clients showed "unknown" for the whole graph saving and left it out of the total.
- The built-in reference, used when no cloud provider has prices, is Claude Opus 5.5's list price: $4 input, $20 output, $0.20 cache reads and $5 five-minute cache writes per million tokens (platform.claude.com pricing, checked 2026-10-29).
- Later re-sends are not valued in the main saving. The upper figure values them at the cache-read price (rule 5).
- Local model work keeps today's split: fresh input at the input price, cached input at the cache-read price, output at the output price (`models.vue:39-46`). Cached input is what the local runtime reports: `prompt_tokens_details.cached_tokens` (vLLM, LM Studio) or llama.cpp's `timings.cache_n`. Ollama reports no cache use, so the tile says "none reported" instead of $0. The gateway's local answers record the same count as `gateway_local_tokens_total{kind="cached"}` and price it at the cache-read price.
- `PriceReference` gains `cache_write_per_mtok`.
- When a needed price is missing, that part of the saving is "unknown", never $0.
- The provider's own prompt-cache saving (`llm_savings`) stays a separate, labelled figure. It is never added to "Saved by CodeSynapse".

**Where it applies.**

- The admin Usage and Models sections and the existing Statistics pages (`stats/agents.vue`, `stats/models.vue`) all use this one definition.
- The server computes it in `agentStats` as new fields: `work_tokens`, `share_avoided_pct`, `share_avoided_known` and `share_avoided_basis`.
- The pages stop computing shares in the browser, so the two views cannot drift.
- "Kept off cloud models" uses W, not `tokens.total`, for the cloud-agent part of its pool.

## API

All routes are account routes of class `governanceRoute`, each calling `PlatformStats` first. `TestEveryRouteIsClassified` covers them.

| Route | Answers |
|---|---|
| `GET /api/v1/admin/stats/summary?range=` | The tiles: organisations, users, active users, graphs, jobs waiting and failed, cost, firing alerts, chain status |
| `GET /api/v1/admin/stats/{section}?range=&org=` | One section: `tenants`, `usage`, `jobs`, `ingest`, `models`, `storage`, `security`, `audit`, `sessions`, `alerts`. `org` is an organisation ID for the drill-down, validated against PostgreSQL. Registered as ten explicit patterns, not a wildcard, so the classification test lists each one |
| `GET /api/v1/admin/stats/series?panel=&range=&org=` | One chart from a separate `adminPanels` table, using the same code as `handleSeries`; series grouped by `tenant_id` come back with organisation names |
| `GET /api/v1/admin/stats/series/batch` | Several panels in one request and one audit event, as `/stats/series/batch` |
| `GET /api/v1/admin/stats/export?section=&range=&format=csv\|json` | A section as a file. CSV cells are formula-neutralised as in the audit export. Audited as `admin.stats.export` |

The former `platform: true` usage panels on `/api/v1/stats/series` (`tenants`, `platform_llm_cost`, `platform_tool_latency`, `platform_db_latency`) moved to `adminPanels` in phase 3 (`org_sessions`, `llm_cost`, `tool_latency`, `db_latency`); the old names answer 404. The `platform_*` health panels of the admin overview stay on `/api/v1/stats/series`.

As built (phase 3):

- `adminPanels` (`services/api/internal/api/admin_series.go`): `sessions`, `mcp_calls`, `mcp_errors`, `mcp_tokens`, `local_answers`, `tool_latency`, `tool_latency_by_tool`, `org_tool_calls`, `org_sessions`, `org_tokens`, `api_routes`, `api_failures`, `api_route_latency`, `gateway_requests`, `gateway_latency`, `gateway_latency_avg`, `gateway_fallthrough`, `llm_cost`, `db_latency`. Their only placeholder is `$w`; `org` becomes a VictoriaMetrics extra filter, never query text.
- Panels grouped by tool or client fold a group that fewer than three organisations use into "other" with a second query that counts the organisations per group; with one organisation selected nothing folds. Then the usual top-N fold of `/stats/series` applies.
- Panels grouped by `tenant_id` answer with organisation names; an ID with no organisation reads "deleted organisation".
- The `usage` section adds MCP usage buckets per organisation: sessions, calls, errors, tokens served and avoided, the token share, the workflow counts and unindexed checkouts, with tool and client breakdowns. Clients are a fixed list of known families; any other name is "other".
- 90d and 365d read the daily rollup, which keeps tool and client rows per organisation. The share there counts only organisation-days with token counts (`share_basis: known_only`, with `share_days` and `share_unknown_days`); unindexed checkouts are not kept per day and read `null`. VictoriaMetrics keeps 30 days, so the charts show the last 30 for those ranges.
- The upper share figure is in the JSON beside the main one, never in tiles, exports or the summary.

## Dashboard

A new **Statistics** tab under Administration (`services/web/app/pages/admin/stats.vue` with child pages), next to Overview, Organisations, Users and Audit log. The Overview tab keeps system health; its "Usage across organisations" section moves to Statistics → Usage.

- **Layout.** A row of range chips (1 hour to 1 year) and an organisation picker ("All organisations" by default) at the top. Below it a row of `StatTile`s with the summary. Then sub-tabs, one per group: Tenants, Usage, Jobs and indexing, Ingest, Models, Storage, Security, Audit and sign-in, Sessions, Alerts. Each sub-tab is a grid of `SeriesCard` charts and plain tables.
- **Design.** The "Agent spark" rules in `AGENTS.md`: tokens from `app/assets/css/main.css` only, 2px borders, pill buttons, `--s-1`…`--s-4` for series with the dataviz palette checks, status tints (`--success`, `--warning`, `--danger`, `--info` and their `-soft`) for health and alert state, `SevBadge` for severities, no coloured left-border stripes. Charts are the existing in-house SVG; new shapes (a stacked bar for grades, a share bar for local versus cloud) are plain SVG or CSS. No chart or UI library.
- **Drill-down.** Clicking an organisation in any table or chart opens the same sections filtered to that organisation. Clicking an audit figure opens the audit log with the action filter set. Nothing opens a repository.
- **Empty and partial states.** "Off" when VictoriaMetrics or vmalert is not configured, "No measurements recorded in this window" for absent series, "Not measured yet" for figures whose instrument ships in a later telemetry phase.
- **Data.** Through `useApiGet` and `/api/cg/*` only. The page loads one batch per sub-tab. Auto refresh is a toggle, off by default, at most once a minute.
- **Export.** A button per sub-tab for CSV and JSON.
- **Access in the UI.** The tab is hidden for anyone who is not a platform admin and shows a notice when the session is not a browser session. The API enforces both.

## Phases

Each phase is one pull request with its API, page, tests and docs (`docs/operations/accounts-access.md`, `docs/features/telemetry.md`). Each runs `just test`, `just lint` and `just web-check`; phases with a migration also run `just test-postgres`, and phases with new store reads run `just test-nebula`.

1. **Access, tenants and sessions.** `PlatformStats` helper and audit event; the existing admin reads (`/admin/users`, `/admin/orgs`, `/admin/health`, platform series panels) become session-only and audited on view, which breaks scripts that use an API key; `summary`, `tenants` and `sessions` sections from PostgreSQL; the Statistics tab with tiles, the range picker (1h to 30d) and the organisation picker; CSV and JSON export. Tests: `TestEveryRouteIsClassified` and `TestRootRefusedEveryBusinessRoute` stay green; a scope test per route (non-admin 403, API key 403, installation token 403, root allowed, suspended refused, admin with no memberships allowed; the existing admin reads refuse API keys and write their view events); a fixture with one person in three organisations counts one user and three memberships, and root is in no user, member or seat count; the audit event is written and a failed write answers 503 with no body figures; responses for fixtures with a repository called `secret-repo` and a user `a@example.com` never contain either string.
2. **Realistic token savings** (implemented). Puts the definition from [token savings without the prompt cache](#token-savings-without-the-prompt-cache) on the existing Statistics pages first.
   - Hook: the `(tokens:cache-known-turns)` counter, `avoided_repeat_ranges` and `avoided_resend_tokens`. This needs a CLI release.
     - As built, the hook cannot know a repository's benchmark ratio, so it sends `resend_token_turns` per tool (the new part of each answer's tokens × the turns after it) and the server turns that into `avoided_resend_tokens` with the tool's avoided-per-response-token rate. It is a lower bound within one report window and does not reset on compaction.
     - Repeat and re-send counts are kept per day, organisation, client and tool (`(repeat:<tool>)`, `(resend:<tool>)`, and `(ranges:repeat-aware)` for clients that send the field); a client is matched to its MCP buckets by client name.
     - Codex turns are counted from `token_count` events whose total grew.
   - Server: `work_tokens`, `share_avoided_pct`, `share_avoided_known` and `share_avoided_basis` in `agentStats`, and `cache_write_per_mtok` in `PriceReference`. The pages read the server's share.
   - Tests:
     - A fixture with 950,000 cache reads in 1,000,000 input gives the same share as the same fixture with no cache reads. Doubling the cache reads changes nothing.
     - A bucket without cache fields makes the share unknown: not 0, and not the raw figure.
     - Repeat ranges lower the avoided figure. A client without `avoided_repeat_ranges` adds nothing to the share.
     - The upper figure is never smaller than the main one, and it is never used in tiles, exports or the summary.
     - Avoided tokens are valued once, at the cache-write or input price, and never for re-sends. A missing price makes that part unknown.
     - `llm_savings` is never part of the CodeSynapse total.
     - The Claude and Codex transcript parsers keep the cache fields apart, and the fuzz test covers the new fields.
3. **Usage and traffic.** `adminPanels`, `/admin/stats/series` and `/batch`, tenant ID to name mapping; the `usage` section from VictoriaMetrics and usage buckets; panels for `http_requests_total` and the gateway; the four existing platform panels move. Tests: every admin panel query has no placeholder other than `$w`; the small-group fold (fewer than three organisations folds to "other"); top-N fold; unknown tenant IDs map to "deleted organisation"; the old panel names are removed and answer 404.
4. **Jobs, indexing and ingest.** Depends on telemetry coverage phases 1 and 3. `jobs` and `ingest` sections; `platform_codesynapse_worker_roles` gauge; oldest waiting job from PostgreSQL. Tests: the gauge's label set (`ingest`, `ai`, `fix`) and its `platform_` export; no job `Repo` or `Error` value in any response; the `repo` label never appears in a platform panel's grouping.
5. **Models and cost.** Done (`services/api/internal/api/admin_stats_models.go`, Administration → Statistics → Models; live metrics only, so 90 and 365 days answer 400 and the page shows the last 30 days). `models` section with local versus cloud split, price source per provider and the unknown-cost counts. Tests: cost reads "unknown" when every call lacks a price; a local provider has no cost; a configured model not in the allowlist is not shown by name.
6. **Storage and alerts.** Depends on telemetry coverage phase 2 for alerts. Daily store sampler under `api-loops`; `platform_codesynapse_store_rows` and `_bytes`; `storage` and `alerts` sections; `CODESYNAPSE_ALERTS_URL` for the internal vmalert, from the environment, https or the internal service name only, no redirects, response capped. Tests: label sets from the allowlists; constant statements (no caller value in nGQL or SQL text); the sampler skips while a replace lease is held; alert labels outside the passed-on set are dropped.
7. **Security and audit.** `security` and `audit` sections; scheduled chain verification with the stored result; the `(at, action)` index if missing. Tests: no finding text, path or symbol in the response; rule IDs fold below three organisations; verification result read, not recomputed, on a view.
8. **Long ranges.** Done (migration 0032; the ranges wired into `summary`, `tenants`, `sessions` and the export afterwards). Migration for `platform_stats_daily`, the daily writer under `api-loops`, the 90-day and 365-day ranges, 400-day pruning. Tests: the writer is idempotent (run twice, same rows); metric and dimension allowlist enforced by the store; pruning; memstore and pgstore agree (`accountstest`).

Phases 1, 2 and 3 need nothing from the telemetry branches. Phase 4 waits for telemetry phases 1 and 3, and phase 6 waits for telemetry phase 2. Phase 3 shows the share from phase 2 per organisation. Until then those figures show "Not measured yet".

## Decided questions

The user answered the open questions of the first draft. The answers are written into the sections above.

1. **Seats:** active non-guest members, counted per organisation and as distinct people on the platform. There is no quota.
2. **Root and names:** root sees organisation names in the admin tables. Root is never a member, so it is never counted as a user, member or seat. Each person counts once in platform totals and once per organisation in membership counts.
3. **Existing admin reads:** they become session-only and audited on view in phase 1. API keys are refused, which breaks scripts that use one.
4. **Retention:** daily rollups are kept for 400 days.
5. **Small groups:** a breakdown that does not name organisations needs at least three organisations.
6. **Re-sends:** an upper figure values re-sends at the cache-read price. It is shown apart and clearly labelled, and the conservative figure is the main one.
7. **Repeats before the hook reports them:** that client's avoided tokens are left out of the share. There is no "upper estimate" for them.
