<script setup lang="ts">
import { useStatistics } from '~/composables/useStatistics'
const { days, s, error, pending } = useStatistics()
const w = computed(() => s.value?.workspace)
const j = computed(() => s.value?.jobs)
const outcomes = computed(() => [
  { label: 'Succeeded', value: j.value?.succeeded ?? 0, color: 'var(--success)' },
  { label: 'Failed', value: j.value?.failed ?? 0, color: 'var(--danger)' },
  { label: 'Running', value: j.value?.running ?? 0, color: 'var(--info)' },
])
const finished = computed(() => (j.value?.succeeded ?? 0) + (j.value?.failed ?? 0))
const successPct = computed(() => (finished.value ? Math.round((100 * (j.value?.succeeded ?? 0)) / finished.value) : 0))
const secs = (n: number) => (n >= 90 ? `${Math.round(n / 60)} min` : `${Math.round(n)} s`)
const age = (h: number) => (h >= 48 ? `${Math.round(h / 24)} days` : `${Math.round(h)} h`)

// Oldest first: the repositories furthest behind their remote.
const stale = computed(() => (s.value?.repos ?? []).filter((r) => r.stale).sort((a, b) => b.age_hours - a.age_hours)
  .map((r) => ({ label: r.name, value: Math.round(r.age_hours), detail: `${age(r.age_hours)} since the last ingest` })))
const schedules = computed(() => {
  const by = { active: 0, paused: 0, manual: 0 }
  for (const r of s.value?.repos ?? []) by[r.schedule?.state ?? 'manual'] += 1
  return [
    { label: 'Synced on a schedule', value: by.active },
    { label: 'Paused', value: by.paused },
    { label: 'Manual only', value: by.manual },
  ]
})

// Live job and indexing measurements from VictoriaMetrics, from every API
// and worker process.
const range = computed(() => seriesRangeFor(days.value))
const livePanels = ['jobs_finished', 'job_failures', 'job_wait', 'job_duration', 'ingest_runs', 'ingest_duration', 'ingest_phases', 'ingest_extract', 'git_operations', 'git_duration']
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
  <div v-else-if="s && j" :class="{ refreshing: pending }" class="stack">
    <section class="panel">
      <p class="eyebrow">Sync health</p><h2>{{ fmtInt(j.succeeded) }} of {{ fmtInt(j.total) }} jobs succeeded</h2>
      <p class="muted lead">Clone, sync and index jobs the workers ran for these repositories.</p>
      <div class="grid kpis">
        <StatTile label="Success rate" :value="finished ? `${successPct}%` : '–'" :hint="`${fmtInt(j.failed)} failed`" />
        <StatTile label="Running now" :value="fmtInt(j.running)" hint="jobs in progress" />
        <StatTile label="Average run" :value="secs(j.avg_duration_sec)" :hint="`longest ${secs(j.max_duration_sec)}`" />
        <StatTile label="Stale repositories" :value="fmtInt(w?.stale ?? 0)" :hint="`of ${fmtInt(w?.repos ?? 0)} indexed`" />
      </div>
    </section>

    <div class="grid cols-2">
      <section class="panel">
        <p class="eyebrow">Outcomes</p><h2>Job results</h2>
        <DonutChart v-if="j.total" :segments="outcomes" title="Job outcomes" unit="jobs" :center="finished ? `${successPct}%` : '–'" center-hint="succeeded" :size="150" />
        <div v-else class="empty"><strong>No jobs yet</strong>Jobs appear when a repository is cloned, synced or indexed.</div>
      </section>
      <section class="panel">
        <p class="eyebrow">Schedules</p><h2>How repositories stay current</h2>
        <RankList :rows="schedules" color="var(--s-1)" :limit="3" />
      </section>
    </div>

    <div class="grid cols-2">
      <section class="panel">
        <p class="eyebrow">Behind</p><h2>Stale repositories</h2>
        <RankList :rows="stale" color="var(--warning)" suffix=" h" empty="Every repository is current." />
      </section>
      <section class="panel">
        <p class="eyebrow">Failures</p><h2>Recent failed jobs</h2>
        <ul v-if="j.recent_failures.length" class="fails">
          <li v-for="f in j.recent_failures" :key="f.id">
            <div class="fail-head">
              <NuxtLink :to="`/repos/${encodeURIComponent(f.repo)}`">{{ f.repo }}</NuxtLink>
              <span class="badge">{{ f.kind || 'job' }}</span>
              <span class="muted small">{{ relTime(f.finished_at || f.started_at) }}</span>
            </div>
            <p v-if="f.error" class="mono small err">{{ f.error }}</p>
          </li>
        </ul>
        <div v-else class="empty"><strong>No failures</strong>Every recent job finished cleanly.</div>
      </section>
    </div>

    <section class="panel">
      <p class="eyebrow">Live</p><h2>Jobs and indexing over time</h2>
      <p class="muted lead">How long jobs wait and run, how they end, and where indexing spends its time, measured in every API and worker process.</p>
      <div v-if="metricsOff" class="empty"><strong>Time series are not switched on</strong>They need VictoriaMetrics: the compose stack runs it, other installs set <code>CODESYNAPSE_METRICS_URL</code> on the API and worker services.</div>
      <div v-else class="grid cols-2">
        <template v-for="p in livePanels" :key="p">
          <SeriesCard v-if="!hiddenPanels.includes(p)" :panel="p" :range="range" :batched="batch[p] ?? null" :batch-pending="batchPending" @unavailable="(status) => unavailable(p, status)" />
        </template>
      </div>
    </section>
  </div>
  <p v-else class="muted">Loading…</p>
</template>

<style scoped>
.small { font-size: 12.5px; }
.lead { max-width: 72ch; margin: 4px 0 0; }
.kpis { grid-template-columns: repeat(auto-fit, minmax(170px, 1fr)); margin: 16px 0 0; }
.fails { list-style: none; margin: 12px 0 0; padding: 0; display: grid; gap: 8px; }
.fails li { padding: 10px 12px; border-radius: var(--radius-sm); background: var(--danger-soft); }
.fail-head { display: flex; flex-wrap: wrap; align-items: center; gap: 8px; }
.err { margin: 6px 0 0; white-space: pre-wrap; word-break: break-word; max-height: 4.5em; overflow: hidden; }
.refreshing { opacity: 0.6; transition: opacity 150ms; }
</style>
