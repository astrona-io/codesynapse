<script setup lang="ts">
import type { ModelStats, SavingsSeries } from '~/utils/types'
import { useStatistics } from '~/composables/useStatistics'

// What the connected models do and what they save. Agent tokens and tokens
// avoided come from the daily usage counters (the window chips); model work
// comes from the metrics store, which keeps at most 30 days. Both cover the
// same UTC days, so the savings chart adds up to the tiles.
const { days, s, error: statsError } = useStatistics()
const { data: m, error: modelsError } = useApiGet<ModelStats>(() => `stats/models?days=${days.value}`)
const { data: series, error: seriesError, pending: seriesPending } = useApiGet<SavingsSeries>(() => `stats/savings?days=${days.value}`)

const a = computed(() => s.value?.agents)
const models = computed(() => m.value?.models ?? [])
const localModels = computed(() => models.value.filter((x) => x.local))
const sum = (xs: typeof models.value, f: (x: (typeof xs)[number]) => number) => xs.reduce((t, x) => t + f(x), 0)
const localIn = computed(() => sum(localModels.value, (x) => x.input_tokens))
const localOut = computed(() => sum(localModels.value, (x) => x.output_tokens))
const cloudTokens = computed(() => sum(models.value.filter((x) => !x.local), (x) => x.input_tokens + x.output_tokens))
// The graph tools' part of tokens avoided; ask_local's part is the local
// models' reading, counted once under them.
const graphAvoided = computed(() => Math.max(0, (a.value?.avoided_tokens ?? 0) - (a.value?.local_avoided_tokens ?? 0)))
// The agents' own work: fresh input, cache writes and output. Prompt-cache
// reads are left out; CodeSynapse neither causes nor removes them.
const agentTokens = computed(() => a.value?.work_tokens ?? a.value?.tokens.total ?? 0)

const pool = computed(() => [
  { label: 'Cloud agents', value: agentTokens.value, color: 'var(--s-1)', detail: 'fresh input, cache writes and output Claude Code and Codex reported; prompt-cache reads left out' },
  { label: 'Never read (graph answers)', value: graphAvoided.value, color: 'var(--s-2)', detail: 'file tokens the graph tools answered instead' },
  { label: 'Local models', value: localIn.value + localOut.value, color: 'var(--s-3)', detail: 'ask_local, reviews, memory and other AI work on your machine' },
  { label: 'CodeSynapse on cloud models', value: cloudTokens.value, color: 'var(--s-4)', detail: 'AI work routed to a cloud provider' },
])
const poolTotal = computed(() => pool.value.reduce((t, p) => t + p.value, 0))
const offCloud = computed(() => (poolTotal.value ? Math.round((100 * (graphAvoided.value + localIn.value + localOut.value)) / poolTotal.value) : null))

// Valued at the configured cloud model's prices: what the local work and the
// avoided reading would have cost there. An estimate, and only with prices.
const price = computed(() => m.value?.reference_price)
const localCached = computed(() => Math.min(sum(localModels.value, (x) => x.cached_tokens ?? 0), localIn.value))
// The graph answers' part is valued on the server, once, at the price the
// avoided context would have entered at (stats_share.go): total_dollars adds
// the agents that cannot tell repeats apart, at the input price; null is
// unknown and stays out of the total. The provider's own prompt-cache saving
// is never ours.
const graphSaved = computed(() => a.value?.saved?.total_dollars ?? a.value?.saved?.dollars ?? null)
const graphHint = computed(() => {
  const sv = a.value?.saved
  if (graphSaved.value === null || !sv) return 'the reference model has no input price; not in the total'
  const unaware = sv.unaware_graph_tokens ?? 0
  const parts = sv.tokens > 0 || unaware === 0 ? [`${fmtCompact(sv.tokens)} tokens never read, each once at the ${sv.price === 'cache_write' ? 'cache-write' : 'input'} price`] : []
  if (unaware > 0) parts.push(`${fmtCompact(unaware)} tokens never read by agents that cannot tell repeated answers apart${sv.unaware_dollars != null ? ` (${money(sv.unaware_dollars)})` : ''}, at the input price; left out of the share`)
  return parts.join('; ')
})
// Split by what the tokens would have been billed as: fresh input, cache
// reads (at the cache price, else the input price) and output.
const savedParts = computed(() => (price.value ? localSaved(price.value, localIn.value, localCached.value, localOut.value) : null))
// Prompt compression is priced on the server per model; null when a price is missing.
const compressionSaved = computed(() => m.value?.compression_saved_dollars ?? null)
// An API older than compression_enabled leaves it out: treat that as on.
const compressionOn = computed(() => m.value?.compression_enabled ?? true)
const COMPRESSION_OFF: Record<string, string> = {
  disabled: 'compress_prompts is false in config/llm.yaml',
  no_local_model: 'no local model to shorten with',
  no_ai: 'no AI providers configured',
}
const compressionOffHint = computed(() => `${COMPRESSION_OFF[m.value?.compression_off ?? ''] ?? 'not running'}; covers CodeSynapse's own AI tasks, not gateway traffic`)
// Gateway answers the local model gave, priced at the reference model on the server; null when a local answer reported no usage.
const gw = computed(() => m.value?.gateway)
const gwSaved = computed(() => gw.value?.saved_dollars ?? null)
const spent = computed(() => m.value?.spent_dollars ?? null)
const saved = computed(() => totalSaved(price.value, { input: localIn.value, cached: localCached.value, output: localOut.value }, [graphSaved.value, compressionSaved.value, gwSaved.value]))
// Where a price comes from, shown as its host and path.
const sourceLabel = (u: string) => { try { const x = new URL(u); return x.host + x.pathname + (x.hash ? ` (${x.hash.slice(1)})` : '') } catch { return u } }
const money = (v: number) => (v >= 100 ? `$${Math.round(v)}` : `$${v.toFixed(2)}`)

