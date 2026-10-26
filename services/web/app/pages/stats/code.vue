<script setup lang="ts">
import { useStatistics } from '~/composables/useStatistics'
import type { WorkspaceStats } from '~/utils/types'

const { s, error, pending } = useStatistics()
const w = computed(() => s.value?.workspace)
const repos = computed(() => s.value?.repos ?? [])
const sumOf = (o?: Record<string, number>) => Object.values(o ?? {}).reduce((t, n) => t + n, 0)
const avg = (f: (r: WorkspaceStats['repos'][number]) => number) => {
  const rs = repos.value
  return rs.length ? Math.round(rs.reduce((t, r) => t + f(r), 0) / rs.length) : 0
}

// Grades as a strip of six cells; A and B read as healthy, D and below as work.
const GRADES = ['A', 'B', 'C', 'D', 'E', 'F']
const gradeTone = (g: string) => (g <= 'B' ? 'good' : g === 'C' ? 'warn' : 'bad')
const gradeCells = computed(() => GRADES.map((g) => ({ g, n: w.value?.grades?.[g] ?? 0 })))
const gradedRepos = computed(() => gradeCells.value.reduce((t, c) => t + c.n, 0))
const healthyPct = computed(() => (gradedRepos.value ? Math.round((100 * ((w.value?.grades?.A ?? 0) + (w.value?.grades?.B ?? 0))) / gradedRepos.value) : 0))

// Vulnerabilities an import path reaches against those only listed.
const vulnTotal = computed(() => sumOf(w.value?.vulns))
const vulnSplit = computed(() => [
  { label: 'Called from the code', value: w.value?.vulns_called ?? 0, color: 'var(--s-3)', detail: 'a call path reaches the vulnerable function' },
  { label: 'Present, not called', value: Math.max(0, vulnTotal.value - (w.value?.vulns_called ?? 0)), color: 'var(--s-other)', detail: 'imported or required only' },
])
const rules = computed(() => (s.value?.hygiene.top_rules ?? []).map((r) => ({ label: r.rule, value: r.repos, detail: `${r.severity} · ${fmtInt(r.findings)} findings` })))

// Weakest repositories first, so the table opens on what needs attention.
const showAll = ref(false)
const ranked = computed(() => [...repos.value].sort((a, b) => a.score - b.score || a.name.localeCompare(b.name)))
const shown = computed(() => (showAll.value ? ranked.value : ranked.value.slice(0, 10)))
const reachTone = (p: number) => (p >= 60 ? 'var(--success)' : p >= 30 ? 'var(--warning)' : 'var(--danger)')
</script>

