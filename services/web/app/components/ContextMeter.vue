<script setup lang="ts">
// How full a model's context window got: a same-hue track filled to the
// typical request (median) with a tick at the 95th percentile, and the
// figures written out, so nothing depends on reading the bar.
const props = defineProps<{ p50?: number | null; p95?: number | null; window?: number }>()
const pct = (v?: number | null) => (v == null ? null : Math.min(100, Math.max(0, v * 100)))
const typical = computed(() => pct(props.p50))
const peak = computed(() => pct(props.p95))
const tokens = (v?: number | null) => (v == null || !props.window ? '' : ` (${fmtCompact(Math.round(v * props.window))})`)
const shown = (v: number | null) => (v === null ? '–' : v < 1 ? '<1%' : `${Math.round(v)}%`)
</script>

<template>
  <div class="ctx-meter" role="img" :aria-label="`Context use: typical ${shown(typical)}, 95th percentile ${shown(peak)}`">
    <div class="track">
      <span v-if="typical !== null" class="fill" :style="{ width: `${Math.max(typical, 0.5)}%` }" />
      <span v-if="peak !== null" class="tick" :style="{ left: `${peak}%` }" />
    </div>
    <div class="figures">
      <span><strong>{{ shown(typical) }}</strong>{{ tokens(p50) }} typical</span>
      <span class="muted"><strong>{{ shown(peak) }}</strong>{{ tokens(p95) }} at the 95th percentile</span>
    </div>
  </div>
</template>

<style scoped>
.ctx-meter { display: grid; gap: 6px; }
.track { position: relative; height: 10px; border-radius: 5px; background: var(--line); }
.fill { position: absolute; inset: 0 auto 0 0; border-radius: 5px; background: var(--s-1); }
.tick { position: absolute; top: -3px; bottom: -3px; width: 2px; margin-left: -1px; border-radius: 1px; background: var(--ink); }
.figures { display: flex; flex-wrap: wrap; gap: 4px 16px; font-size: 12.5px; color: var(--ink); }
.figures strong { color: var(--ink); font-variant-numeric: tabular-nums; }
</style>
