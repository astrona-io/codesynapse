<script setup lang="ts">
import type { BranchTrend, HistoryTrend, SeriesLine } from '~/utils/types'

// The security grade and findings by severity after each sync, one branch
// at a time: a sync is only compared with earlier syncs of the same branch,
// so switching the indexed branch never reads as a change.
const props = defineProps<{ repo: string }>()
const { data: trend } = useApiGet<HistoryTrend>(() => repoPath(props.repo, 'history?limit=100'))

const MAX_GRADES = 24
const branches = computed<BranchTrend[]>(() => trend.value?.branches ?? [])
const picked = ref<string | null>(null)
const current = computed(() => branches.value.find((b) => b.branch === picked.value) ?? branches.value[0])
const label = (b: string) => b || 'Branch not recorded'

const times = computed(() => (current.value?.points ?? []).map((p) => Math.floor(Date.parse(p.at) / 1000)))
const step = computed(() => {
  const t = times.value
  if (t.length < 2) return 86400
  const gaps = t.slice(1).map((v, i) => v - t[i]!).sort((a, b) => a - b)
  return gaps[Math.floor(gaps.length / 2)] || 86400
})
const lines = computed<SeriesLine[]>(() => {
  const pts = current.value?.points ?? []
  const line = (name: string, get: (p: (typeof pts)[number]) => number): SeriesLine => {
    const values = pts.map(get)
    return { name, values, total: values.at(-1) ?? 0 }
  }
  return [line('Critical', (p) => p.critical), line('High', (p) => p.high), line('Medium', (p) => p.medium), line('Low', (p) => p.low)]
})
const grades = computed(() => (current.value?.points ?? []).slice(-MAX_GRADES))
const gradeTone = (g?: string) => (g === 'A' ? 'good' : g === 'B' || g === 'C' ? 'warn' : 'bad')
const securityKeys = new Set(['grade', 'score', 'findings_high', 'vulns_high', 'vulns_called'])
const worse = computed(() => (current.value?.regressions ?? []).filter((c) => securityKeys.has(c.key)))
const when = (iso?: string) => (iso ? new Date(iso).toLocaleString([], { month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit' }) : '')
</script>

<template>
  <section v-if="trend" class="panel">
    <div class="panel__header">
      <div><p class="eyebrow">Security trend</p><h2>Grade and findings after each sync</h2></div>
      <NuxtLink :to="`/repos/${encodeURIComponent(repo)}/history`" class="btn btn--ghost">All trends</NuxtLink>
    </div>
    <div v-if="branches.length > 1" class="segmented" role="group" aria-label="Branch">
      <button v-for="b in branches" :key="b.branch" type="button" :aria-pressed="b.branch === current?.branch" @click="picked = b.branch">{{ label(b.branch) }}</button>
    </div>
    <div v-if="!current || current.points.length < 2" class="empty">
      <strong>{{ current?.points.length ? 'One sync so far' : 'No history yet' }}</strong>
      The trend starts after the second sync of {{ current?.branch || 'this repository' }}.
    </div>
    <template v-else>
      <p v-if="worse.length" class="notice notice--danger">
        Worse since {{ when(current.since) }}: {{ worse.map((c) => c.detail).join('; ') }}.
      </p>
      <div>
        <p class="muted small">Grade after each sync, oldest first<span v-if="current.points.length > MAX_GRADES"> (the latest {{ MAX_GRADES }})</span></p>
        <ol class="grades" aria-label="Security grade after each sync">
          <li v-for="p in grades" :key="p.at" :class="`grade--${gradeTone(p.grade)}`" :title="`${when(p.at)}: grade ${p.grade || 'not set'}, score ${p.score}`">{{ p.grade || '–' }}</li>
        </ol>
      </div>
      <LineChart :times="times" :series="lines" title="Security findings by severity" unit="" :step-seconds="step" :height="170" />
    </template>
  </section>
</template>

<style scoped>
.small { font-size: 12.5px; margin-bottom: 6px; }
.grades { display: flex; flex-wrap: wrap; gap: 4px; margin: 0; padding: 0; list-style: none; }
.grades li { display: grid; place-items: center; min-width: 26px; height: 26px; padding: 0 4px; border-radius: var(--radius-xs); font: 700 12px var(--font-mono); color: var(--ink); }
.grade--good { background: var(--success-soft); }
.grade--warn { background: var(--warning-soft); }
.grade--bad { background: var(--danger-soft); }
.segmented { margin-bottom: 4px; max-width: 100%; overflow-x: auto; }
</style>
