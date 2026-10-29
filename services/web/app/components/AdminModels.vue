<script setup lang="ts">
import type { ModelFigures, ModelsSection, SeriesRange, StatsRange } from '~/utils/types'

// The models tab of the platform statistics: CodeSynapse's own model calls
// across organisations, the local and cloud split and what they cost. The
// figures come from live statistics, which keep 30 days, so 90 days and 1
// year show the last 30. A cost nobody priced reads "unknown", never $0,
// local models have no cost, and the provider's prompt-cache saving stays
// apart from CodeSynapse's savings. Models the configuration does not name
// are "other".
const props = defineProps<{ range: StatsRange; org: string; tick: number }>()

const live = computed(() => props.range !== '90d' && props.range !== '365d')
const win = computed<SeriesRange>(() => (live.value ? props.range as SeriesRange : '30d'))
const sort = ref('name')
const page = ref(1)
watch([() => props.range, () => props.org, sort], () => { page.value = 1 })
const filter = computed(() => `range=${win.value}${props.org ? `&org=${encodeURIComponent(props.org)}` : ''}`)
const { data, error, pending, refresh } = useApiGet<ModelsSection>(() => `admin/stats/models?${filter.value}&sort=${sort.value}&page=${page.value}`)
watch(() => props.tick, () => refresh())
const exportHref = (format: 'csv' | 'json') => apiPath(`admin/stats/export?section=models&${filter.value}&format=${format}`)

const basisLabel: Record<string, string> = {
  none: 'no calls', local: 'local models have no cost', estimate: 'from the configured prices',
  partial: 'some calls without a price left out', unknown: 'no call had a price',
}
function cost(f: ModelFigures): string {
  if (f.cost_basis === 'local') return 'No cost'
  if (f.cost_basis === 'none') return '–'
  if (f.cost_dollars === null) return 'Unknown'
  return fmtUnit(f.cost_dollars, 'USD')
}
const unknownCost = (f: ModelFigures) => f.unknown_usage_calls + f.pricing_missing_calls
const costHint = computed(() => {
  const t = data.value?.totals
  if (!t) return ''
  const n = unknownCost(t)
  return n ? `${basisLabel[t.cost_basis]} · ${fmtCompact(n)} calls with unknown cost` : basisLabel[t.cost_basis]
})
const costTip = 'Estimated from the token counts providers report and the prices in the provider configuration. '
  + 'Calls without reported usage or without a configured price are counted, not guessed: the cost reads unknown when no call had a price.'
const pct = (v: number | null) => (v === null ? '–' : `${v}%`)
const secs = (v: number | null) => fmtUnit(v, 'seconds')
const kindBadge: Record<string, string> = { local: 'badge--green', cloud: 'badge--cyan', unknown: 'badge--violet' }
</script>

