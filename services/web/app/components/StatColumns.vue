<script setup lang="ts">
// Stacked columns over time: one column per bucket, segments in the fixed
// order of keys (colour follows the key, never its rank), 2px surface gaps,
// rounded tops on the baseline, a legend, a tooltip per column on hover or
// focus, and the same figures as a table for screen readers.
const props = defineProps<{
  buckets: { label: string; values: Record<string, number> }[]
  keys: { key: string; label: string; color: string }[]
  title: string
}>()

const totals = computed(() => props.buckets.map((b) => props.keys.reduce((t, k) => t + Math.max(0, b.values[k.key] ?? 0), 0)))
const peak = computed(() => Math.max(1, ...totals.value))
const height = (b: number, key: string) => (100 * Math.max(0, props.buckets[b]!.values[key] ?? 0)) / peak.value
const hover = ref<number | null>(null)
</script>

<template>
  <figure class="cols">
    <figcaption class="head">
      <span class="eyebrow">{{ title }}</span>
      <ul class="legend">
        <li v-for="k in keys" :key="k.key"><span class="key" :style="{ background: k.color }" />{{ k.label }}</li>
      </ul>
    </figcaption>
    <div v-if="buckets.length" class="plot" aria-hidden="true">
      <div
        v-for="(b, i) in buckets" :key="b.label" class="col" tabindex="-1"
        :class="{ dim: hover !== null && hover !== i }" @pointerenter="hover = i" @pointerleave="hover = null"
      >
        <template v-for="k in keys" :key="k.key">
          <span v-if="height(i, k.key) > 0" class="seg" :style="{ height: `${height(i, k.key)}%`, background: k.color }" />
        </template>
      </div>
      <div v-if="hover !== null" class="tip" role="status">
        <strong>{{ buckets[hover]!.label }}</strong>
        <span v-for="k in keys" :key="k.key"><span class="key" :style="{ background: k.color }" />{{ k.label }} {{ fmtInt(buckets[hover]!.values[k.key] ?? 0) }}</span>
      </div>
    </div>
    <p v-else class="muted small">Nothing in this window.</p>
    <div v-if="buckets.length" class="axis muted small" aria-hidden="true">
      <span>{{ buckets[0]!.label }}</span><span>peak {{ fmtCompact(peak) }}</span><span>{{ buckets[buckets.length - 1]!.label }}</span>
    </div>
    <table class="sr-only">
      <caption>{{ title }}</caption>
      <thead><tr><th>When</th><th v-for="k in keys" :key="k.key">{{ k.label }}</th></tr></thead>
      <tbody><tr v-for="b in buckets" :key="b.label"><td>{{ b.label }}</td><td v-for="k in keys" :key="k.key">{{ b.values[k.key] ?? 0 }}</td></tr></tbody>
    </table>
  </figure>
</template>

<style scoped>
.cols { margin: 0; display: grid; gap: 8px; }
.head { display: flex; justify-content: space-between; align-items: center; gap: 10px; flex-wrap: wrap; }
.legend { list-style: none; margin: 0; padding: 0; display: flex; gap: 12px; font-size: 12.5px; color: var(--muted); }
.legend li { display: inline-flex; align-items: center; gap: 6px; }
.key { display: inline-block; width: 10px; height: 10px; border-radius: 3px; }
.plot { position: relative; height: 140px; display: flex; align-items: flex-end; gap: 2px; border-bottom: 1px solid var(--line); }
.col { flex: 1 1 0; min-width: 2px; height: 100%; display: flex; flex-direction: column-reverse; gap: 2px; cursor: default; }
.col.dim { opacity: 0.45; }
.seg { display: block; width: 100%; min-height: 2px; }
.col .seg:last-child { border-radius: 4px 4px 0 0; }
.tip { position: absolute; top: 4px; right: 4px; display: grid; gap: 2px; padding: 8px 10px; background: var(--panel); border: 2px solid var(--line); border-radius: var(--radius-sm); font-size: 12.5px; pointer-events: none; }
.tip span { display: inline-flex; align-items: center; gap: 6px; font-variant-numeric: tabular-nums; }
.axis { display: flex; justify-content: space-between; }
</style>
