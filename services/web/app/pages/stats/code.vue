<script setup lang="ts">
import { useStatistics } from '~/composables/useStatistics'
import type { WorkspaceStats } from '~/utils/types'

const { days, s, error, pending } = useStatistics()
const w = computed(() => s.value?.workspace)
const SEV = ['critical', 'high', 'medium', 'low', 'info']
const findingsBySev = computed(() => SEV.map((k) => ({ label: k, value: w.value?.findings?.[k] ?? 0 })).filter((r) => r.value))
const vulnsBySev = computed(() => SEV.map((k) => ({ label: k, value: w.value?.vulns?.[k] ?? 0 })).filter((r) => r.value))
const grades = computed(() => ['A', 'B', 'C', 'D', 'E', 'F'].map((g) => ({ label: `Grade ${g}`, value: w.value?.grades?.[g] ?? 0 })).filter((r) => r.value))
const avg = (f: (r: WorkspaceStats['repos'][number]) => number) => {
  const rs = s.value?.repos ?? []
  return rs.length ? Math.round(rs.reduce((t, r) => t + f(r), 0) / rs.length) : 0
}
const rules = computed(() => (s.value?.hygiene.top_rules ?? []).map((r) => ({ label: r.rule, value: r.repos, detail: `${r.severity} · ${fmtInt(r.findings)} findings` })))
</script>

<template>
  <p v-if="error" class="panel error">{{ apiErrorMessage(error) }}</p>
  <div v-else-if="s" :class="{ refreshing: pending }" class="stack">
    <section v-if="w" class="panel">
      <p class="eyebrow">Code &amp; security</p><h2>{{ w.repos }} repositories, {{ fmtCompact(w.loc) }} lines</h2>
      <div class="grid kpis">
        <StatTile label="Findings" :value="fmtCompact(Object.values(w.findings).reduce((t, n) => t + n, 0))" :hint="`${w.findings.critical ?? 0} critical · ${w.findings.high ?? 0} high`" />
        <StatTile label="Vulnerabilities" :value="fmtCompact(Object.values(w.vulns).reduce((t, n) => t + n, 0))" :hint="`${w.vulns_called} called · ${w.vulnerable_modules} modules`" />
        <StatTile label="Test reach (avg)" :value="`${avg((x) => x.test_reach_pct)}%`" :hint="`doc coverage ${avg((x) => x.doc_coverage_pct)}%`" />
        <StatTile label="Unused code" :value="fmtCompact(w.dead_loc)" :hint="`lines · ${w.unused_modules} unused modules`" />
      </div>
      <div class="grid cols-3">
        <div><p class="label">Security grades</p><RankList :rows="grades" color="var(--info)" /></div>
        <div><p class="label">Findings by severity</p><RankList :rows="findingsBySev" color="var(--danger)" empty="No findings." /></div>
        <div><p class="label">Vulnerabilities by severity</p><RankList :rows="vulnsBySev" color="var(--danger)" empty="No known vulnerabilities." /></div>
      </div>
      <div><p class="label">Most widespread rules (repositories affected)</p><RankList :rows="rules" color="var(--warning)" :limit="12" /></div>
      <p class="muted small">Graph answers need {{ w.reduction_pct }}% fewer tokens than reading the files (token benchmark measured at ingest, {{ fmtCompact(w.tokens_saved) }} tokens saved over the sampled questions).</p>
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
</style>
