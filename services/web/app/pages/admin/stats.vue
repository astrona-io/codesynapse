<script setup lang="ts">
import type { AdminOrg, IngestSection, JobsSection, OrgSessionRow, OrgTenantRow, SeriesRange, SessionTotals, StatsRange, StatsSummary, StatsTable, TenantTotals, UsageSection } from '~/utils/types'

// Platform statistics: counts across every organisation, aggregates only.
// Every load is recorded in the audit log, so nothing refreshes unless the
// administrator turns it on, and then at most once a minute. 90 days and
// 1 year come from the daily rollup: complete UTC days up to yesterday.
const ranges: { id: StatsRange; label: string }[] = [
  { id: '1h', label: '1 hour' }, { id: '6h', label: '6 hours' }, { id: '24h', label: '24 hours' }, { id: '7d', label: '7 days' }, { id: '30d', label: '30 days' },
  { id: '90d', label: '90 days' }, { id: '365d', label: '1 year' },
]
const range = ref<StatsRange>('24h')
const org = ref('')
type Tab = 'tenants' | 'sessions' | 'usage' | 'jobs' | 'ingest' | 'security' | 'audit' | 'models' | 'storage' | 'alerts'
const tab = ref<Tab>('tenants')
// Security, audit, models, storage and alerts load in their own components, only while shown.
const sectionTick = ref(0)
const tabled = computed(() => tab.value !== 'models' && tab.value !== 'storage' && tab.value !== 'alerts')
const page = ref(1)
const tenantSort = ref('name')
const sessionSort = ref('name')
const usageSort = ref('name')
const jobSort = ref('name')
const ingestSort = ref('name')
watch([range, org, tab], () => { page.value = 1 })

const filter = computed(() => `range=${range.value}${org.value ? `&org=${encodeURIComponent(org.value)}` : ''}`)
const { data: orgs } = useApiGet<{ orgs: AdminOrg[] }>('admin/orgs')
const { data: summary, error: summaryError, refresh: refreshSummary, pending } = useApiGet<StatsSummary>(() => `admin/stats/summary?range=${range.value}`)
const { data: tenants, error: tenantsError, refresh: refreshTenants } = useApiGet<StatsTable<TenantTotals, OrgTenantRow>>(
  () => `admin/stats/tenants?${filter.value}&sort=${tenantSort.value}&page=${page.value}`, { immediate: true })
const { data: sessions, error: sessionsError, refresh: refreshSessions, execute: loadSessions } = useApiGet<StatsTable<SessionTotals, OrgSessionRow>>(
  () => `admin/stats/sessions?${filter.value}&sort=${sessionSort.value}&page=${page.value}`, { immediate: false })
const { data: usage, error: usageError, refresh: refreshUsage, execute: loadUsage } = useApiGet<UsageSection>(
  () => `admin/stats/usage?${filter.value}&sort=${usageSort.value}&page=${page.value}`, { immediate: false })
const { data: jobs, error: jobsError, refresh: refreshJobs, execute: loadJobs } = useApiGet<JobsSection>(
  () => `admin/stats/jobs?${filter.value}&sort=${jobSort.value}&page=${page.value}`, { immediate: false })
const { data: ingest, error: ingestError, refresh: refreshIngest, execute: loadIngest } = useApiGet<IngestSection>(
  () => `admin/stats/ingest?${filter.value}&sort=${ingestSort.value}&page=${page.value}`, { immediate: false })

// Usage charts come from VictoriaMetrics, which keeps 30 days: 90 days and
// 1 year show the last 30. Tools and clients fewer than three organisations
// use are "other" unless one organisation is selected.
const chartPanels: Partial<Record<Tab, string[]>> = {
  usage: ['sessions', 'mcp_calls', 'mcp_errors', 'mcp_tokens', 'local_answers', 'tool_latency', 'tool_latency_by_tool',
  'org_tool_calls', 'org_sessions', 'org_tokens', 'api_routes', 'api_failures', 'api_route_latency', 'gateway_requests', 'gateway_latency_local', 'gateway_latency_cloud', 'gateway_latency_avg', 'gateway_fallthrough', 'llm_cost', 'db_latency'],
  jobs: ['jobs_finished', 'jobs_failures', 'jobs_org_finished', 'jobs_queue', 'jobs_wait', 'jobs_duration', 'jobs_processes', 'jobs_worker_roles', 'jobs_scheduler', 'jobs_leases'],
  ingest: ['ingest_runs', 'ingest_org_runs', 'ingest_phases', 'ingest_extract', 'ingest_files', 'ingest_skips'],
}
// The charts follow the last tab that has any, so switching to a table
// without charts does not ask for them again.
const chartTab = ref<Tab>('usage')
const panelNames = computed(() => chartPanels[chartTab.value] ?? [])
const live = computed(() => range.value !== '90d' && range.value !== '365d')
const seriesRange = computed<SeriesRange>(() => (live.value ? range.value as SeriesRange : '30d'))
const { panels: charts, pending: chartsPending, refresh: refreshCharts, execute: loadCharts } = useSeriesBatch(panelNames, seriesRange, undefined,
  { base: 'admin/stats/series/batch', org, immediate: false })
