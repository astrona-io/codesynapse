<script setup lang="ts">
import type { QueueCell, QueueMetric } from '~/utils/types'

// Peak hours of one model queue: weekdays as rows (Monday first) and
// 15-minute slots as columns, in the viewer's timezone. The colour steps
// are fixed bins, the wait bins at the verdict's thresholds, so the same
// colour means the same thing from week to week. A slot with no
// measurements is a neutral grey; a measured slot without requests is the
// palest tint.
const props = defineProps<{
  cells: (QueueCell | null)[]
  metric: QueueMetric
  timezone: string
  waitNoticeable?: number
  waitHigh?: number
}>()

const DAYS = ['Mon', 'Tue', 'Wed', 'Thu', 'Fri', 'Sat', 'Sun']
const SLOTS = 96
const CW = 10
const CH = 20
const GAP = 2
const LEFT = 40
const TOP = 18
const width = LEFT + SLOTS * CW
const height = TOP + DAYS.length * CH

function secs(v: number): string {
  if (v < 1) return `${Math.round(v * 1000)} ms`
  return `${v < 10 ? v.toFixed(1) : Math.round(v)} s`
}
function clock(slot: number): string {
  const m = slot * 15
  return `${String(Math.floor(m / 60)).padStart(2, '0')}:${String(m % 60).padStart(2, '0')}`
}

const maxRequests = computed(() => Math.max(0, ...props.cells.map((c) => c?.requests ?? 0)))

// Upper bounds of steps 1..4; anything above the last is step 5.
const bins = computed<{ bounds: number[]; label: (lo: number | null, hi: number | null) => string }>(() => {
  const notable = props.waitNoticeable ?? 3
  const high = props.waitHigh ?? 10
  switch (props.metric) {
    case 'share':
      return { bounds: [0.1, 0.25, 0.5, 0.75], label: (lo, hi) => (lo === null ? `under ${hi! * 100}%` : hi === null ? `${lo * 100}% or more` : `${lo * 100}–${hi * 100}%`) }
    case 'requests': {
      const m = Math.max(maxRequests.value, 1)
      const b = [0.2, 0.4, 0.6, 0.8].map((f) => Math.max(1, Math.round(m * f)))
      return { bounds: b, label: (lo, hi) => (lo === null ? `up to ${hi}` : hi === null ? `over ${lo}` : `${lo}–${hi}`) }
    }
    default:
      return { bounds: [1, notable, high, 3 * high], label: (lo, hi) => (lo === null ? `under ${secs(hi!)}` : hi === null ? `${secs(lo)} or more` : `${secs(lo)}–${secs(hi)}`) }
  }
})

function valueOf(c: QueueCell): number | null {
  switch (props.metric) {
    case 'requests': return c.requests
    case 'avg': return c.avg ?? null
    case 'share': return c.share ?? null
    default: return c.p95 ?? null
  }
}

function fill(c: QueueCell | null): string {
  if (!c) return 'var(--seq-empty)'
  const v = valueOf(c)
  if (c.requests <= 0 || v === null) return 'var(--seq-zero)'
  const step = bins.value.bounds.findIndex((b) => (props.metric === 'requests' ? v <= b : v < b))
  return `var(--seq-${step < 0 ? 5 : step + 1})`
}

const rects = computed(() => props.cells.slice(0, DAYS.length * SLOTS).map((c, i) => ({
  i,
  x: LEFT + (i % SLOTS) * CW,
  y: TOP + Math.floor(i / SLOTS) * CH,
  fill: fill(c),
})))

const legend = computed(() => {
  const b = bins.value.bounds
  const steps = [0, 1, 2, 3, 4].map((k) => ({
    fill: `var(--seq-${k + 1})`,
    label: bins.value.label(k === 0 ? null : b[k - 1]!, k === 4 ? null : b[k]!),
  }))
  return [...steps, { fill: 'var(--seq-zero)', label: 'no requests' }, { fill: 'var(--seq-empty)', label: 'no measurements' }]
})

function describe(i: number): string {
  const day = DAYS[Math.floor(i / SLOTS)]
  const slot = i % SLOTS
  const when = `${day} ${clock(slot)}–${clock(slot + 1)}`
  const c = props.cells[i]
  if (!c) return `${when}: no measurements`
  if (c.requests <= 0) return `${when}: no requests`
  const parts = [`${Math.round(c.requests)} request${Math.round(c.requests) === 1 ? '' : 's'}`]
  if (c.avg != null) parts.push(`avg wait ${secs(c.avg)}`)
  if (c.p95 != null) parts.push(`p95 ${secs(c.p95)}`)
  if (c.share != null) parts.push(`${Math.round(c.share * 100)}% found it full`)
  return `${when}: ${parts.join(' · ')}`
}

