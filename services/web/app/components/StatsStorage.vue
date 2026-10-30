<script setup lang="ts">
import type { SeriesRange, StatsRange, StatsStorage } from '~/utils/types'

// Storage section of the platform statistics: rows and bytes per store and
// kind from the daily sample (00:15 UTC), with the change over the window.
// NebulaGraph and Milvus report rows only; PostgreSQL rows are estimates
// except the audit log's.
const props = defineProps<{ range: StatsRange; tick: number }>()
const { data, error, refresh } = useApiGet<StatsStorage>(() => `admin/stats/storage?range=${props.range}`)
watch(() => props.tick, () => { refresh(); refreshCharts() })

const stores: Record<string, string> = { nebulagraph: 'NebulaGraph', milvus: 'Milvus', postgres: 'PostgreSQL' }
const kindNames: Record<string, string> = { node: 'all vertices', vectors: 'memory vectors', session_vectors: 'session vectors', database: 'whole database' }
const panels = ['store_rows', 'store_bytes']
const seriesRange = computed<SeriesRange>(() => (props.range === '90d' || props.range === '365d' ? '30d' : props.range as SeriesRange))
const { panels: charts, pending: chartsPending, refresh: refreshCharts } = useSeriesBatch(panels, seriesRange, undefined, { base: 'admin/stats/series/batch' })
const chartsOff = ref(false)

const totals = computed(() => {
  const out: { system: string; rows: number; bytes: number | null }[] = []
  for (const r of data.value?.stores ?? []) {
    let t = out.find((x) => x.system === r.db_system)
    if (!t) out.push(t = { system: r.db_system, rows: 0, bytes: null })
    if (r.rows !== null) t.rows += r.rows
    if (r.kind === 'database' && r.bytes !== null) t.bytes = r.bytes
  }
  return out
})
const change = (v: number | null, unit = '') => {
  if (v === null) return '–'
  if (v === 0) return '0'
  return (v > 0 ? '+' : '−') + fmtUnit(Math.abs(v), unit)
}
// 90 days and 1 year read the daily rollup, which keeps one sample a day:
// the date is the day, not a time.
const sampled = computed(() => {
  const at = data.value?.sampled_at
  if (!at) return ''
  const day = { year: 'numeric', month: 'short', day: 'numeric', timeZone: 'UTC' } as const
  return data.value?.rollup
    ? `on ${new Date(at).toLocaleDateString([], day)} (UTC)`
    : new Date(at).toLocaleString([], { ...day, hour: '2-digit', minute: '2-digit' }) + ' UTC'
})
const unavailable = computed(() => {
  const d = data.value
  if (!d || d.available) return ''
  if (d.rollup) return 'No store sizes in the daily figures for this window yet: they are recorded each night from the day\'s sample.'
  return d.reason === 'no metrics store' ? 'Store sizes need the metrics store (CODESYNAPSE_METRICS_URL).' : 'No store sample yet: the API samples once a day at 00:15 UTC.'
})
</script>

<template>
  <div class="stack">
    <p v-if="error" class="notice notice--danger">{{ apiErrorMessage(error) }}</p>
    <template v-if="data">
      <p v-if="!data.available" class="muted small">{{ unavailable }}</p>
      <template v-else>
        <div class="grid cols-3">
          <StatTile v-for="t in totals" :key="t.system" :label="stores[t.system] ?? t.system" :value="fmtCompact(t.rows)"
            :hint="t.bytes !== null ? `rows · ${fmtUnit(t.bytes, 'bytes')} on disk` : 'rows'" />
        </div>
        <p class="muted small">
          Sampled {{ sampled }}. Change is over the window<template v-if="data.rollup">, from the first recorded day ({{ data.rollup.written }} of {{ data.rollup.days }} days recorded); the charts show the last 30 days</template>. NebulaGraph and Milvus report rows only; PostgreSQL rows are planner estimates, exact for the audit log.</p>
        <div class="table-wrap">
          <table>
            <thead><tr><th>Store</th><th>Kind</th><th class="num">Rows</th><th class="num">Change</th><th class="num">Size</th><th class="num">Change</th></tr></thead>
            <tbody>
              <tr v-for="r in data.stores" :key="`${r.db_system}/${r.kind}`">
                <td>{{ stores[r.db_system] ?? r.db_system }}</td>
                <td class="mono small">{{ kindNames[r.kind] ?? r.kind }}</td>
                <td class="num">{{ r.rows === null ? '–' : fmtCompact(r.rows) }}</td>
                <td class="num muted">{{ change(r.rows_change) }}</td>
                <td class="num">{{ r.bytes === null ? '–' : fmtUnit(r.bytes, 'bytes') }}</td>
                <td class="num muted">{{ change(r.bytes_change, 'bytes') }}</td>
              </tr>
            </tbody>
          </table>
        </div>
      </template>
    </template>
    <div v-if="!chartsOff" class="grid cols-2">
      <SeriesCard v-for="p in panels" :key="p" :panel="p" :range="seriesRange" :tick="0" :batched="charts[p] ?? null" :batch-pending="chartsPending" @unavailable="chartsOff = true" />
    </div>
  </div>
</template>
