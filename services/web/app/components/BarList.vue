<script setup lang="ts">
// Horizontal bars for ranked values; labels sit outside the bar so long names
// stay readable.
const props = defineProps<{ items: { label: string; value: number; to?: string; title?: string }[]; unit?: string }>()
const maxValue = computed(() => Math.max(1, ...props.items.map((i) => i.value)))
</script>

<template>
  <div v-if="items.length" class="bars">
    <div v-for="item in items" :key="item.label" class="bar-row" :title="item.title ?? item.label">
      <div class="bar-label">
        <NuxtLink v-if="item.to" :to="item.to" class="truncate mono">{{ item.label }}</NuxtLink>
        <span v-else class="truncate mono">{{ item.label }}</span>
      </div>
      <div class="bar-track"><div class="bar-fill" :style="{ width: `${(item.value / maxValue) * 100}%` }" /></div>
      <div class="bar-value">{{ item.value.toLocaleString('en-US') }}{{ unit ?? '' }}</div>
    </div>
  </div>
  <p v-else class="muted">No data.</p>
</template>

<style scoped>
.bars { display: grid; gap: 6px; }
.bar-row { display: grid; grid-template-columns: minmax(0, 1.4fr) minmax(60px, 1fr) 56px; gap: 10px; align-items: center; }
.bar-track { height: 8px; background: var(--panel-2); border-radius: 4px; overflow: hidden; }
.bar-fill { height: 100%; background: var(--accent); border-radius: 4px; }
.bar-value { text-align: right; font-variant-numeric: tabular-nums; color: var(--muted); font-size: 12.5px; }
</style>
