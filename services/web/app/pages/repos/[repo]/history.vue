<script setup lang="ts">
import type { HistoryTrend, SeriesLine } from '~/utils/types'

const route = useRoute()
const repo = computed(() => route.params.repo as string)
useHead({ title: () => `Trends · ${repo.value}` })
const { data: trend, error } = useApiGet<HistoryTrend>(() => repoPath(repo.value, 'history?limit=200'))

const times = computed(() => (trend.value?.snapshots ?? []).map((s) => Math.floor(Date.parse(s.at) / 1000)))
const step = computed(() => {
  const t = times.value
  if (t.length < 2) return 86400
  const gaps = t.slice(1).map((v, i) => v - t[i]!).sort((a, b) => a - b)
  return gaps[Math.floor(gaps.length / 2)] || 86400
})
function line(key: string, name: string): SeriesLine | undefined {
  const s = trend.value?.series.find((x) => x.key === key)
  return s ? { name, values: s.values, total: s.values.at(-1) ?? 0 } : undefined
}
const charts = computed(() => [
  { title: 'Security score (higher is better)', lines: [line('score', 'Score')] },
  { title: 'Vulnerabilities and findings (lower is better)', lines: [line('vulns_called', 'Called by your code'), line('vulns_high', 'High advisories'), line('findings_high', 'High findings')] },
  { title: 'Duplicate code, % of function lines (lower is better)', lines: [line('dup_pct', 'Duplicate')] },
  { title: 'Unused code (lower is better)', lines: [line('dead_functions', 'Unused functions'), line('unused_modules', 'Unused dependencies')] },
  { title: 'Tests and docs, % (higher is better)', lines: [line('test_reach_pct', 'Reached by tests'), line('doc_coverage_pct', 'Documented')] },
  { title: 'Licence conflicts (lower is better)', lines: [line('licence_conflicts', 'Conflicts')] },
].map((c) => ({ ...c, lines: c.lines.filter((l): l is SeriesLine => !!l) })))
const when = (iso?: string) => (iso ? new Date(iso).toLocaleString([], { month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit', timeZoneName: 'short' }) : '')
</script>

<template>
  <div class="stack">
    <PageHeader
      eyebrow="Repository" title="Trends"
      lede="How this repository changes from one sync to the next: security, dependencies, duplicate and unused code, tests and documentation. A point is added after every sync; nothing about the code itself is kept, only these numbers."
      :crumbs="[{ label: 'Repositories', to: '/' }, { label: repo, to: `/repos/${encodeURIComponent(repo)}` }, { label: 'Trends' }]"
    >
      <SyncControl :repo="repo" />
    </PageHeader>

    <p v-if="error" class="notice notice--danger">{{ apiErrorMessage(error) }}</p>
    <template v-else-if="trend">
      <div v-if="trend.snapshots.length < 2" class="empty">
        <strong>{{ trend.snapshots.length ? 'One point so far' : 'No history yet' }}</strong>
        A trend needs two syncs. Press Sync again after the code changes, or wait for the next scheduled sync.
      </div>
      <template v-else>
        <div class="grid cols-2">
          <section class="panel">
            <p class="eyebrow">Got worse</p>
            <h2>Since {{ when(trend.since) }}</h2>
            <ul v-if="trend.regressions.length" class="plain"><li v-for="c in trend.regressions" :key="c.key">{{ c.detail }}</li></ul>
            <p v-else class="muted">Nothing got worse by more than the noise.</p>
          </section>
          <section class="panel">
            <p class="eyebrow eyebrow--green">Got better</p>
            <h2>Since {{ when(trend.since) }}</h2>
            <ul v-if="trend.improvements.length" class="plain"><li v-for="c in trend.improvements" :key="c.key">{{ c.detail }}</li></ul>
            <p v-else class="muted">No metric improved by more than the noise.</p>
          </section>
        </div>
        <div class="grid cols-2">
          <section v-for="c in charts" :key="c.title" class="panel">
            <h2>{{ c.title }}</h2>
            <LineChart :times="times" :series="c.lines" :title="c.title" unit="" :step-seconds="step" :height="170" />
          </section>
        </div>
        <p class="muted small">{{ trend.snapshots.length }} syncs, from {{ when(trend.snapshots[0]?.at) }} to {{ when(trend.snapshots.at(-1)?.at) }}. Small moves (less than 1 point of duplicate code, 5 unused functions, 2 points of test reach) are not called regressions.</p>
      </template>
    </template>
    <p v-else class="muted">Loading…</p>
  </div>
</template>

<style scoped>
.plain { list-style: none; padding: 0; margin: 0; display: grid; gap: 6px; }
.small { font-size: 12.5px; }
</style>
