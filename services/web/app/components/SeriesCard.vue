<script setup lang="ts">
import type { SeriesPanel, SeriesRange } from '~/utils/types'
import type { BatchedPanel } from '~/composables/useSeriesBatch'

// One time-series panel from /stats/series. Emits "unavailable" when the
// API says metrics are not configured (409) or the panel is not for this
// user (403), so the page can explain once instead of per chart.
// A page that loads its panels together (useSeriesBatch) passes batched;
// a card on its own fetches its panel.
// yMax shares one y scale between cards shown side by side.
const props = defineProps<{ panel: string; range: SeriesRange; tick?: number; repo?: string; batched?: BatchedPanel | null; batchPending?: boolean; yMax?: number }>()
const emit = defineEmits<{ unavailable: [status: number] }>()
const own = props.batched === undefined
  ? useApiGet<SeriesPanel>(() => `stats/series?panel=${props.panel}&range=${props.range}${props.repo ? `&repo=${encodeURIComponent(props.repo)}` : ""}`)
  : null
if (own) watch(() => props.tick, () => own.refresh())
const data = computed<SeriesPanel | null>(() => (own ? own.data.value ?? null : props.batched?.data ?? null))
const pending = computed(() => (own ? own.pending.value : !!props.batchPending))
const error = computed<{ statusCode: number; message: string } | null>(() => {
  if (own) {
    const e = own.error.value as { statusCode?: number } | null
    return e ? { statusCode: e.statusCode ?? 0, message: apiErrorMessage(own.error.value) } : null
  }
  const b = props.batched
  return b && b.status !== 200 ? { statusCode: b.status, message: b.error ?? '' } : null
})
watch(error, (e) => {
  if (e?.statusCode === 409 || e?.statusCode === 403) emit('unavailable', e.statusCode)
}, { immediate: true })
const empty = computed(() => !data.value?.series.some((s) => s.values.some((v) => v !== null)))
const headline = computed(() => {
  const d = data.value
  if (!d?.series.length) return ''
  if (d.kind === 'count') return d.series.length === 1 ? fmtUnit(d.series[0]!.total, d.unit) : ''
  return fmtUnit(Math.max(...d.series.map((s) => s.total)), d.unit) + ' peak'
})
</script>

<template>
  <div v-if="data || pending || error" class="series-card" :class="{ refreshing: pending }">
    <div class="head">
      <p class="label">{{ data?.title ?? panel }}</p>
      <span v-if="headline" class="headline mono">{{ headline }}</span>
    </div>
    <DataState v-if="error && error.statusCode !== 409 && error.statusCode !== 403" :error="error.message" compact />
    <DataState v-else-if="pending && !data" pending empty compact />
    <template v-else-if="data">
      <LineChart v-if="!empty" :times="data.times" :series="data.series" :title="data.title" :unit="data.unit" :step-seconds="data.step_seconds" :y-max="yMax ?? 0" />
      <p v-else class="muted small empty-chart">{{ data.empty }}</p>
      <p class="muted small">{{ data.help }}</p>
    </template>
  </div>
</template>

<style scoped>
.series-card { display: grid; gap: 6px; min-width: 0; }
.head { display: flex; align-items: baseline; justify-content: space-between; gap: 8px; }
.headline { color: var(--ink); font-size: 13px; }
.empty-chart { padding: 24px 0; text-align: center; border: 2px dashed var(--line); border-radius: var(--radius-xs); }
.refreshing { opacity: 0.7; }
</style>
