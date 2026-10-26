<script setup lang="ts">
// Part-to-whole for a few distinct shares (at most four, the chart palette):
// a ring with a 2px surface gap between segments, the headline in the hole,
// a legend with every value and share (so colour is never the only cue), a
// tooltip per segment on hover and focus, and a table view.
const props = withDefaults(defineProps<{
  segments: { label: string; value: number; color: string; detail?: string }[]
  title: string
  unit?: string
  center?: string
  centerHint?: string
  size?: number
}>(), { unit: 'tokens', center: '', centerHint: '', size: 180 })

const R = 80
const STROKE = 22
// 2px of surface between segments, as an angle on the ring.
const GAP = 2 / R
const total = computed(() => props.segments.reduce((t, s) => t + Math.max(0, s.value), 0))
const pct = (v: number) => (total.value ? (100 * v) / total.value : 0)

// Arc paths from the top, clockwise; a segment of the whole ring is drawn
// as two halves, since one arc cannot start and end at the same point.
const arcs = computed(() => {
  let start = -Math.PI / 2
  const shown = props.segments.filter((s) => s.value > 0).length
  return props.segments.map((s) => {
    const share = total.value ? (Math.max(0, s.value) / total.value) * Math.PI * 2 : 0
    const from = start
    start += share
    // A lone segment closes the ring; otherwise each gives up the gap.
    const gap = shown > 1 ? Math.min(GAP, share / 2) : 0
    return { ...s, from: from + gap / 2, sweep: share - gap }
  })
})
function point(a: number): string {
  return `${(100 + R * Math.cos(a)).toFixed(2)},${(100 + R * Math.sin(a)).toFixed(2)}`
}
function arc(from: number, sweep: number): string {
  if (sweep <= 0) return ''
  if (sweep >= Math.PI * 2 - 1e-6) {
    return `M${point(from)}A${R},${R} 0 1 1 ${point(from + Math.PI)}A${R},${R} 0 1 1 ${point(from)}`
  }
  return `M${point(from)}A${R},${R} 0 ${sweep > Math.PI ? 1 : 0} 1 ${point(from + sweep)}`
}

const hover = ref(-1)
const tip = computed(() => (hover.value >= 0 ? props.segments[hover.value] : undefined))
</script>

<template>
  <figure class="donut">
    <div class="ring" :style="{ width: `${size}px`, height: `${size}px` }">
      <svg viewBox="0 0 200 200" role="img" :aria-label="title">
        <circle cx="100" cy="100" :r="R" class="track" :stroke-width="STROKE" />
        <g v-for="(a, i) in arcs" :key="a.label">
          <path
            v-if="a.sweep > 0" :d="arc(a.from, a.sweep)" :stroke="a.color" :stroke-width="STROKE" class="seg"
            :class="{ dim: hover >= 0 && hover !== i }" tabindex="0" :aria-label="`${a.label}: ${fmtInt(a.value)} ${unit}, ${pct(a.value).toFixed(1)}%`"
            @pointerenter="hover = i" @pointerleave="hover = -1" @focus="hover = i" @blur="hover = -1"
          />
        </g>
      </svg>
      <div class="hole">
        <template v-if="tip">
          <strong>{{ pct(tip.value).toFixed(0) }}%</strong>
          <span>{{ tip.label }}</span>
        </template>
        <template v-else>
          <strong>{{ center }}</strong>
          <span>{{ centerHint }}</span>
        </template>
      </div>
    </div>
    <ul class="legend">
      <li v-for="(s, i) in segments" :key="s.label" :class="{ active: hover === i }" @pointerenter="hover = i" @pointerleave="hover = -1">
        <span class="key" :style="{ background: s.color }" />
        <span class="name">{{ s.label }}</span>
        <span class="val">{{ fmtCompact(s.value) }} <span class="muted">· {{ pct(s.value).toFixed(1) }}%</span></span>
        <span v-if="s.detail" class="muted detail">{{ s.detail }}</span>
      </li>
    </ul>
    <details class="table-view">
      <summary>Table</summary>
      <table>
        <thead><tr><th>{{ title }}</th><th class="num">{{ unit }}</th><th class="num">Share</th></tr></thead>
        <tbody><tr v-for="s in segments" :key="s.label"><td>{{ s.label }}</td><td class="num">{{ fmtInt(s.value) }}</td><td class="num">{{ pct(s.value).toFixed(1) }}%</td></tr></tbody>
      </table>
    </details>
  </figure>
</template>

<style scoped>
.donut { margin: 0; display: grid; grid-template-columns: auto 1fr; gap: 12px 24px; align-items: center; }
.ring { position: relative; }
svg { width: 100%; height: 100%; display: block; }
.track { fill: none; stroke: var(--panel-2); }
.seg { fill: none; stroke-linecap: butt; outline: none; transition: opacity 120ms; }
.seg.dim { opacity: 0.4; }
.seg:focus-visible { opacity: 1; stroke-width: 26; }
.hole { position: absolute; inset: 0; display: grid; place-content: center; text-align: center; pointer-events: none; padding: 0 30px; }
.hole strong { font-size: 24px; color: var(--ink); }
.hole span { max-width: 100px; margin: 0 auto; font-size: 11.5px; color: var(--muted); line-height: 1.3; }
.legend { list-style: none; margin: 0; padding: 0; display: grid; gap: 8px; }
.legend li { display: grid; grid-template-columns: 12px 1fr auto; gap: 2px 8px; align-items: center; font-size: 13px; padding: 2px 4px; border-radius: var(--radius-xs); }
.legend li.active { background: var(--panel-2); }
.key { width: 12px; height: 12px; border-radius: 3px; }
.val { font-variant-numeric: tabular-nums; color: var(--ink); text-align: right; }
.detail { grid-column: 2 / 4; font-size: 12px; }
.table-view { grid-column: 1 / -1; }
.table-view summary { cursor: pointer; color: var(--muted); font-size: 12px; }
.table-view table { margin-top: 6px; }
@media (max-width: 640px) { .donut { grid-template-columns: 1fr; justify-items: center; } .legend { width: 100%; } }
</style>
