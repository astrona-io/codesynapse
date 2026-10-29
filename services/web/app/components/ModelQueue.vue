<script setup lang="ts">
import type { QueueMetric, QueuePeak, QueueProvider, SeriesRange } from '~/utils/types'

// The model queue: how many requests run and wait now, how long they waited
// over time, and when in the week they wait, folded into weekday and
// 15-minute slot in the viewer's timezone. scope "platform" reads the
// administration endpoints (every organisation, or the one in org).
const props = defineProps<{ scope: 'org' | 'platform'; org?: string }>()

const WEEKS = [1, 4, 12] as const
const METRICS: { id: QueueMetric; label: string }[] = [
  { id: 'p95', label: 'p95 wait' }, { id: 'avg', label: 'Average wait' },
  { id: 'requests', label: 'Requests' }, { id: 'share', label: 'Found it full' },
]
const weeks = ref<1 | 4 | 12>(4)
const metric = ref<QueueMetric>('p95')
// The browser's zone is only known after mounting: the server rendering
// the page has its own.
const tz = ref('')
const base = computed(() => (props.scope === 'platform' ? 'admin/stats/queue' : 'stats/queue'))
const path = () => `${base.value}?weeks=${weeks.value}&tz=${encodeURIComponent(tz.value)}${props.org ? `&org=${encodeURIComponent(props.org)}` : ''}`
const { data, error, pending, execute } = useApiGet<QueuePeak>(path, { immediate: false })
onMounted(() => {
  try { tz.value = Intl.DateTimeFormat().resolvedOptions().timeZone ?? '' } catch { tz.value = '' }
  execute()
})

const status = computed(() => (error.value as { statusCode?: number } | null)?.statusCode ?? 0)
const metricsOff = computed(() => status.value === 409)

// Time series keep 30 days at most: twelve weeks of peak hours, the last
// thirty days of charts.
const range = computed<SeriesRange>(() => (weeks.value === 1 ? '7d' : '30d'))
const PANELS = ['queue_load', 'queue_wait', 'queue_saturation', 'queue_start']
const { panels: charts, pending: chartsPending } = useSeriesBatch(PANELS, range, undefined,
  props.scope === 'platform' ? { base: 'admin/stats/series/batch', org: () => props.org } : {})
const hiddenPanels = ref<string[]>([])
function unavailable(panel: string) {
  if (!hiddenPanels.value.includes(panel)) hiddenPanels.value.push(panel)
}

const num = (v: number | null | undefined) => (v == null ? '–' : String(Math.round(v)))
function slotsLabel(p: QueueProvider): string {
  if (props.scope === 'platform' && p.capacity != null) return `${Math.round(p.capacity)} slots over every process`
  if (p.slots > 0) return `${p.slots} slot${p.slots === 1 ? '' : 's'} per process`
  return 'no limit configured here'
}
const verdictTip = computed(() => {
  const t = data.value?.thresholds
  if (!t) return ''
  return `Judged on the busiest stretch of one weekday (slots with at least ${t.peak_min_requests} requests), after at least ${t.min_requests} requests in the period. `
    + `"Add an endpoint" when requests wait ${t.wait_high_seconds} s or more at p95 and ${Math.round(t.share_high * 100)}% or more found every slot busy; `
    + `"noticeable" from ${t.wait_noticeable_seconds} s at p95 or ${Math.round(t.share_noticeable * 100)}% finding it full. `
    + `A request found it full when it waited longer than ${t.free_slot_seconds * 1000} ms for a slot.`
})
const LEVEL: Record<string, string> = { unknown: 'Not enough data', fine: 'One endpoint is enough', noticeable: 'Noticeable waits', add_endpoint: 'Add an endpoint' }
</script>

<template>
  <section class="panel stack">
    <div>
      <p class="eyebrow">Model queue</p>
      <h2>When requests wait for a model</h2>
      <p class="muted small">
        Each model endpoint runs a fixed number of requests at once (<code>max_concurrent</code>; local models one by default); the others wait in line.
        This shows how many run and wait now, how long they waited, and at which hours of the week, so you can tell whether another endpoint would help.
        <template v-if="scope === 'platform'">Across every organisation{{ org ? ' (the selected one only)' : '' }}; each view is recorded in the audit log.</template>
        <template v-else>Your organisation's requests only.</template>
      </p>
    </div>
    <div class="row filters">
      <span id="queue-weeks" class="label">Period</span>
      <div class="segmented" role="group" aria-labelledby="queue-weeks">
        <button v-for="w in WEEKS" :key="w" type="button" :aria-pressed="weeks === w" @click="weeks = w">{{ w === 1 ? '1 week' : `${w} weeks` }}</button>
      </div>
      <span v-if="pending" class="muted small">refreshing…</span>
    </div>

    <div v-if="metricsOff" class="empty"><strong>Time series are not switched on</strong>They need VictoriaMetrics: the compose stack runs it, other installs set <code>CODESYNAPSE_METRICS_URL</code> on the API and MCP services.</div>
    <DataState v-else :error="error" :pending="pending" :empty="!data?.providers?.length" title="No model queue yet" text="No model endpoint with a request limit has answered requests in this period.">
      <section v-for="p in data!.providers" :key="p.name" class="provider stack">
        <h3>{{ p.name }}</h3>
        <div class="grid cols-3">
          <StatTile label="Running now" :value="`${num(p.running)}`" :hint="slotsLabel(p)" tip="Requests holding a slot at the latest report; the services report every 15 seconds." />
          <StatTile label="Waiting now" :value="num(p.waiting)" hint="requests in line for a slot" />
          <StatTile :label="LEVEL[p.verdict.level] ?? 'Capacity'" :value="p.verdict.level === 'add_endpoint' ? 'Add one' : p.verdict.level === 'noticeable' ? 'Watch' : p.verdict.level === 'fine' ? 'Enough' : '–'" :hint="p.verdict.text" :tip="verdictTip" :tone="p.verdict.level === 'add_endpoint' ? 'warning' : undefined" />
        </div>
        <div class="row filters">
          <span :id="`queue-metric-${p.name}`" class="label">Peak hours by</span>
          <div class="segmented" role="group" :aria-labelledby="`queue-metric-${p.name}`">
            <button v-for="m in METRICS" :key="m.id" type="button" :aria-pressed="metric === m.id" @click="metric = m.id">{{ m.label }}</button>
          </div>
        </div>
        <QueueHeatmap :cells="p.cells" :metric="metric" :timezone="data!.timezone" :wait-noticeable="data!.thresholds.wait_noticeable_seconds" :wait-high="data!.thresholds.wait_high_seconds" />
      </section>
    </DataState>

    <div v-if="!metricsOff" class="grid cols-2">
      <template v-for="panel in PANELS" :key="panel">
        <SeriesCard v-if="!hiddenPanels.includes(panel)" :panel="panel" :range="range" :batched="charts[panel] ?? null" :batch-pending="chartsPending" @unavailable="unavailable(panel)" />
      </template>
    </div>
    <p v-if="weeks === 12 && !metricsOff" class="muted small">The charts show the last 30 days; the peak hours cover all twelve weeks.</p>
  </section>
</template>

<style scoped>
.filters { gap: 8px; }
.small { font-size: 12.5px; }
.provider + .provider { border-top: 1px solid var(--line); padding-top: 12px; }
h3 { margin: 0; font-size: 15px; }
</style>
