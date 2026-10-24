<script setup lang="ts">
import type { SeriesRange, WorkspaceStats } from '~/utils/types'

// Live activity: time series from VictoriaMetrics, by minute up to hours.
const RANGES: { id: SeriesRange; label: string }[] = [
  { id: '1h', label: 'last hour' }, { id: '6h', label: '6 hours' }, { id: '24h', label: '24 hours' },
  { id: '7d', label: '7 days' }, { id: '30d', label: '30 days' },
]
const GROUPS = [
  { title: 'Tokens and model cost', help: 'Provider-reported tokens and configured prices. Missing usage or prices leave a measurement gap.', panels: ['llm_tokens', 'llm_cost', 'llm_savings', 'llm_coverage', 'llm_ttft', 'served_tokens'] },
  { title: 'Network and cloud bandwidth', help: 'Application payload bytes. Avoided cloud traffic needs an equivalent configured fallback; local execution alone has no savings baseline.', panels: ['bandwidth', 'cloud_traffic', 'cloud_savings'] },
  { title: 'Database performance', help: 'Measured request latency, volume and returned rows. Graph rows do not measure internal nodes traversed.', panels: ['db_latency', 'db_latency_tail', 'db_requests', 'db_rows', 'db_errors'] },
  { title: 'Agent memory and context offloading', help: 'Durable facts are current stored state. Saved-fact and recall counters measure activity.', panels: ['memory_facts', 'memory', 'sessions', 'tool_calls', 'tool_latency', 'tool_errors'] },
]

// The window lives in the URL (?range=1h) so a view can be shared.
const route = useRoute()
const router = useRouter()
const isRange = (v: unknown): v is SeriesRange => RANGES.some((r) => r.id === v)
const range = ref<SeriesRange>(isRange(route.query.range) ? route.query.range : '24h')
watch(range, (r) => router.replace({ query: { ...route.query, range: r } }))
const tick = ref(0)
const metricsOff = ref(false)
const hiddenPanels = ref<string[]>([])
function unavailable(panel: string, status: number) {
  if (status === 409) metricsOff.value = true
  else if (!hiddenPanels.value.includes(panel)) hiddenPanels.value.push(panel)
}
let timer: ReturnType<typeof setInterval> | undefined
onMounted(() => {
  // Short windows move by the minute: refresh them while the page is open.
  timer = setInterval(() => { if (range.value === '1h' || range.value === '6h') tick.value++ }, 60_000)
})
onBeforeUnmount(() => clearInterval(timer))

const days = ref(30)
const { data: s, error, pending } = useApiGet<WorkspaceStats>(() => `stats?days=${days.value}`)

const a = computed(() => s.value?.agents)
const m = computed(() => s.value?.memory)
const errorRate = computed(() => (a.value?.calls ? `${((a.value.errors / a.value.calls) * 100).toFixed(1)}%` : '–'))
const callsPerDay = computed(() => (a.value?.daily ?? []).map((d) => ({ label: d.day, value: d.calls, detail: `${d.sessions} session(s), ${d.errors} error(s)` })))
const avoidedPerDay = computed(() => (a.value?.daily ?? []).map((d) => ({ label: d.day, value: d.avoided_tokens, detail: `${fmtInt(d.response_tokens)} served` })))
// Share of the line ranges each tool's answers gave that the agent then
// read (Claude Code plugin hooks), most read first.
const readShare = computed(() => (a.value?.tools ?? []).filter((t) => t.ranges_returned > 0)
  .map((t) => ({ label: t.tool, value: Math.round((100 * Math.min(t.ranges_read, t.ranges_returned)) / t.ranges_returned), detail: `${fmtInt(t.ranges_read)} of ${fmtInt(t.ranges_returned)} line ranges read` }))
  .sort((x, y) => y.value - x.value))