const TASK_LABEL: Record<string, string> = {
  'local-answer': 'Agent questions', 'local-draft': 'Code drafts', review: 'Code review', 'security-triage': 'Security triage', briefing: 'Briefings', fix: 'Fixes',
  memory: 'Memory', embeddings: 'Embeddings', 'code-annotation': 'Annotations', 'pr-summary': 'Change summaries', 'cleanup-briefing': 'Cleanup briefings', other: 'Other',
}
// Three most used tasks keep their colour; the rest fold into Other.
const taskKeys = computed(() => {
  const totals: Record<string, number> = {}
  for (const x of models.value) for (const t of x.tasks) totals[t.task] = (totals[t.task] ?? 0) + t.input_tokens + t.output_tokens
  const top = Object.entries(totals).sort((p, q) => q[1] - p[1]).slice(0, 3).map(([k]) => k).sort()
  const colors = ['var(--s-1)', 'var(--s-2)', 'var(--s-3)']
  const keys = top.map((k, i) => ({ key: k, label: TASK_LABEL[k] ?? k, color: colors[i]! }))
  if (Object.keys(totals).length > top.length) keys.push({ key: '(other)', label: 'Other tasks', color: 'var(--muted)' })
  return keys
})
const taskRows = computed(() => models.value.map((x) => {
  const values: Record<string, number> = {}
  for (const t of x.tasks) {
    const k = taskKeys.value.some((k) => k.key === t.task) ? t.task : '(other)'
    values[k] = (values[k] ?? 0) + t.input_tokens + t.output_tokens
  }
  return { label: x.model, values }
}))

const readSpeed = computed(() => models.value.filter((x) => x.read_tokens_per_second != null)
  .map((x) => ({ label: x.model, value: x.read_tokens_per_second!, detail: `${fmtCompact(x.avg_prompt_tokens)} tokens per prompt on average` }))
  .sort((p, q) => q.value - p.value))
const writeSpeed = computed(() => models.value.filter((x) => x.write_tokens_per_second != null)
  .map((x) => ({ label: x.model, value: x.write_tokens_per_second!, detail: `${fmtCompact(x.output_tokens)} tokens written` }))
  .sort((p, q) => q.value - p.value))
const modelWindow = computed(() => (days.value > 30 ? 'the last 30 days (the metrics store keeps no more)' : windowLabel(days.value)))
</script>

