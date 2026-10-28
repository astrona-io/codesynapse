<script setup lang="ts">
import type { Attention, CleanupReport, Dashboard, MetricsReport, SecurityReport } from '~/utils/types'

const route = useRoute()
const repo = computed(() => route.params.repo as string)
const base = computed(() => `/repos/${encodeURIComponent(repo.value)}`)
const { data: d, error } = useApiGet<Dashboard>(() => repoPath(repo.value, 'dashboard'))
const { data: attention } = useApiGet<Attention>(() => `attention?repo=${encodeURIComponent(repo.value)}`)
// The cards and next steps read a few fields of three large reports; only
// those are kept, so the page payload does not carry every finding.
const { data: sec } = useApiGet(() => repoPath(repo.value, 'security'), { view: {
  name: 'summary',
  pick: (s: SecurityReport) => ({
    grade: s.grade, finding_counts: s.finding_counts ?? {}, vuln_counts: s.vuln_counts ?? {},
    findings: (s.findings ?? []).filter((f) => f.severity === 'critical' || (f.severity === 'high' && f.exposed)),
    vulnerabilities: (s.vulnerabilities ?? []).filter((v) => v.reachability !== 'required' && v.fixed),
  }),
} })
const { data: clean } = useApiGet(() => repoPath(repo.value, 'cleanup'), { view: {
  name: 'summary',
  pick: (c: CleanupReport) => {
    const mods = c.modules ?? []
    const mod = mods.find((m) => m.vulnerabilities?.length) ?? mods[0]
    return { summary: c.summary, modules: mod ? [mod] : [], items: (c.items ?? []).filter((i) => i.security?.length) }
  },
} })
const { data: met } = useApiGet(() => repoPath(repo.value, 'metrics'), { view: {
  name: 'summary',
  pick: (m: MetricsReport) => ({
    tests: m.tests && { ...m.tests, untested_complex: (m.tests.untested_complex ?? []).slice(0, 1) },
    benchmark: m.benchmark && { samples: m.benchmark.samples, median_reduction_pct: m.benchmark.median_reduction_pct },
  }),
} })

const explore = (id: string) => ({ path: `${base.value}/explore`, query: { id } })
const { data: repos, refresh: refreshRepos } = useApiGet<{ repos: { name: string; managed?: boolean; remote?: string; branch?: string }[] }>('repos')
const repoInfo = computed(() => repos.value?.repos.find((r) => r.name === repo.value))
const isManaged = computed(() => repoInfo.value?.managed ?? false)
const ageDays = computed(() => (d.value?.ingested_at ? (Date.now() - Date.parse(d.value.ingested_at)) / 86_400_000 : 0))

// --- health cards ---------------------------------------------------------
const secCard = computed(() => {
  const s = sec.value
  if (!s) return null
  const urgent = (s.finding_counts.critical ?? 0) + (s.finding_counts.high ?? 0)
  const vulns = Object.values(s.vuln_counts).reduce((a, b) => a + b, 0)
  const tone = s.grade === 'A' ? 'good' : s.grade === 'B' || s.grade === 'C' ? 'warn' : 'bad'
  const parts = [urgent ? `${plural(urgent, 'critical/high issue')}` : 'no critical or high issues', vulns ? plural(vulns, 'known vulnerability', 'known vulnerabilities') : 'no known vulnerabilities']
  return { value: s.grade, tone, verdict: `${GRADE_TEXT[s.grade]} — ${parts.join(', ')}.` }
})
const testCard = computed(() => {
  const t = met.value?.tests
  if (!t || !t.total) return null
  const pctv = Math.round(t.reachable_pct)
  return { value: `${pctv}%`, tone: pctv >= 70 ? 'good' : pctv >= 40 ? 'warn' : 'bad', verdict: `${t.reachable} of ${plural(t.total, 'function')} are reached by a test.` }
})
const cleanCard = computed(() => {
  const c = clean.value?.summary
  if (!c) return null
  const n = c.unreachable + c.test_only
  if (!n && !c.unused_modules) return { value: '0', tone: 'good', verdict: 'No unused code found.' }
  const mods = c.unused_modules ? `, ${plural(c.unused_modules, 'unused dependency', 'unused dependencies')}` : ''
  return { value: String(n), tone: c.security_relevant ? 'bad' : 'warn', verdict: `${plural(n, 'unused function')} (${plural(c.dead_loc, 'line')})${mods} can be removed.` }
})
const docCard = computed(() => {
  const s = d.value?.stats
  if (!s || !s.exported) return null
  const p = Math.round((s.documented / s.exported) * 100)
  return { value: `${p}%`, tone: p >= 80 ? 'good' : p >= 50 ? 'warn' : 'bad', verdict: `${s.documented} of ${plural(s.exported, 'public symbol')} have a doc comment.` }
})
const savingsCard = computed(() => {
  const b = met.value?.benchmark
  if (!b?.samples) return null
  return { value: `${(b.median_reduction_pct ?? 0).toFixed(0)}%`, tone: 'good', verdict: 'less context an AI agent needs per question, compared with reading the files.' }
})