const tools = computed(() => (a.value?.tools ?? []).map((t) => ({ label: t.tool, value: t.calls, detail: `${t.kind} · ${fmtInt(Math.round(t.avg_tokens))} tokens/call · ${t.avg_ms} ms avg${t.errors ? ` · ${t.errors} errors` : ''}` })))
const clients = computed(() => (a.value?.clients ?? []).map((c) => ({ label: c.client, value: c.calls, detail: `${c.sessions} session(s)` })))
const repoUse = computed(() => (a.value?.repos ?? []).map((r) => ({ label: r.repo || '(no repo argument)', value: r.calls, detail: `${fmtCompact(r.response_tokens)} served · ~${fmtCompact(r.avoided_tokens)} avoided` })))
const memRepos = computed(() => (m.value?.repos ?? []).map((r) => ({ label: r.repo, value: r.facts, detail: `${r.entities} entities · ${r.confirmations} confirmations · updated ${relTime(r.last_updated)}` })))
const memSources = computed(() => Object.entries(m.value?.sources ?? {}).map(([label, value]) => ({ label, value })).sort((x, y) => y.value - x.value))

const w = computed(() => s.value?.workspace)
const SEV = ['critical', 'high', 'medium', 'low', 'info']
const findingsBySev = computed(() => SEV.map((k) => ({ label: k, value: w.value?.findings?.[k] ?? 0 })).filter((r) => r.value))
const vulnsBySev = computed(() => SEV.map((k) => ({ label: k, value: w.value?.vulns?.[k] ?? 0 })).filter((r) => r.value))
const grades = computed(() => ['A', 'B', 'C', 'D', 'E', 'F'].map((g) => ({ label: `Grade ${g}`, value: w.value?.grades?.[g] ?? 0 })).filter((r) => r.value))
const avg = (f: (r: WorkspaceStats['repos'][number]) => number) => {
  const rs = s.value?.repos ?? []
  return rs.length ? Math.round(rs.reduce((t, r) => t + f(r), 0) / rs.length) : 0
}
const rules = computed(() => (s.value?.hygiene.top_rules ?? []).map((r) => ({ label: r.rule, value: r.repos, detail: `${r.severity} · ${fmtInt(r.findings)} findings` })))
const l = computed(() => s.value?.licenses)
const licences = computed(() => (l.value?.by_license ?? []).map((c) => ({ label: c.license, value: c.count, detail: c.category })))
const projects = computed(() => Object.entries(l.value?.projects ?? {}).map(([label, value]) => ({ label, value })).sort((x, y) => y.value - x.value))
const conflicts = computed(() => (l.value?.conflicts ?? []).map((c) => ({ label: c.repo, value: c.count })))
const r = computed(() => s.value?.reviews)
const entries = (o?: Record<string, number>) => Object.entries(o ?? {}).map(([label, value]) => ({ label, value })).sort((x, y) => y.value - x.value)
const automationPct = computed(() => {
  const total = w.value?.repos ?? 0
  const missing = s.value?.hygiene.no_update_automation.length ?? 0
  return total ? `${Math.round(((total - missing) / total) * 100)}%` : '–'
})
</script>

