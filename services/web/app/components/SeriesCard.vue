<script setup lang="ts">
import type { SeriesPanel, SeriesRange } from '~/utils/types'

// One time-series panel from /stats/series. Emits "unavailable" when the
// API says metrics are not configured (409) or the panel is not for this
// user (403), so the page can explain once instead of per chart.
const props = defineProps<{ panel: string; range: SeriesRange; tick: number; repo?: string }>()
const emit = defineEmits<{ unavailable: [status: number] }>()
const { data, error, pending, refresh } = useApiGet<SeriesPanel>(() => `stats/series?panel=${props.panel}&range=${props.range}${props.repo ? `&repo=${encodeURIComponent(props.repo)}` : ""}`)
watch(() => props.tick, () => refresh())
watch(error, (e) => {
  const status = (e as { statusCode?: number } | null)?.statusCode
  if (status === 409 || status === 403) emit('unavailable', status)
})
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
    <p v-if="error && (error as any).statusCode !== 409 && (error as any).statusCode !== 403" class="muted small">{{ apiErrorMessage(error) }}</p>
    <template v-else-if="data">
      <LineChart v-if="!empty" :times="data.times" :series="data.series" :title="data.title" :unit="data.unit" :step-seconds="data.step_seconds" />
      <p v-else class="muted small empty-chart">{{ data.empty }}</p>
      <p class="muted small">{{ data.help }}</p>
    </template>
  </div>
</template>

<style scoped>
.series-card { display: grid; gap: 6px; min-width: 0; }
.head { display: flex; align-items: baseline; justify-content: space-between; gap: 8px; }
.headline { color: var(--ink); font-size: 13px; }
.empty-chart { padding: 24px 0; text-align: center; border: 1px dashed var(--line); border-radius: var(--radius-xs); }
.refreshing { opacity: 0.7; }
</style>
