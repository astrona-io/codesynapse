<script setup lang="ts">
import type { ReferencePrice, SavingsBucket, SavingsSeries } from '~/utils/types'

// Savings over time: one stacked column per bucket of GET /api/v1/stats/savings
// (hours, six hours, days or weeks, cut in UTC), in tokens or in dollars; one
// axis per view, so the two scales never share a chart. The dollars use the
// money tiles' own formula (utils/savings.ts), so the columns add up to them.
// Every column has a tooltip with both views, times in the viewer's local
// time, and a table view carries every value.
const props = defineProps<{ series: SavingsSeries; price: ReferencePrice | null | undefined }>()

const view = ref<'tokens' | 'money'>('tokens')
watch(() => props.price, (p) => { if (!p) view.value = 'tokens' }, { immediate: true })

interface Row {
  b: SavingsBucket
  future: boolean
  tokens: { graph: number; local: number }
  money: { graph: number | null; local: number | null; prompts: number | null }
  total: number | null
}
const nowSec = Math.floor(Date.now() / 1000)
const rows = computed<Row[]>(() => props.series.buckets.map((b) => {
  const p = props.price
  const l = p ? localSaved(p, b.local_input_tokens, b.local_cached_tokens, b.local_output_tokens) : null
  const gw = b.gateway_saved_dollars
  return {
    b,
    future: b.start > nowSec,
    tokens: { graph: b.never_read_tokens, local: b.local_input_tokens + b.local_output_tokens },
    money: {
      graph: p ? b.graph_saved_dollars : null,
      local: l ? l.input + l.cached + l.output + (gw ?? 0) : null,
      prompts: p ? b.compression_saved_dollars : null,
    },
    total: totalSaved(p, { input: b.local_input_tokens, cached: b.local_cached_tokens, output: b.local_output_tokens }, [b.graph_saved_dollars, b.compression_saved_dollars, gw]),
  }
}))

const KEYS = {
  tokens: [
    { key: 'graph', label: 'Never read (graph answers)', color: 'var(--s-2)' },
    { key: 'local', label: 'Local models', color: 'var(--s-3)' },
  ],
  money: [
    { key: 'graph', label: 'Graph answers', color: 'var(--s-2)' },
    { key: 'local', label: 'Local models and gateway answers', color: 'var(--s-3)' },
    { key: 'prompts', label: 'Shorter prompts', color: 'var(--s-4)' },
  ],
} as const
const keys = computed(() => KEYS[view.value])
const valueOf = (r: Row, key: string) => ((r[view.value] as Record<string, number | null>)[key] ?? 0)
const fmt = (v: number) => (view.value === 'money' ? money(v) : fmtCompact(v))
const money = (v: number) => (v === 0 ? '$0' : v < 0.01 ? '<$0.01' : v >= 100 ? `$${Math.round(v).toLocaleString('en-US')}` : `$${v.toFixed(2)}`)
const totals = computed(() => keys.value.map((k) => ({ ...k, total: rows.value.reduce((t, r) => t + (r.future ? 0 : valueOf(r, k.key)), 0) })))

// Drawn at the container's own width, so the 4px rounded ends and the text
// keep their shape on a phone.
const box = ref<HTMLElement | null>(null)
const W = ref(640)
let ro: ResizeObserver | undefined
onMounted(() => {
  if (!box.value || typeof ResizeObserver === 'undefined') return
  ro = new ResizeObserver(([e]) => { if (e) W.value = Math.max(280, Math.round(e.contentRect.width)) })
  ro.observe(box.value)
})
onBeforeUnmount(() => ro?.disconnect())

