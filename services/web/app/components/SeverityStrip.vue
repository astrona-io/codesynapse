<script setup lang="ts">
// Counts per severity as one row of tinted cells, most severe first: the
// severity colours carry the tone, the number and word carry the meaning.
const props = withDefaults(defineProps<{ counts?: Record<string, number>; label: string; levels?: string[] }>(), {
  counts: () => ({}), levels: () => ['critical', 'high', 'medium', 'low', 'info'],
})
const cells = computed(() => props.levels.map((k) => ({ k, n: props.counts[k] ?? 0 })))
</script>

<template>
  <ul class="sev-strip" :aria-label="label">
    <li v-for="c in cells" :key="c.k" class="sev" :class="[`sev--${c.k}`, { zero: !c.n }]">
      <strong>{{ fmtCompact(c.n) }}</strong><span>{{ c.k }}</span>
    </li>
  </ul>
</template>

<style scoped>
.sev-strip { list-style: none; margin: 0; padding: 0; display: grid; grid-template-columns: repeat(auto-fit, minmax(72px, 1fr)); gap: 6px; }
.sev-strip li { display: grid; gap: 2px; padding: 10px 12px; border-radius: var(--radius-sm); }
.sev-strip strong { font: 700 22px/1.1 var(--font-display); font-variant-numeric: tabular-nums; }
.sev-strip span { font-size: 11.5px; text-transform: capitalize; }
.sev-strip li.zero { background: var(--panel-2); color: var(--muted); }
</style>
