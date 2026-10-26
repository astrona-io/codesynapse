<script setup lang="ts">
// One horizontal 100% bar per row, split by category: colour follows the
// category (fixed order from keys, never re-ranked), 2px surface gaps
// between segments, the row total at the end, a legend, a tooltip per
// segment, and a table view.
const props = withDefaults(defineProps<{
  rows: { label: string; values: Record<string, number> }[]
  keys: { key: string; label: string; color: string }[]
  title: string
  unit?: string
}>(), { unit: '' })

const totals = computed(() => props.rows.map((r) => props.keys.reduce((t, k) => t + Math.max(0, r.values[k.key] ?? 0), 0)))
const share = (row: number, key: string) => (totals.value[row] ? (100 * Math.max(0, props.rows[row]!.values[key] ?? 0)) / totals.value[row]! : 0)
const hover = ref<{ row: number; key: string } | null>(null)
const tip = computed(() => {
  if (!hover.value) return null
  const k = props.keys.find((x) => x.key === hover.value!.key)
  const v = props.rows[hover.value.row]?.values[hover.value.key] ?? 0
  return { label: `${props.rows[hover.value.row]?.label} · ${k?.label}`, value: v, pct: share(hover.value.row, hover.value.key) }
})
</script>

<template>
  <figure class="stacked">
    <ul class="legend">
      <li v-for="k in keys" :key="k.key"><span class="key" :style="{ background: k.color }" />{{ k.label }}</li>
    </ul>
    <div v-for="(r, i) in rows" :key="r.label" class="bar-row">
      <div class="head"><span class="name mono" :title="r.label">{{ r.label }}</span><span class="val">{{ fmtCompact(totals[i]) }} {{ unit }}</span></div>
      <div class="bar">
        <template v-for="k in keys" :key="k.key">
          <span
            v-if="share(i, k.key) > 0" class="seg" tabindex="0"
            :style="{ width: `${share(i, k.key)}%`, background: k.color }"
            :class="{ dim: hover && !(hover.row === i && hover.key === k.key) }"
            :aria-label="`${r.label}, ${k.label}: ${fmtInt(r.values[k.key])} ${unit}, ${share(i, k.key).toFixed(1)}%`"
            @pointerenter="hover = { row: i, key: k.key }" @pointerleave="hover = null" @focus="hover = { row: i, key: k.key }" @blur="hover = null"
          />
        </template>
      </div>
    </div>
    <p v-if="tip" class="tip small" role="status"><strong>{{ fmtInt(tip.value) }}</strong> {{ unit }} · {{ tip.pct.toFixed(1) }}% · <span class="muted">{{ tip.label }}</span></p>
    <details class="table-view">
      <summary>Table</summary>
      <table>
        <thead><tr><th>{{ title }}</th><th v-for="k in keys" :key="k.key" class="num">{{ k.label }}</th></tr></thead>
        <tbody><tr v-for="r in rows" :key="r.label"><td class="mono">{{ r.label }}</td><td v-for="k in keys" :key="k.key" class="num">{{ fmtInt(r.values[k.key] ?? 0) }}</td></tr></tbody>
      </table>
    </details>
  </figure>
</template>

<style scoped>
.stacked { margin: 0; display: grid; gap: 10px; }
.legend { list-style: none; margin: 0; padding: 0; display: flex; flex-wrap: wrap; gap: 4px 14px; font-size: 12.5px; color: var(--muted); }
.legend li { display: flex; align-items: center; gap: 6px; }
.key { width: 10px; height: 10px; border-radius: 3px; }
.bar-row { display: grid; }
.head { display: flex; justify-content: space-between; gap: 12px; font-size: 13px; }
.name { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.val { font-variant-numeric: tabular-nums; color: var(--ink); }
.bar { display: flex; gap: 2px; height: 12px; margin-top: 4px; }
.seg { min-width: 3px; outline: none; transition: opacity 120ms; }
.seg:first-child { border-radius: 4px 0 0 4px; }
.seg:last-child { border-radius: 0 4px 4px 0; }
.seg:only-child { border-radius: 4px; }
.seg.dim { opacity: 0.4; }
.seg:focus-visible { box-shadow: 0 0 0 2px var(--line-strong); }
.tip { margin: 0; }
.tip strong { color: var(--ink); }
.small { font-size: 12.5px; }
.table-view summary { cursor: pointer; color: var(--muted); font-size: 12px; }
.table-view table { margin-top: 6px; }
</style>