<template>
  <div class="stack">
    <PageHeader
      eyebrow="Workspace" title="Statistics"
      lede="How agents use CodeSynapse, what context offloading saves, and where the workspace stands on security, licences, update automation and reviews. Estimates say how they are estimated."
      :crumbs="[{ label: 'Repositories', to: '/' }, { label: 'Statistics' }]"
    />

    <div class="row filters">
      <span class="label">Window</span>
      <button v-for="d in [7, 30, 90]" :key="d" class="chip" :class="{ active: days === d }" @click="days = d">last {{ d }} days</button>
      <span v-if="pending" class="muted small">refreshing…</span>
    </div>

    <!-- Live activity ------------------------------------------------- -->
    <section class="panel edge--cyan">
      <p class="eyebrow">Live activity</p><h2>By the minute and hour</h2>
      <p class="muted small">Recorded by the services every 15 seconds: agent calls, latency, AI model tokens and cost, database work and memory. Counts are per step of the chart ({{ range === '1h' ? '1 minute' : range === '6h' ? '5 minutes' : range === '24h' ? '15 minutes' : range === '7d' ? '1 hour' : '6 hours' }}). These panels show your organisation's activity. Platform-wide telemetry is available in Administration.</p>
      <div class="row filters">
        <span class="label">Window</span>
        <button v-for="r in RANGES" :key="r.id" class="chip" :class="{ active: range === r.id }" @click="range = r.id">{{ r.label }}</button>
      </div>
      <div v-if="metricsOff" class="empty"><strong>Time series are not switched on</strong>They need VictoriaMetrics: the compose stack runs it, other installs set <code>CODESYNAPSE_METRICS_URL</code> (and <code>CODESYNAPSE_METRICS_PASSWORD</code>) on the API and MCP services.</div>
      <div v-else class="stack">
        <section v-for="group in GROUPS" :key="group.title" class="telemetry-group">
          <h3>{{ group.title }}</h3>
          <p class="muted small">{{ group.help }}</p>
          <div class="grid cols-2">
            <template v-for="p in group.panels" :key="p">
              <SeriesCard v-if="!hiddenPanels.includes(p)" :panel="p" :range="range" :tick="tick" @unavailable="(status) => unavailable(p, status)" />
            </template>
          </div>
        </section>
      </div>
    </section>

    <p v-if="error" class="panel error">{{ apiErrorMessage(error) }}</p>
    <template v-else-if="s && a && m && w && l && r">
      <div :class="{ refreshing: pending }" class="stack">
        <!-- Agents ------------------------------------------------------- -->
        <section class="panel edge--cyan">
          <p class="eyebrow">Agents &amp; context</p><h2>MCP sessions and tool calls</h2>
          <p class="muted small">Counted by the MCP server per day, repository, tool and client. Arguments and results are never stored, only their size.</p>
          <div class="grid kpis">
            <StatTile label="Sessions" :value="fmtCompact(a.sessions)" :hint="`${a.clients.length} client(s)`" />
            <StatTile label="Tool calls" :value="fmtCompact(a.calls)" :hint="`${errorRate} errors · ${a.avg_latency_ms} ms avg`" />
            <StatTile label="Tokens served" :value="fmtCompact(a.response_tokens)" :hint="a.calls ? `${fmtInt(Math.round(a.response_tokens / a.calls))} per call` : 'no calls yet'" />
            <StatTile label="Tokens avoided (est.)" :value="fmtCompact(a.avoided_tokens)" hint="vs reading the files instead" />
          </div>
          <div v-if="a.calls || a.sessions" class="grid cols-2">
            <div><p class="label">Tool calls per day</p><ColumnChart :points="callsPerDay" title="Tool calls per day" unit="calls" /></div>
            <div><p class="label">Tokens avoided per day (estimate)</p><ColumnChart :points="avoidedPerDay" title="Tokens avoided per day" unit="tokens" color="var(--green)" /></div>
          </div>
          <div v-else class="empty"><strong>No agent activity yet</strong>Connect an agent (<code>just mcp</code>) and use it on an indexed repository; counts appear here within a minute.</div>
          <div v-if="a.calls" class="grid cols-3">
            <div><p class="label">Most used tools</p><RankList :rows="tools" /></div>
            <div><p class="label">Clients</p><RankList :rows="clients" color="var(--violet)" /></div>
            <div><p class="label">Repositories asked about</p><RankList :rows="repoUse" /></div>
          </div>
          <div v-if="a.calls" class="read-share">
            <p class="label">Answers the agent read</p>
            <RankList :rows="readShare" suffix="%" :scale="100" color="var(--green)" empty="Install the Claude Code plugin to see which tools point agents to code they then read." />
            <p v-if="readShare.length" class="muted small">Of the file line ranges a tool's answers gave, the share the agent read afterwards. A low share means the tool answers with places that do not help; reported by the plugin's hooks as counts only.</p>
          </div>
          <details class="method"><summary>How tokens avoided are estimated</summary><p class="muted small">{{ a.avoided_method }}</p></details>
        </section>

        <!-- Offloading ---------------------------------------------------- -->
        <section class="panel edge--violet">
          <p class="eyebrow">Context offloading</p><h2>Agent memory</h2>
          <div class="grid kpis">
            <StatTile label="Facts stored" :value="fmtCompact(m.facts)" :hint="`${fmtInt(m.entities)} entities · ${fmtInt(m.confirmations)} confirmations`" />
            <StatTile label="Saves" :value="fmtCompact(m.saves)" :hint="`upsert_graph_triples, last ${days} days`" />
            <StatTile label="Recalls" :value="fmtCompact(m.recalls)" :hint="`${m.model.recalls_per_session} per session`" />
            <StatTile label="Tokens per recall" :value="fmtInt(Math.round(m.model.tokens_per_recall))" :hint="`${m.model.tokens_per_fact} tokens per fact`" />
          </div>
          <div v-if="m.facts" class="grid cols-2">
            <div><p class="label">Facts per repository</p><RankList :rows="memRepos" color="var(--violet)" /></div>
            <div><p class="label">Saved by</p><RankList :rows="memSources" color="var(--violet)" /></div>
          </div>
          <h3>What offloading should save — theoretical</h3>
          <p class="muted small">Adjust the session to match how you work. The recall cost comes from your agents once they have recalled facts.</p>
          <OffloadModel :tokens-per-recall="m.model.tokens_per_recall" :recalls-per-session="m.model.recalls_per_session" :tokens-per-fact="m.model.tokens_per_fact" :measured="m.model.measured" />
        </section>

        <!-- Code & security ---------------------------------------------- -->
        <section class="panel edge--pink">
          <p class="eyebrow">Code &amp; security</p><h2>{{ w.repos }} repositories, {{ fmtCompact(w.loc) }} lines</h2>
          <div class="grid kpis">
            <StatTile label="Findings" :value="fmtCompact(Object.values(w.findings).reduce((t, n) => t + n, 0))" :hint="`${w.findings.critical ?? 0} critical · ${w.findings.high ?? 0} high`" />
            <StatTile label="Vulnerabilities" :value="fmtCompact(Object.values(w.vulns).reduce((t, n) => t + n, 0))" :hint="`${w.vulns_called} called · ${w.vulnerable_modules} modules`" />
            <StatTile label="Test reach (avg)" :value="`${avg((x) => x.test_reach_pct)}%`" :hint="`doc coverage ${avg((x) => x.doc_coverage_pct)}%`" />
            <StatTile label="Unused code" :value="fmtCompact(w.dead_loc)" :hint="`lines · ${w.unused_modules} unused modules`" />
          </div>
          <div class="grid cols-3">
            <div><p class="label">Security grades</p><RankList :rows="grades" color="var(--cyan)" /></div>
            <div><p class="label">Findings by severity</p><RankList :rows="findingsBySev" color="var(--pink)" empty="No findings." /></div>
            <div><p class="label">Vulnerabilities by severity</p><RankList :rows="vulnsBySev" color="var(--pink)" empty="No known vulnerabilities." /></div>
          </div>
          <div><p class="label">Most widespread rules (repositories affected)</p><RankList :rows="rules" color="var(--amber)" :limit="12" /></div>
          <p class="muted small">Graph answers need {{ w.reduction_pct }}% fewer tokens than reading the files (token benchmark measured at ingest, {{ fmtCompact(w.tokens_saved) }} tokens saved over the sampled questions).</p>
        </section>

        <!-- Dependencies & licences -------------------------------------- -->
        <section class="panel edge--orange">
          <p class="eyebrow">Dependencies &amp; licences</p><h2>{{ fmtCompact(l.dependencies) }} dependencies checked</h2>
          <div class="grid kpis">
            <StatTile label="Licence conflicts" :value="fmtInt(l.conflict)" :hint="`in ${l.conflicts.length} repositories`" />
            <StatTile label="To review" :value="fmtInt(l.review)" :hint="`${fmtInt(l.unknown)} without a known licence`" />
            <StatTile label="Without own licence" :value="l.unlicensed.length" hint="repositories" />
            <StatTile label="Update automation" :value="automationPct" :hint="`${s.hygiene.no_update_automation.length} repositories without Renovate or Dependabot`" />
          </div>
          <div class="grid cols-3">
            <div><p class="label">Licences in use</p><RankList :rows="licences" color="var(--orange)" /></div>
            <div><p class="label">Repository licences</p><RankList :rows="projects" color="var(--orange)" /></div>
            <div><p class="label">Repositories with conflicts</p><RankList :rows="conflicts" color="var(--pink)" empty="No licence conflicts." /></div>
          </div>
          <div v-if="s.hygiene.renovate_issues.length || s.hygiene.no_update_automation.length" class="grid cols-2">
            <div><p class="label">Renovate configuration issues</p><RankList :rows="s.hygiene.renovate_issues.map((x) => ({ label: x.repo, value: x.count }))" color="var(--amber)" empty="No Renovate issues." /></div>
            <div>
              <p class="label">No update automation</p>
              <p class="muted small mono wrap">{{ s.hygiene.no_update_automation.join(' · ') || '–' }}</p>
            </div>
          </div>
        </section>

        <!-- Reviews, AI, sync -------------------------------------------- -->
        <div class="grid cols-3">
          <section class="panel">
            <p class="eyebrow">Reviews</p><h2>{{ r.reviews }} reviews, {{ r.comments }} comments</h2>
            <p class="label">Comment decisions</p><RankList :rows="entries(r.by_status)" color="var(--cyan)" />
            <p class="label">Source</p><RankList :rows="entries(r.by_source)" color="var(--violet)" />
            <p v-if="Object.keys(r.fixes).length" class="label">Fix branches</p><RankList v-if="Object.keys(r.fixes).length" :rows="entries(r.fixes)" color="var(--green)" />
          </section>
          <section class="panel">
            <p class="eyebrow">AI providers</p><h2>Model usage</h2>
            <p class="muted small">Since the API started.</p>
            <table v-if="s.ai.length">
              <thead><tr><th>Provider</th><th class="num">Requests</th><th class="num">Tokens in / out</th></tr></thead>
              <tbody>
                <tr v-for="p in s.ai" :key="p.provider">
                  <td>{{ p.provider }} <div class="muted small mono">{{ p.model }}{{ p.local ? ' · local' : '' }}</div></td>
                  <td class="num">{{ fmtInt(p.usage.requests) }}<div v-if="p.usage.failures" class="muted small">{{ p.usage.failures }} failed</div></td>
                  <td class="num">{{ fmtCompact(p.usage.input_tokens) }} / {{ fmtCompact(p.usage.output_tokens) }}</td>
                </tr>
              </tbody>
            </table>
            <p v-else class="muted small">No AI providers configured.</p>
          </section>
          <section class="panel">
            <p class="eyebrow">Sync health</p><h2>{{ s.jobs.succeeded }} of {{ s.jobs.total }} jobs succeeded</h2>
            <div class="facts">
              <div><span class="label">Failed</span><strong>{{ s.jobs.failed }}</strong></div>
              <div><span class="label">Running</span><strong>{{ s.jobs.running }}</strong></div>
              <div><span class="label">Average</span><strong>{{ Math.round(s.jobs.avg_duration_sec) }} s</strong></div>
              <div><span class="label">Longest</span><strong>{{ Math.round(s.jobs.max_duration_sec) }} s</strong></div>
              <div><span class="label">Stale repositories</span><strong>{{ w.stale }}</strong></div>
            </div>
          </section>
        </div>
      </div>
    </template>
    <p v-else class="muted">Loading…</p>
  </div>
</template>

<style scoped>
.telemetry-group + .telemetry-group { border-top: 1px solid var(--line); padding-top: 12px; }
.filters { gap: 8px; }
.small { font-size: 12.5px; }
.kpis { grid-template-columns: repeat(auto-fit, minmax(200px, 1fr)); margin: 12px 0; }
.label { display: block; margin: 12px 0 8px; }
h3 { margin: 18px 0 4px; font-size: 15px; }
.read-share { margin-top: 16px; max-width: 560px; }
.method summary { cursor: pointer; color: var(--muted); font-size: 12.5px; margin-top: 12px; }
.refreshing { opacity: 0.6; transition: opacity 150ms; }
.wrap { white-space: normal; word-break: break-word; }
.facts { display: grid; gap: 8px; }
.facts > div { display: flex; justify-content: space-between; border-bottom: 1px dashed var(--line); padding-bottom: 6px; }
</style>
