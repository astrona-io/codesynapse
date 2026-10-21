<script setup lang="ts">
import type { WorkspaceOverview } from '~/utils/types'

const { data: ov, error, refresh } = useApiGet<WorkspaceOverview>('overview')
const SEV = ['critical', 'high', 'medium', 'low', 'info']
const GRADES = ['A', 'B', 'C', 'D', 'F']
const gradeColor: Record<string, string> = { A: 'var(--green)', B: 'var(--cyan)', C: 'var(--amber)', D: 'var(--orange)', F: 'var(--pink)' }
const sorted = (m: Record<string, number> | undefined) =>
  Object.entries(m ?? {}).sort((a, b) => b[1] - a[1]).map(([label, value]) => ({ label, value }))
const sevBars = (m: Record<string, number> | undefined) => SEV.filter((s) => (m ?? {})[s]).map((s) => ({ label: s, value: m![s]! }))
const crit = computed(() => (ov.value?.totals.findings.critical ?? 0) + (ov.value?.totals.findings.high ?? 0))
const age = (h: number) => (h < 1 ? `${Math.round(h * 60)} min` : h < 48 ? `${Math.round(h)} h` : `${Math.round(h / 24)} d`)
// The table lists 25 repositories per page; the totals above cover all of them.
const PAGE_SIZE = 25
const page = ref(1)
const pageCount = computed(() => Math.max(1, Math.ceil((ov.value?.repos.length ?? 0) / PAGE_SIZE)))
watch(pageCount, (n) => { if (page.value > n) page.value = n })
const pageRepos = computed(() => (ov.value?.repos ?? []).slice((page.value - 1) * PAGE_SIZE, page.value * PAGE_SIZE))
const repoLink = (r: string, p = '') => `/repos/${encodeURIComponent(r)}${p}`
</script>

