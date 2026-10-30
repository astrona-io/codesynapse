<script setup lang="ts">
import type { QueueMetric, QueuePeak, QueueProvider, SeriesRange } from '~/utils/types'

// The model queue: how many requests run and wait now, how long they waited
// over time, and when in the week they wait, folded into weekday and
// 15-minute slot in the viewer's timezone. scope "platform" reads the
// administration endpoints (every organisation, or the one in org).
// The overview tab lists every model endpoint with the charts of all of
// them; each endpoint has a tab of its own (?model=<name>) with its peak
// hours and its charts only.
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
// "Running now" and "Waiting now" follow the services' 15-second reports
// while the tab is visible, without the "refreshing…" note each time, and
// catch up as soon as the tab is shown again.
const LIVE_MS = 15_000
const live = ref(false)
const updated = ref<Date | null>(null)
// ready: the first answer (or error) is in, so the charts know which
// provider the link asked for.
const ready = ref(false)
async function load(background = false) {
  live.value = background
  await execute()
  live.value = false
  ready.value = true
  if (!error.value) updated.value = new Date()
}
const refreshIfVisible = () => { if (document.visibilityState === 'visible' && !pending.value) load(true) }
let timer: ReturnType<typeof setInterval> | undefined
onMounted(() => {
  try { tz.value = Intl.DateTimeFormat().resolvedOptions().timeZone ?? '' } catch { tz.value = '' }
  load()
  timer = setInterval(refreshIfVisible, LIVE_MS)
  document.addEventListener('visibilitychange', refreshIfVisible)
})
onBeforeUnmount(() => {
  clearInterval(timer)
  document.removeEventListener('visibilitychange', refreshIfVisible)
})
const updatedAt = computed(() => updated.value?.toLocaleTimeString() ?? '')

const status = computed(() => (error.value as { statusCode?: number } | null)?.statusCode ?? 0)
const metricsOff = computed(() => status.value === 409)

// Tabs: "" is the overview, then one per provider in the answer's order
// (busiest first). The selected one lives in ?model= so it can be linked.
const route = useRoute()
const router = useRouter()
const providers = computed<QueueProvider[]>(() => data.value?.providers ?? [])
const names = computed(() => providers.value.map((p) => p.name))
const tab = computed(() => queueTabOf(route.query.model, names.value))
const current = computed(() => providers.value.find((p) => p.name === tab.value) ?? null)
const tabIds = computed(() => ['', ...names.value])
function pick(name: string) {
  if (name === tab.value) return
  router.replace({ query: { ...route.query, model: name || undefined } })
}
const uid = useId()
const tabId = (name: string) => `${uid}-tab-${tabIds.value.indexOf(name)}`
function onTabKey(e: KeyboardEvent) {
  const next = queueTabKey(tabIds.value, tab.value, e.key)
  if (next == null) return
  e.preventDefault()
  pick(next)
  nextTick(() => document.getElementById(tabId(next))?.focus())
}

// Time series keep 30 days at most: twelve weeks of peak hours, the last
// thirty days of charts. A provider the configuration no longer has
// cannot be charted on its own.
const range = computed<SeriesRange>(() => (weeks.value === 1 ? '7d' : '30d'))
const chartProvider = computed(() => (current.value?.configured ? current.value.name : undefined))

