<script setup lang="ts">
import type { SeriesLine } from '~/utils/types'

// A multi-series line chart over time. Colours follow the series in fixed
// order (never their rank changes), "other" is neutral; 2px lines, a legend
// for two or more series plus direct end labels up to four, a crosshair
// with every series' value on hover or focus, gaps where a step has no data,
// and a table view so nothing depends on hovering.
const props = withDefaults(defineProps<{
  times: number[]
  series: SeriesLine[]
  title: string
  unit: string
  stepSeconds: number
  height?: number
  // yMax raises the top of the y axis so small multiples share one scale.
  yMax?: number
}>(), { height: 180, yMax: 0 })

const PALETTE = ['var(--s-1)', 'var(--s-2)', 'var(--s-3)', 'var(--s-4)']
const color = (s: SeriesLine, i: number) => (s.name === 'other' ? 'var(--s-other)' : PALETTE[i % PALETTE.length])

const W = 640
const PAD = { top: 12, right: 96, bottom: 22, left: 48 }
const plotW = W - PAD.left - PAD.right
const plotH = computed(() => props.height - PAD.top - PAD.bottom)
const n = computed(() => props.times.length)
const maxV = computed(() => niceMax(Math.max(0, props.yMax, ...props.series.flatMap((s) => s.values.map((v) => v ?? 0)))))
const x = (i: number) => PAD.left + (n.value <= 1 ? plotW / 2 : (i / (n.value - 1)) * plotW)
const y = (v: number) => PAD.top + plotH.value - (maxV.value ? (v / maxV.value) * plotH.value : 0)

function niceMax(v: number): number {
  if (v <= 0) return 1
  const p = 10 ** Math.floor(Math.log10(v))
  return ([1, 2, 2.5, 5, 10].map((m) => m * p).find((m) => m >= v)) ?? v
}
// Break the line where a step has no data instead of drawing through it.
function path(values: (number | null)[]): string {
  let d = ''
  let pen = false
  values.forEach((v, i) => {
    if (v == null) { pen = false; return }
    d += `${pen ? 'L' : 'M'}${x(i).toFixed(1)},${y(v).toFixed(1)}`
    pen = true
  })
  return d
}
// Points with no neighbour draw no line segment: mark them with a dot.
function isolated(values: (number | null)[]): number[] {
  return values.flatMap((v, i) => (v != null && values[i - 1] == null && values[i + 1] == null ? [i] : []))
}
function lastPoint(values: (number | null)[]): { i: number; v: number } | undefined {
  for (let i = values.length - 1; i >= 0; i--) if (values[i] != null) return { i, v: values[i] as number }
}
// Direct labels at the line ends, nudged apart so they never overlap.
const endLabels = computed(() => {
  if (props.series.length > 4) return []
  const ls = props.series.map((s, i) => ({ s, i, p: lastPoint(s.values) })).filter((l) => l.p)
  const placed = ls.map((l) => ({ ...l, ly: y(l.p!.v) })).sort((a, b) => a.ly - b.ly)
  placed.forEach((l, k) => { if (k > 0) l.ly = Math.max(l.ly, (placed[k - 1]?.ly ?? 0) + 12) })
  return placed
})

const short = computed(() => props.stepSeconds < 86400)
function tickLabel(t: number): string {
  const d = new Date(t * 1000)
  const span = (props.times.at(-1) ?? 0) - (props.times[0] ?? 0)
  return short.value && span <= 86400
    ? d.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' })
    : d.toLocaleDateString([], { month: 'short', day: 'numeric' }) + (props.stepSeconds < 86400 ? ` ${d.getHours()}h` : '')
}
const ticks = computed(() => {
  const every = Math.max(1, Math.ceil(n.value / 6))
  return props.times.map((t, i) => ({ t, i })).filter(({ i }) => i % every === 0)
})

