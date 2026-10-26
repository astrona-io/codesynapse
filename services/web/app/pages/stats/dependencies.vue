<script setup lang="ts">
import { useStatistics } from '~/composables/useStatistics'
const { s, error, pending } = useStatistics()
const w = computed(() => s.value?.workspace)
const l = computed(() => s.value?.licenses)
const licences = computed(() => (l.value?.by_license ?? []).map((c) => ({ label: c.license, value: c.count, detail: c.category })))
const projects = computed(() => Object.entries(l.value?.projects ?? {}).map(([label, value]) => ({ label, value })).sort((x, y) => y.value - x.value))
const conflicts = computed(() => (l.value?.conflicts ?? []).map((c) => ({ label: c.repo, value: c.count })))
const renovate = computed(() => (s.value?.hygiene.renovate_issues ?? []).map((x) => ({ label: x.repo, value: x.count })))

// The licence verdict of every dependency: what passes, what a person should
// look at, and what clashes with the repository's own licence.
const verdicts = computed(() => [
  { label: 'Compatible', value: l.value?.ok ?? 0, color: 'var(--success)' },
  { label: 'To review', value: l.value?.review ?? 0, color: 'var(--warning)' },
  { label: 'Conflict', value: l.value?.conflict ?? 0, color: 'var(--danger)' },
  { label: 'Unknown licence', value: l.value?.unknown ?? 0, color: 'var(--s-other)' },
])
const okPct = computed(() => (l.value?.dependencies ? Math.round((100 * l.value.ok) / l.value.dependencies) : 0))

// Licence families, so copyleft weight shows without reading every licence.
const CATEGORY: Record<string, string> = {
  'public-domain': 'Public domain', permissive: 'Permissive', 'weak-copyleft': 'Weak copyleft', 'strong-copyleft': 'Strong copyleft',
  'network-copyleft': 'Network copyleft', restricted: 'Restricted', unknown: 'Unknown',
}
const categories = computed(() => {
  const by: Record<string, number> = {}
  for (const c of l.value?.by_license ?? []) by[c.category] = (by[c.category] ?? 0) + c.count
  return Object.entries(by).map(([k, value]) => ({ label: CATEGORY[k] ?? k, value })).sort((x, y) => y.value - x.value)
})

const missing = computed(() => s.value?.hygiene.no_update_automation ?? [])
const covered = computed(() => Math.max(0, (w.value?.repos ?? 0) - missing.value.length))
const automationPct = computed(() => (w.value?.repos ? Math.round((100 * covered.value) / w.value.repos) : 0))
</script>

<template>
  <p v-if="error" class="panel error">{{ apiErrorMessage(error) }}</p>
  <div v-else-if="s && l" :class="{ refreshing: pending }" class="stack">
    <section class="panel">
      <p class="eyebrow">Dependencies &amp; licences</p><h2>{{ fmtCompact(l.dependencies) }} dependencies checked</h2>
      <p class="muted lead">Licences are read from the module files on disk, or from deps.dev when they are missing; only package names and versions leave the machine.</p>
      <div class="grid kpis">
        <StatTile label="Compatible" :value="`${okPct}%`" :hint="`${fmtInt(l.ok)} of ${fmtInt(l.dependencies)} dependencies`" />
        <StatTile label="Licence conflicts" :value="fmtInt(l.conflict)" :hint="`in ${l.conflicts.length} repositories`" />
        <StatTile label="To review" :value="fmtInt(l.review)" :hint="`${fmtInt(l.unknown)} without a known licence`" />
        <StatTile label="Vulnerable modules" :value="fmtInt(w?.vulnerable_modules ?? 0)" :hint="`${fmtInt(w?.unused_modules ?? 0)} modules never imported`" />
        <StatTile label="Without own licence" :value="l.unlicensed.length" hint="repositories" />
      </div>
    </section>

    <div class="grid cols-2">
      <section class="panel">
        <p class="eyebrow">Verdicts</p><h2>How dependencies fit their repository</h2>
        <DonutChart v-if="l.dependencies" :segments="verdicts" title="Licence verdicts" unit="dependencies" :center="`${okPct}%`" center-hint="compatible" :size="150" />
        <div v-else class="empty"><strong>No dependencies found</strong>No module files were found in the indexed repositories.</div>
      </section>
      <section class="panel">
        <p class="eyebrow">Licence families</p><h2>From permissive to copyleft</h2>
        <RankList :rows="categories" color="var(--s-1)" :limit="7" empty="No licences found." />
      </section>
    </div>

    <div class="grid cols-3">
      <section class="panel">
        <p class="eyebrow">In use</p><h2>Licences</h2>
        <RankList :rows="licences" color="var(--s-2)" />
      </section>
      <section class="panel">
        <p class="eyebrow">Repositories</p><h2>Their own licence</h2>
        <RankList :rows="projects" color="var(--s-4)" empty="No repository declares a licence." />
        <p v-if="l.unlicensed.length" class="muted small mono wrap">Without: {{ l.unlicensed.join(' · ') }}</p>
      </section>
      <section class="panel">
        <p class="eyebrow">Conflicts</p><h2>Where licences clash</h2>
        <RankList :rows="conflicts" color="var(--danger)" empty="No licence conflicts." />
      </section>
    </div>

    <section class="panel">
      <p class="eyebrow">Update automation</p><h2>{{ automationPct }}% of repositories keep dependencies current</h2>
      <div class="meter" role="img" :aria-label="`${covered} of ${w?.repos ?? 0} repositories use Renovate or Dependabot`">
        <span :style="{ width: `${automationPct}%` }" />
      </div>
      <p class="muted small">{{ covered }} of {{ w?.repos ?? 0 }} repositories use Renovate or Dependabot.</p>
      <div class="grid cols-2">
        <div><p class="label">Renovate configuration issues</p><RankList :rows="renovate" color="var(--warning)" empty="No Renovate issues." /></div>
        <div>
          <p class="label">No update automation</p>
          <ul v-if="missing.length" class="chips"><li v-for="m in missing" :key="m"><NuxtLink :to="`/repos/${encodeURIComponent(m)}`">{{ m }}</NuxtLink></li></ul>
          <p v-else class="muted small">Every repository has it.</p>
        </div>
      </div>
    </section>
  </div>
  <p v-else-if="s" class="empty"><strong>No dependency data yet</strong>It appears after the first ingest of a repository with module files.</p>
  <p v-else class="muted">Loading…</p>
</template>

<style scoped>
.small { font-size: 12.5px; }
.lead { max-width: 72ch; margin: 4px 0 0; }
.kpis { grid-template-columns: repeat(auto-fit, minmax(170px, 1fr)); margin: 16px 0 0; }
.label { display: block; margin: 14px 0 8px; }
.wrap { white-space: normal; word-break: break-word; margin-top: 10px; }
.meter { height: 10px; border-radius: 999px; background: var(--panel-2); overflow: hidden; margin: 14px 0 6px; }
.meter span { display: block; height: 100%; border-radius: 999px; background: var(--success); }
.chips { list-style: none; margin: 0; padding: 0; display: flex; flex-wrap: wrap; gap: 6px; }
.chips li { padding: 3px 10px; border-radius: var(--pill); background: var(--panel-2); font: 12px var(--font-mono); }
.refreshing { opacity: 0.6; transition: opacity 150ms; }
</style>