const num = (v: number | null | undefined) => (v == null ? '–' : String(Math.round(v)))
const share = (v: number | null | undefined) => (v == null ? '–' : `${Math.round(v * 100)}%`)
function slotsLabel(p: QueueProvider): string {
  if (props.scope === 'platform' && p.capacity != null) return `${Math.round(p.capacity)} slots over every process`
  if (p.slots > 0) return `${p.slots} slot${p.slots === 1 ? '' : 's'} per process`
  return 'no limit configured here'
}
function slotsShort(p: QueueProvider): string {
  if (props.scope === 'platform' && p.capacity != null) return String(Math.round(p.capacity))
  return p.slots > 0 ? String(p.slots) : '∞'
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
const BADGE: Record<string, string> = { unknown: 'badge--violet', fine: 'badge--green', noticeable: 'badge--amber', add_endpoint: 'badge--pink' }
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
      <span v-if="pending && !live" class="muted small">refreshing…</span>
      <span v-else-if="updatedAt" class="muted small" aria-live="off">Live: updated {{ updatedAt }}, every 15 seconds</span>
    </div>

    <div v-if="metricsOff" class="empty"><strong>Time series are not switched on</strong>They need VictoriaMetrics: the compose stack runs it, other installs set <code>CODESYNAPSE_METRICS_URL</code> on the API and MCP services.</div>
    <template v-else>
      <div v-if="providers.length" class="tabs" role="tablist" aria-label="Model endpoints" @keydown="onTabKey">
        <button v-for="name in tabIds" :id="tabId(name)" :key="`t:${name}`" type="button" role="tab" :aria-selected="tab === name" :aria-controls="`${uid}-panel`" :tabindex="tab === name ? 0 : -1" @click="pick(name)">{{ name || 'Overview' }}</button>
      </div>

      <div :id="`${uid}-panel`" class="stack" :role="providers.length ? 'tabpanel' : undefined" :aria-labelledby="providers.length ? tabId(tab) : undefined">
        <DataState :error="error" :pending="pending && !data" :empty="!providers.length" title="No model queue yet" text="No model endpoint with a request limit has answered requests in this period.">
          <!-- Overview: every endpoint at a glance. -->
          <div v-if="!current" class="table-wrap">
            <table>
              <caption class="sr-only">Model endpoints in this period; pick one to see its peak hours and charts.</caption>
              <thead>
                <tr>
                  <th scope="col">Model endpoint</th>
                  <th scope="col" class="num">Running / slots</th>
                  <th scope="col" class="num">Waiting</th>
                  <th scope="col" class="num">p95 wait</th>
                  <th scope="col" class="num">Requests</th>
                  <th scope="col" class="num">Found it full</th>
                  <th scope="col">Capacity <InfoTip :text="verdictTip" label="About capacity" /></th>
                </tr>
              </thead>
              <tbody>
                <tr v-for="p in providers" :key="p.name" class="pick" @click="pick(p.name)">
                  <td>
                    <button type="button" class="link" @click.stop="pick(p.name)">{{ p.name }}</button>
                    <span v-if="p.models.length" class="models muted mono">{{ p.models.join(', ') }}</span>
                    <span v-else-if="!p.configured" class="models muted">no longer configured</span>
                  </td>
                  <td class="num" :title="slotsLabel(p)">{{ num(p.running) }} / {{ slotsShort(p) }}</td>
                  <td class="num">{{ num(p.waiting) }}</td>
                  <td class="num">{{ fmtUnit(p.p95, 'seconds') }}</td>
                  <td class="num">{{ fmtInt(p.requests) }}</td>
                  <td class="num">{{ share(p.share) }}</td>
                  <td><span class="badge" :class="BADGE[p.verdict.level]">{{ LEVEL[p.verdict.level] ?? p.verdict.level }}</span></td>
                </tr>
              </tbody>
            </table>
          </div>

          <!-- One endpoint. -->
          <template v-else>
            <div class="row head">
              <h3>{{ current.name }}</h3>
              <span v-for="m in current.models" :key="m" class="pill mono">{{ m }}</span>
            </div>
            <div class="grid cols-3">
              <StatTile label="Running now" :value="`${num(current.running)}`" :hint="slotsLabel(current)" tip="Requests holding a slot at the latest report; the services report every 15 seconds." />
              <StatTile label="Waiting now" :value="num(current.waiting)" hint="requests in line for a slot" />
              <StatTile :label="LEVEL[current.verdict.level] ?? 'Capacity'" :value="current.verdict.level === 'add_endpoint' ? 'Add one' : current.verdict.level === 'noticeable' ? 'Watch' : current.verdict.level === 'fine' ? 'Enough' : '–'" :hint="current.verdict.text" :tip="verdictTip" :tone="current.verdict.level === 'add_endpoint' ? 'warning' : undefined" />
            </div>
            <p class="muted small">
              In the period: {{ fmtInt(current.requests) }} requests, {{ fmtUnit(current.p95, 'seconds') }} wait at p95, {{ share(current.share) }} found every slot taken.
            </p>
            <div class="row filters">
              <span :id="`${uid}-metric`" class="label">Peak hours by</span>
              <div class="segmented" role="group" :aria-labelledby="`${uid}-metric`">
                <button v-for="m in METRICS" :key="m.id" type="button" :aria-pressed="metric === m.id" @click="metric = m.id">{{ m.label }}</button>
              </div>
            </div>
            <QueueHeatmap :cells="current.cells" :metric="metric" :timezone="data!.timezone" :wait-noticeable="data!.thresholds.wait_noticeable_seconds" :wait-high="data!.thresholds.wait_high_seconds" />
          </template>
        </DataState>

        <template v-if="ready">
          <h3 class="charts-title">{{ current ? `${current.name} over time` : 'Every model endpoint over time' }}</h3>
          <p v-if="current && !current.configured" class="muted small">{{ current.name }} is no longer in the model configuration, so its charts cannot be shown on their own; the overview charts every endpoint.</p>
          <QueueCharts v-else :scope="scope" :org="org" :provider="chartProvider" :range="range" />
          <p v-if="weeks === 12" class="muted small">The charts show the last 30 days; the peak hours cover all twelve weeks.</p>
        </template>
      </div>
    </template>
  </section>
</template>

<style scoped>
.filters { gap: 8px; }
.small { font-size: 12.5px; }
h3 { margin: 0; font-size: 15px; }
.head { gap: 8px; align-items: center; }
.charts-title { margin-top: 8px; }
.models { display: block; font-size: 12px; margin-top: 2px; overflow-wrap: anywhere; }
tr.pick { cursor: pointer; }
</style>
