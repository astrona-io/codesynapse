<script setup lang="ts">
import { useStatistics } from '~/composables/useStatistics'
const { days, s, error, pending } = useStatistics()
const w = computed(() => s.value?.workspace)
const l = computed(() => s.value?.licenses)
const licences = computed(() => (l.value?.by_license ?? []).map((c) => ({ label: c.license, value: c.count, detail: c.category })))
const projects = computed(() => Object.entries(l.value?.projects ?? {}).map(([label, value]) => ({ label, value })).sort((x, y) => y.value - x.value))
const conflicts = computed(() => (l.value?.conflicts ?? []).map((c) => ({ label: c.repo, value: c.count })))
const automationPct = computed(() => {
  const total = w.value?.repos ?? 0
  const missing = s.value?.hygiene.no_update_automation.length ?? 0
  return total ? `${Math.round(((total - missing) / total) * 100)}%` : '–'
})
</script>

<template>
  <p v-if="error" class="panel error">{{ apiErrorMessage(error) }}</p>
  <div v-else-if="s" :class="{ refreshing: pending }" class="stack">
    <section v-if="l" class="panel">
      <p class="eyebrow">Dependencies &amp; licences</p><h2>{{ fmtCompact(l.dependencies) }} dependencies checked</h2>
      <div class="grid kpis">
        <StatTile label="Licence conflicts" :value="fmtInt(l.conflict)" :hint="`in ${l.conflicts.length} repositories`" />
        <StatTile label="To review" :value="fmtInt(l.review)" :hint="`${fmtInt(l.unknown)} without a known licence`" />
        <StatTile label="Without own licence" :value="l.unlicensed.length" hint="repositories" />
        <StatTile label="Update automation" :value="automationPct" :hint="`${s.hygiene.no_update_automation.length} repositories without Renovate or Dependabot`" />
      </div>
      <div class="grid cols-3">
        <div><p class="label">Licences in use</p><RankList :rows="licences" color="var(--accent)" /></div>
        <div><p class="label">Repository licences</p><RankList :rows="projects" color="var(--accent)" /></div>
        <div><p class="label">Repositories with conflicts</p><RankList :rows="conflicts" color="var(--danger)" empty="No licence conflicts." /></div>
      </div>
      <div v-if="s.hygiene.renovate_issues.length || s.hygiene.no_update_automation.length" class="grid cols-2">
        <div><p class="label">Renovate configuration issues</p><RankList :rows="s.hygiene.renovate_issues.map((x) => ({ label: x.repo, value: x.count }))" color="var(--warning)" empty="No Renovate issues." /></div>
        <div>
          <p class="label">No update automation</p>
          <p class="muted small mono wrap">{{ s.hygiene.no_update_automation.join(' · ') || '–' }}</p>
        </div>
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
.wrap { white-space: normal; word-break: break-word; }
</style>
