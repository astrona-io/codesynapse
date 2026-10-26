<script setup lang="ts">
import { useStatistics } from '~/composables/useStatistics'
const { days, s, error, pending } = useStatistics()
const m = computed(() => s.value?.memory)

// Recall cost of one fact (its text plus JSON) and of a repository's whole
// memory: what an agent would pay to load everything instead of recalling.
const perFact = computed(() => m.value?.model.tokens_per_fact ?? 20)
const loadAll = computed(() => Math.round((m.value?.facts ?? 0) * perFact.value))
const recallSharePct = computed(() => (loadAll.value && m.value?.model.tokens_per_recall ? Math.min(100, Math.round((100 * m.value.model.tokens_per_recall) / loadAll.value)) : null))
const versions = computed(() => (m.value?.facts ?? 0) + (m.value?.history ?? 0))
const replacedPct = computed(() => (versions.value ? Math.round((100 * (m.value?.history ?? 0)) / versions.value) : 0))
const confirmsPerFact = computed(() => (m.value?.facts ? (m.value.confirmations / m.value.facts).toFixed(1) : '0'))
const activeRepos = computed(() => (m.value?.repos ?? []).filter((r) => r.facts > 0).length)

// Daily activity, one stacked column per day.
const daily = computed(() => m.value?.daily ?? [])
const dailyKeys = [
  { key: 'saves', label: 'Saves', color: 'var(--s-2)' },
  { key: 'recalls', label: 'Recalls', color: 'var(--s-1)' },
  { key: 'auto', label: 'Prompts looked up', color: 'var(--s-3)' },
  { key: 'offload', label: 'Facts from offloading', color: 'var(--s-4)' },
]
const dailyRows = computed(() => daily.value.map((d) => ({ label: d.day, values: { saves: d.saves, recalls: d.recalls, auto: d.auto_recalls, offload: d.offload_facts } })))
const activeDays = computed(() => daily.value.filter((d) => d.saves + d.recalls + d.auto_recalls + d.offloads > 0).length)
const busiest = computed(() => {
  let best: { day: string; n: number } | null = null
  for (const d of daily.value) {
    const n = d.saves + d.recalls + d.auto_recalls + d.offloads
    if (n > 0 && (!best || n > best.n)) best = { day: d.day, n }
  }
  return best
})
// Which tool the recalls came through; recall_context is the one agents start with.
const recallTools = computed(() => Object.entries(m.value?.recalls_by_tool ?? {})
  .map(([label, value]) => ({ label, value, detail: `${m.value?.recalls ? Math.round((100 * value) / m.value.recalls) : 0}% of recalls` }))
  .sort((x, y) => y.value - x.value))
const hasActivity = computed(() => activeDays.value > 0)

// How memory was reached and how facts arrived: by the agent asking, or by
// the hooks on their own.
const reached = computed(() => [
  { label: 'Agent recalls', value: m.value?.recalls ?? 0, color: 'var(--s-1)', detail: `${fmtCompact(m.value?.recall_tokens ?? 0)} tokens returned` },
  { label: 'Automatic lookups', value: m.value?.auto_recalls ?? 0, color: 'var(--s-3)', detail: `${fmtCompact(m.value?.auto_recall_tokens ?? 0)} tokens added to prompts` },
])
const arrived = computed(() => [
  { label: 'Saved by agents', value: m.value?.saves ?? 0, color: 'var(--s-2)', detail: 'upsert_graph_triples calls' },
  { label: 'Extracted from offloading', value: m.value?.offload_facts ?? 0, color: 'var(--s-4)', detail: 'by the local model' },
])
const autoShare = computed(() => {
  const total = reached.value.reduce((t, x) => t + x.value, 0)
  return total ? Math.round((100 * (m.value?.auto_recalls ?? 0)) / total) : 0
})
const per = (a: number, b: number) => (b ? Math.round(a / b) : 0)

// Where memory lives, largest first.
const repos = computed(() => [...(m.value?.repos ?? [])].sort((x, y) => y.facts - x.facts || x.repo.localeCompare(y.repo)))
const repoName = (ns: string) => ns.split('|').at(-1) ?? ns
const topSource = (src: Record<string, number>) => Object.entries(src ?? {}).sort((x, y) => y[1] - x[1])[0]?.[0] ?? '–'
const repoTokens = (r: { facts: number; fact_chars: number }) => Math.round(r.fact_chars / 4 + r.facts * 15)
const repoBars = computed(() => repos.value.slice(0, 8).map((r) => ({ label: repoName(r.repo), values: { current: r.facts, replaced: r.history ?? 0 } })))
const sources = computed(() => Object.entries(m.value?.sources ?? {}).map(([label, value]) => ({ label, value, detail: `${m.value?.facts ? Math.round((100 * value) / m.value.facts) : 0}% of stored facts` })).sort((x, y) => y.value - x.value))

// Agents that saved and recalled in the window.
const clientBars = computed(() => (m.value?.clients ?? []).slice(0, 8).map((c) => ({ label: c.client || 'unknown', values: { saves: c.saves, recalls: c.recalls } })))

