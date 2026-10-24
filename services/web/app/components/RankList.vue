<script setup lang="ts">
// Ranked horizontal bars: one hue for magnitude, the value at the tip in text
// ink, and an optional second line of detail. The list itself is the table.
const props = withDefaults(defineProps<{
  rows: { label: string; value: number; detail?: string }[]
  color?: string
  limit?: number
  empty?: string
  // suffix follows each value ("%"); scale fixes the full bar (100 for shares).
  suffix?: string
  scale?: number
}>(), { color: 'var(--cyan)', limit: 10, empty: 'Nothing yet.', suffix: '', scale: 0 })
const shown = computed(() => props.rows.slice(0, props.limit))
const max = computed(() => props.scale || Math.max(1, ...shown.value.map((r) => r.value)))
</script>

<template>
  <ul v-if="shown.length" class="bar-list">
    <li v-for="r in shown" :key="r.label">
      <div class="head"><span class="name mono" :title="r.label">{{ r.label }}</span><span class="val">{{ fmtInt(r.value) }}{{ suffix }}</span></div>
      <div class="track"><span :style="{ width: `${(r.value / max) * 100}%`, background: color }" /></div>
      <div v-if="r.detail" class="muted detail">{{ r.detail }}</div>
    </li>
    <li v-if="rows.length > limit" class="muted detail">and {{ rows.length - limit }} more</li>
  </ul>
  <p v-else class="muted small">{{ empty }}</p>
</template>

<style scoped>
.bar-list { list-style: none; margin: 0; padding: 0; display: grid; gap: 10px; }
.head { display: flex; justify-content: space-between; gap: 12px; font-size: 13px; }
.name { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.val { font-variant-numeric: tabular-nums; color: var(--ink); }
.track { height: 6px; margin-top: 4px; }
.track > span { display: block; height: 100%; min-width: 2px; border-radius: 0 3px 3px 0; }
.detail { font-size: 12px; margin-top: 2px; }
.small { font-size: 12.5px; }
</style>