<template>
  <p v-if="statsError" class="panel error">{{ apiErrorMessage(statsError) }}</p>
  <div v-else class="stack">
    <section class="panel">
      <p class="eyebrow">Token pool</p><h2>Where the reading happened</h2>
      <p class="muted small">Every token an agent or CodeSynapse spent, or did not have to spend, in the window. Cloud agents' tokens come from Claude Code and Codex hooks; the rest from CodeSynapse's own counters. Estimates say how they are estimated.</p>
      <h3 class="row-title">Counts</h3>
      <div class="grid kpis">
        <StatTile label="Kept off cloud models" :value="offCloud === null ? '–' : `${offCloud}%`" hint="never read, or read by a local model" />
        <StatTile label="Tokens never read" :value="fmtCompact(a?.avoided_tokens ?? 0)" :hint="`graph answers and ask_local, estimated`" />
        <StatTile label="Handled by local models" :value="fmtCompact(localIn + localOut)" :hint="`${fmtCompact(localIn)} read · ${fmtCompact(localOut)} written`" />
        <StatTile label="Prompts pre-briefed" :value="fmtCompact(a?.local_briefs ?? 0)" hint="a local model briefed the agent first" />
        <StatTile label="Code drafts" :value="fmtCompact(a?.local_drafts ?? 0)" hint="a local model drafted code the agent then checked" />
      </div>
      <h3 class="row-title">Money (estimate)</h3>
      <div class="grid kpis">
        <StatTile
          label="Total saved (est.)" :value="saved === null ? 'unknown' : money(saved)"
          :hint="price ? (price.estimate ? `at ${price.model}, rough estimate` : `at ${price.model} prices${price.checked ? ` of ${price.checked}` : ''}, estimated`) : 'no prices to estimate with: add them in config/llm.yaml'"
        />
        <StatTile label="Spent on cloud models (est.)" :value="spent === null ? 'unknown' : money(spent)" :hint="spent === null ? 'a cloud model has no price set, so its cost is unknown' : 'reported usage at the configured prices'" />
        <StatTile
          v-if="savedParts" label="Saved by shorter prompts"
          :value="!compressionOn ? 'off' : compressionSaved === null ? 'unknown' : money(compressionSaved)"
          :hint="!compressionOn ? compressionOffHint : compressionSaved === null ? 'a cloud model has no price set; not in the total' : 'CodeSynapse\'s own AI tasks: prose a local model shortened before it went to the cloud, at the input price; gateway traffic is passed on unchanged'"
        />
        <StatTile v-if="savedParts" label="Saved by graph answers" :value="graphSaved === null ? 'unknown' : money(graphSaved)" :hint="graphHint" />
        <StatTile v-if="savedParts" label="Saved on input" :value="money(savedParts.input)" :hint="`${fmtCompact(Math.max(0, localIn - localCached))} tokens read by local models instead`" />
        <StatTile
          v-if="savedParts" label="Saved on cached input"
          :value="localCached ? money(savedParts.cached) : 'none reported'"
          :hint="localCached ? `${fmtCompact(localCached)} tokens the local models' prompt cache served, at the cache-read price; agents' own prompt cache is never counted` : 'the local runtime reported no prompt-cache hits (Ollama does not report them); agents\' own prompt cache is never counted'"
        />
        <StatTile v-if="savedParts" label="Saved on output" :value="money(savedParts.output)" :hint="`${fmtCompact(localOut)} tokens written locally`" />
      </div>
      <template v-if="gw && gw.requests > 0">
        <h3 class="row-title">Gateway</h3>
        <div class="grid kpis">
          <StatTile label="Gateway requests" :value="fmtCompact(gw.requests)" :hint="`${fmtCompact(gw.local)} local · ${fmtCompact(gw.cloud)} cloud · ${fmtCompact(gw.rejected)} refused${gw.failed ? ` · ${fmtCompact(gw.failed)} failed` : ''}`" />
          <StatTile label="Answered locally" :value="`${gw.local_share_pct}%`" hint="of gateway requests, by the local model" />
          <StatTile label="Bytes in / out" :value="`${fmtCompact(gw.bytes_in)} / ${fmtCompact(gw.bytes_out)}`" hint="request and response sizes through the gateway" />
          <StatTile label="Saved by local answers" :value="gwSaved === null ? 'unknown' : money(gwSaved)" :hint="gwSaved === null ? `${fmtCompact(gw.local_unmetered)} local answers reported no token usage; not in the total` : `${fmtCompact(gw.local_input_tokens)} read${gw.local_cached_tokens ? ` (${fmtCompact(gw.local_cached_tokens)} from its prompt cache)` : ''} · ${fmtCompact(gw.local_output_tokens)} written locally, at the reference prices`" />
        </div>
      </template>
      <DonutChart
        v-if="poolTotal" :segments="pool" title="Token pool" unit="tokens"
        :center="offCloud === null ? '' : `${offCloud}%`" center-hint="kept off cloud models"
      />
      <p v-else class="muted small">No tokens counted yet in this window.</p>
    </section>

    <section class="panel">
      <p class="eyebrow">Over time</p><h2>Savings, {{ windowLabel(days) }}</h2>
      <DataState
        :pending="seriesPending" :error="seriesError" :empty="!series || !series.buckets.length"
        title="No savings to chart" text="Nothing was counted in this window." compact
      >
        <SavingsChart v-if="series" :series="series" :price="price ?? series.reference_price" />
      </DataState>
    </section>

    <p v-if="price" class="muted small source">
      The total saved values local work and unread tokens at {{ price.model }}'s prices: ${{ price.input_per_mtok }} in<template v-if="price.cached_input_per_mtok">, ${{ price.cached_input_per_mtok }} cached</template> and ${{ price.output_per_mtok }} out per million tokens.
      <template v-if="price.estimate">This is a built-in estimate<template v-if="price.source"> from <a :href="price.source" target="_blank" rel="noopener noreferrer">{{ sourceLabel(price.source) }}</a><template v-if="price.checked">, checked {{ price.checked }}</template></template>; add prices to a cloud provider in config/llm.yaml (or <code>price_feed: openrouter</code>) for your own.</template>
      <template v-else-if="price.source">Source: <a :href="price.source" target="_blank" rel="noopener noreferrer">{{ sourceLabel(price.source) }}</a><template v-if="price.checked">, checked {{ price.checked }}</template>.</template>
      <template v-else>No source given: add <code>price_source</code> and <code>price_checked</code> to that provider in config/llm.yaml, or <code>price_feed: openrouter</code>.</template>
    </p>
    <p v-if="modelsError" class="panel muted small">Model statistics need live metrics: {{ apiErrorMessage(modelsError) }}</p>
    <template v-else-if="m">
      <section class="panel">
        <p class="eyebrow">Speed</p><h2>How fast each model reads and writes</h2>
        <p class="muted small">Tokens per second from streamed answers ({{ modelWindow }}). Reading is the prompt, up to the first token, including loading the model into memory; writing is the answer after it. Local models stream; cloud models do not and show no speed.</p>
        <div class="grid two">
          <div><h3>Input speed (prompt tokens/s)</h3><RankList :rows="readSpeed" color="var(--s-1)" suffix=" t/s" empty="No streamed answers yet." /></div>
          <div><h3>Output speed (answer tokens/s)</h3><RankList :rows="writeSpeed" color="var(--s-2)" suffix=" t/s" empty="No streamed answers yet." /></div>
        </div>
      </section>

      <section class="panel">
        <p class="eyebrow">Work</p><h2>Which models get which work</h2>
        <p class="muted small">Tokens (read + written) per model and task, {{ modelWindow }}.</p>
        <StackedBars v-if="taskRows.length" :rows="taskRows" :keys="taskKeys" title="Model" unit="tokens" />
        <p v-else class="muted small">No model calls recorded in this window.</p>
      </section>

      <section v-if="models.length" class="panel">
        <p class="eyebrow">Context</p><h2>How full each context window gets</h2>
        <p class="muted small">Prompt plus answer as a share of the model's configured context window. Low use with large prompts is headroom; use near 100% means requests are being split or trimmed.</p>
        <div class="models">
          <article v-for="x in models" :key="x.provider + x.model" class="model">
            <header>
              <strong class="mono">{{ x.model }}</strong>
              <span class="badge" :class="x.local ? 'badge--green' : 'badge--violet'">{{ x.local ? 'local' : 'cloud' }}</span>
            </header>
            <p class="muted small">
              {{ fmtInt(x.requests) }} requests<template v-if="x.failures"> · {{ fmtInt(x.failures) }} failed</template> · {{ fmtCompact(x.avg_prompt_tokens) }} tokens per prompt
              <template v-if="x.context_window"> · window {{ fmtCompact(x.context_window) }}</template>
            </p>
            <ContextMeter v-if="x.context_window" :p50="x.context_p50" :p95="x.context_p95" :window="x.context_window" />
            <p v-else class="muted small">No context window configured (`context_window` in llm.yaml).</p>
          </article>
        </div>
      </section>
    </template>
  </div>
</template>

<style scoped>
.row-title { font-size: 13px; margin: 14px 0 8px; color: var(--muted); }
.kpis { grid-template-columns: repeat(auto-fit, minmax(170px, 1fr)); margin-bottom: 16px; }
.two { grid-template-columns: repeat(auto-fit, minmax(260px, 1fr)); gap: 24px; }
h3 { font-size: 14px; margin: 0 0 10px; }
.models { display: grid; grid-template-columns: repeat(auto-fit, minmax(260px, 1fr)); gap: 16px; }
.model { display: grid; gap: 8px; padding: 14px; border: 2px solid var(--line); border-radius: var(--radius-sm); }
.model header { display: flex; justify-content: space-between; align-items: center; gap: 8px; }
.small { font-size: 12.5px; }
</style>