// Live counters from VictoriaMetrics, when the install has it.
const range = computed(() => seriesRangeFor(days.value))
const livePanels = ['memory_facts', 'memory']
const { panels: batch, pending: batchPending } = useSeriesBatch(livePanels, range)
const metricsOff = ref(false)
const hiddenPanels = ref<string[]>([])
function unavailable(panel: string, status: number) {
  if (status === 409) metricsOff.value = true
  else if (!hiddenPanels.value.includes(panel)) hiddenPanels.value.push(panel)
}
</script>

<template>
  <p v-if="error" class="panel error">{{ apiErrorMessage(error) }}</p>
  <div v-else-if="s && m" :class="{ refreshing: pending }" class="stack">
    <section class="panel">
      <p class="eyebrow">Context offloading</p><h2>Agent memory</h2>
      <p class="muted lead">Decisions, conventions and open issues agents keep between chats, so a new session recalls a few facts instead of re-reading the code. Activity covers the last {{ days }} days; stored facts are counted now.</p>
      <div class="grid kpis">
        <StatTile label="Facts stored" :value="fmtCompact(m.facts)" :hint="`${fmtInt(m.entities)} entities in ${activeRepos} ${activeRepos === 1 ? 'repository' : 'repositories'}`" />
        <StatTile label="Replaced facts" :value="fmtCompact(m.history ?? 0)" :hint="`${replacedPct}% of all versions kept as history`" />
        <StatTile label="Confirmations" :value="fmtCompact(m.confirmations)" :hint="`${confirmsPerFact} per fact: saved again by a later chat`" />
        <StatTile label="Saves" :value="fmtCompact(m.saves)" :hint="`${fmtCompact(m.offload_facts ?? 0)} more facts from offloading`" />
        <StatTile label="Recalls" :value="fmtCompact(m.recalls)" :hint="`${m.model.recalls_per_session} per session${m.model.measured ? '' : ' (assumed)'}`" />
        <StatTile label="Tokens per recall" :value="fmtInt(Math.round(m.model.tokens_per_recall))" :hint="`${m.model.tokens_per_fact} per fact${m.model.measured ? ', measured' : ', estimated'}`" />
      </div>
      <div v-if="loadAll" class="meter">
        <div class="meter__head">
          <span class="label">One recall against loading everything</span>
          <span class="muted small">~{{ fmtInt(Math.round(m.model.tokens_per_recall)) }} of ~{{ fmtCompact(loadAll) }} tokens</span>
        </div>
        <div class="meter__track" role="img" :aria-label="`A recall returns about ${recallSharePct}% of what loading every fact would cost`">
          <div class="meter__fill" :style="{ width: `${Math.max(1, recallSharePct ?? 0)}%` }" />
        </div>
        <p class="muted small">A recall returns about {{ recallSharePct }}% of what pasting all {{ fmtInt(m.facts) }} facts into the prompt would cost.</p>
      </div>
    </section>

    <section class="panel">
      <p class="eyebrow">Over time</p><h2>Memory activity per day</h2>
      <div v-if="hasActivity" class="activity">
        <StackedColumns :rows="dailyRows" :keys="dailyKeys" title="Memory activity per day" unit="events" />
        <aside class="side">
          <div class="mini"><span class="label">Active days</span><strong>{{ activeDays }}<small> of {{ days }}</small></strong></div>
          <div v-if="busiest" class="mini"><span class="label">Busiest day</span><strong>{{ fmtInt(busiest.n) }}<small> on {{ busiest.day.slice(5) }}</small></strong></div>
          <div>
            <span class="label">Recalls by tool</span>
            <RankList :rows="recallTools" color="var(--s-1)" :limit="3" empty="No agent recalls yet." />
          </div>
        </aside>
      </div>
      <div v-else class="empty"><strong>No memory activity in the last {{ days }} days</strong>Agents save with <code>upsert_graph_triples</code> and recall with <code>recall_context</code>, <code>search_graph_nodes</code> or <code>query_subgraph</code>; the Claude Code plugin also looks up memory for each prompt and offloads older turns before a compaction.</div>
    </section>

    <section class="panel">
      <p class="eyebrow">Manual and automatic</p><h2>Who did the work</h2>
      <p class="muted lead">The agent asking for memory, against the Claude Code hooks doing it on their own: {{ autoShare }}% of lookups were automatic.</p>
      <div class="grid cols-2">
        <DonutChart :segments="reached" title="How memory was reached" unit="lookups" :center="fmtCompact(reached[0]!.value + reached[1]!.value)" center-hint="lookups" :size="170" />
        <DonutChart :segments="arrived" title="How facts arrived" unit="facts" :center="fmtCompact(arrived[0]!.value + arrived[1]!.value)" center-hint="saved" :size="170" />
      </div>
      <div class="grid kpis">
        <StatTile label="Prompts looked up" :value="fmtCompact(m.auto_recalls ?? 0)" :hint="`~${fmtInt(per(m.auto_recall_tokens ?? 0, m.auto_recalls ?? 0))} tokens added per prompt`" />
        <StatTile label="Conversations offloaded" :value="fmtCompact(m.offloads ?? 0)" :hint="`${fmtCompact(m.offloaded_turns ?? 0)} turns, ~${fmtInt(per(m.offloaded_turns ?? 0, m.offloads ?? 0))} per offload`" />
        <StatTile label="Facts per offload" :value="fmtInt(per(m.offload_facts ?? 0, m.offloads ?? 0))" :hint="`${fmtCompact(m.offload_facts ?? 0)} facts from the local model`" />
      </div>
      <p v-if="!(m.offloads ?? 0)" class="muted small">No offloading yet: it runs when a chat compacts, which happens in long sessions.</p>
    </section>

    <section class="panel">
      <p class="eyebrow">Where it lives</p><h2>Memory per repository</h2>
      <template v-if="repos.length">
        <div class="grid cols-2">
          <StackedBars :rows="repoBars" :keys="[{ key: 'current', label: 'Current', color: 'var(--s-1)' }, { key: 'replaced', label: 'Replaced', color: 'var(--s-other)' }]" title="Current and replaced facts per repository" unit="facts" />
          <div><p class="label">Saved by</p><RankList :rows="sources" color="var(--s-2)" empty="No sources recorded." /></div>
        </div>
        <div class="table-wrap">
          <table>
            <thead><tr><th>Repository</th><th class="num">Facts</th><th class="num">Replaced</th><th class="num">Entities</th><th class="num">Confirmations</th><th class="num">To load all</th><th>Mostly from</th><th>Updated</th></tr></thead>
            <tbody>
              <tr v-for="r in repos" :key="r.repo">
                <td><NuxtLink :to="`/repos/${encodeURIComponent(repoName(r.repo))}`">{{ repoName(r.repo) }}</NuxtLink></td>
                <td class="num">{{ fmtInt(r.facts) }}</td>
                <td class="num">{{ fmtInt(r.history ?? 0) }}</td>
                <td class="num">{{ fmtInt(r.entities) }}</td>
                <td class="num">{{ fmtInt(r.confirmations) }}</td>
                <td class="num">~{{ fmtCompact(repoTokens(r)) }} tokens</td>
                <td>{{ topSource(r.sources) }}</td>
                <td class="muted">{{ r.last_updated ? relTime(r.last_updated) : '–' }}</td>
              </tr>
            </tbody>
          </table>
        </div>
      </template>
      <div v-else class="empty"><strong>No facts stored yet</strong>Facts appear here once an agent saves a decision, convention or open issue for a repository.</div>
    </section>

    <section v-if="clientBars.length" class="panel">
      <p class="eyebrow">Agents</p><h2>Saves and recalls per agent</h2>
      <StackedBars :rows="clientBars" :keys="[{ key: 'saves', label: 'Saves', color: 'var(--s-2)' }, { key: 'recalls', label: 'Recalls', color: 'var(--s-1)' }]" title="Saves and recalls per agent" unit="calls" />
    </section>

    <section class="panel">
      <p class="eyebrow">Live</p><h2>Memory counters</h2>
      <div v-if="metricsOff" class="empty"><strong>Time series are not switched on</strong>They need VictoriaMetrics: the compose stack runs it, other installs set <code>CODESYNAPSE_METRICS_URL</code> on the API and MCP services.</div>
      <div v-else class="grid cols-2">
        <template v-for="p in livePanels" :key="p">
          <SeriesCard v-if="!hiddenPanels.includes(p)" :panel="p" :range="range" :batched="batch[p] ?? null" :batch-pending="batchPending" @unavailable="(status) => unavailable(p, status)" />
        </template>
      </div>
      <p class="muted small">What offloading could save in a long session, with these measured costs, is worked through on the <NuxtLink to="/agents">Agents page</NuxtLink> under “Keep each chat small”.</p>
    </section>
  </div>
  <p v-else class="muted">Loading…</p>
