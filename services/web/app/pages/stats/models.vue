<script setup lang="ts">
import type { ModelStats } from '~/utils/types'
import { useStatistics } from '~/composables/useStatistics'

// What the connected models do and what they save. Agent tokens and tokens
// avoided come from the daily usage counters (the window chips); model work
// comes from the metrics store, which keeps at most 30 days.
const { days, s, error: statsError } = useStatistics()
const range = computed(() => seriesRangeFor(days.value))
const { data: m, error: modelsError } = useApiGet<ModelStats>(() => `stats/models?range=${range.value}`)

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
const agentTokens = computed(() => a.value?.tokens.total ?? 0)

const pool = computed(() => [
  { label: 'Cloud agents', value: agentTokens.value, color: 'var(--s-1)', detail: 'what Claude Code and Codex reported for their own model' },
  { label: 'Never read (graph answers)', value: graphAvoided.value, color: 'var(--s-2)', detail: 'file tokens the graph tools answered instead' },
  { label: 'Local models', value: localIn.value + localOut.value, color: 'var(--s-3)', detail: 'ask_local, reviews, memory and other AI work on your machine' },
  { label: 'CodeSynapse on cloud models', value: cloudTokens.value, color: 'var(--s-4)', detail: 'AI work routed to a cloud provider' },
])
const poolTotal = computed(() => pool.value.reduce((t, p) => t + p.value, 0))
const offCloud = computed(() => (poolTotal.value ? Math.round((100 * (graphAvoided.value + localIn.value + localOut.value)) / poolTotal.value) : null))

// Valued at the configured cloud model's prices: what the local work and the
// avoided reading would have cost there. An estimate, and only with prices.
const price = computed(() => m.value?.reference_price)
const saved = computed(() => {
  const p = price.value
  if (!p) return null
  return ((localIn.value + graphAvoided.value) * p.input_per_mtok + localOut.value * p.output_per_mtok) / 1e6
})
const money = (v: number) => (v >= 100 ? `$${Math.round(v)}` : `$${v.toFixed(2)}`)

const TASK_LABEL: Record<string, string> = {
  'local-answer': 'Agent questions', review: 'Code review', 'security-triage': 'Security triage', briefing: 'Briefings', fix: 'Fixes',
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
const windowLabel = computed(() => (days.value > 30 ? 'last 30 days (the metrics store keeps no more)' : `last ${range.value === '7d' ? 7 : 30} days`))
</script>

<template>
  <p v-if="statsError" class="panel error">{{ apiErrorMessage(statsError) }}</p>
  <div v-else class="stack">
    <section class="panel">
      <p class="eyebrow">Token pool</p><h2>Where the reading happened</h2>
      <p class="muted small">Every token an agent or CodeSynapse spent, or did not have to spend, in the window. Cloud agents' tokens come from Claude Code and Codex hooks; the rest from CodeSynapse's own counters. Estimates say how they are estimated.</p>
      <div class="grid kpis">
        <StatTile label="Kept off cloud models" :value="offCloud === null ? '–' : `${offCloud}%`" hint="never read, or read by a local model" />
        <StatTile label="Tokens never read" :value="fmtCompact(a?.avoided_tokens ?? 0)" :hint="`graph answers and ask_local, estimated`" />
        <StatTile label="Handled by local models" :value="fmtCompact(localIn + localOut)" :hint="`${fmtCompact(localIn)} read · ${fmtCompact(localOut)} written`" />
        <StatTile label="Prompts pre-briefed" :value="fmtCompact(a?.local_briefs ?? 0)" hint="a local model briefed the agent first" />
        <StatTile
          label="Cloud cost avoided" :value="saved === null ? '–' : money(saved)"
          :hint="price ? `at ${price.model} prices, estimated` : 'add prices to a cloud provider to estimate'"
        />
      </div>
      <DonutChart
        v-if="poolTotal" :segments="pool" title="Token pool" unit="tokens"
        :center="offCloud === null ? '' : `${offCloud}%`" center-hint="kept off cloud models"
      />
      <p v-else class="muted small">No tokens counted yet in this window.</p>
    </section>

    <p v-if="modelsError" class="panel muted small">Model statistics need live metrics: {{ apiErrorMessage(modelsError) }}</p>
    <template v-else-if="m">
      <section class="panel">
        <p class="eyebrow">Speed</p><h2>How fast each model reads and writes</h2>
        <p class="muted small">Tokens per second from streamed answers ({{ windowLabel }}). Reading is the prompt, up to the first token, including loading the model into memory; writing is the answer after it. Local models stream; cloud models do not and show no speed.</p>
        <div class="grid two">
          <div><h3>Read (prompt tokens/s)</h3><RankList :rows="readSpeed" color="var(--s-1)" suffix=" t/s" empty="No streamed answers yet." /></div>
          <div><h3>Write (answer tokens/s)</h3><RankList :rows="writeSpeed" color="var(--s-2)" suffix=" t/s" empty="No streamed answers yet." /></div>
        </div>
      </section>

      <section class="panel">
        <p class="eyebrow">Work</p><h2>Which models get which work</h2>
        <p class="muted small">Tokens (read + written) per model and task, {{ windowLabel }}.</p>
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
.kpis { grid-template-columns: repeat(auto-fit, minmax(170px, 1fr)); margin-bottom: 16px; }
.two { grid-template-columns: repeat(auto-fit, minmax(260px, 1fr)); gap: 24px; }
h3 { font-size: 14px; margin: 0 0 10px; }
.models { display: grid; grid-template-columns: repeat(auto-fit, minmax(260px, 1fr)); gap: 16px; }
.model { display: grid; gap: 8px; padding: 14px; border: 2px solid var(--line); border-radius: var(--radius-sm); }
.model header { display: flex; justify-content: space-between; align-items: center; gap: 8px; }
.small { font-size: 12.5px; }
</style>
