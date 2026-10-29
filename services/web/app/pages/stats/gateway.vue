<script setup lang="ts">
import type { GatewaySeriesPanel, ModelStats, SeriesRange } from '~/utils/types'
import { useStatistics } from '~/composables/useStatistics'

// The agent gateway on a page of its own: the request tiles come from the
// daily figures (the window chips), the charts from the metrics store, which
// keeps 30 days. Values over time are lines; only the breakdown of why the
// local model did not answer, a set of categories, is bars.
const { days } = useStatistics()
const { data: m, error: modelsError } = useApiGet<ModelStats>(() => `stats/models?days=${days.value}`)
const gw = computed(() => m.value?.gateway)

const range = computed<SeriesRange>(() => seriesRangeFor(days.value) as SeriesRange)
const PANELS: GatewaySeriesPanel[] = ['gateway_latency_local', 'gateway_latency_cloud', 'gateway_latency_avg', 'gateway_queue_wait', 'gateway_timeouts']
const { panels: batch, pending: batchPending } = useSeriesBatch(PANELS, range)
const metricsOff = ref(false)
const hiddenPanels = ref<string[]>([])
function unavailable(panel: string, status: number) {
  if (status === 409) metricsOff.value = true
  else if (!hiddenPanels.value.includes(panel)) hiddenPanels.value.push(panel)
}
const show = (p: GatewaySeriesPanel) => !hiddenPanels.value.includes(p)

// Local and cloud share one y scale by default, so the two charts compare
// at a glance; "own scale" spreads each to its own peak.
const sharedScale = ref(true)
const quantileMax = computed(() => {
  if (!sharedScale.value) return 0
  const vals = (['gateway_latency_local', 'gateway_latency_cloud'] as const)
    .flatMap((p) => batch.value[p]?.data?.series.flatMap((s) => s.values) ?? [])
  return Math.max(0, ...vals.map((v) => v ?? 0))
})

// Why the local model did not answer, in the gateway's fixed reasons.
const FALL_LABEL: Record<string, string> = {
  queue_timeout: 'waited too long for a slot', answer_timeout: 'answered too slowly', cancelled: 'client gave up',
  model_error: 'model error', bad_status: 'error status', empty_answer: 'empty answer', truncated: 'cut off', bad_answer: 'unreadable answer',
  not_eligible: 'not eligible', too_large: 'too large', has_thinking: 'needs thinking', has_image: 'has an image or document',
  no_local_model: 'no local model', excluded: 'excluded', other: 'other',
}
const gwReasons = computed(() => Object.entries(gw.value?.fallthrough_reasons ?? {}).sort((p, q) => q[1] - p[1]))
const gwReasonText = (n: number) => gwReasons.value.slice(0, n).map(([k, v]) => `${fmtCompact(v)} ${FALL_LABEL[k] ?? k}`).join(' · ')
const reasonRows = computed(() => gwReasons.value.map(([k, v]) => ({ label: FALL_LABEL[k] ?? k, value: v, detail: k })))
const secs = (v: number | null | undefined) => (v == null ? '–' : v < 10 ? `${v.toFixed(1)} s` : `${Math.round(v)} s`)
const chartWindow = computed(() => `${seriesRangeLabel(range.value)}${days.value > 30 ? ' (the metrics store keeps 30 days)' : ''}`)
</script>