<template>
  <div class="stack">
    <div class="row head">
      <p class="muted small grow">
        From live statistics<template v-if="!live">, which keep 30 days: the last 30 days</template>.
        Models the configuration does not name, and those past the ten busiest, are counted as other.
      </p>
      <select v-model="sort" class="inline" aria-label="Sort organisations">
        <option value="name">By name</option><option value="requests">Most calls</option><option value="failures">Most failures</option>
        <option value="tokens">Most tokens</option><option value="cost">Highest cost</option><option value="local">Most local</option>
      </select>
      <a :href="exportHref('csv')" download class="chip">CSV</a>
      <a :href="exportHref('json')" download class="chip">JSON</a>
    </div>
    <DataState :pending="pending" :error="error" :empty="!data" title="No model figures">
      <template v-if="data">
        <div class="grid cols-4">
          <StatTile label="Model calls" :value="fmtCompact(data.totals.requests)" :hint="`${fmtCompact(data.totals.failures)} failed`" />
          <StatTile label="Estimated cloud cost" :value="cost(data.totals)" :hint="costHint" :tip="costTip"
            :tone="data.totals.cost_basis === 'unknown' || data.totals.cost_basis === 'partial' ? 'warning' : undefined" />
          <StatTile label="Answered locally" :value="pct(data.totals.local_share_pct)"
            :hint="`${fmtCompact(data.totals.local_requests)} local · ${fmtCompact(data.totals.cloud_requests)} cloud calls`"
            tip="The share of model calls a local model answered. Calls of a provider this installation no longer configures count as neither." />
          <StatTile label="Sent to cloud models" :value="fmtUnit(data.totals.cloud_bytes, 'bytes')" hint="request bodies, retries included" />
        </div>
        <p class="muted small">
          Tokens: {{ fmtCompact(data.totals.input_tokens) }} in ({{ fmtCompact(data.totals.cached_tokens) }} from the provider's cache) ·
          {{ fmtCompact(data.totals.output_tokens) }} out · {{ fmtCompact(data.totals.local_tokens) }} local, {{ fmtCompact(data.totals.cloud_tokens) }} cloud ·
          calls with unknown cost: {{ fmtCompact(data.totals.unknown_usage_calls) }} without reported usage, {{ fmtCompact(data.totals.pricing_missing_calls) }} without a price ·
          memory: {{ fmtCompact(data.totals.embedded_entities) }} entities embedded, {{ fmtCompact(data.totals.offload_turns) }} turns handed to extraction
        </p>
        <p v-if="data.totals.provider_cache_saved_dollars !== null" class="muted small">
          The providers' prompt caches took {{ fmtUnit(data.totals.provider_cache_saved_dollars, 'USD') }} off these calls. That is the provider's discount, not a CodeSynapse saving, and it is not part of any savings figure.
        </p>

        <h3>Models</h3>
        <div class="table-wrap">
          <table>
            <thead>
              <tr>
                <th>Model</th><th class="num">Calls</th><th class="num">Failed</th><th class="num">Tokens in</th><th class="num">Tokens out</th>
                <th class="num">Cost</th><th class="num">Unknown cost</th><th class="num">First token p95</th><th class="num">Request p95</th><th>Prices</th>
              </tr>
            </thead>
            <tbody>
              <tr v-for="m in data.models" :key="`${m.provider}/${m.model}/${m.kind}`">
                <td>
                  <span class="mono small">{{ m.provider }} · {{ m.model }}</span>
                  <span class="badge" :class="kindBadge[m.kind]">{{ m.kind }}</span>
                  <div v-if="m.tasks.length" class="muted small">{{ m.tasks.map((t) => `${t.task} ${fmtCompact(t.requests)}`).join(' · ') }}</div>
                </td>
                <td class="num">{{ fmtCompact(m.requests) }}</td>
                <td class="num">{{ fmtCompact(m.failures) }}</td>
                <td class="num">{{ fmtCompact(m.input_tokens) }}</td>
                <td class="num">{{ fmtCompact(m.output_tokens) }}</td>
                <td class="num" :title="basisLabel[m.cost_basis]">{{ cost(m) }}</td>
                <td class="num">{{ m.kind === 'local' ? '–' : fmtCompact(unknownCost(m)) }}</td>
                <td class="num">{{ secs(m.ttft_p95_seconds) }}</td>
                <td class="num">{{ secs(m.request_p95_seconds) }}</td>
                <td class="small">
                  <template v-if="m.kind !== 'cloud' || m.folded">–</template>
                  <template v-else-if="!m.priced">No price configured</template>
                  <template v-else>
                    <a v-if="m.price_source" :href="m.price_source" target="_blank" rel="noopener noreferrer">{{ m.price_feed ? 'Price feed' : 'Price source' }}</a>
                    <span v-else class="muted">Source not named</span>
                    <span v-if="m.price_checked" class="muted"> · {{ m.price_checked }}</span>
                  </template>
                </td>
              </tr>
              <tr v-if="!data.models.length"><td colspan="10" class="muted small">No model calls in this window.</td></tr>
            </tbody>
          </table>
        </div>

        <h3>Organisations</h3>
        <div class="table-wrap">
          <table>
            <thead><tr><th>Organisation</th><th class="num">Calls</th><th class="num">Failed</th><th class="num">Tokens</th><th class="num">Local</th><th class="num">Cost</th><th class="num">Unknown cost</th></tr></thead>
            <tbody>
              <tr v-for="o in data.rows" :key="o.org_id">
                <td>{{ o.name }} <span v-if="!o.deleted" class="muted mono small">{{ o.slug }}</span></td>
                <td class="num">{{ fmtCompact(o.requests) }}</td>
                <td class="num">{{ fmtCompact(o.failures) }}</td>
                <td class="num">{{ fmtCompact(o.input_tokens + o.output_tokens) }}</td>
                <td class="num">{{ pct(o.local_share_pct) }}</td>
                <td class="num" :title="basisLabel[o.cost_basis]">{{ cost(o) }}</td>
                <td class="num">{{ fmtCompact(unknownCost(o)) }}</td>
              </tr>
            </tbody>
          </table>
        </div>
        <p class="muted small">Calls made without an organisation (background work) count in the totals only.</p>
        <div v-if="data.pages > 1" class="row pager">
          <button class="secondary tiny" :disabled="page <= 1" @click="page--">Previous</button>
          <span class="muted small">Page {{ data.page }} of {{ data.pages }} · {{ data.total }} organisations</span>
          <button class="secondary tiny" :disabled="page >= data.pages" @click="page++">Next</button>
        </div>
      </template>
    </DataState>
  </div>
</template>

<style scoped>
.head { gap: 10px; flex-wrap: wrap; align-items: center; }
.grow { flex: 1 1 260px; margin: 0; }
td .badge { margin-left: 6px; }
.pager { gap: 10px; justify-content: flex-end; }
h3 { margin: 6px 0 0; }
</style>
