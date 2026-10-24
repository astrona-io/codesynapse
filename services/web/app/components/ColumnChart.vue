<script setup lang="ts">
// A single-series column chart: one hue, bars capped at 24px with a rounded
// data end, a hairline baseline, a tooltip per bar on hover and focus, and a
// table view so no value depends on hovering.
const props = withDefaults(defineProps<{
  points: { label: string; value: number; detail?: string }[]
  title: string
  unit?: string
  color?: string
  height?: number
}>(), { unit: '', color: 'var(--info)', height: 160 })

const W = 640
const PAD = { top: 12, right: 8, bottom: 22, left: 40 }
const plotH = computed(() => props.height - PAD.top - PAD.bottom)
const maxV = computed(() => niceMax(Math.max(0, ...props.points.map((p) => p.value))))
const slot = computed(() => (W - PAD.left - PAD.right) / Math.max(1, props.points.length))
const barW = computed(() => Math.max(2, Math.min(24, slot.value - 2)))
const y = (v: number) => PAD.top + plotH.value - (maxV.value ? (v / maxV.value) * plotH.value : 0)

function niceMax(v: number): number {
  if (v <= 0) return 1
  const p = 10 ** Math.floor(Math.log10(v))
  return ([1, 2, 2.5, 5, 10].map((m) => m * p).find((m) => m >= v)) ?? v
}
// Rounded top corners only: square where the bar meets the baseline.
function bar(i: number, v: number): string {
  const x = PAD.left + i * slot.value + (slot.value - barW.value) / 2
  const top = y(v)
  const base = PAD.top + plotH.value
  const h = base - top
  if (h <= 0) return ''
  const r = Math.min(4, h, barW.value / 2)
  return `M${x},${base}V${top + r}Q${x},${top} ${x + r},${top}H${x + barW.value - r}Q${x + barW.value},${top} ${x + barW.value},${top + r}V${base}Z`
}
// Label every nth column so dates never collide.
const every = computed(() => Math.max(1, Math.ceil(props.points.length / 8)))

const hover = ref(-1)
const tip = computed(() => (hover.value >= 0 ? props.points[hover.value] : undefined))
const tipLeft = computed(() => `${((PAD.left + (hover.value + 0.5) * slot.value) / W) * 100}%`)
</script>

<template>
  <figure class="col-chart">
    <div class="plot">
      <svg :viewBox="`0 0 ${W} ${height}`" role="img" :aria-label="title" preserveAspectRatio="none" :style="{ height: `${height}px` }">
        <line :x1="PAD.left" :x2="W - PAD.right" :y1="PAD.top" :y2="PAD.top" class="grid-line" />
        <line :x1="PAD.left" :x2="W - PAD.right" :y1="y(maxV / 2)" :y2="y(maxV / 2)" class="grid-line" />
        <line :x1="PAD.left" :x2="W - PAD.right" :y1="PAD.top + plotH" :y2="PAD.top + plotH" class="axis-line" />
        <text :x="PAD.left - 6" :y="PAD.top + 4" class="tick" text-anchor="end">{{ fmtCompact(maxV) }}</text>
        <text :x="PAD.left - 6" :y="y(maxV / 2) + 4" class="tick" text-anchor="end">{{ fmtCompact(maxV / 2) }}</text>
        <text :x="PAD.left - 6" :y="PAD.top + plotH + 4" class="tick" text-anchor="end">0</text>
        <g v-for="(p, i) in points" :key="p.label">
          <path :d="bar(i, p.value)" :fill="color" :class="{ dim: hover >= 0 && hover !== i }" />
          <rect
            :x="PAD.left + i * slot" :y="PAD.top" :width="slot" :height="plotH" class="hit" tabindex="0"
            :aria-label="`${p.label}: ${fmtInt(p.value)} ${unit}`"
            @pointerenter="hover = i" @pointerleave="hover = -1" @focus="hover = i" @blur="hover = -1"
          />
          <text v-if="i % every === 0" :x="PAD.left + (i + 0.5) * slot" :y="height - 6" class="tick" text-anchor="middle">{{ p.label.slice(5) }}</text>
        </g>
      </svg>
      <div v-if="tip" class="tooltip" :style="{ left: tipLeft }" role="status">
        <strong>{{ fmtInt(tip.value) }}</strong> <span>{{ unit }}</span>
        <div class="muted">{{ tip.label }}<template v-if="tip.detail"> · {{ tip.detail }}</template></div>
      </div>
    </div>
    <details class="table-view">
      <summary>Table</summary>
      <table>
        <thead><tr><th>Day</th><th class="num">{{ unit || 'Value' }}</th></tr></thead>
        <tbody><tr v-for="p in points" :key="p.label"><td class="mono">{{ p.label }}</td><td class="num">{{ fmtInt(p.value) }}</td></tr></tbody>
      </table>
    </details>
  </figure>
</template>

<style scoped>
.col-chart { margin: 0; display: grid; gap: 6px; }
.plot { position: relative; }
svg { width: 100%; display: block; overflow: visible; }
.grid-line { stroke: var(--line); stroke-width: 1; vector-effect: non-scaling-stroke; }
.axis-line { stroke: var(--line-strong); stroke-width: 1; vector-effect: non-scaling-stroke; }
.tick { fill: var(--muted); font: 10px var(--font-mono); }
path { transition: opacity 120ms; }
path.dim { opacity: 0.45; }
.hit { fill: transparent; cursor: default; outline: none; }
.hit:focus-visible { stroke: var(--line-strong); stroke-width: 1; }
.tooltip { position: absolute; top: 0; transform: translate(-50%, -100%); pointer-events: none; padding: 6px 10px; border-radius: var(--radius-xs); background: var(--panel); border: 1px solid var(--line-strong); font-size: 12px; white-space: nowrap; z-index: 2; }
.tooltip strong { color: var(--ink); font-size: 14px; }
.table-view summary { cursor: pointer; color: var(--muted); font-size: 12px; }
.table-view table { margin-top: 6px; }
</style>