const chartsOff = ref(false)
// Security and audit load and refresh themselves (AdminSecurityStats,
// AdminAuditStats) when their tab opens.
const securityTab = ref<{ refresh: () => void; error: unknown } | null>(null)
const auditTab = ref<{ refresh: () => void; error: unknown } | null>(null)
watch(tab, (t) => {
  if (t === 'sessions' && !sessions.value) loadSessions()
  if (t === 'usage' && !usage.value) loadUsage()
  if (t === 'jobs' && !jobs.value) loadJobs()
  if (t === 'ingest' && !ingest.value) loadIngest()
  if (!chartPanels[t]) return
  if (t !== chartTab.value) { chartTab.value = t; chartsOff.value = false; loadCharts() }
  else if (!Object.keys(charts.value).length) loadCharts()
})
const errors: Partial<Record<Tab, Ref<unknown>>> = { tenants: tenantsError, sessions: sessionsError, usage: usageError, jobs: jobsError, ingest: ingestError }
const error = computed(() => summaryError.value ?? (tab.value === 'security' ? securityTab.value?.error : tab.value === 'audit' ? auditTab.value?.error : errors[tab.value]?.value) ?? null)

const auto = ref(false)
let timer: ReturnType<typeof setInterval> | undefined
function refreshAll() {
  refreshSummary()
  if (tab.value === 'tenants') refreshTenants()
  else if (tab.value === 'sessions') refreshSessions()
  else if (tab.value === 'security') securityTab.value?.refresh()
  else if (tab.value === 'audit') auditTab.value?.refresh()
  else if (tab.value === 'usage') { refreshUsage(); refreshCharts() }
  else if (tab.value === 'jobs') { refreshJobs(); refreshCharts() }
  else if (tab.value === 'ingest') { refreshIngest(); refreshCharts() }
  else sectionTick.value++
}
watch(auto, (on) => {
  clearInterval(timer)
  timer = on ? setInterval(refreshAll, 60_000) : undefined
})
onBeforeUnmount(() => clearInterval(timer))

const exportHref = (format: 'csv' | 'json') => apiPath(`admin/stats/export?section=${tab.value}&${tab.value === 'audit' ? `range=${range.value}` : filter.value}&format=${format}`)
const tables: Partial<Record<Tab, Ref<StatsTable<unknown, unknown> | null | undefined>>> = { tenants, sessions, usage, jobs, ingest }
const table = computed(() => tables[tab.value]?.value ?? null)
const pct = (v: number | null | undefined) => (v === null || v === undefined ? '–' : `${v}%`)
const shareHint = computed(() => {
  const t = usage.value?.totals
  if (!t?.share_known) return 'not measured in this window'
  return t.share_basis === 'known_only' ? 'of work tokens · some left out' : 'of work tokens'
})
const shareTip = 'The share of the agents\' work tokens that CodeSynapse answers saved, from the token counts the hooks report. '
  + 'Organisations (or, for 90 days and 1 year, organisation-days) without token counts are left out rather than counted as zero.'