// --- next steps -----------------------------------------------------------
interface Step { tone: 'bad' | 'warn' | 'neutral'; title: string; why: string; to: string | { path: string; query?: Record<string, string> }; cta: string }
const steps = computed<Step[]>(() => {
  const out: Step[] = []
  const s = sec.value
  if (s) {
    const urgent = s.findings // critical, or high and exposed
    if (urgent.length) {
      const f = urgent[0]!
      out.push({ tone: 'bad', title: `Fix ${plural(urgent.length, 'urgent security issue')}`, why: `Start with “${f.title}” in ${f.location}${f.exposed ? ', reachable from outside' : ''}.`, to: `${base.value}/security`, cta: 'See how to fix' })
    }
    const reached = s.vulnerabilities // fixable and not only required
    if (reached.length) {
      const v = reached[0]!
      out.push({ tone: v.reachability === 'called' ? 'bad' : 'warn', title: `Upgrade ${v.module} to ${v.fixed}`, why: `${v.id}${v.cve?.length ? ` (${v.cve[0]})` : ''}: ${v.summary}. ${REACH_TEXT[v.reachability]}.`, to: `${base.value}/security`, cta: 'Review vulnerability' })
    }
  }
  const c = clean.value
  if (c) {
    const mod = c.modules[0] // the first with vulnerabilities, else the first
    if (mod) out.push({ tone: mod.vulnerabilities?.length ? 'bad' : 'warn', title: `Remove unused dependency ${mod.path}`, why: mod.vulnerabilities?.length ? `Nothing imports it, and removing it eliminates ${plural(mod.vulnerabilities.length, 'vulnerability', 'vulnerabilities')}.` : 'Nothing in the repository imports it.', to: `${base.value}/cleanup`, cta: 'Open cleanup' })
    const risky = c.items // the ones with security impact
    if (risky.length) out.push({ tone: 'warn', title: `Delete ${plural(risky.length, 'unused function')} with security impact`, why: `For example ${risky[0]!.name}: ${risky[0]!.security![0]}. Dead code is still shipped and attackable.`, to: `${base.value}/cleanup`, cta: 'Open cleanup' })
  }
  const gap = met.value?.tests.untested_complex?.[0]
  if (gap) out.push({ tone: 'warn', title: `Add tests for ${gap.name}`, why: 'It is complex and no test reaches it, so changes here are risky.', to: explore(gap.id), cta: 'Open symbol' })
  const undoc = d.value?.undocumented?.[0]
  if (undoc && undoc.score > 1) out.push({ tone: 'neutral', title: `Document ${shortName(undoc.node)}`, why: `Public, called from ${plural(undoc.score, 'place')}, and has no doc comment.`, to: explore(undoc.node.id), cta: 'Open symbol' })
  if (ageDays.value > 7) out.push({ tone: 'neutral', title: 'Refresh the index', why: `Last indexed ${relTime(d.value?.ingested_at)}; answers may be out of date.`, to: base.value, cta: 'Use Sync above' })
  return out.slice(0, 5)
})
</script>

