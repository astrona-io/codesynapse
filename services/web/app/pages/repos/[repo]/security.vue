<script setup lang="ts">
import type { RuleGuide, SecFinding, SecurityReport } from '~/utils/types'

const route = useRoute()
const repo = computed(() => route.params.repo as string)
const { data: rep, error } = useApiGet<SecurityReport>(() => repoPath(repo.value, 'security'))
const { data: catalog } = useApiGet<{ rules: RuleGuide[] }>('rules')
const guides = computed(() => new Map((catalog.value?.rules ?? []).map((g) => [g.rule, g])))
const ruleFilter = ref('')
const open = ref(new Set<string>())
const toggle = (id: string) => {
  const next = new Set(open.value)
  if (next.has(id)) next.delete(id)
  else next.add(id)
  open.value = next
}
const findingsEl = ref<HTMLElement | null>(null)
function showRule(rule: string) {
  ruleFilter.value = ruleFilter.value === rule ? '' : rule
  sevFilter.value = 'all'
  nextTick(() => findingsEl.value?.scrollIntoView({ behavior: 'smooth', block: 'start' }))
}
const filesByRule = computed(() => {
  const m = new Map<string, Set<string>>()
  for (const f of rep.value?.findings ?? []) {
    if (!m.has(f.rule)) m.set(f.rule, new Set())
    m.get(f.rule)!.add(f.location.split(':')[0]!)
  }
  return m
})
const langMatch = (a?: string, b?: string) => {
  const n = (l?: string) => (['javascript', 'typescript', 'vue', 'js', 'ts'].includes(l ?? '') ? 'typescript' : l === 'yml' ? 'yaml' : l)
  return n(a) === n(b)
}
function examplesFor(f: SecFinding) {
  const g = guides.value.get(f.rule)
  if (!g) return []
  const match = g.examples.filter((e) => langMatch(e.language, f.language))
  return match.length ? match : g.examples.slice(0, 1)
}

const SEVERITIES = ['critical', 'high', 'medium', 'low', 'info']
const sevFilter = ref<string>('all')
const reachFilter = ref<string>('all')
const exposedOnly = ref(false)
const q = ref('')

const findings = computed(() =>
  (rep.value?.findings ?? []).filter((f) =>
    (sevFilter.value === 'all' || f.severity === sevFilter.value) &&
    (!ruleFilter.value || f.rule === ruleFilter.value) &&
    (!exposedOnly.value || f.exposed) &&
    (!q.value || `${f.rule} ${f.location} ${f.owner?.name ?? ''} ${f.detail}`.toLowerCase().includes(q.value.toLowerCase())),
  ),
)
const vulns = computed(() =>
  (rep.value?.vulnerabilities ?? []).filter((v) =>
    (sevFilter.value === 'all' || v.severity === sevFilter.value) &&
    (reachFilter.value === 'all' || v.reachability === reachFilter.value),
  ),
)
const total = (m: Record<string, number> | undefined) => Object.values(m ?? {}).reduce((a, b) => a + b, 0)
const gradeColor = computed(() => ({ A: 'var(--green)', B: 'var(--cyan)', C: 'var(--amber)', D: 'var(--orange)', F: 'var(--pink)' }[rep.value?.grade ?? 'A']))
const explore = (id: string) => ({ path: `/repos/${encodeURIComponent(repo.value)}/explore`, query: { id } })
const reachClass = (r: string) => ({ called: 'badge--pink', imported: 'badge--amber', required: '' }[r] ?? '')
</script>