</template>

<style scoped>
.small { font-size: 12.5px; }
.lead { max-width: 72ch; margin: 4px 0 0; }
.kpis { grid-template-columns: repeat(auto-fit, minmax(180px, 1fr)); margin: 16px 0; }
.label { display: block; margin: 12px 0 8px; }
.activity { display: grid; grid-template-columns: minmax(0, 1fr) 240px; gap: 24px; align-items: start; margin-top: 12px; }
.side { display: grid; gap: 14px; }
.side .label { margin: 0 0 4px; }
.mini strong { display: block; font: 600 22px/1.1 var(--font-display); font-variant-numeric: tabular-nums; }
.mini small { font: 400 12.5px var(--font-sans); color: var(--muted); }
@media (max-width: 760px) { .activity { grid-template-columns: 1fr; } }
.meter { margin-top: 4px; padding: 14px 16px; border-radius: var(--radius); background: var(--panel-2); }
.meter__head { display: flex; justify-content: space-between; gap: 12px; flex-wrap: wrap; }
.meter__track { height: 12px; margin: 8px 0; border-radius: 999px; background: var(--line); overflow: hidden; }
.meter__fill { height: 100%; border-radius: 999px; background: var(--s-1); transition: width 400ms ease; }
.meter p { margin: 0; }
.table-wrap { margin-top: 16px; }
.refreshing { opacity: 0.6; transition: opacity 150ms; }
</style>