<template>
  <p v-if="error" class="panel error">{{ apiErrorMessage(error) }}</p>
  <div v-else-if="d" class="stack">
    <PageHeader eyebrow="Repository summary" :title="repo" :crumbs="[{ label: 'Repositories', to: '/' }, { label: repo }]">
      <template #meta>
        <p class="muted">
          <span v-if="d.head">Commit <span class="mono">{{ d.head.slice(0, 10) }}</span> · </span>indexed {{ relTime(d.ingested_at) }}
          · {{ fmtInt(d.stats.total_loc) }} lines of code
        </p>
      </template>
      <SyncControl :repo="repo" />
      <SwitchToRemote v-if="repos && !isManaged" :repo="repo" :remote="repoInfo?.remote" :branch="repoInfo?.branch" @switched="refreshRepos" />
      <RemoveRepo :repo="repo" :managed="isManaged" @removed="navigateTo('/')" />
    </PageHeader>
    <div class="panel autosync-bar"><AutoSync :repo="repo" /></div>

    <section v-if="attention" class="panel">
      <div class="panel__header"><h2>Needs attention <span class="muted">{{ attention.items.length }}</span></h2></div>
      <AttentionList v-if="attention.items.length" :items="attention.items" :show-repo="false" :limit="6" />
      <p v-else class="notice notice--ok">Nothing flagged: no critical or high findings, no vulnerable or conflicting dependencies, update automation in place, and indexed recently.</p>
    </section>

    <section class="panel">
      <div class="panel__header">
        <div><p class="eyebrow">What's indexed</p><h2>Everything CodeSynapse understands in this repository</h2></div>
      </div>
      <FormatChips :formats="d.stats.formats" :helm-charts="d.stats.helm_charts" :k8s="d.stats.k8s_resources" />
      <div class="row quick">
        <NuxtLink v-if="d.stats.k8s_resources || d.stats.helm_charts || d.stats.formats?.yaml" :to="`${base}/config`" class="btn btn--ghost">Kubernetes & Helm</NuxtLink>
        <NuxtLink v-if="d.stats.formats?.markdown" :to="`${base}/docs`" class="btn btn--ghost">Read the docs</NuxtLink>
        <NuxtLink :to="`${base}/graph`" class="btn btn--ghost">Explore the graph</NuxtLink>
      </div>
    </section>

    <div class="grid grid--health">
      <HealthCard v-if="secCard" label="Security" :value="secCard.value" :verdict="secCard.verdict" :tone="secCard.tone as any" :to="`${base}/security`" action="Open security review" />
      <HealthCard v-if="testCard" label="Test reach" tip="tested" :value="testCard.value" :verdict="testCard.verdict" :tone="testCard.tone as any" :to="`${base}/metrics`" action="See test gaps" />
      <HealthCard v-if="cleanCard" label="Unused code" tip="unreachable" :value="cleanCard.value" :verdict="cleanCard.verdict" :tone="cleanCard.tone as any" :to="`${base}/cleanup`" action="Clean up" />
      <HealthCard v-if="docCard" label="Documentation" :value="docCard.value" :verdict="docCard.verdict" :tone="docCard.tone as any" :to="`${base}/docs`" action="Open docs" />
      <HealthCard v-if="savingsCard" label="Context for AI agents" tip="reduction" :value="savingsCard.value" :verdict="savingsCard.verdict" tone="good" :to="`${base}/metrics`" action="See benchmark" />
    </div>

    <SecurityTrend :repo="repo" />

    <section class="panel">
      <div class="panel__header">
        <div><p class="eyebrow">Next steps</p><h2>What to do now, most important first</h2></div>
      </div>
      <ol v-if="steps.length" class="steps">
        <li v-for="(s, i) in steps" :key="i" class="step" :class="`step--${s.tone}`">
          <span class="num">{{ i + 1 }}</span>
          <div class="step-body">
            <strong>{{ s.title }}</strong>
            <p class="muted">{{ s.why }}</p>
          </div>
          <NuxtLink :to="s.to" class="btn btn--ghost cta">{{ s.cta }}</NuxtLink>
        </li>
      </ol>
      <div v-else class="empty"><strong>Nothing urgent</strong>Security, tests, cleanup and docs all look fine. Keep syncing as the code changes.</div>
    </section>

    <h2 class="section-title">Details</h2>
    <div class="grid cols-2">
      <section class="panel">
        <p class="eyebrow">Risk<InfoTip term="hotspot" /></p><h2>Hotspots</h2>
        <p class="muted small">Complex functions in files that change often. Review and test these first.</p>
        <div class="table-wrap">
          <table>
            <thead><tr><th>Function</th><th class="num">Complexity</th><th class="num">Changes</th><th class="num">Callers</th></tr></thead>
            <tbody>
              <tr v-for="h in d.hotspots.slice(0, 8)" :key="h.id">
                <td><NuxtLink :to="explore(h.id)" class="mono">{{ h.name }}</NuxtLink><div class="mono dim">{{ h.location }}</div></td>
                <td class="num">{{ h.complexity }}</td><td class="num">{{ h.file_churn }}</td><td class="num">{{ h.fan_in }}</td>
              </tr>
            </tbody>
          </table>
        </div>
        <p v-if="!d.hotspots.length" class="muted">No hotspots yet — history is needed to measure change frequency.</p>
      </section>
      <section class="panel">
        <p class="eyebrow">Architecture</p><h2>Most-used functions</h2>
        <p class="muted small">Changing these affects the most callers.</p>
        <BarList :items="d.most_called.slice(0, 8).map((r) => ({ label: shortName(r.node), value: r.score, to: `${base}/explore?id=${encodeURIComponent(r.node.id)}` }))" unit=" callers" />
      </section>
      <section class="panel">
        <p class="eyebrow">Activity</p><h2>Recent commits</h2>
        <div class="table-wrap">
          <table><tbody>
            <tr v-for="c in d.recent_commits.slice(0, 8)" :key="c.node.id">
              <td class="mono dim">{{ c.node.name.slice(0, 8) }}</td><td><span class="truncate">{{ c.node.summary }}</span></td><td class="muted nowrap">{{ relTime(c.node.time) }}</td>
            </tr>
          </tbody></table>
        </div>
        <p v-if="!d.recent_commits.length" class="muted">No git history indexed.</p>
      </section>
      <section class="panel">
        <p class="eyebrow">Activity</p><h2>Most-changed files</h2>
        <BarList :items="d.churn_files.filter((r) => r.score).slice(0, 8).map((r) => ({ label: r.node.file ?? r.node.name, value: r.score }))" unit=" commits" />
      </section>
    </div>
  </div>
  <p v-else class="muted">Loading…</p>
