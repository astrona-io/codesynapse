<script setup lang="ts">
import type { GatewaySeriesPanel, ModelStats, SeriesRange } from '~/utils/types'
import { useStatistics } from '~/composables/useStatistics'

// The agent gateway on a page of its own: the request tiles come from the
// daily figures (the window chips), the charts from the metrics store, which
// keeps 30 days. Values over time are lines; only the breakdown of why the
// self-hosted model did not answer, a set of categories, is bars.
const { days } = useStatistics()
const { data: m, error: modelsError } = useApiGet<ModelStats>(() => `stats/models?days=${days.value}`)
const gw = computed(() => m.value?.gateway)
const tiers = computed(() => m.value?.tiers)
// The gateway's self-hosted model by its configured name, never an address.
const selfHosted = computed(() => [gw.value?.local_provider, gw.value?.local_model].filter(Boolean).join('/') || 'the self-hosted model')

const range = computed<SeriesRange>(() => seriesRangeFor(days.value) as SeriesRange)
const PANELS: GatewaySeriesPanel[] = ['gateway_local_share', 'gateway_routes', 'gateway_fallthrough', 'gateway_latency_local', 'gateway_latency_cloud', 'gateway_latency_avg', 'gateway_queue_wait', 'gateway_timeouts']
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

// Why the self-hosted model did not answer: whatever reason codes the gateway
// records, named in plain words (utils/gatewayReasons), new codes included.
const reasonRows = computed(() => gatewayReasonRows(gw.value?.fallthrough_reasons))
const gwReasonText = (n: number) => reasonRows.value.slice(0, n).map((r) => `${fmtCompact(r.value)} ${r.label}`).join(' · ')
// Failed requests by route and status class: cloud ones are the provider's
// own answers passed through, not the self-hosted model.
const FAIL_LABEL: Record<string, string> = {
  cloud_4xx: 'cloud 4xx', cloud_5xx: 'cloud 5xx', local_4xx: 'self-hosted 4xx', local_5xx: 'self-hosted 5xx', rejected_4xx: 'refused', rejected_5xx: 'gateway 5xx',
}
const failText = computed(() => Object.entries(gw.value?.failed_by ?? {}).sort((p, q) => q[1] - p[1]).map(([k, v]) => `${fmtCompact(v)} ${FAIL_LABEL[k] ?? k}`).join(' · '))
const secs = (v: number | null | undefined) => (v == null ? '–' : v < 10 ? `${v.toFixed(1)} s` : `${Math.round(v)} s`)
const chartWindow = computed(() => `${seriesRangeLabel(range.value)}${days.value > 30 ? ' (the metrics store keeps 30 days)' : ''}`)
</script>

