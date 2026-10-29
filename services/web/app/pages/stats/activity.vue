<script setup lang="ts">
import type { SeriesRange } from '~/utils/types'

// Live activity: time series from VictoriaMetrics, by minute up to hours.
const RANGES: { id: SeriesRange; label: string }[] = [
  { id: '1h', label: 'last hour' }, { id: '6h', label: '6 hours' }, { id: '24h', label: '24 hours' },
  { id: '7d', label: '7 days' }, { id: '30d', label: '30 days' },
]
const GROUPS = [
  { title: 'Tokens and model cost', help: 'Provider-reported tokens and configured prices. Missing usage or prices leave a measurement gap.', panels: ['llm_tokens', 'llm_cost', 'llm_savings', 'llm_coverage', 'llm_failure_causes', 'llm_ttft', 'served_tokens'] },
  { title: 'Model gateway', help: 'Requests agents sent through the gateway: time per route (average and 95th percentile), how long requests waited for the local model, how often it timed out, and why requests offered to it went to the cloud. Prompts are never recorded.', panels: ['gateway_latency_avg', 'gateway_latency', 'gateway_timeouts', 'gateway_queue_wait', 'gateway_fallthrough'] },
  { title: 'Network and cloud bandwidth', help: 'Application payload bytes. Avoided cloud traffic needs an equivalent configured fallback; local execution alone has no savings baseline.', panels: ['bandwidth', 'cloud_traffic', 'cloud_savings'] },
  { title: 'External services and the audit log', help: 'Requests to osv.dev, deps.dev and git host APIs carry only package names, versions and organisation names. Every changing request waits for its audit entry.', panels: ['lookups', 'lookup_latency', 'audit_write_latency'] },
  { title: 'Database performance', help: 'Measured request latency, volume and returned rows. Graph rows do not measure internal nodes traversed.', panels: ['db_latency', 'db_latency_tail', 'db_requests', 'db_rows', 'db_errors'] },
  { title: 'Agent memory and context offloading', help: 'Durable facts are current stored state. Saved-fact and recall counters measure activity.', panels: ['memory_facts', 'memory', 'sessions', 'tool_calls', 'tool_latency', 'tool_errors', 'tool_refusals', 'local_answers'] },
  { title: 'Assistant lifecycle activity', help: 'Codex and Cursor hooks report lifecycle events and observed byte counts. Prompts, source and responses are not retained. Several hooks can observe one operation.', panels: ['agent_hook_events', 'agent_hook_bytes'] },
]

// The window lives in the URL (?range=1h) so a view can be shared.
const route = useRoute()
const router = useRouter()
const isRange = (v: unknown): v is SeriesRange => RANGES.some((r) => r.id === v)
const range = ref<SeriesRange>(isRange(route.query.range) ? route.query.range : '24h')
watch(range, (r) => router.replace({ query: { ...route.query, range: r } }))
// Every panel of the page in one request; a panel refused for this user
// is hidden, as before.
const allPanels = GROUPS.flatMap((g) => g.panels)
const { panels: batch, pending: batchPending, refresh: refreshBatch } = useSeriesBatch(allPanels, range)
const tick = ref(0)
watch(tick, () => refreshBatch())
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

</script>

<template>
<section class="panel">
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
          <SeriesCard v-if="!hiddenPanels.includes(p)" :panel="p" :range="range" :batched="batch[p] ?? null" :batch-pending="batchPending" @unavailable="(status) => unavailable(p, status)" />
        </template>
      </div>
    </section>
  </div>
</section>

</template>

<style scoped>
.telemetry-group + .telemetry-group { border-top: 1px solid var(--line); padding-top: 12px; }
.filters { gap: 8px; }
.small { font-size: 12.5px; }
h3 { margin: 18px 0 4px; font-size: 15px; }
</style>