// The tooltip floats over the page (fixed, in <body>) instead of sitting in
// the scrolling plot: inside it, a tooltip at the right edge or on the top
// row overflowed the plot and gave it scroll bars. It is kept inside the
// viewport: anchored to the cell's left or right edge near the sides, and
// below the cell when there is no room above.
const hovered = ref<number | null>(null)
const anchor = ref<{ x: number; top: number; bottom: number } | null>(null)
const TIP_HALF = 180
function enter(i: number, e: MouseEvent) {
  hovered.value = i
  const r = (e.currentTarget as Element).getBoundingClientRect()
  anchor.value = { x: r.left + r.width / 2, top: r.top, bottom: r.bottom }
}
function leave() {
  hovered.value = null
  anchor.value = null
}
const tip = computed(() => {
  if (hovered.value === null || !anchor.value) return null
  const { x, top, bottom } = anchor.value
  const vw = window.innerWidth
  const below = top < 48
  const ty = below ? '6px' : 'calc(-100% - 6px)'
  const tx = x < TIP_HALF ? '-8px' : x > vw - TIP_HALF ? 'calc(-100% + 8px)' : '-50%'
  return { text: describe(hovered.value), style: { left: `${x}px`, top: `${below ? bottom : top}px`, transform: `translate(${tx}, ${ty})` } }
})
// A fixed tooltip would stay put while the page scrolls under it.
onMounted(() => window.addEventListener('scroll', leave, { passive: true, capture: true }))
onBeforeUnmount(() => window.removeEventListener('scroll', leave, { capture: true }))
const hours = [0, 3, 6, 9, 12, 15, 18, 21]
</script>

<template>
  <figure class="heatmap">
    <div class="plot">
      <svg :viewBox="`0 0 ${width} ${height}`" role="img" :aria-label="`Model queue by weekday and 15-minute slot, ${timezone}`" @mouseleave="leave">
        <text v-for="h in hours" :key="h" :x="LEFT + h * 4 * CW" :y="TOP - 6" class="axis">{{ String(h).padStart(2, '0') }}</text>
        <text v-for="(d, r) in DAYS" :key="d" :x="LEFT - 8" :y="TOP + r * CH + CH / 2 + 4" class="axis day">{{ d }}</text>
        <rect
          v-for="c in rects" :key="c.i" :data-cell="c.i"
          :x="c.x + GAP / 2" :y="c.y + GAP / 2" :width="CW - GAP" :height="CH - GAP" rx="2"
          :fill="c.fill" :class="{ on: hovered === c.i }"
          @mouseenter="enter(c.i, $event)"
        ><title>{{ describe(c.i) }}</title></rect>
      </svg>
    </div>
    <Teleport to="body">
      <div v-if="tip" class="queue-tooltip" :style="tip.style" role="status">{{ tip.text }}</div>
    </Teleport>
    <figcaption class="legend">
      <span v-for="l in legend" :key="l.label" class="key"><i :style="{ background: l.fill }" />{{ l.label }}</span>
      <span class="muted">Times in {{ timezone }}</span>
    </figcaption>
  </figure>
</template>

<style scoped>
.heatmap { margin: 0; display: grid; gap: 8px; min-width: 0; }
.plot { position: relative; overflow-x: auto; }
svg { display: block; width: 100%; min-width: 640px; height: auto; }
.axis { fill: var(--muted); font: 600 11px var(--font-sans); }
.day { text-anchor: end; }
rect.on { stroke: var(--ink); stroke-width: 1.5; }
.queue-tooltip {
  position: fixed; z-index: 50; pointer-events: none; white-space: nowrap; max-width: calc(100vw - 16px); overflow: hidden; text-overflow: ellipsis;
  padding: 6px 10px; border-radius: var(--radius-xs); background: var(--panel); border: 2px solid var(--line-strong);
  color: var(--ink); font-size: 12.5px; box-shadow: 0 4px 12px var(--backdrop);
}
.legend { display: flex; flex-wrap: wrap; gap: 6px 14px; align-items: center; font-size: 12.5px; color: var(--ink); }
.key { display: inline-flex; align-items: center; gap: 6px; }
.key i { width: 14px; height: 14px; border-radius: 3px; display: inline-block; }
.legend .muted { margin-left: auto; }
</style>
