<script setup lang="ts">
import { useStatistics } from '~/composables/useStatistics'
const { days, s, error, pending } = useStatistics()
const w = computed(() => s.value?.workspace)
</script>

<template>
  <p v-if="error" class="panel error">{{ apiErrorMessage(error) }}</p>
  <div v-else-if="s" :class="{ refreshing: pending }" class="stack">
    <section v-if="w" class="panel">
      <p class="eyebrow">Sync health</p><h2>{{ s.jobs.succeeded }} of {{ s.jobs.total }} jobs succeeded</h2>
      <div class="facts">
        <div><span class="label">Failed</span><strong>{{ s.jobs.failed }}</strong></div>
        <div><span class="label">Running</span><strong>{{ s.jobs.running }}</strong></div>
        <div><span class="label">Average</span><strong>{{ Math.round(s.jobs.avg_duration_sec) }} s</strong></div>
        <div><span class="label">Longest</span><strong>{{ Math.round(s.jobs.max_duration_sec) }} s</strong></div>
        <div><span class="label">Stale repositories</span><strong>{{ w.stale }}</strong></div>
      </div>
    </section>
  </div>
  <p v-else class="muted">Loading…</p>
</template>

<style scoped>
.small { font-size: 12.5px; }
.kpis { grid-template-columns: repeat(auto-fit, minmax(200px, 1fr)); margin: 12px 0; }
.label { display: block; margin: 12px 0 8px; }
h3 { margin: 18px 0 4px; font-size: 15px; }
.refreshing { opacity: 0.6; transition: opacity 150ms; }
.facts { display: grid; gap: 8px; }
.facts > div { display: flex; justify-content: space-between; border-bottom: 1px dashed var(--line); padding-bottom: 6px; }
</style>