// The upper figure (repeats counted) stays beside the main one in the text
// below the tiles, never in a tile or an export.
const upperShare = computed(() => {
  const t = usage.value?.totals
  const u = t?.share_upper_pct
  return u !== null && u !== undefined && u > (t?.share_avoided_pct ?? 0) ? u : null
})
const n = (v: number | null | undefined) => (v === null || v === undefined ? '–' : fmtCompact(v))
// wait reads a queue age: seconds under two minutes, then minutes, hours.
const wait = (s: number | null) => (s === null ? '–' : s < 120 ? `${s} s` : s < 7200 ? `${Math.round(s / 60)} min` : `${Math.round(s / 3600)} h`)
const runs = (f: { succeeded: number; failed: number; cancelled: number }) => f.succeeded + f.failed + f.cancelled
const roles = (r: Record<string, number>) => Object.entries(r).map(([k, v]) => `${v} ${k}`).join(' · ') || '–'

// A long range names figures the daily rollup cannot answer (distinct
// people or keys over the window); those read "–", not 0.
const rollup = computed(() => table.value?.rollup ?? summary.value?.rollup)
const missing = (figure: string) => table.value?.rollup?.unavailable.includes(figure) ?? false
const day = (iso: string) => new Date(iso).toLocaleDateString([], { year: 'numeric', month: 'short', day: 'numeric', timeZone: 'UTC' })
const activeHint = computed(() => {
  const r = summary.value?.rollup
  if (!r) return ''
  return r.latest ? `in the 30 days to ${day(r.latest)}` : 'no daily figures recorded yet'
})
</script>

