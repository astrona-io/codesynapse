<script setup lang="ts">
import type { WorkspaceStats } from '~/utils/types'

const days = ref(30)
const { data: s, error, pending } = useApiGet<WorkspaceStats>(() => `stats?days=${days.value}`)

const a = computed(() => s.value?.agents)
const m = computed(() => s.value?.memory)
const errorRate = computed(() => (a.value?.calls ? `${((a.value.errors / a.value.calls) * 100).toFixed(1)}%` : '–'))
const callsPerDay = computed(() => (a.value?.daily ?? []).map((d) => ({ label: d.day, value: d.calls, detail: `${d.sessions} session(s), ${d.errors} error(s)` })))
const avoidedPerDay = computed(() => (a.value?.daily ?? []).map((d) => ({ label: d.day, value: d.avoided_tokens, detail: `${fmtInt(d.response_tokens)} served` })))
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
.filters { gap: 8px; }
.small { font-size: 12.5px; }
.kpis { grid-template-columns: repeat(auto-fit, minmax(200px, 1fr)); margin: 12px 0; }
.label { display: block; margin: 12px 0 8px; }
h3 { margin: 18px 0 4px; font-size: 15px; }
.method summary { cursor: pointer; color: var(--muted); font-size: 12.5px; margin-top: 12px; }
.refreshing { opacity: 0.6; transition: opacity 150ms; }
.wrap { white-space: normal; word-break: break-word; }
.facts { display: grid; gap: 8px; }
.facts > div { display: flex; justify-content: space-between; border-bottom: 1px dashed var(--line); padding-bottom: 6px; }
</style>