const H = 200
const PAD = { top: 12, right: 8, bottom: 22, left: 48 }
const plotH = H - PAD.top - PAD.bottom
const base = PAD.top + plotH
const sumOf = (r: Row) => keys.value.reduce((t, k) => t + valueOf(r, k.key), 0)
function niceMax(v: number): number {
  if (v <= 0) return 1
  const p = 10 ** Math.floor(Math.log10(v))
  return [1, 2, 2.5, 5, 10].map((m) => m * p).find((m) => m >= v) ?? v
}
const maxV = computed(() => niceMax(Math.max(0, ...rows.value.map(sumOf))))
const slot = computed(() => (W.value - PAD.left - PAD.right) / Math.max(1, rows.value.length))
const barW = computed(() => Math.max(2, Math.min(24, slot.value - 2)))
const scale = (v: number) => (v / maxV.value) * plotH

// Segments bottom-up with a 2px surface gap between them; the top one alone
// gets the 4px rounded end, the baseline stays square.
function segments(i: number) {
  const r = rows.value[i]!
  const x = PAD.left + i * slot.value + (slot.value - barW.value) / 2
  const parts = keys.value.map((k) => ({ key: k.key, color: k.color, h: scale(valueOf(r, k.key)) })).filter((p) => p.h > 0)
  let top = base
  return parts.map((p, n) => {
    const gap = n > 0 ? 2 : 0
    const bottom = top - gap
    const y = Math.min(bottom - 1, top - p.h)
    top = y
    const rad = n === parts.length - 1 ? Math.min(4, bottom - y, barW.value / 2) : 0
    const w = barW.value
    const d = rad
      ? `M${x},${bottom}V${y + rad}Q${x},${y} ${x + rad},${y}H${x + w - rad}Q${x + w},${y} ${x + w},${y + rad}V${bottom}Z`
      : `M${x},${bottom}V${y}H${x + w}V${bottom}Z`
    return { key: p.key, color: p.color, d }
  })
}