<template>
  <p v-if="error" class="panel error">{{ apiErrorMessage(error) }}</p>
  <div v-else-if="s && w" :class="{ refreshing: pending }" class="stack">
    <section class="panel">
      <p class="eyebrow">Code &amp; security</p><h2>{{ fmtInt(w.repos) }} repositories, {{ fmtCompact(w.loc) }} lines</h2>
      <p class="muted lead">Measured at each repository's last ingest. Findings come from the security rules, vulnerabilities from OSV, test reach from the call graph.</p>
      <div class="grid kpis">
        <StatTile label="Findings" :value="fmtCompact(sumOf(w.findings))" :hint="`${w.findings.critical ?? 0} critical · ${w.findings.high ?? 0} high`" />
        <StatTile label="Vulnerabilities" :value="fmtCompact(vulnTotal)" :hint="`${w.vulns_called} called · ${w.vulnerable_modules} modules`" />
        <StatTile label="Test reach (avg)" :value="`${avg((x) => x.test_reach_pct)}%`" :hint="`doc coverage ${avg((x) => x.doc_coverage_pct)}%`" />
        <StatTile label="Unused code" :value="fmtCompact(w.dead_loc)" :hint="`lines · ${w.unused_modules} unused modules`" />
        <StatTile label="Tokens saved" :value="`${w.reduction_pct}%`" :hint="`${fmtCompact(w.tokens_saved)} tokens over the benchmark questions`" />
      </div>
    </section>

    <div class="grid cols-2">
      <section class="panel">
        <p class="eyebrow">Grades</p><h2>{{ healthyPct }}% graded A or B</h2>
        <ul class="grades" aria-label="Repositories per security grade">
          <li v-for="c in gradeCells" :key="c.g" :class="[`tone-${gradeTone(c.g)}`, { zero: !c.n }]">
            <span class="letter">{{ c.g }}</span><strong>{{ c.n }}</strong>
          </li>
        </ul>
        <p class="muted small">Each repository's grade weighs its findings and reachable vulnerabilities against its size.</p>
      </section>
      <section class="panel">
        <p class="eyebrow">Vulnerabilities</p><h2>{{ fmtInt(w.vulns_called) }} reachable from the code</h2>
        <DonutChart v-if="vulnTotal" :segments="vulnSplit" title="Called and listed vulnerabilities" unit="vulnerabilities" :center="fmtCompact(vulnTotal)" center-hint="known" :size="150" />
        <div v-else class="empty"><strong>No known vulnerabilities</strong>OSV knows nothing against the versions in use.</div>
      </section>
    </div>

    <section class="panel">
      <p class="eyebrow">Severity</p><h2>Findings and vulnerabilities by severity</h2>
      <div class="grid cols-2 sev-grid">
        <div><p class="label">Security findings</p><SeverityStrip :counts="w.findings" label="Findings by severity" /></div>
        <div><p class="label">Dependency vulnerabilities</p><SeverityStrip :counts="w.vulns" label="Vulnerabilities by severity" :levels="['critical', 'high', 'medium', 'low']" /></div>
      </div>
      <p class="label">Most widespread rules (repositories affected)</p>
      <RankList :rows="rules" color="var(--warning)" :limit="10" empty="No findings: every rule passed." />
    </section>

    <section v-if="repos.length" class="panel">
      <p class="eyebrow">Per repository</p><h2>Weakest first</h2>
      <div class="table-wrap">
        <table>
          <thead><tr><th>Repository</th><th>Grade</th><th class="num">Critical / high</th><th class="num">Vulns (called)</th><th>Test reach</th><th class="num">Docs</th><th class="num">Lines</th><th class="num">Unused</th></tr></thead>
          <tbody>
            <tr v-for="r in shown" :key="r.name">
              <td><NuxtLink :to="`/repos/${encodeURIComponent(r.name)}`">{{ r.name }}</NuxtLink></td>
              <td><span class="grade" :class="`tone-${gradeTone(r.grade)}`">{{ r.grade || '–' }}</span></td>
              <td class="num">{{ fmtInt(r.findings?.critical ?? 0) }} / {{ fmtInt(r.findings?.high ?? 0) }}</td>
              <td class="num">{{ fmtInt(sumOf(r.vulns)) }} <span class="muted">({{ fmtInt(r.vulns_called) }})</span></td>
              <td><span class="reach"><span class="reach__track"><span :style="{ width: `${r.test_reach_pct}%`, background: reachTone(r.test_reach_pct) }" /></span>{{ r.test_reach_pct }}%</span></td>
              <td class="num">{{ r.doc_coverage_pct }}%</td>
              <td class="num">{{ fmtCompact(r.loc) }}</td>
              <td class="num">{{ fmtCompact(r.dead_loc) }}</td>
            </tr>
          </tbody>
        </table>
      </div>
      <button v-if="ranked.length > 10" class="secondary more" @click="showAll = !showAll">{{ showAll ? 'Show the weakest 10' : `Show all ${ranked.length}` }}</button>
    </section>
  </div>
  <p v-else-if="s" class="empty"><strong>No repositories indexed yet</strong>Add a repository to see its grades and findings here.</p>
  <p v-else class="muted">Loading…</p>
</template>

<style scoped>
.small { font-size: 12.5px; }
.lead { max-width: 72ch; margin: 4px 0 0; }
.kpis { grid-template-columns: repeat(auto-fit, minmax(170px, 1fr)); margin: 16px 0 0; }
.label { display: block; margin: 14px 0 8px; }
.sev-grid { gap: 20px; }
.grades { list-style: none; margin: 14px 0 10px; padding: 0; display: grid; grid-template-columns: repeat(6, 1fr); gap: 6px; }
.grades li { display: grid; justify-items: center; gap: 2px; padding: 12px 4px; border-radius: var(--radius-sm); }
.grades .letter { font: 800 13px var(--font-mono); }
.grades strong { font: 700 24px/1.1 var(--font-display); font-variant-numeric: tabular-nums; }
.tone-good { background: var(--success-soft); color: var(--success); }
.tone-warn { background: var(--warning-soft); color: var(--warning); }
.tone-bad { background: var(--danger-soft); color: var(--danger); }
.grades li.zero { background: var(--panel-2); color: var(--muted); }
.grade { display: inline-block; min-width: 26px; text-align: center; padding: 2px 8px; border-radius: var(--pill); font: 700 12px var(--font-mono); }
.reach { display: inline-flex; align-items: center; gap: 8px; font-variant-numeric: tabular-nums; }
.reach__track { width: 64px; height: 6px; border-radius: 999px; background: var(--line); overflow: hidden; display: inline-block; }
.reach__track span { display: block; height: 100%; border-radius: 999px; }
.table-wrap { margin-top: 12px; overflow-x: auto; }
.more { margin-top: 12px; }
.refreshing { opacity: 0.6; transition: opacity 150ms; }
</style>
