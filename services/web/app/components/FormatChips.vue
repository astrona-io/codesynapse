<script setup lang="ts">
// What a repository or source contains, in plain words: "Go · 120 files".
const props = defineProps<{ formats?: Record<string, number>; helmCharts?: number; k8s?: number; compact?: boolean }>()
const entries = computed(() => formatEntries(props.formats).map((e) => {
  if (e.key === 'helm' && props.helmCharts) return { ...e, text: plural(props.helmCharts, 'chart') }
  return { ...e, text: props.compact ? e.count.toLocaleString('en-US') : plural(e.count, 'file') }
}))
</script>

<template>
  <div class="formats">
    <span v-for="e in entries" :key="e.key" class="fmt" :class="`fmt--${e.group}`">
      <strong>{{ e.label }}</strong> <span>{{ e.text }}</span>
    </span>
    <span v-if="k8s" class="fmt fmt--config"><strong>Kubernetes</strong> <span>{{ plural(k8s, 'resource') }}</span></span>
    <span v-if="!entries.length && !k8s" class="muted">Nothing indexable found yet.</span>
  </div>
</template>

<style scoped>
.formats { display: flex; flex-wrap: wrap; gap: 6px; }
.fmt { display: inline-flex; gap: 6px; align-items: baseline; padding: 4px 10px; border-radius: var(--pill); font-size: 12.5px; border: 1px solid var(--line); background: var(--hover); }
.fmt strong { font-weight: 600; }
.fmt span { color: var(--muted); font-family: var(--font-mono); font-size: 11.5px; }
.fmt--code strong { color: var(--cyan); }
.fmt--config strong { color: var(--orange-ink); }
.fmt--docs strong { color: var(--violet); }
.fmt--deps strong { color: var(--green); }
</style>
