<script setup lang="ts">
import type { AdminOrg, OrgSessionRow, OrgTenantRow, SeriesRange, SessionTotals, StatsRange, StatsSummary, StatsTable, TenantTotals, UsageSection } from '~/utils/types'

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
const tab = ref<'tenants' | 'sessions' | 'usage' | 'security' | 'audit'>('tenants')
const page = ref(1)
const tenantSort = ref('name')
const sessionSort = ref('name')
const usageSort = ref('name')
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

// Usage charts come from VictoriaMetrics, which keeps 30 days: 90 days and
// 1 year show the last 30. Tools and clients fewer than three organisations
// use are "other" unless one organisation is selected.
const usagePanels = ['sessions', 'mcp_calls', 'mcp_errors', 'mcp_tokens', 'local_answers', 'tool_latency', 'tool_latency_by_tool',
  'org_tool_calls', 'org_sessions', 'org_tokens', 'api_routes', 'api_failures', 'api_route_latency', 'gateway_requests', 'gateway_latency_local', 'gateway_latency_cloud', 'gateway_latency_avg', 'gateway_fallthrough', 'llm_cost', 'db_latency']
const live = computed(() => range.value !== '90d' && range.value !== '365d')
const seriesRange = computed<SeriesRange>(() => (live.value ? range.value as SeriesRange : '30d'))
const { panels: charts, pending: chartsPending, refresh: refreshCharts, execute: loadCharts } = useSeriesBatch(usagePanels, seriesRange, undefined,
  { base: 'admin/stats/series/batch', org, immediate: false })
const chartsOff = ref(false)
// Security and audit load and refresh themselves (AdminSecurityStats,
// AdminAuditStats) when their tab opens.
const securityTab = ref<{ refresh: () => void; error: unknown } | null>(null)
const auditTab = ref<{ refresh: () => void; error: unknown } | null>(null)
watch(tab, (t) => {
  if (t === 'sessions' && !sessions.value) loadSessions()
  if (t === 'usage' && !usage.value) { loadUsage(); loadCharts() }
})
const error = computed(() => summaryError.value ?? (tab.value === 'tenants' ? tenantsError.value : tab.value === 'sessions' ? sessionsError.value
  : tab.value === 'security' ? securityTab.value?.error : tab.value === 'audit' ? auditTab.value?.error : usageError.value))

const auto = ref(false)
let timer: ReturnType<typeof setInterval> | undefined
function refreshAll() {
  refreshSummary()
  if (tab.value === 'tenants') refreshTenants()
  else if (tab.value === 'sessions') refreshSessions()
  else if (tab.value === 'security') securityTab.value?.refresh()
  else if (tab.value === 'audit') auditTab.value?.refresh()
  else { refreshUsage(); refreshCharts() }
}
watch(auto, (on) => {
  clearInterval(timer)
  timer = on ? setInterval(refreshAll, 60_000) : undefined
})
onBeforeUnmount(() => clearInterval(timer))

const exportHref = (format: 'csv' | 'json') => apiPath(`admin/stats/export?section=${tab.value}&${tab.value === 'audit' ? `range=${range.value}` : filter.value}&format=${format}`)
const table = computed(() => (tab.value === 'tenants' ? tenants.value : tab.value === 'sessions' ? sessions.value : tab.value === 'usage' ? usage.value : null))
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
      <StatTile label="Jobs waiting" :value="n(summary.jobs_waiting)" hint="not measured yet" />
      <StatTile label="Audit chain" :value="!summary.chain ? 'Not verified yet' : summary.chain.run_error ? 'Could not finish' : summary.chain.ok ? 'Intact' : 'Broken'"
        :hint="summary.chain ? `checked ${new Date(summary.chain.at).toLocaleString()}` : 'checked daily'" :tone="summary.chain?.ok ? undefined : 'warning'"
        tip="The stored result of the last audit chain verification; opening this page does not walk the chain." />
    </div>
    <section class="panel">
      <div class="panel__header">
        <div class="segmented" role="group" aria-label="Table">
          <button type="button" :aria-pressed="tab === 'tenants'" @click="tab = 'tenants'">Tenants</button>
          <button type="button" :aria-pressed="tab === 'sessions'" @click="tab = 'sessions'">Sessions</button>
          <button type="button" :aria-pressed="tab === 'usage'" @click="tab = 'usage'">Usage</button>
          <button type="button" :aria-pressed="tab === 'security'" @click="tab = 'security'">Security</button>
          <button type="button" :aria-pressed="tab === 'audit'" @click="tab = 'audit'">Audit</button>
        </div>
        <div class="row">
          <select v-if="tab === 'tenants'" v-model="tenantSort" class="inline" aria-label="Sort tenants">
            <option value="name">By name</option><option value="members">Most members</option><option value="seats">Most seats</option><option value="active">Most active</option><option value="entries">Most repositories</option>
          </select>
          <select v-else-if="tab === 'sessions'" v-model="sessionSort" class="inline" aria-label="Sort sessions">
            <option value="name">By name</option><option value="execution">Most runtime sessions</option><option value="keys">Most API keys</option>
          </select>
          <select v-else-if="tab === 'usage'" v-model="usageSort" class="inline" aria-label="Sort usage">
            <option value="name">By name</option><option value="sessions">Most sessions</option><option value="calls">Most calls</option><option value="errors">Most errors</option><option value="tokens">Most tokens</option><option value="avoided">Most avoided</option>
          </select>
          <a :href="exportHref('csv')" download class="chip">CSV</a>
          <a :href="exportHref('json')" download class="chip">JSON</a>
        </div>
      </div>
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
      <div v-if="table && table.pages > 1" class="row pager">
        <button class="secondary tiny" :disabled="page <= 1" @click="page--">Previous</button>
        <span class="muted small">Page {{ table.page }} of {{ table.pages }} · {{ table.total }} organisations</span>
        <button class="secondary tiny" :disabled="page >= table.pages" @click="page++">Next</button>
      </div>
    </section>
    <section v-if="tab === 'usage' && !chartsOff" class="panel">
      <h2>Over time</h2>
      <p class="muted small">From live statistics<template v-if="!live">, which keep 30 days: the charts show the last 30</template>. Organisations deleted since read "deleted organisation".</p>
      <div class="grid cols-2">
        <SeriesCard v-for="p in usagePanels" :key="p" :panel="p" :range="seriesRange" :tick="0" :batched="charts[p] ?? null" :batch-pending="chartsPending" @unavailable="chartsOff = true" />
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
