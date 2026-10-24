<script setup lang="ts">
// A turn-by-turn model of one long agent session, with and without
// offloading. Every turn re-sends the whole context, so the sum of context
// sizes over all turns is the input-token cost. Theoretical: it counts
// tokens, not answer quality.
const props = defineProps<{ tokensPerRecall: number; recallsPerSession: number; tokensPerFact: number; measured: boolean }>()

const p = reactive({
  window: 200_000, // context window
  base: 15_000, // system prompt, tool definitions, instructions
  perTurn: 3_000, // tokens a turn adds (messages, tool results, code)
  turns: 150,
  compactAt: 90, // % of the window where the client compacts
  compactTo: 15, // % of the window left after compaction
  target: 25, // the "sweet spot": offload when the context reaches this %
})

interface Run { series: number[]; total: number; peak: number; events: number; recallTokens: number }
function simulate(offload: boolean): Run {
  let ctx = p.base
  let total = 0
  let peak = 0
  let events = 0
  let recallTokens = 0
  const series: number[] = []
  const limit = (offload ? p.target : p.compactAt) / 100 * p.window
  const recall = Math.max(0, props.tokensPerRecall) * Math.max(1, props.recallsPerSession)
  for (let t = 0; t < p.turns; t++) {
    ctx += p.perTurn
    if (ctx > limit) {
      events++
      if (offload) {
        // Durable facts go to memory; keep half the target as working
        // context and recall what the next steps need.
        ctx = Math.max(p.base, limit / 2) + recall
        recallTokens += recall
      } else {
        ctx = Math.max(p.base, (p.compactTo / 100) * p.window)
      }
    }
    peak = Math.max(peak, ctx)
    total += ctx
    series.push(ctx)
  }
  return { series, total, peak, events, recallTokens }
}
const without = computed(() => simulate(false))
const withOffload = computed(() => simulate(true))
const saved = computed(() => without.value.total - withOffload.value.total)
const savedPct = computed(() => (without.value.total ? Math.round((saved.value / without.value.total) * 100) : 0))
const factsPerOffload = computed(() => Math.round(((p.target / 100) * p.window / 2) / 200)) // one fact per ~200 tokens of conversation

// Chart geometry
const W = 640
const H = 200
const PAD = { top: 14, right: 64, bottom: 22, left: 44 }
const x = (i: number) => PAD.left + (i / Math.max(1, p.turns - 1)) * (W - PAD.left - PAD.right)
const yMax = computed(() => p.window)
const y = (v: number) => PAD.top + (H - PAD.top - PAD.bottom) * (1 - v / yMax.value)
const line = (s: number[]) => s.map((v, i) => `${i ? 'L' : 'M'}${x(i).toFixed(1)},${y(v).toFixed(1)}`).join('')
const hover = ref(-1)
function move(e: PointerEvent) {
  const r = (e.currentTarget as SVGElement).getBoundingClientRect()
  const px = ((e.clientX - r.left) / r.width) * W
  hover.value = Math.min(p.turns - 1, Math.max(0, Math.round(((px - PAD.left) / (W - PAD.left - PAD.right)) * (p.turns - 1))))
}
</script>