<template>
  <div class="stack">
    <section class="panel">
      <p class="eyebrow">Gateway</p><h2>Model requests agents sent through CodeSynapse</h2>
      <p class="muted small">Requests in {{ windowLabel(days) }}: how many the CodeSynapse model (self-hosted: the installation's own model, which may run on another host than CodeSynapse, never your machine) answered, how often it was too slow, and how long answers took. Prompts are never recorded. What its answers saved is part of the total under <NuxtLink to="/stats/models">Models &amp; savings</NuxtLink>.</p>
      <p v-if="modelsError" class="muted small">Gateway figures need live metrics: {{ apiErrorMessage(modelsError) }}</p>
      <div v-else-if="gw && gw.requests > 0" class="grid kpis">
        <StatTile label="Gateway requests" :value="fmtCompact(gw.requests)" :hint="`${fmtCompact(gw.local)} self-hosted · ${fmtCompact(gw.cloud)} cloud · ${fmtCompact(gw.rejected)} refused`" />
        <StatTile
          v-if="gw.failed" label="Failed requests" :value="fmtCompact(gw.failed)"
          :hint="failText || 'answered 4xx or 5xx'"
          tip="requests answered with a 4xx or 5xx status. Cloud ones are the provider's own answer passed on unchanged (a rate limit, an unknown model, an overloaded provider), not the self-hosted model; its timeouts and errors go to the cloud instead and are counted under Not answered by the self-hosted model"
        />
        <StatTile label="Answered self-hosted" :value="`${gw.local_share_pct}%`" :hint="`of gateway requests, by ${selfHosted}`" />
        <StatTile
          v-if="gw.timeouts !== undefined" label="Self-hosted model timeouts" :value="fmtCompact(gw.timeouts)"
          :tone="gw.timeouts ? 'warning' : undefined"
          :hint="`${gw.timeout_share_pct ?? 0}% of requests · ${fmtCompact(gw.fallthrough_reasons?.queue_timeout ?? 0)} queue · ${fmtCompact(gw.fallthrough_reasons?.answer_timeout ?? 0)} answer`"
          tip="requests that went to the cloud because the self-hosted model was too slow: they waited too long for its queue slot (queue) or for its answer (answer); a share above a few percent means the self-hosted model is overloaded or too slow for its traffic"
        />
        <StatTile
          v-if="gw.fallthroughs !== undefined" label="Not answered self-hosted" :value="fmtCompact(gw.fallthroughs)"
          :hint="gwReasonText(2)"
          :tip="gw.fallthroughs ? `by reason: ${gwReasonText(reasonRows.length)}. Requests it does not take (too large, thinking, images, not eligible) are expected; timeouts and errors are the self-hosted model's own` : 'the self-hosted model answered every request offered to it'"
        />
        <StatTile
          v-if="gw.local_avg_seconds !== undefined" label="Response time, self-hosted" :value="secs(gw.local_avg_seconds)"
          :hint="`p95 ${secs(gw.local_p95_seconds)}`"
          tip="average time from request to last byte for requests the self-hosted model answered, and the 95th percentile"
        />
        <StatTile
          v-if="gw.cloud_avg_seconds !== undefined" label="Response time, cloud" :value="secs(gw.cloud_avg_seconds)"
          :hint="`p95 ${secs(gw.cloud_p95_seconds)}`"
          tip="average time from request to last byte for requests the cloud answered, including any time first spent on the self-hosted model, and the 95th percentile"
        />
        <StatTile
          v-if="gw.queue_avg_seconds !== undefined" label="Queue wait, self-hosted" :value="secs(gw.queue_avg_seconds)"
          :hint="`p95 ${secs(gw.queue_p95_seconds)}`"
          :tip="`time requests waited for the self-hosted model's slot before it started${gw.local_answer_avg_seconds != null ? `; its answers then took ${secs(gw.local_answer_avg_seconds)} on average` : ''}`"
        />
        <StatTile label="Bytes in / out" :value="`${fmtCompact(gw.bytes_in)} / ${fmtCompact(gw.bytes_out)}`" tip="request and response sizes through the gateway" />
      </div>
      <p v-else-if="m" class="muted small">No request went through the gateway in this window.</p>
    </section>

    <section v-if="tiers" class="panel">
      <p class="eyebrow">Where work ran</p><h2>Your machine, the CodeSynapse model or the cloud</h2>
      <p class="muted small">Work in {{ windowLabel(days) }}, gateway and CodeSynapse's own model requests together.</p>
      <div class="grid kpis">
        <StatTile label="Your machine" :value="fmtCompact(tiers.machine)" hint="answers with no model" tip="calls the Claude Code bridge answered on the developer's machine without any model: git and gh tools, read_ranges, code graph answers for unsynced changes and fetched pages" />
        <StatTile label="CodeSynapse model (self-hosted)" :value="fmtCompact(tiers.self_hosted)" :hint="tiers.self_hosted_models.join(' · ') || 'no model requests'" tip="requests the installation's own models answered (providers configured as local, which may run on another host): gateway answers, ask_local, local_draft, briefings, page extracts, prompt shortening and AI tasks routed there" />
        <StatTile label="Cloud" :value="fmtCompact(tiers.cloud)" hint="model requests" tip="gateway requests passed on to the cloud and CodeSynapse's AI tasks on any other provider" />
      </div>
    </section>

    <section v-if="gw && gw.turns" class="panel">
      <p class="eyebrow">Over a conversation</p><h2>Does the cloud agent's input grow?</h2>
      <p class="muted small">Requests in {{ windowLabel(days) }}, grouped by how many messages they carried: a conversation grows by about two messages per tool call and shrinks when the agent compacts it. The cloud input counts cached tokens too; the cache share is what the cloud's prompt cache served. Answers on your machine are not tied to a request, so they are only in the tiles above.</p>
      <GatewayTurns :turns="gw.turns" />
    </section>

    <div v-if="metricsOff" class="empty"><strong>Time series are not switched on</strong>They need VictoriaMetrics: the compose stack runs it, other installs set <code>CODESYNAPSE_METRICS_URL</code> (and <code>CODESYNAPSE_METRICS_PASSWORD</code>) on the API and MCP services.</div>
    <template v-else>
      <section class="panel">
        <p class="eyebrow">Self-hosted or cloud</p><h2>Which route took the requests, over time</h2>
        <p class="muted small">{{ chartWindow }}. The share the self-hosted model answered per step, and the requests each route took.</p>
        <div class="grid cols-2">
          <SeriesCard v-if="show('gateway_local_share')" panel="gateway_local_share" :range="range" :batched="batch.gateway_local_share ?? null" :batch-pending="batchPending" @unavailable="(st) => unavailable('gateway_local_share', st)" />
          <SeriesCard v-if="show('gateway_routes')" panel="gateway_routes" :range="range" :batched="batch.gateway_routes ?? null" :batch-pending="batchPending" :label="gatewayRouteLabel" @unavailable="(st) => unavailable('gateway_routes', st)" />
        </div>
      </section>

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
        <p class="eyebrow">Self-hosted model</p><h2>Waiting, timeouts and why requests went to the cloud</h2>
        <p class="muted small">{{ chartWindow }}. A request waits for the self-hosted model's queue slot, then for its answer; too long at either step and it goes to the cloud.</p>
        <div class="grid cols-2">
          <SeriesCard v-if="show('gateway_queue_wait')" panel="gateway_queue_wait" :range="range" :batched="batch.gateway_queue_wait ?? null" :batch-pending="batchPending" @unavailable="(st) => unavailable('gateway_queue_wait', st)" />
          <SeriesCard v-if="show('gateway_timeouts')" panel="gateway_timeouts" :range="range" :batched="batch.gateway_timeouts ?? null" :batch-pending="batchPending" @unavailable="(st) => unavailable('gateway_timeouts', st)" />
        </div>
        <h3>Not answered self-hosted, by reason</h3>
        <p class="muted small">Requests in {{ windowLabel(days) }}. Requests it does not take (too large, thinking, images, not eligible) are expected; timeouts and errors are the self-hosted model's own.</p>
        <RankList :rows="reasonRows" color="var(--s-1)" empty="The self-hosted model answered every request offered to it." />
        <p v-if="gw?.cloud_without_reason" class="muted small">{{ fmtCompact(gw.cloud_without_reason) }} cloud {{ gw.cloud_without_reason === 1 ? 'request has' : 'requests have' }} no recorded reason: the gateway sent {{ gw.cloud_without_reason === 1 ? 'it' : 'them' }} to the cloud without offering {{ gw.cloud_without_reason === 1 ? 'it' : 'them' }} to the self-hosted model.</p>
        <SeriesCard v-if="show('gateway_fallthrough')" panel="gateway_fallthrough" :range="range" :batched="batch.gateway_fallthrough ?? null" :batch-pending="batchPending" :label="gatewayReasonLabel" @unavailable="(st) => unavailable('gateway_fallthrough', st)" />
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