</template>

<style scoped>
.quick { margin-top: 6px; }
.autosync-bar { padding: 10px 16px; margin-top: -14px; }
.grid--health { display: grid; gap: 16px; grid-template-columns: repeat(auto-fit, minmax(210px, 1fr)); }
.steps { list-style: none; margin: 0; padding: 0; display: grid; gap: 10px; }
.step { display: grid; grid-template-columns: 32px minmax(0, 1fr) auto; gap: 14px; align-items: center; padding: 12px 14px; border-radius: var(--radius-sm); background: var(--panel-2); }
.step--bad { background: var(--danger-soft); }
.step--warn { background: var(--warning-soft); }
.step--neutral { background: var(--info-soft); }
.num { display: grid; place-items: center; width: 28px; height: 28px; border-radius: 50%; background: var(--panel-2); font: 600 12px var(--font-mono); color: var(--muted); }
.step-body { display: grid; gap: 2px; min-width: 0; }
.step-body p { font-size: 13.5px; }
.cta { white-space: nowrap; }
.section-title { margin: 8px 0 -8px; font-size: 15px; color: var(--muted); font-weight: 500; }
.small { font-size: 13px; }
.nowrap { white-space: nowrap; }
@media (max-width: 640px) { .step { grid-template-columns: 28px 1fr; } .cta { grid-column: 2; justify-self: start; } }
</style>