<template>
  <div class="stack">
    <PageHeader
      eyebrow="Security review" title="Threats & vulnerabilities"
      lede="Code findings from type-checked rules and known CVEs in dependencies, ranked by severity, exposure from entry points and test reachability."
      :crumbs="[{ label: 'Repositories', to: '/' }, { label: repo, to: `/repos/${encodeURIComponent(repo)}` }, { label: 'Security' }]"
    >
      <SyncControl :repo="repo" />
      <a class="btn btn--ghost" :href="apiPath(repoPath(repo, 'security')) + '?format=markdown'" target="_blank" rel="noopener">Markdown report</a>
    </PageHeader>

    <p v-if="error" class="panel error">{{ apiErrorMessage(error) }}</p>
    <template v-else-if="rep">
      <div class="grid grid--stats">
        <div class="panel stat-card grade" :style="{ borderLeftColor: gradeColor }">
          <p class="eyebrow">Security grade</p>
          <p class="stat__value" :style="{ color: gradeColor }">{{ rep.grade }} <span class="score">{{ rep.score }}/100</span></p>
          <p class="stat__hint">Weighted by severity, exposure and reachability</p>
        </div>
        <StatTile label="Code findings" :value="total(rep.finding_counts)" :hint="SEVERITIES.filter((s) => rep!.finding_counts[s]).map((s) => `${rep!.finding_counts[s]} ${s}`).join(' · ') || 'none'" />
        <StatTile label="Vulnerabilities" :value="total(rep.vuln_counts)" :hint="SEVERITIES.filter((s) => rep!.vuln_counts[s]).map((s) => `${rep!.vuln_counts[s]} ${s}`).join(' · ') || 'none'" />
        <StatTile label="Dependencies" :value="rep.modules.total" :hint="`${rep.modules.direct} direct · ${rep.modules.vulnerable} vulnerable${rep.modules.stdlib ? ` · Go ${rep.modules.stdlib.replace(/^v/, '')}` : ''}`" />
      </div>

      <div class="panel panel--glass filters">
        <div class="row">
          <span class="label">Severity</span>
          <button class="chip" :class="{ active: sevFilter === 'all' }" @click="sevFilter = 'all'">All</button>
          <button v-for="s in SEVERITIES" :key="s" class="chip" :class="{ active: sevFilter === s }" @click="sevFilter = s">{{ s }}</button>
        </div>
        <div class="row">
          <span class="label">Reach</span>
          <button v-for="r in ['all', 'called', 'imported', 'required']" :key="r" class="chip" :class="{ active: reachFilter === r }" @click="reachFilter = r">{{ r }}</button>
          <label class="row compact"><input v-model="exposedOnly" type="checkbox"> exposed findings only</label>
          <input v-model="q" type="search" placeholder="Filter findings…" class="filter-input" aria-label="Filter findings">
        </div>
      </div>

      <section class="panel edge--pink">
        <div class="panel__header">
          <div><p class="eyebrow">Dependencies</p><h2>Vulnerable modules <span class="muted">· {{ vulns.length }}</span></h2></div>
        </div>
        <div v-if="vulns.length" class="table-wrap">
          <table>
            <thead><tr><th>Severity</th><th>Advisory</th><th>Module</th><th>Version → fix</th><th>Reach</th><th>Summary</th></tr></thead>
            <tbody>
              <tr v-for="v in vulns" :key="`${v.module}${v.id}`">
                <td><SevBadge :severity="v.severity" /></td>
                <td>
                  <a :href="v.url" target="_blank" rel="noopener noreferrer" class="mono">{{ v.id }}</a>
                  <div v-for="c in v.cve" :key="c" class="mono muted">{{ c }}</div>
                  <div v-if="v.cwe?.length" class="mono dim">{{ v.cwe.join(' ') }}</div>
                </td>
                <td class="mono">{{ v.module }}</td>
                <td class="mono"><span class="muted">{{ v.version }}</span> → <span :class="v.fixed ? 'fix' : 'error'">{{ v.fixed || 'no fix' }}</span></td>
                <td>
                  <span class="badge" :class="reachClass(v.reachability)">{{ v.reachability }}</span>
                  <div v-for="s in v.call_sites ?? []" :key="s.id" class="callsite"><NuxtLink :to="explore(s.id)" class="mono">{{ s.name }}</NuxtLink></div>
                </td>
                <td>{{ v.summary }}</td>
              </tr>
            </tbody>
          </table>
        </div>
        <div v-else class="empty"><strong>No known vulnerabilities</strong>{{ rep.modules.total ? 'for the current filter.' : 'Ingest with vulnerability scanning enabled (CODESYNAPSE_VULN_SCAN=true).' }}</div>
      </section>

      <section ref="findingsEl" class="panel edge--orange">
        <div class="panel__header">
          <div><p class="eyebrow">Source code & config</p><h2>Findings <span class="muted">· {{ findings.length }}</span></h2></div>
          <button v-if="ruleFilter" class="chip active" @click="ruleFilter = ''">rule: {{ ruleFilter }} ✕</button>
        </div>
        <div v-if="findings.length" class="table-wrap">
          <table>
            <thead><tr><th>Severity</th><th>Rule</th><th>Where</th><th>Exposed</th><th>Tested</th><th>Detail</th><th /></tr></thead>
            <tbody>
              <template v-for="f in findings" :key="f.id">
              <tr>
                <td><SevBadge :severity="f.severity" /></td>
                <td><span class="mono">{{ f.rule }}</span><div class="mono dim">{{ f.cwe?.join(' ') }}</div></td>
                <td>
                  <NuxtLink v-if="f.owner && f.owner.kind !== 'File'" :to="explore(f.owner.id)" class="mono">{{ f.owner.name }}</NuxtLink>
                  <div class="mono muted">{{ f.location }}</div>
                </td>
                <td><span class="badge" :class="f.exposed ? 'badge--pink' : ''">{{ f.exposed ? 'yes' : 'no' }}</span></td>
                <td><span class="badge" :class="f.tested ? 'badge--green' : 'badge--amber'">{{ f.tested ? 'yes' : 'no' }}</span></td>
                <td class="detail">{{ f.detail }}</td>
                <td><button class="secondary tiny" :aria-expanded="open.has(f.id)" @click="toggle(f.id)">{{ open.has(f.id) ? 'Hide fix' : 'How to fix' }}</button></td>
              </tr>
              <tr v-if="open.has(f.id)" class="fix-row">
                <td colspan="7">
                  <div class="fix">
                    <p v-if="guides.get(f.rule)?.why"><strong>Why it matters.</strong> {{ guides.get(f.rule)!.why }}</p>
                    <p><strong>Fix.</strong> {{ guides.get(f.rule)?.fix }}</p>
                    <div v-for="(ex, i) in examplesFor(f)" :key="i" class="pair">
                      <div><p class="label bad">Vulnerable · {{ ex.language }}</p><pre class="code bad-code">{{ ex.vulnerable }}</pre></div>
                      <div><p class="label good">Fixed · {{ ex.language }}</p><pre class="code good-code">{{ ex.fixed }}</pre></div>
                    </div>
                  </div>
                </td>
              </tr>
              </template>
            </tbody>
          </table>
        </div>
        <div v-else class="empty"><strong>No findings</strong>for the current filter.</div>
      </section>

      <div class="grid cols-2">
        <section class="panel">
          <p class="eyebrow">Rules</p><h2>What fired</h2>
          <div class="table-wrap">
            <table>
              <tbody>
                <tr v-for="r in rep.rules" :key="r.rule" class="clickable" :class="{ selected: ruleFilter === r.rule }" tabindex="0" @click="showRule(r.rule)" @keydown.enter="showRule(r.rule)">
                  <td><SevBadge :severity="r.severity" /></td>
                  <td><a href="#" @click.prevent>{{ r.title }}</a><div class="mono dim">{{ r.rule }} · {{ filesByRule.get(r.rule)?.size ?? 0 }} file(s)</div></td>
                  <td class="num">{{ r.count }}</td>
                </tr>
              </tbody>
            </table>
          </div>
          <p v-if="!rep.rules.length" class="muted">No rule fired.</p>
        </section>
        <section class="panel">
          <p class="eyebrow">Attack surface</p><h2>Functions calling sensitive APIs</h2>
          <div class="table-wrap">
            <table>
              <tbody>
                <tr v-for="s in rep.sensitive_functions ?? []" :key="s.id">
                  <td><NuxtLink :to="explore(s.id)" class="mono">{{ s.name }}</NuxtLink><div class="mono muted">{{ s.location }}</div></td>
                  <td><span v-for="t in (s.summary ?? '').split(', ')" :key="t" class="tag sec">{{ t }}</span></td>
                </tr>
              </tbody>
            </table>
          </div>
        </section>
      </div>
    </template>
    <p v-else class="muted">Loading…</p>
  </div>
</template>

<style scoped>
.grade .score { font-size: 15px; color: var(--muted); font-weight: 500; }
.filters { gap: 10px; padding: 16px 20px; }
.filter-input { max-width: 280px; margin-left: auto; }
.compact { gap: 6px; }
.fix { color: var(--green); }
.callsite { margin-top: 4px; font-size: 12px; }
.detail { max-width: 460px; color: var(--muted); font-size: 13px; }
.clickable { cursor: pointer; }
.clickable.selected td { background: var(--tint-orange); }
.fix-row td { background: var(--panel-faint); }
.fix { display: grid; gap: 10px; padding: 6px 4px 10px; max-width: 1100px; }
.pair { display: grid; grid-template-columns: repeat(auto-fit, minmax(min(100%, 360px), 1fr)); gap: 12px; }
.code { margin: 4px 0 0; padding: 10px 12px; border-radius: var(--radius-xs); font: 12.5px/1.5 var(--font-mono); white-space: pre-wrap; word-break: break-word; background: var(--bg); border: 1px solid var(--line); }
.bad-code { border-left: 3px solid var(--pink); }
.good-code { border-left: 3px solid var(--green); }
.label.bad { color: var(--pink); }
.label.good { color: var(--green); }
</style>
