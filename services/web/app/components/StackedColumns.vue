<script setup lang="ts">
// Daily columns stacked by kind: one column per day, segments in the series
// colours with a 1px gap between them, a legend carrying each kind's total,
// a tooltip per day and a table view so no value depends on hovering.
const props = withDefaults(defineProps<{
  rows: { label: string; values: Record<string, number> }[]
  keys: { key: string; label: string; color: string }[]
  title: string
  unit?: string
  height?: number
}>(), { unit: '', height: 140 })

const W = 640
const PAD = { top: 10, right: 8, bottom: 20, left: 36 }
const plotH = computed(() => props.height - PAD.top - PAD.bottom)
const sumOf = (r: { values: Record<string, number> }) => props.keys.reduce((t, k) => t + (r.values[k.key] ?? 0), 0)
const maxV = computed(() => niceMax(Math.max(0, ...props.rows.map(sumOf))))
const slot = computed(() => (W - PAD.left - PAD.right) / Math.max(1, props.rows.length))
const barW = computed(() => Math.max(2, Math.min(22, slot.value - 2)))
const scale = (v: number) => (maxV.value ? (v / maxV.value) * plotH.value : 0)
const base = computed(() => PAD.top + plotH.value)
const totals = computed(() => props.keys.map((k) => ({ ...k, total: props.rows.reduce((t, r) => t + (r.values[k.key] ?? 0), 0) })))

function niceMax(v: number): number {
  if (v <= 0) return 1
  const p = 10 ** Math.floor(Math.log10(v))
  return ([1, 2, 2.5, 5, 10].map((m) => m * p).find((m) => m >= v)) ?? v
}
// Segments bottom-up; the top one alone gets rounded corners.
function segments(i: number) {
  const r = props.rows[i]!
  const x = PAD.left + i * slot.value + (slot.value - barW.value) / 2
  const parts = props.keys.map((k) => ({ key: k.key, color: k.color, h: scale(r.values[k.key] ?? 0) })).filter((p) => p.h > 0)
  let top = base.value
  return parts.map((p, n) => {
    const y = top - p.h
    const gap = n > 0 ? 1 : 0
    const h = Math.max(0.5, p.h - gap)
    const last = n === parts.length - 1
    const rad = last ? Math.min(3, h, barW.value / 2) : 0
    const bottom = top - gap
    top = y
    const d = rad
      ? `M${x},${bottom}V${y + rad}Q${x},${y} ${x + rad},${y}H${x + barW.value - rad}Q${x + barW.value},${y} ${x + barW.value},${y + rad}V${bottom}Z`
      : `M${x},${bottom}V${y}H${x + barW.value}V${bottom}Z`
    return { key: p.key, color: p.color, d }
  })
}
const every = computed(() => Math.max(1, Math.ceil(props.rows.length / 8)))

const hover = ref(-1)
const tip = computed(() => (hover.value >= 0 ? props.rows[hover.value] : undefined))
const tipLeft = computed(() => `${((PAD.left + (hover.value + 0.5) * slot.value) / W) * 100}%`)
</script>

<template>
  <figure class="stacked-cols">
    <ul class="legend">
      <li v-for="k in totals" :key="k.key"><i :style="{ background: k.color }" />{{ k.label }} <strong>{{ fmtCompact(k.total) }}</strong></li>
    </ul>
    <div class="plot">
      <svg :viewBox="`0 0 ${W} ${height}`" role="img" :aria-label="title" preserveAspectRatio="none" :style="{ height: `${height}px` }">
        <line :x1="PAD.left" :x2="W - PAD.right" :y1="PAD.top" :y2="PAD.top" class="grid-line" />
        <line :x1="PAD.left" :x2="W - PAD.right" :y1="base" :y2="base" class="axis-line" />
        <text :x="PAD.left - 6" :y="PAD.top + 4" class="tick" text-anchor="end">{{ fmtCompact(maxV) }}</text>
        <text :x="PAD.left - 6" :y="base + 4" class="tick" text-anchor="end">0</text>
        <g v-for="(r, i) in rows" :key="r.label" :class="{ dim: hover >= 0 && hover !== i }">
          <path v-for="seg in segments(i)" :key="seg.key" :d="seg.d" :fill="seg.color" />
          <rect
            :x="PAD.left + i * slot" :y="PAD.top" :width="slot" :height="plotH" class="hit" tabindex="0"
            :aria-label="`${r.label}: ${keys.map((k) => `${fmtInt(r.values[k.key] ?? 0)} ${k.label}`).join(', ')}`"
            @pointerenter="hover = i" @pointerleave="hover = -1" @focus="hover = i" @blur="hover = -1"
          />
          <text v-if="i % every === 0" :x="PAD.left + (i + 0.5) * slot" :y="height - 5" class="tick" text-anchor="middle">{{ r.label.slice(5) }}</text>
        </g>
      </svg>
      <div v-if="tip" class="tooltip" :style="{ left: tipLeft }" role="status">
        <div class="muted">{{ tip.label }}</div>
        <div v-for="k in keys" :key="k.key" class="tip-row"><i :style="{ background: k.color }" />{{ k.label }} <strong>{{ fmtInt(tip.values[k.key] ?? 0) }}</strong></div>
      </div>
    </div>
    <details class="table-view">
      <summary>Table</summary>
      <table>
        <thead><tr><th>Day</th><th v-for="k in keys" :key="k.key" class="num">{{ k.label }}</th></tr></thead>
        <tbody><tr v-for="r in rows" :key="r.label"><td class="mono">{{ r.label }}</td><td v-for="k in keys" :key="k.key" class="num">{{ fmtInt(r.values[k.key] ?? 0) }}</td></tr></tbody>
      </table>
    </details>
  </figure>
</template>

<style scoped>
.stacked-cols { margin: 0; display: grid; gap: 6px; }
.legend { display: flex; flex-wrap: wrap; gap: 4px 16px; margin: 0; padding: 0; list-style: none; font-size: 12.5px; color: var(--muted); }
.legend li, .tip-row { display: inline-flex; align-items: center; gap: 6px; }
.legend i, .tip-row i { width: 10px; height: 10px; border-radius: 3px; display: inline-block; }
.legend strong, .tip-row strong { color: var(--ink); font-variant-numeric: tabular-nums; }
.plot { position: relative; }
svg { width: 100%; display: block; overflow: visible; }
.grid-line { stroke: var(--line); stroke-width: 1; vector-effect: non-scaling-stroke; }
.axis-line { stroke: var(--line-strong); stroke-width: 1; vector-effect: non-scaling-stroke; }
.tick { fill: var(--muted); font: 10px var(--font-mono); }
g { transition: opacity 120ms; }
g.dim { opacity: 0.45; }
.hit { fill: transparent; cursor: default; outline: none; }
.hit:focus-visible { stroke: var(--focus-ring); stroke-width: 2; }
.tooltip { position: absolute; top: 0; transform: translate(-50%, -100%); pointer-events: none; padding: 6px 10px; border-radius: var(--radius-xs); background: var(--panel); border: 1px solid var(--line-strong); font-size: 12px; white-space: nowrap; z-index: 2; display: grid; gap: 2px; }
.table-view summary { cursor: pointer; color: var(--muted); font-size: 12px; }
.table-view table { margin-top: 6px; }
</style>