<template>
  <div class="stack">
    <section class="panel">
      <div class="row controls">
        <span id="stats-range" class="label">Window</span>
        <div class="segmented" role="group" aria-labelledby="stats-range">
          <button v-for="r in ranges" :key="r.id" type="button" :aria-pressed="range === r.id" @click="range = r.id">{{ r.label }}</button>
        </div>
        <select v-model="org" class="inline" aria-label="Organisation">
          <option value="">All organisations</option>
          <option v-for="o in orgs?.orgs ?? []" :key="o.id" :value="o.id">{{ o.name }}</option>
        </select>
        <label class="small auto"><input v-model="auto" type="checkbox"> Refresh every minute</label>
        <button class="secondary tiny" :disabled="pending" @click="refreshAll()">{{ pending ? 'Loading…' : 'Refresh' }}</button>
      </div>
      <p v-if="rollup" class="notice">
        <span>
          Daily figures: {{ rollup.written }} of {{ rollup.days }} days recorded<template v-if="rollup.latest">, up to {{ day(rollup.latest) }} (UTC)</template>.
          New and ended counts are summed over those days; totals and what is open are as of now.
        </span>
      </p>
      <p class="muted small">Counts only: no repository names, addresses or IDs. Root is never counted, a person counts once on the platform and once per organisation, and seats are active members other than guests. Each view is recorded in the audit log.</p>
    </section>
    <p v-if="error" class="notice notice--danger">{{ apiErrorMessage(error) }}</p>
    <div v-if="summary" class="grid cols-4">
      <StatTile label="Organisations" :value="n(summary.orgs)" :hint="`${summary.orgs_created} new in this window`" />
      <StatTile label="People" :value="n(summary.users)" :hint="`${summary.users_created} new · ${summary.memberships} memberships`" />
      <StatTile label="Active people" :value="n(summary.active_users)" :hint="activeHint" tip="signed in, opened a session or used a key" />
      <StatTile label="Seats" :value="n(summary.seats)" :hint="`held by ${summary.seated_people} people`" />
      <StatTile label="Runtime sessions" :value="n(summary.execution_active)" :hint="`${summary.execution_created} started in this window`" />
      <StatTile label="API keys" :value="n(summary.keys_active)" :hint="`${summary.browser_active} browser sessions open`" />
      <StatTile label="Indexed graphs" :value="n(summary.graphs)" hint="not measured yet" />
      <StatTile label="Jobs waiting" :value="n(summary.jobs_waiting)" :hint="summary.jobs_failed === null ? 'failed jobs not measured' : `${fmtCompact(summary.jobs_failed)} failed in this window`" />
      <StatTile label="Audit chain" :value="!summary.chain ? 'Not verified yet' : summary.chain.run_error ? 'Could not finish' : summary.chain.ok ? 'Intact' : 'Broken'"
        :hint="summary.chain ? `checked ${new Date(summary.chain.at).toLocaleString()}` : 'checked daily'" :tone="summary.chain?.ok ? undefined : 'warning'"
        tip="The stored result of the last audit chain verification; opening this page does not walk the chain." />
      <StatTile label="Alerts firing" :value="n(summary.alerts_firing)" :hint="summary.alerts_firing === null ? 'alerts off or not answering' : 'now, from vmalert'"
        :tone="summary.alerts_firing ? 'warning' : undefined" />
    </div>
    <section class="panel">
      <div class="panel__header">
        <div class="segmented" role="group" aria-label="Table">
          <button type="button" :aria-pressed="tab === 'tenants'" @click="tab = 'tenants'">Tenants</button>
          <button type="button" :aria-pressed="tab === 'sessions'" @click="tab = 'sessions'">Sessions</button>
          <button type="button" :aria-pressed="tab === 'usage'" @click="tab = 'usage'">Usage</button>
          <button type="button" :aria-pressed="tab === 'security'" @click="tab = 'security'">Security</button>
          <button type="button" :aria-pressed="tab === 'audit'" @click="tab = 'audit'">Audit</button>
          <button type="button" :aria-pressed="tab === 'jobs'" @click="tab = 'jobs'">Jobs</button>
          <button type="button" :aria-pressed="tab === 'ingest'" @click="tab = 'ingest'">Indexing</button>
          <button type="button" :aria-pressed="tab === 'models'" @click="tab = 'models'">Models</button>
          <button type="button" :aria-pressed="tab === 'storage'" @click="tab = 'storage'">Storage</button>
          <button type="button" :aria-pressed="tab === 'alerts'" @click="tab = 'alerts'">Alerts</button>
        </div>
        <div v-if="tabled" class="row">
          <select v-if="tab === 'tenants'" v-model="tenantSort" class="inline" aria-label="Sort tenants">
            <option value="name">By name</option><option value="members">Most members</option><option value="seats">Most seats</option><option value="active">Most active</option><option value="entries">Most repositories</option>
          </select>
          <select v-else-if="tab === 'sessions'" v-model="sessionSort" class="inline" aria-label="Sort sessions">
            <option value="name">By name</option><option value="execution">Most runtime sessions</option><option value="keys">Most API keys</option>
          </select>
          <select v-else-if="tab === 'jobs'" v-model="jobSort" class="inline" aria-label="Sort jobs">
            <option value="name">By name</option><option value="finished">Most finished</option><option value="ok">Most succeeded</option><option value="error">Most failed</option>
          </select>
          <select v-else-if="tab === 'ingest'" v-model="ingestSort" class="inline" aria-label="Sort indexing">
            <option value="name">By name</option><option value="runs">Most runs</option><option value="failed">Most failed</option><option value="scanned">Most files scanned</option><option value="indexed">Most files indexed</option>
          </select>
          <select v-else-if="tab === 'usage'" v-model="usageSort" class="inline" aria-label="Sort usage">
            <option value="name">By name</option><option value="sessions">Most sessions</option><option value="calls">Most calls</option><option value="errors">Most errors</option><option value="tokens">Most tokens</option><option value="avoided">Most avoided</option>
          </select>
          <a :href="exportHref('csv')" download class="chip">CSV</a>
          <a :href="exportHref('json')" download class="chip">JSON</a>
        </div>
      </div>
      <AdminModels v-if="tab === 'models'" :range="range" :org="org" :tick="sectionTick" />
      <template v-if="tab === 'tenants' && tenants">
        <p class="muted small">
          {{ tenants.totals.orgs_without_members }} organisations without members · {{ tenants.totals.orgs_with_sso }} with single sign-on ·
          {{ tenants.totals.multi_org_users }} people in more than one · {{ tenants.totals.guest_only_users }} guests only · {{ tenants.totals.users_suspended }} suspended ·
          {{ tenants.totals.entries }} repository entries ({{ tenants.totals.personal_entries }} personal, {{ tenants.totals.lost_entries }} without access) over {{ tenants.totals.graphs }} graphs ·
          {{ tenants.totals.watches }} watches<template v-if="!missing('watch_adds')"> added {{ tenants.totals.watch_adds }}</template>
        </p>
        <div class="table-wrap">
          <table>
            <thead><tr><th>Organisation</th><th class="num">Members</th><th>Roles</th><th class="num">Seats</th><th class="num">Active</th><th class="num">Teams</th><th class="num">Repositories</th><th>Single sign-on</th><th class="num">Watches</th></tr></thead>
            <tbody>
              <tr v-for="o in tenants.rows" :key="o.org_id">
                <td>{{ o.name }} <span class="muted mono small">{{ o.slug }}</span></td>
                <td class="num">{{ o.members }}</td>
                <td class="small muted">{{ roles(o.roles) }}</td>
                <td class="num">{{ o.seats }}</td>
                <td class="num">{{ missing('active_members') ? '–' : o.active_members }}</td>
                <td class="num">{{ o.teams }}</td>
                <td class="num">{{ o.entries }}<span v-if="o.lost_entries" class="badge badge--amber">{{ o.lost_entries }} without access</span></td>
                <td>{{ o.sso ? 'On' : '–' }}</td>
                <td class="num">{{ o.watches }}</td>
              </tr>
            </tbody>
          </table>
        </div>
      </template>
      <template v-if="tab === 'sessions' && sessions">
        <p class="muted small">
          {{ sessions.totals.browser_active }} browser sessions open, {{ sessions.totals.browser_created }} started ·
          runtime sessions started: {{ sessions.totals.execution.runtime }} by agents, {{ sessions.totals.execution.dashboard }} by the dashboard, {{ sessions.totals.execution.gateway }} by the gateway ·
          {{ sessions.totals.keys.expiring }} keys expire within 14 days
        </p>
        <div class="table-wrap">
          <table>
            <thead><tr><th>Organisation</th><th class="num">Runtime sessions</th><th class="num">Started</th><th class="num">Ended</th><th class="num">API keys</th><th class="num">Used</th><th class="num">Expiring</th></tr></thead>
            <tbody>
              <tr v-for="o in sessions.rows" :key="o.org_id">
                <td>{{ o.name }} <span class="muted mono small">{{ o.slug }}</span></td>
                <td class="num">{{ o.execution.active }}</td>
                <td class="num">{{ o.execution.created }}</td>
                <td class="num">{{ o.execution.revoked }}</td>
                <td class="num">{{ o.keys.active }}</td>
                <td class="num">{{ missing('keys_used') ? '–' : o.keys.used }}</td>
                <td class="num">{{ o.keys.expiring }}</td>
              </tr>
            </tbody>
          </table>
        </div>
      </template>
      <template v-if="tab === 'usage' && usage">
        <div class="grid cols-4 usage-tiles">
          <StatTile label="Agent sessions" :value="n(usage.totals.sessions)" :hint="`${usage.from} to ${usage.to} (UTC)`" />
          <StatTile label="Tool calls" :value="n(usage.totals.calls)" :hint="`${fmtCompact(usage.totals.errors)} failed`" />
          <StatTile label="Tokens avoided" :value="pct(usage.totals.share_avoided_pct)" :hint="shareHint" :tip="shareTip" />
          <StatTile label="Unindexed checkouts" :value="n(usage.totals.unindexed_checkouts)" :hint="usage.totals.unindexed_checkouts === null ? 'live ranges only' : 'without an index'"
            tip="Distinct checkouts agents worked in that CodeSynapse has not indexed. Not kept per day, so 90 days and 1 year show none." />
        </div>
        <p class="muted small">
          {{ fmtCompact(usage.totals.response_tokens) }} tokens served · {{ fmtCompact(usage.totals.avoided_tokens) }} avoided<template v-if="upperShare !== null">
          ({{ pct(usage.totals.share_avoided_pct) }} of work tokens, up to {{ pct(upperShare) }} counting repeated context)</template> ·
          sessions starting with CodeSynapse {{ usage.totals.first_codesynapse }}, with file reads {{ usage.totals.first_native }} ·
          {{ usage.totals.reads_turned_back }} whole-file reads turned back · {{ usage.totals.files_edited_found }} of {{ usage.totals.files_edited }} edited files named by an answer ·
          {{ usage.totals.bash }} shell commands with a smaller answer
          <template v-if="usage.totals.share_days !== undefined"> · token share from {{ usage.totals.share_days }} organisation-days<template v-if="usage.totals.share_unknown_days">, {{ usage.totals.share_unknown_days }} without token counts left out</template></template>
        </p>
        <div class="table-wrap">
          <table>
            <thead><tr><th>Organisation</th><th class="num">Sessions</th><th class="num">Calls</th><th class="num">Failed</th><th class="num">Tokens served</th><th class="num">Avoided</th><th class="num">Share</th><th class="num">Unindexed</th></tr></thead>
            <tbody>
              <tr v-for="o in usage.rows" :key="o.org_id">
                <td>{{ o.name }} <span v-if="!o.deleted" class="muted mono small">{{ o.slug }}</span></td>
                <td class="num">{{ o.sessions }}</td>
                <td class="num">{{ o.calls }}</td>
                <td class="num">{{ o.errors }}</td>
                <td class="num">{{ fmtCompact(o.response_tokens) }}</td>
                <td class="num">{{ fmtCompact(o.avoided_tokens) }}</td>
                <td class="num">{{ pct(o.share_avoided_pct) }}</td>
                <td class="num">{{ n(o.unindexed_checkouts) }}</td>
              </tr>
            </tbody>
          </table>
        </div>
        <div class="grid cols-2 breakdowns">
          <div v-for="b in [{ title: 'Tools', rows: usage.tools, second: 'Failed' }, { title: 'Clients', rows: usage.clients, second: 'Sessions' }]" :key="b.title" class="table-wrap">
            <table>
              <thead><tr><th>{{ b.title }}</th><th class="num">Calls</th><th class="num">{{ b.second }}</th></tr></thead>
              <tbody>
                <tr v-for="c in b.rows" :key="c.name"><td class="mono small">{{ c.name }}</td><td class="num">{{ c.calls }}</td><td class="num">{{ b.title === 'Tools' ? c.errors : c.sessions }}</td></tr>
                <tr v-if="!b.rows.length"><td colspan="3" class="muted small">Nothing in this window.</td></tr>
              </tbody>
            </table>
          </div>
        </div>
        <p class="muted small">Tools and clients that fewer than three organisations use are counted as other, unless one organisation is selected.</p>
      </template>
      <AdminSecurityStats v-if="tab === 'security'" ref="securityTab" v-model:page="page" :filter="filter" />
      <template v-if="tab === 'audit'">
        <p v-if="org" class="muted small">The audit log is counted for the whole platform; the organisation filter does not apply.</p>
        <AdminAuditStats ref="auditTab" :range="range" />
      </template>
      <template v-if="tab === 'jobs' && jobs">
        <div class="grid cols-2 breakdowns">
          <div class="table-wrap">
            <table>
              <thead><tr><th>Queue</th><th class="num">Waiting</th><th class="num">Running</th><th class="num">Oldest wait</th></tr></thead>
              <tbody>
                <tr v-for="q in jobs.queue" :key="q.kind"><td class="mono small">{{ q.kind }}</td><td class="num">{{ q.waiting }}</td><td class="num">{{ q.active }}</td><td class="num">{{ wait(q.oldest_waiting_seconds) }}</td></tr>
                <tr v-if="!jobs.queue.length"><td colspan="4" class="muted small">No job is queued or running.</td></tr>
              </tbody>
            </table>
          </div>
          <div class="table-wrap">
            <table>
              <thead><tr><th>Finished by kind</th><th class="num">Finished</th><th class="num">Failed</th></tr></thead>
              <tbody>
                <tr v-for="k in jobs.kinds" :key="k.kind"><td class="mono small">{{ k.kind }}</td><td class="num">{{ k.finished }}</td><td class="num">{{ k.error }}</td></tr>
                <tr v-if="!jobs.kinds.length"><td colspan="3" class="muted small">{{ jobs.measured ? 'Nothing finished in this window.' : 'Not measured: live statistics are off or did not answer.' }}</td></tr>
              </tbody>
            </table>
          </div>
        </div>
        <p class="muted small">
          The queue is as of now. Finished {{ jobs.totals.finished }} · succeeded {{ jobs.totals.ok }} · failed {{ jobs.totals.error }}<template v-if="!missing('left_behind')">
          · left for another process {{ jobs.totals.left_behind }}</template><template v-if="!missing('cancelled')"> · cancelled {{ jobs.totals.cancelled }}</template>.
          Jobs without an organisation count in these totals but in no row.
        </p>
        <div class="table-wrap">
          <table>
            <thead><tr><th>Organisation</th><th class="num">Finished</th><th class="num">Succeeded</th><th class="num">Failed</th><th class="num">Left behind</th><th class="num">Cancelled</th></tr></thead>
            <tbody>
              <tr v-for="o in jobs.rows" :key="o.org_id">
                <td>{{ o.name }} <span v-if="!o.deleted" class="muted mono small">{{ o.slug }}</span></td>
                <td class="num">{{ o.finished }}</td>
                <td class="num">{{ o.ok }}</td>
                <td class="num">{{ o.error }}</td>
                <td class="num">{{ missing('left_behind') ? '–' : o.left_behind }}</td>
                <td class="num">{{ missing('cancelled') ? '–' : o.cancelled }}</td>
              </tr>
            </tbody>
          </table>
        </div>
      </template>
      <template v-if="tab === 'ingest' && ingest">
        <div class="grid cols-4 usage-tiles">
          <StatTile label="Indexing runs" :value="n(runs(ingest.totals))" :hint="`${ingest.totals.failed} failed · ${ingest.totals.cancelled} cancelled`" />
          <StatTile label="Running now" :value="n(ingest.totals.running)" hint="not finished yet" />
          <StatTile label="Files scanned" :value="n(ingest.totals.scanned)" :hint="`${fmtCompact(ingest.totals.indexed)} indexed`" />
          <StatTile label="Files skipped" :value="n(ingest.totals.skipped)" :hint="`${fmtCompact(ingest.totals.files_failed)} failed · ${fmtCompact(ingest.totals.unprocessed)} not processed`" />
        </div>
        <p class="muted small">
          Skipped: {{ ingest.totals.skips.ignored_path }} ignored paths · {{ ingest.totals.skips.generated_code }} generated code · {{ ingest.totals.skips.binary }} binary ·
          {{ ingest.totals.skips.non_k8s_yaml }} YAML that is not Kubernetes · {{ ingest.totals.incomplete_discovery }} runs did not see every file ·
          {{ ingest.totals.without_report }} runs whose report expired count by outcome only.
        </p>
        <div class="table-wrap">
          <table>
            <thead><tr><th>Organisation</th><th class="num">Runs</th><th class="num">Failed</th><th class="num">Running</th><th class="num">Scanned</th><th class="num">Indexed</th><th class="num">Skipped</th><th class="num">Files failed</th></tr></thead>
            <tbody>
              <tr v-for="o in ingest.rows" :key="o.org_id">
                <td>{{ o.name }} <span v-if="!o.deleted" class="muted mono small">{{ o.slug }}</span></td>
                <td class="num">{{ runs(o) }}</td>
                <td class="num">{{ o.failed }}</td>
                <td class="num">{{ o.running }}</td>
                <td class="num">{{ fmtCompact(o.scanned) }}</td>
                <td class="num">{{ fmtCompact(o.indexed) }}</td>
                <td class="num">{{ fmtCompact(o.skipped) }}</td>
                <td class="num">{{ fmtCompact(o.files_failed) }}</td>
              </tr>
            </tbody>
          </table>
        </div>
      </template>
      <StatsStorage v-if="tab === 'storage'" :range="range" :tick="sectionTick" />
      <StatsAlerts v-if="tab === 'alerts'" :tick="sectionTick" />
      <div v-if="table && table.pages > 1" class="row pager">
        <button class="secondary tiny" :disabled="page <= 1" @click="page--">Previous</button>
        <span class="muted small">Page {{ table.page }} of {{ table.pages }} · {{ table.total }} organisations</span>
        <button class="secondary tiny" :disabled="page >= table.pages" @click="page++">Next</button>
      </div>
    </section>
    <section v-if="chartPanels[tab] && tab === chartTab && !chartsOff" class="panel">
      <h2>Over time</h2>
      <p class="muted small">From live statistics<template v-if="!live">, which keep 30 days: the charts show the last 30</template>. Organisations deleted since read "deleted organisation".</p>
      <div class="grid cols-2">
        <SeriesCard v-for="p in panelNames" :key="p" :panel="p" :range="seriesRange" :tick="0" :batched="charts[p] ?? null" :batch-pending="chartsPending" @unavailable="chartsOff = true" />
      </div>
    </section>
  </div>
</template>

<style scoped>
.controls { gap: 10px; flex-wrap: wrap; }
.auto { display: inline-flex; align-items: center; gap: 6px; }
.pager { gap: 10px; justify-content: flex-end; margin-top: 10px; }
td .badge { margin-left: 6px; }
.usage-tiles { margin-bottom: 10px; }
.breakdowns { margin-top: 14px; }
</style>