// Axis labels: sub-day columns in local time, days and weeks by their UTC date.
const subDay = computed(() => props.series.step === 'hour' || props.series.step === '6h')
const hourFmt = new Intl.DateTimeFormat(undefined, { hour: '2-digit', minute: '2-digit' })
const dayHourFmt = new Intl.DateTimeFormat(undefined, { weekday: 'short', hour: '2-digit' })
const utcDayFmt = new Intl.DateTimeFormat(undefined, { month: 'short', day: 'numeric', timeZone: 'UTC' })
const tipFmt = new Intl.DateTimeFormat(undefined, { weekday: 'short', month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit' })
const tick = (b: SavingsBucket) => {
  const d = new Date(b.start * 1000)
  return props.series.step === 'hour' ? hourFmt.format(d) : props.series.step === '6h' ? dayHourFmt.format(d) : utcDayFmt.format(d)
}
const every = computed(() => {
  const fit = Math.max(2, Math.floor((W.value - PAD.left) / (subDay.value ? 64 : 52)))
  const n = Math.ceil(rows.value.length / fit)
  // Six-hour columns label whole days (every fourth column) at most.
  return props.series.step === '6h' ? Math.max(4, Math.ceil(n / 4) * 4) : Math.max(1, n)
})
// Day and week columns are UTC days, so the tooltip names those dates (the
// last day is the one before the end); hours are shown in local time.
const span = (b: SavingsBucket) => {
  if (subDay.value) return `${tipFmt.format(new Date(b.start * 1000))} – ${hourFmt.format(new Date(b.end * 1000))}`
  const last = utcDayFmt.format(new Date((b.end - 86400) * 1000))
  const first = utcDayFmt.format(new Date(b.start * 1000))
  return first === last ? `${first} (UTC)` : `${first} – ${last} (UTC)`
}

const STEP_NOTE: Record<SavingsSeries['step'], string> = {
  hour: 'One column per hour, in your local time.',
  '6h': 'One column per six hours (from 00:00, 06:00, 12:00 and 18:00 UTC); labels in your local time.',
  day: 'One column per day (UTC).',
  week: 'One column per week, Monday to Sunday (UTC); the first and last weeks can be partial.',
}

const hover = ref(-1)
const tip = computed(() => (hover.value >= 0 ? rows.value[hover.value] : undefined))
const tipLeft = computed(() => `${Math.min(Math.max(PAD.left + (hover.value + 0.5) * slot.value, 90), W.value - 90)}px`)
const unknown = (v: number | null) => (v === null ? 'unknown' : money(v))
</script>

<template>
  <figure class="savings">
    <div class="head">
      <div class="segmented" role="group" aria-label="Show">
        <button type="button" :aria-pressed="view === 'tokens'" @click="view = 'tokens'">Tokens</button>
        <button type="button" :aria-pressed="view === 'money'" :disabled="!price" :title="price ? undefined : 'no prices to estimate with'" @click="view = 'money'">Money (est.)</button>
      </div>
      <ul class="legend">
        <li v-for="k in totals" :key="k.key"><i :style="{ background: k.color }" />{{ k.label }} <strong>{{ fmt(k.total) }}</strong></li>
      </ul>
    </div>
    <div ref="box" class="plot">
      <svg :viewBox="`0 0 ${W} ${H}`" :width="W" :height="H" role="img" :aria-label="`Savings in ${view}, ${STEP_NOTE[series.step]}`">
        <line :x1="PAD.left" :x2="W - PAD.right" :y1="PAD.top" :y2="PAD.top" class="grid-line" />
        <line :x1="PAD.left" :x2="W - PAD.right" :y1="PAD.top + plotH / 2" :y2="PAD.top + plotH / 2" class="grid-line" />
        <line :x1="PAD.left" :x2="W - PAD.right" :y1="base" :y2="base" class="axis-line" />
        <text :x="PAD.left - 6" :y="PAD.top + 4" class="tick" text-anchor="end">{{ fmt(maxV) }}</text>
        <text :x="PAD.left - 6" :y="PAD.top + plotH / 2 + 4" class="tick" text-anchor="end">{{ fmt(maxV / 2) }}</text>
        <text :x="PAD.left - 6" :y="base + 4" class="tick" text-anchor="end">0</text>
        <g v-for="(r, i) in rows" :key="r.b.start" :class="{ dim: hover >= 0 && hover !== i }">
          <path v-for="seg in (r.future ? [] : segments(i))" :key="seg.key" :d="seg.d" :fill="seg.color" />
          <rect
            v-if="!r.future"
            :x="PAD.left + i * slot" :y="PAD.top" :width="slot" :height="plotH" class="hit" tabindex="0"
            :aria-label="`${span(r.b)}: ${fmtInt(r.tokens.graph)} tokens never read, ${fmtInt(r.tokens.local)} tokens on local models${r.total !== null ? `, ${money(r.total)} saved` : ''}`"
            @pointerenter="hover = i" @pointerleave="hover = -1" @focus="hover = i" @blur="hover = -1"
          />
          <text v-if="i % every === 0" :x="PAD.left + (i + 0.5) * slot" :y="H - 6" class="tick" text-anchor="middle">{{ tick(r.b) }}</text>
        </g>
      </svg>
      <div v-if="tip" class="tooltip" :style="{ left: tipLeft }" role="status">
        <div class="muted">{{ span(tip.b) }}</div>
        <div class="tip-row"><i style="background: var(--s-2)" />Never read <strong>{{ fmtInt(tip.tokens.graph) }}</strong><span v-if="price" class="muted">{{ unknown(tip.money.graph) }}</span></div>
        <div class="tip-row"><i style="background: var(--s-3)" />Local models <strong>{{ fmtInt(tip.tokens.local) }}</strong><span v-if="price" class="muted">{{ unknown(tip.money.local) }}</span></div>
        <div v-if="price" class="tip-row"><i style="background: var(--s-4)" />Shorter prompts <span class="muted">{{ unknown(tip.money.prompts) }}</span></div>
        <div v-if="price" class="tip-row total">Total saved (est.) <strong>{{ tip.total === null ? 'unknown' : money(tip.total) }}</strong></div>
      </div>
    </div>
    <figcaption class="muted small">
      {{ STEP_NOTE[series.step] }}
      <template v-if="!series.metrics"> Local model work needs live metrics, so only graph answers show<template v-if="subDay">, and they cannot be placed below a day</template>.</template>
      <template v-else-if="series.days > 30"> The metrics store keeps 30 days: local model work before that is not shown.</template>
      <template v-if="series.unplaced_never_read_tokens > 0"> {{ fmtCompact(series.unplaced_never_read_tokens) }} tokens never read<template v-if="price"> ({{ money(series.unplaced_graph_saved_dollars) }})</template> fell on days without an hourly measurement: they are in the totals above, in no column.</template>
    </figcaption>
    <details class="table-view">
      <summary>Table</summary>
      <div class="table-wrap">
        <table>
          <thead>
            <tr>
              <th>From</th><th class="num">Never read</th><th class="num">Local models</th>
              <template v-if="price"><th class="num">Graph answers</th><th class="num">Local and gateway</th><th class="num">Shorter prompts</th><th class="num">Total (est.)</th></template>
            </tr>
          </thead>
          <tbody>
            <tr v-for="r in rows.filter((x) => !x.future)" :key="r.b.start">
              <td class="mono">{{ subDay ? tipFmt.format(new Date(r.b.start * 1000)) : tick(r.b) }}</td>
              <td class="num">{{ fmtInt(r.tokens.graph) }}</td><td class="num">{{ fmtInt(r.tokens.local) }}</td>
              <template v-if="price"><td class="num">{{ unknown(r.money.graph) }}</td><td class="num">{{ unknown(r.money.local) }}</td><td class="num">{{ unknown(r.money.prompts) }}</td><td class="num">{{ unknown(r.total) }}</td></template>
            </tr>
          </tbody>
        </table>
      </div>
    </details>
  </figure>
</template>

<style scoped>
.savings { margin: 0; display: grid; gap: 8px; min-width: 0; }
.head { display: flex; flex-wrap: wrap; align-items: center; gap: 8px 16px; }
.legend { display: flex; flex-wrap: wrap; gap: 4px 16px; margin: 0; padding: 0; list-style: none; font-size: 12.5px; color: var(--muted); }
.legend li, .tip-row { display: inline-flex; align-items: center; gap: 6px; }
.legend i, .tip-row i { width: 10px; height: 10px; border-radius: 3px; display: inline-block; flex: none; }
.legend strong, .tip-row strong { color: var(--ink); font-variant-numeric: tabular-nums; }
.plot { position: relative; min-width: 0; }
svg { width: 100%; height: auto; display: block; overflow: visible; }
.grid-line { stroke: var(--line); stroke-width: 1; }
.axis-line { stroke: var(--line-strong); stroke-width: 1; }
.tick { fill: var(--muted); font: 10px var(--font-mono); font-variant-numeric: tabular-nums; }
g { transition: opacity 120ms; }
g.dim { opacity: 0.45; }
.hit { fill: transparent; cursor: default; outline: none; }
.hit:focus-visible { stroke: var(--focus-ring); stroke-width: 2; }
.tooltip { position: absolute; top: 0; transform: translate(-50%, -100%); pointer-events: none; padding: 6px 10px; border-radius: var(--radius-xs); background: var(--panel); border: 1px solid var(--line-strong); font-size: 12px; white-space: nowrap; z-index: 2; display: grid; gap: 2px; }
.tip-row .muted { margin-left: 4px; font-variant-numeric: tabular-nums; }
.tip-row.total { border-top: 1px solid var(--line); padding-top: 3px; margin-top: 2px; }
.small { font-size: 12.5px; }
.table-view summary { cursor: pointer; color: var(--muted); font-size: 12px; }
.table-wrap { overflow-x: auto; margin-top: 6px; }
</style>