<template>
  <div class="stack">
    <section class="panel">
      <p class="eyebrow">Gateway</p><h2>Model requests agents sent through CodeSynapse</h2>
      <p class="muted small">Requests in {{ windowLabel(days) }}: how many the local model answered, how often it was too slow, and how long answers took. Prompts are never recorded. What local answers saved is part of the total under <NuxtLink to="/stats/models">Models &amp; savings</NuxtLink>.</p>
      <p v-if="modelsError" class="muted small">Gateway figures need live metrics: {{ apiErrorMessage(modelsError) }}</p>
      <div v-else-if="gw && gw.requests > 0" class="grid kpis">
        <StatTile label="Gateway requests" :value="fmtCompact(gw.requests)" :hint="`${fmtCompact(gw.local)} local · ${fmtCompact(gw.cloud)} cloud · ${fmtCompact(gw.rejected)} refused${gw.failed ? ` · ${fmtCompact(gw.failed)} failed` : ''}`" />
        <StatTile label="Answered locally" :value="`${gw.local_share_pct}%`" hint="of gateway requests, by the local model" />
        <StatTile
          v-if="gw.timeouts !== undefined" label="Local model timeouts" :value="fmtCompact(gw.timeouts)"
          :tone="gw.timeouts ? 'warning' : undefined"
          :hint="`${gw.timeout_share_pct ?? 0}% of requests · ${fmtCompact(gw.fallthrough_reasons?.queue_timeout ?? 0)} queue · ${fmtCompact(gw.fallthrough_reasons?.answer_timeout ?? 0)} answer`"
          tip="requests that went to the cloud because the local model was too slow: they waited too long for its queue slot (queue) or for its answer (answer); a share above a few percent means the local model is overloaded or too slow for its traffic"
        />
        <StatTile
          v-if="gw.fallthroughs !== undefined" label="Not answered locally" :value="fmtCompact(gw.fallthroughs)"
          :hint="gwReasonText(2)"
          :tip="gw.fallthroughs ? `by reason: ${gwReasonText(gwReasons.length)}. Requests it does not take (too large, thinking, images, not eligible) are expected; timeouts and errors are the local model's own` : 'the local model answered every request offered to it'"
        />
        <StatTile
          v-if="gw.local_avg_seconds !== undefined" label="Response time, local" :value="secs(gw.local_avg_seconds)"
          :hint="`p95 ${secs(gw.local_p95_seconds)}`"
          tip="average time from request to last byte for requests the local model answered, and the 95th percentile"
        />
        <StatTile
          v-if="gw.cloud_avg_seconds !== undefined" label="Response time, cloud" :value="secs(gw.cloud_avg_seconds)"
          :hint="`p95 ${secs(gw.cloud_p95_seconds)}`"
          tip="average time from request to last byte for requests the cloud answered, including any time first spent on the local model, and the 95th percentile"
        />
        <StatTile
          v-if="gw.queue_avg_seconds !== undefined" label="Queue wait, local" :value="secs(gw.queue_avg_seconds)"
          :hint="`p95 ${secs(gw.queue_p95_seconds)}`"
          :tip="`time requests waited for the local model's slot before it started${gw.local_answer_avg_seconds != null ? `; its answers then took ${secs(gw.local_answer_avg_seconds)} on average` : ''}`"
        />
        <StatTile label="Bytes in / out" :value="`${fmtCompact(gw.bytes_in)} / ${fmtCompact(gw.bytes_out)}`" tip="request and response sizes through the gateway" />
      </div>
      <p v-else-if="m" class="muted small">No request went through the gateway in this window.</p>
    </section>

    <div v-if="metricsOff" class="empty"><strong>Time series are not switched on</strong>They need VictoriaMetrics: the compose stack runs it, other installs set <code>CODESYNAPSE_METRICS_URL</code> (and <code>CODESYNAPSE_METRICS_PASSWORD</code>) on the API and MCP services.</div>
    <template v-else>
      <section class="panel">
        <div class="section-head">
          <div>
            <p class="eyebrow">Response time</p><h2>How long answers took</h2>
          </div>
          <div class="segmented" role="group" aria-label="Y axis">
            <button type="button" :aria-pressed="sharedScale" @click="sharedScale = true">Same scale</button>
            <button type="button" :aria-pressed="!sharedScale" @click="sharedScale = false">Own scale</button>
          </div>
        </div>
        <p class="muted small">{{ chartWindow }}. p50 is the typical request, p99 the slowest in a hundred; a gap between them means a few requests take much longer than most. {{ sharedScale ? 'Local and cloud share one y axis, so the route whose lines sit lower is the faster one.' : 'Each chart has its own y axis: read the values, not the heights, to compare them.' }} The quantiles are estimates from histogram buckets: a value is only as precise as the bucket it falls in.</p>
        <div class="grid cols-2">
          <SeriesCard v-if="show('gateway_latency_local')" panel="gateway_latency_local" :range="range" :batched="batch.gateway_latency_local ?? null" :batch-pending="batchPending" :y-max="quantileMax" @unavailable="(st) => unavailable('gateway_latency_local', st)" />
          <SeriesCard v-if="show('gateway_latency_cloud')" panel="gateway_latency_cloud" :range="range" :batched="batch.gateway_latency_cloud ?? null" :batch-pending="batchPending" :y-max="quantileMax" @unavailable="(st) => unavailable('gateway_latency_cloud', st)" />
          <SeriesCard v-if="show('gateway_latency_avg')" panel="gateway_latency_avg" :range="range" :batched="batch.gateway_latency_avg ?? null" :batch-pending="batchPending" @unavailable="(st) => unavailable('gateway_latency_avg', st)" />
        </div>
      </section>

      <section class="panel">
        <p class="eyebrow">Local model</p><h2>Waiting, timeouts and why requests went to the cloud</h2>
        <p class="muted small">{{ chartWindow }}. A request waits for the local model's queue slot, then for its answer; too long at either step and it goes to the cloud.</p>
        <div class="grid cols-2">
          <SeriesCard v-if="show('gateway_queue_wait')" panel="gateway_queue_wait" :range="range" :batched="batch.gateway_queue_wait ?? null" :batch-pending="batchPending" @unavailable="(st) => unavailable('gateway_queue_wait', st)" />
          <SeriesCard v-if="show('gateway_timeouts')" panel="gateway_timeouts" :range="range" :batched="batch.gateway_timeouts ?? null" :batch-pending="batchPending" @unavailable="(st) => unavailable('gateway_timeouts', st)" />
        </div>
        <h3>Not answered locally, by reason</h3>
        <p class="muted small">Requests in {{ windowLabel(days) }}. Requests it does not take (too large, thinking, images, not eligible) are expected; timeouts and errors are the local model's own.</p>
        <RankList :rows="reasonRows" color="var(--s-1)" empty="The local model answered every request offered to it." />
      </section>
    </template>
  </div>
</template>

<style scoped>
.kpis { grid-template-columns: repeat(auto-fit, minmax(170px, 1fr)); }
.section-head { display: flex; justify-content: space-between; align-items: flex-start; gap: 12px; flex-wrap: wrap; }
h3 { font-size: 14px; margin: 18px 0 4px; }
.small { font-size: 12.5px; }
</style>