<template>
  <div class="stack">
    <PageHeader
      eyebrow="Operator" title="Workspace overview"
      lede="Everything CodeSynapse has indexed, in one place: security posture per repository, graph size, freshness, ingest health and how much context the graph saves agents."
      :crumbs="[{ label: 'Workspace' }, { label: 'Overview' }]"
    >
      <span v-if="ov" class="pill"><span class="dot" :class="ov.jobs.running ? 'dot--warn' : 'dot--ok'" />{{ ov.jobs.running ? `${ov.jobs.running} job(s) running` : 'Idle' }}</span>
      <button class="secondary" @click="refresh()">Refresh</button>
    </PageHeader>

    <p v-if="error" class="panel error">{{ apiErrorMessage(error) }}</p>
    <template v-else-if="ov">
      <div class="grid grid--stats">
        <StatTile label="Repositories" :value="ov.totals.repos" :hint="`${ov.totals.stale} stale (> 7 days) · ${fmtInt(ov.totals.loc)} LOC`" />
        <StatTile label="Context reduced" :value="`${ov.totals.reduction_pct.toFixed(1)}%`" :hint="`${fmtInt(ov.totals.tokens_saved)} tokens not spent in benchmarks`" />
        <StatTile label="Graph size" :value="fmtInt(ov.totals.nodes)" :hint="`nodes · ${fmtInt(ov.totals.edges)} edges · ${fmtInt(ov.totals.dead_loc)} dead LOC · ${ov.totals.unused_modules} unused modules`" />
        <StatTile label="Critical + high" :value="crit" :hint="`code findings · ${ov.totals.vulns_called} CVEs called · ${ov.totals.vulnerable_modules} vulnerable modules`" />
      </div>

      <section class="panel edge--pink">
        <div class="panel__header">
          <div><p class="eyebrow">Security posture</p><h2>Repositories, worst first</h2></div>
          <div class="row grades">
            <span v-for="g in GRADES" :key="g" class="grade-chip" :style="{ color: gradeColor[g], borderColor: gradeColor[g] }">{{ g }} · {{ ov.totals.grades[g] ?? 0 }}</span>
          </div>
        </div>
        <div class="table-wrap">
          <table>
            <thead>
              <tr>
                <th>Repository</th><th>Grade</th><th class="num">Crit</th><th class="num">High</th><th class="num">Med</th>
                <th class="num">CVEs</th><th class="num">Called</th><th class="num">Tests reach</th><th class="num">Docs</th>
                <th class="num">Dead code</th><th class="num">Reduced</th><th class="num">Nodes</th><th>Indexed</th>
              </tr>
            </thead>
            <tbody>
              <tr v-for="r in pageRepos" :key="r.name">
                <td>
                  <NuxtLink :to="repoLink(r.name)"><strong>{{ r.name }}</strong></NuxtLink>
                  <div class="mono dim">{{ r.branch || '–' }} · {{ r.managed ? 'cloned' : 'mounted' }}</div>
                </td>
                <td><NuxtLink :to="repoLink(r.name, '/security')" class="grade" :style="{ color: gradeColor[r.grade] }">{{ r.grade }}</NuxtLink> <span class="dim mono">{{ r.score }}</span></td>
                <td class="num" :class="{ hot: r.findings.critical }">{{ r.findings.critical ?? 0 }}</td>
                <td class="num" :class="{ warm: r.findings.high }">{{ r.findings.high ?? 0 }}</td>
                <td class="num">{{ r.findings.medium ?? 0 }}</td>
                <td class="num">{{ Object.values(r.vulns).reduce((a, b) => a + b, 0) }}</td>
                <td class="num" :class="{ hot: r.vulns_called }">{{ r.vulns_called }}</td>
                <td class="num">{{ r.test_reach_pct.toFixed(0) }}%</td>
                <td class="num">{{ r.doc_coverage_pct.toFixed(0) }}%</td>
                <td class="num"><NuxtLink :to="repoLink(r.name, '/cleanup')" :class="{ warm: r.dead_functions }">{{ r.dead_functions }}</NuxtLink> <span class="dim">{{ fmtInt(r.dead_loc) }} loc</span><span v-if="r.unused_modules" class="dim"> · {{ r.unused_modules }} mods</span></td>
                <td class="num saving">{{ r.reduction_pct ? `${r.reduction_pct.toFixed(0)}%` : '–' }}</td>
                <td class="num">{{ fmtInt(r.nodes) }}</td>
                <td><span class="badge" :class="r.stale ? 'badge--amber' : 'badge--green'">{{ age(r.age_hours) }}</span></td>
              </tr>
            </tbody>
          </table>
        </div>
        <nav v-if="pageCount > 1" class="pager" aria-label="Repository pages">
          <button class="secondary tiny" :disabled="page <= 1" @click="page--">Previous</button>
          <span class="muted small">Page {{ page }} of {{ pageCount }} · {{ plural(ov.repos.length, 'repository', 'repositories') }}</span>
          <button class="secondary tiny" :disabled="page >= pageCount" @click="page++">Next</button>
        </nav>
        <div v-if="!ov.repos.length" class="empty"><strong>Nothing indexed yet</strong>Add a repository on the Repositories page.</div>
      </section>

      <div class="grid cols-2">
        <section class="panel edge--orange">
          <p class="eyebrow">Findings</p><h2>Code & config findings by severity</h2>
          <BarList :items="sevBars(ov.totals.findings)" />
        </section>
        <section class="panel edge--pink">
          <p class="eyebrow">Dependencies</p><h2>Vulnerabilities by severity</h2>
          <BarList :items="sevBars(ov.totals.vulns)" />
        </section>
        <section class="panel">
          <p class="eyebrow">Index</p><h2>Nodes by kind</h2>
          <BarList :items="sorted(ov.totals.node_kinds)" />
        </section>
        <section class="panel">
          <p class="eyebrow">Index</p><h2>Edges by kind</h2>
          <BarList :items="sorted(ov.totals.edge_kinds)" />
        </section>
        <section class="panel edge--violet">
          <p class="eyebrow">Attack surface</p><h2>Functions per sensitive API</h2>
          <BarList :items="sorted(ov.totals.security_tags)" unit=" fn" />
        </section>
        <section class="panel edge--cyan">
          <p class="eyebrow">Operations</p><h2>Ingest jobs <span class="muted">· since API start</span></h2>
          <div class="grid grid--stats tight">
            <StatTile label="Succeeded" :value="ov.jobs.succeeded" />
            <StatTile label="Failed" :value="ov.jobs.failed" />
            <StatTile label="Avg duration" :value="`${ov.jobs.avg_duration_sec}s`" :hint="`max ${ov.jobs.max_duration_sec}s`" />
          </div>
          <div v-if="ov.jobs.recent_failures.length" class="table-wrap">
            <table><tbody>
              <tr v-for="j in ov.jobs.recent_failures" :key="j.id"><td class="mono">{{ j.repo }}</td><td class="mono dim">{{ j.kind }}</td><td class="error">{{ j.error }}</td></tr>
            </tbody></table>
          </div>
          <p v-else class="muted">No failed jobs.</p>
        </section>
      </div>
      <p class="muted small mono">Generated {{ relTime(ov.generated_at) }}</p>
    </template>
    <p v-else class="muted">Loading…</p>
  </div>
</template>

<style scoped>
.grades { gap: 6px; }
.grade-chip { font: 500 11px var(--font-mono); padding: 3px 10px; border: 1px solid; border-radius: var(--pill); }
.grade { font-size: 18px; font-weight: 700; }
.hot { color: var(--pink); font-weight: 700; }
.warm { color: var(--orange-ink); font-weight: 700; }
.saving { color: var(--green); }
.tight { gap: 12px; }
.small { font-size: 12px; }
.pager { display: flex; justify-content: center; align-items: center; gap: 14px; padding-top: 14px; }
.small { font-size: 12.5px; }
</style>