<template>
  <div class="model">
    <div class="inputs">
      <label>Context window <input v-model.number="p.window" type="number" min="8000" step="1000"> <span class="muted">tokens</span></label>
      <label>Fixed context <input v-model.number="p.base" type="number" min="0" step="1000"> <span class="muted">system prompt + tools</span></label>
      <label>Added per turn <input v-model.number="p.perTurn" type="number" min="100" step="100"> <span class="muted">tokens</span></label>
      <label>Turns in the session <input v-model.number="p.turns" type="number" min="10" max="1000" step="10"></label>
      <label>Client compacts at <input v-model.number="p.compactAt" type="number" min="30" max="100"> <span class="muted">% → keeps {{ p.compactTo }}%</span></label>
      <label>Offload target ("sweet spot") <input v-model.number="p.target" type="range" min="10" max="60" step="5"> <strong>{{ p.target }}%</strong></label>
    </div>
    <p class="muted small">
      Recall cost: <strong class="ink">{{ fmtInt(Math.round(tokensPerRecall)) }}</strong> tokens × {{ Math.max(1, recallsPerSession) }} recall(s) per offload,
      {{ tokensPerFact }} tokens per fact —
      <span :class="measured ? 'ok' : 'warn'">{{ measured ? 'measured from your agents' : 'defaults until agents have recalled facts' }}</span>.
    </p>

    <div class="grid kpis">
      <StatTile label="Input tokens, no offloading" :value="fmtCompact(without.total)" :hint="`${without.events} compaction(s), peak ${fmtCompact(without.peak)}`" />
      <StatTile label="Input tokens, offloading" :value="fmtCompact(withOffload.total)" :hint="`${withOffload.events} offload(s), peak ${fmtCompact(withOffload.peak)}`" />
      <StatTile label="Saved over the session" :value="`${savedPct}%`" :hint="`${fmtCompact(saved)} tokens, recall cost ${fmtCompact(withOffload.recallTokens)} included`" />
      <StatTile label="Facts per offload (est.)" :value="factsPerOffload" hint="one fact per ~200 tokens of conversation offloaded" />
    </div>

    <figure class="chart">
      <div class="legend">
        <span><i class="key key--base" /> No offloading (client compaction)</span>
        <span><i class="key key--with" /> Offloading at {{ p.target }}%</span>
      </div>
      <div class="plot">
        <svg :viewBox="`0 0 ${W} ${H}`" role="img" aria-label="Context size per turn, with and without offloading" @pointermove="move" @pointerleave="hover = -1">
          <line v-for="f in [0.25, 0.5, 0.75, 1]" :key="f" :x1="PAD.left" :x2="W - PAD.right" :y1="y(f * yMax)" :y2="y(f * yMax)" class="grid-line" />
          <text v-for="f in [0, 0.5, 1]" :key="`t${f}`" :x="PAD.left - 6" :y="y(f * yMax) + 4" class="tick" text-anchor="end">{{ fmtCompact(f * yMax) }}</text>
          <text :x="PAD.left" :y="H - 6" class="tick">turn 1</text>
          <text :x="W - PAD.right" :y="H - 6" class="tick" text-anchor="end">turn {{ p.turns }}</text>
          <line :x1="PAD.left" :x2="W - PAD.right" :y1="y((p.target / 100) * yMax)" :y2="y((p.target / 100) * yMax)" class="target" />
          <path :d="line(without.series)" class="series series--base" />
          <path :d="line(withOffload.series)" class="series series--with" />
          <text :x="W - PAD.right + 6" :y="y(without.series.at(-1) ?? 0) + 4" class="end">no offload</text>
          <text :x="W - PAD.right + 6" :y="y(withOffload.series.at(-1) ?? 0) + 4" class="end">offload</text>
          <template v-if="hover >= 0">
            <line :x1="x(hover)" :x2="x(hover)" :y1="PAD.top" :y2="H - PAD.bottom" class="crosshair" />
            <circle :cx="x(hover)" :cy="y(without.series[hover] ?? 0)" r="4" class="dot dot--base" />
            <circle :cx="x(hover)" :cy="y(withOffload.series[hover] ?? 0)" r="4" class="dot dot--with" />
          </template>
        </svg>
        <div v-if="hover >= 0" class="tooltip" :style="{ left: `${(x(hover) / W) * 100}%` }" role="status">
          <div class="muted">Turn {{ hover + 1 }}</div>
          <div><i class="key key--base" /> <strong>{{ fmtInt(without.series[hover]) }}</strong> no offloading</div>
          <div><i class="key key--with" /> <strong>{{ fmtInt(withOffload.series[hover]) }}</strong> offloading</div>
        </div>
      </div>
    </figure>

    <details class="assumptions">
      <summary>How this is estimated</summary>
      <ul>
        <li>Each turn adds the same number of tokens and re-sends the whole context, so the session's input cost is the sum of the context size over all turns.</li>
        <li>Without offloading, the client compacts at {{ p.compactAt }}% of the window down to {{ p.compactTo }}%. Detail lost in a compaction is gone.</li>
        <li>With offloading, the agent saves durable facts when the context reaches {{ p.target }}%, keeps half of that as working context, and recalls facts at the measured recall size. Nothing is lost as long as the facts were saved.</li>
        <li>It counts tokens only. Effects on answer quality (models attend worse to very long contexts) and prompt caching discounts are not modelled; caching narrows the cost gap, the quality effect usually favours smaller contexts.</li>
      </ul>
    </details>
  </div>
</template>

<style scoped>
.model { display: grid; gap: 14px; }
.inputs { display: grid; grid-template-columns: repeat(auto-fit, minmax(min(100%, 260px), 1fr)); gap: 10px 18px; }
.inputs label { display: flex; align-items: center; gap: 8px; font-size: 13px; flex-wrap: wrap; }
.inputs input[type='number'] { width: 110px; }
.small { font-size: 12.5px; margin: 0; }
.ink { color: var(--ink); }
.ok { color: var(--success); }
.warn { color: var(--warning); }
.kpis { grid-template-columns: repeat(auto-fit, minmax(200px, 1fr)); }
.chart { margin: 0; display: grid; gap: 8px; }
.legend { display: flex; gap: 18px; flex-wrap: wrap; font-size: 12.5px; color: var(--muted); }
.plot { position: relative; }
svg { width: 100%; display: block; overflow: visible; }
.grid-line { stroke: var(--line); stroke-width: 1; }
.target { stroke: var(--line-strong); stroke-width: 1; }
.tick, .end { fill: var(--muted); font: 10px var(--font-mono); }
.series { fill: none; stroke-width: 2; stroke-linejoin: round; stroke-linecap: round; }
.series--base { stroke: var(--muted); }
.series--with { stroke: var(--info); }
.crosshair { stroke: var(--line-strong); stroke-width: 1; }
.dot { stroke: var(--panel); stroke-width: 2; }
.dot--base { fill: var(--muted); }
.dot--with { fill: var(--info); }
.key { display: inline-block; width: 14px; height: 2px; vertical-align: middle; border-radius: 1px; }
.key--base { background: var(--muted); }
.key--with { background: var(--info); }
.tooltip { position: absolute; top: 0; transform: translate(-50%, -105%); pointer-events: none; padding: 6px 10px; border-radius: var(--radius-xs); background: var(--panel); border: 1px solid var(--line-strong); font-size: 12px; white-space: nowrap; z-index: 2; display: grid; gap: 2px; }
.tooltip strong { color: var(--ink); }
.assumptions summary { cursor: pointer; color: var(--muted); font-size: 12.5px; }
.assumptions ul { margin: 8px 0 0; padding-left: 18px; display: grid; gap: 4px; font-size: 12.5px; color: var(--muted); }
</style>
