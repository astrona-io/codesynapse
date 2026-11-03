# Feature health: timings per feature and a per-user breaker

Status: phase 1 done (2026-11-03); phases 2 and 3 in progress.

## Why

A Mac with 48 GB of RAM ran a 27B model with a 262144-token context (34 GB), a Podman VM of 19 GB and two kind clusters, and swapped 27 GB. Every pre-brief, `ask_local` call and gateway local answer then waited on a model that was paging, and Claude Code felt broken. Nothing told the developer which feature was slow, and nothing stopped a slow feature from holding up every prompt.

Two things are missing:

1. **Visibility.** Usage buckets (`internal/usage.Bucket`) keep calls, errors, total and maximum duration per day, repository, tool and client. They have no minimum and no percentiles. Model-backed features that the bridge or the hooks run (pre-brief, `local_draft`, `source-extract`, the gateway local route) are counted, but not all of them are timed.
2. **Protection.** A slow optional feature keeps running on every prompt. The hooks fix (outage mark, pre-brief skip within one chat) covers one case. This plan generalises it.

## What is measured

Every feature has one name and belongs to one class and one tier:

| Class | Features | Tier |
|---|---|---|
| Graph tools | `minimal_context`, `get_symbol_context`, `search`, … (every MCP tool) | server, or your machine for worktree answers |
| Model-backed | `ask_local`, `local_draft`, pre-brief, `source-extract`, gateway local route, prompt shortening | self-hosted model |
| Bridge-local | `read_ranges`, git and gh tools, `fetch_source` | your machine |
| Hooks | session start, prompt recall, stop, retrieval report | your machine → server |

For each feature, client and window (1 h, 24 h, 7 days) the Statistics page shows calls, errors, timeouts, min, max, average, p50 and p95.

**Storage.**
- Daily buckets gain `min_duration_ms` and a fixed latency histogram. The bucket bounds in ms are 50, 100, 250, 500, 1000, 2500, 5000, 10000, 30000, 60000 and 150000, plus overflow.
- Percentiles are read from the histogram, so nothing is stored per call.
- Each timed call also lands in an hourly copy of its bucket (`hour`, key suffix `|h`+hour), kept 8 days, so the 1 h and 24 h windows are exact; the 7-day window reads the hourly copies too.
- Bucket merging stays a sum, which is why it is a histogram and not a list of samples.
- The existing caps (`maxBuckets`, 180-day retention, cleaned client names) apply.
- Hook and bridge timings reach the buckets through the existing `POST /api/v1/usage/local` and retrieval reports. They carry feature name, client, duration and outcome only: never paths, prompts, session IDs or URLs.

**Metrics.** OTel histograms keep their existing tenant labels (`tenant_id`, `mcp_tool`, `llm_provider`, `llm_model`) and series caps. The per-user figures live in PostgreSQL only. A user or session ID is never a metric label.

## The breaker

The breaker is kept per user, organisation and feature. Only optional features trip:

| Feature | Trips | While paused |
|---|---|---|
| Pre-brief | yes | The prompt goes on without a brief |
| `ask_local`, `local_draft`, `source-extract` | yes | The tool answers "paused because it was slow; read the files yourself" (like today's 409) |
| Gateway local route, prompt shortening | yes | The request goes to the cloud (`fall_through` reason `paused_slow`) |
| Graph tools | **no** | Flagged on the page only: pausing them makes agents read whole files, which costs more than a slow answer |
| Hooks | no | Their own timeouts and the outage mark already bound them |

**Trip rule.** A feature trips when:
- the p95 of the user's last 20 calls is over the feature's limit, or
- 3 calls in a row time out.

Errors that are not timeouts (refused, bad input) never trip it.

**Default limits:**

| Feature | Limit |
|---|---|
| Pre-brief | 15 s total |
| Gateway local route | 10 s to the first token |
| Prompt shortening | 10 s |
| `ask_local` | 60 s |
| `local_draft` | 90 s |
| `source-extract` | 30 s |

Owners and admins can change the limits per organisation (bounded: at least 1 s, at most the feature's hard timeout). The change is audited.

**Recovery.**
- **Manual:** "Enable" per feature or "Enable all" on the page. Each runs a test call at once and shows its time.
- **Automatic:** a paused feature allows one trial call after 30 minutes (half-open). A fast trial closes the breaker; a slow one keeps it paused for another 30 minutes.
- A user can turn the automatic retry off.

**Where it is enforced.**
- The API and the MCP server read the user's breaker state with the caller's identity before running a model-backed task.
- The bridge and the hooks get the state with their existing calls (in the answer of session registration and through a small `GET /api/v1/me/feature-health`), and keep it in the local session state for at most 60 s.
- The server never relies on the client: a paused feature is refused server-side too.

**Telling the user.** The first time a feature pauses in a chat, the agent gets one short note through the hook context: "CodeSynapse paused the pre-brief: p95 41 s over the last 20 calls. Enable it again under Statistics → Feature health." It is not repeated in that chat.

## Security

- Timings, outcomes and breaker state only. No prompts, arguments, paths, file contents, URLs or session IDs are stored with them.
- **Breaker state:** a new accounts table keyed by organisation, user and feature, holding the state, when it tripped, the p95, the call count and the reason enum. Each user reads and changes only their own. Owners and admins see the organisation's totals, and per user only the counts of paused features, never another user's timings.
- **Changes:** enabling, disabling and changing a limit need a signed-in session (API keys get 403) and are audited before the write (`feature_health.enable`, `feature_health.disable`, `feature_health.limit.set`). A failed audit refuses the change.
- Automatic trips and recoveries are audited as `feature_health.trip` and `feature_health.recover`, with the actor `automation` and the reason enum.
- Feature names come from a fixed allowlist (`usage` tool names plus the model-backed features); unknown names are refused.
- Tenant scoping follows `s.route` with the narrowest policy: `anyone` for the caller's own state, `orgRole` full for organisation limits and totals.

## Phases

1. **Timings.** Min and histogram in usage buckets (memory and NebulaGraph stores, store contract), hook and bridge timings reported, and a Statistics → Feature health table per feature and client. API, MCP and web, plus a CLI release for the hook reports.
2. **Breaker.** Accounts table and migration, trip rule, server-side refusal for paused features, gateway `paused_slow` fall-through, the enable, disable and limit endpoints with audit, and the page controls.
3. **Clients.** The bridge and the hooks read the state, the pre-brief and the local tools skip while paused, the one-time note, and `codesynapse doctor` lists paused features. Needs a CLI release.

## Open questions

- Should an owner be able to pause a feature for the whole organisation (for example while the model host is down)? Proposed: yes, as a separate organisation switch, audited, in phase 2.
- Should the half-open retry be on by default? Proposed: yes.