const hover = ref(-1)
function onMove(e: PointerEvent) {
  const svg = (e.currentTarget as SVGElement).closest('svg')!
  const r = svg.getBoundingClientRect()
  const px = ((e.clientX - r.left) / r.width) * W
  hover.value = Math.min(n.value - 1, Math.max(0, Math.round(((px - PAD.left) / plotW) * (n.value - 1))))
}
function onKey(e: KeyboardEvent) {
  if (e.key === 'ArrowRight') hover.value = Math.min(n.value - 1, hover.value + 1)
  else if (e.key === 'ArrowLeft') hover.value = Math.max(0, hover.value < 0 ? n.value - 1 : hover.value - 1)
}
const tipLeft = computed(() => `${(x(hover.value) / W) * 100}%`)
const fullTime = (t: number) => new Date(t * 1000).toLocaleString([], { month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit' })
</script>

<template>
  <figure class="line-chart">
    <ul v-if="series.length >= 2" class="legend" aria-label="Series">
      <li v-for="(s, i) in series" :key="s.name"><span class="swatch" :style="{ background: color(s, i) }" />{{ s.name }}</li>
    </ul>
    <div class="plot">
      <svg :viewBox="`0 0 ${W} ${height}`" role="img" :aria-label="title" preserveAspectRatio="none" :style="{ height: `${height}px` }">
        <line :x1="PAD.left" :x2="W - PAD.right" :y1="PAD.top" :y2="PAD.top" class="grid-line" />
        <line :x1="PAD.left" :x2="W - PAD.right" :y1="y(maxV / 2)" :y2="y(maxV / 2)" class="grid-line" />
        <line :x1="PAD.left" :x2="W - PAD.right" :y1="PAD.top + plotH" :y2="PAD.top + plotH" class="axis-line" />
        <text :x="PAD.left - 6" :y="PAD.top + 4" class="tick" text-anchor="end">{{ fmtUnit(maxV, unit) }}</text>
        <text :x="PAD.left - 6" :y="y(maxV / 2) + 4" class="tick" text-anchor="end">{{ fmtUnit(maxV / 2, unit) }}</text>
        <text :x="PAD.left - 6" :y="PAD.top + plotH + 4" class="tick" text-anchor="end">0</text>
        <text v-for="tk in ticks" :key="tk.t" :x="x(tk.i)" :y="height - 6" class="tick" text-anchor="middle">{{ tickLabel(tk.t) }}</text>
        <template v-for="(s, i) in series" :key="s.name">
          <path :d="path(s.values)" :stroke="color(s, i)" class="line" />
          <circle v-for="k in isolated(s.values)" :key="`iso-${s.name}-${k}`" :cx="x(k)" :cy="y(s.values[k] as number)" r="2.5" :fill="color(s, i)" />
        </template>
        <template v-for="l in endLabels" :key="`end-${l.s.name}`">
          <circle :cx="x(l.p!.i)" :cy="y(l.p!.v)" r="3" :fill="color(l.s, l.i)" class="end-dot" />
          <text :x="W - PAD.right + 8" :y="l.ly + 4" class="end-label">{{ l.s.name }}</text>
        </template>
        <g v-if="hover >= 0">
          <line :x1="x(hover)" :x2="x(hover)" :y1="PAD.top" :y2="PAD.top + plotH" class="crosshair" />
          <template v-for="(s, i) in series" :key="`dot-${s.name}`">
            <circle v-if="s.values[hover] != null" :cx="x(hover)" :cy="y(s.values[hover] as number)" r="4" :fill="color(s, i)" class="hover-dot" />
          </template>
        </g>
        <rect
          :x="PAD.left" :y="PAD.top" :width="plotW" :height="plotH" class="hit" tabindex="0"
          :aria-label="`${title}: use the arrow keys to read values`"
          @pointermove="onMove" @pointerleave="hover = -1" @focus="hover = n - 1" @blur="hover = -1" @keydown="onKey"
        />
      </svg>
      <div v-if="hover >= 0" class="tooltip" :style="{ left: tipLeft }" role="status">
        <div class="muted">{{ fullTime(times[hover] ?? 0) }}</div>
        <div v-for="(s, i) in series" :key="`tip-${s.name}`" class="tip-row">
          <span class="swatch" :style="{ background: color(s, i) }" />{{ s.name }} <strong>{{ fmtUnit(s.values[hover], unit) }}</strong>
        </div>
      </div>
    </div>
    <details class="table-view">
      <summary>Table</summary>
      <div class="table-scroll">
        <table>
          <thead><tr><th>Time</th><th v-for="s in series" :key="`th-${s.name}`" class="num">{{ s.name }}</th></tr></thead>
          <tbody>
            <tr v-for="(t, i) in times" :key="t"><td class="mono">{{ fullTime(t) }}</td><td v-for="s in series" :key="`td-${s.name}`" class="num">{{ fmtUnit(s.values[i], unit) }}</td></tr>
          </tbody>
        </table>
      </div>
    </details>
  </figure>
</template>

<style scoped>
.line-chart { margin: 0; display: grid; gap: 6px; }
.legend { display: flex; flex-wrap: wrap; gap: 4px 14px; margin: 0; padding: 0; list-style: none; font-size: 12px; color: var(--muted); }
.legend li, .tip-row { display: inline-flex; align-items: center; gap: 6px; }
.swatch { width: 10px; height: 2px; border-radius: 1px; display: inline-block; }
.plot { position: relative; }
svg { width: 100%; display: block; overflow: visible; }
.grid-line { stroke: var(--line); stroke-width: 1; vector-effect: non-scaling-stroke; }
.axis-line { stroke: var(--line-strong); stroke-width: 1; vector-effect: non-scaling-stroke; }
.tick { fill: var(--muted); font: 10px var(--font-mono); }
.line { fill: none; stroke-width: 2; stroke-linejoin: round; stroke-linecap: round; vector-effect: non-scaling-stroke; }
.end-dot { stroke: var(--panel); stroke-width: 2; vector-effect: non-scaling-stroke; }
.end-label { fill: var(--muted); font: 11px var(--font-sans); }
.crosshair { stroke: var(--line-strong); stroke-width: 1; vector-effect: non-scaling-stroke; }
.hover-dot { stroke: var(--panel); stroke-width: 2; vector-effect: non-scaling-stroke; }
.hit { fill: transparent; cursor: crosshair; outline: none; }
.hit:focus-visible { stroke: var(--focus-ring); stroke-width: 2; }
.tooltip { position: absolute; top: 0; transform: translate(-50%, -100%); pointer-events: none; padding: 6px 10px; border-radius: var(--radius-xs); background: var(--panel); border: 1px solid var(--line-strong); font-size: 12px; white-space: nowrap; z-index: 2; display: grid; gap: 2px; }
.tooltip strong { color: var(--ink); margin-left: 4px; }
.table-view summary { cursor: pointer; color: var(--muted); font-size: 12px; }
.table-scroll { max-height: 240px; overflow: auto; margin-top: 6px; }
</style>
