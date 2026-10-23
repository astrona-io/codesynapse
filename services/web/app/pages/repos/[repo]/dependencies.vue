<script setup lang="ts">
import type { DependencyList, LicenseReport, SecurityReport, SecVuln } from '~/utils/types'

const route = useRoute()
const repo = computed(() => route.params.repo as string)
useHead({ title: () => `Dependencies · ${repo.value}` })
const sbomHref = computed(() => apiPath(repoPath(repo.value, 'sbom')))
const { data: deps, error } = useApiGet<DependencyList>(() => repoPath(repo.value, 'dependencies'))
const { data: sec } = useApiGet<SecurityReport>(() => repoPath(repo.value, 'security'))
const { data: lic } = useApiGet<LicenseReport>(() => repoPath(repo.value, 'licenses'))

// Update automation: the Renovate and Dependabot rules.
const UPDATE_RULES = /^(dependency-updates-missing|renovate-)/
const automation = computed(() => (sec.value?.findings ?? []).filter((f) => UPDATE_RULES.test(f.rule)))
const missing = computed(() => automation.value.some((f) => f.rule === 'dependency-updates-missing'))

const vulnsByModule = computed(() => {
  const m = new Map<string, SecVuln[]>()
  for (const v of sec.value?.vulnerabilities ?? []) m.set(v.module, [...(m.get(v.module) ?? []), v])
  return m
})
const verdicts = computed(() => new Map((lic.value?.issues ?? []).map((i) => [`${i.ecosystem}:${i.name}`, i.verdict])))
const REACH = { called: 'Your code calls it', imported: 'Imported, not called', required: 'Required only' } as const

type Row = { name: string; version: string; ecosystem: string; direct: boolean; dev: boolean; license: string; via: string; roots: number; vulns: SecVuln[]; licence?: { level: string; reason: string } }
// Every package (up to the store's dependency limit); vulnerable packages and
// licence issues missing from it still come from their own reports.
const rows = computed<Row[]>(() => {
  const out: Row[] = (deps.value?.packages ?? []).map((p) => ({
    name: p.name, version: p.version ?? '', ecosystem: p.ecosystem, direct: p.direct, dev: !!p.dev, license: p.license, via: p.via ?? '', roots: p.roots ?? 0,
    vulns: vulnsByModule.value.get(p.name) ?? [], licence: verdicts.value.get(`${p.ecosystem}:${p.name}`),
  }))
  const seen = new Set(out.map((r) => r.name))
  for (const [name, vulns] of vulnsByModule.value) {
    if (!seen.has(name)) { seen.add(name); out.push({ name, version: vulns[0]?.version ?? '', ecosystem: '', direct: true, dev: false, license: '', via: '', roots: 0, vulns }) }
  }
  for (const i of lic.value?.issues ?? []) {
    if (!seen.has(i.name)) {
      seen.add(i.name)
      out.push({ name: i.name, version: i.version, ecosystem: i.ecosystem, direct: i.direct, dev: !!i.dev, license: i.license ?? '', via: '', roots: 0, vulns: [], licence: i.verdict })
    }
  }
  return out
})
const total = computed(() => Math.max(lic.value?.total ?? 0, rows.value.length))
type Show = 'attention' | 'direct' | 'all'
// A link from the Licences page (?license=MIT) opens every package under it.
const licenseFilter = ref(typeof route.query.license === 'string' ? route.query.license : '')
const show = ref<Show>(licenseFilter.value ? 'all' : 'attention')
const query = ref('')
const ecosystem = ref(typeof route.query.ecosystem === 'string' ? route.query.ecosystem : 'all')
const shown = computed(() => rows.value
  .filter((r) => show.value === 'all' || (show.value === 'direct' ? r.direct : r.vulns.length || (r.licence && r.licence.level !== 'ok')))
  .filter((r) => !licenseFilter.value || (r.license || 'unknown') === licenseFilter.value)
  .filter((r) => !query.value || r.name.toLowerCase().includes(query.value.toLowerCase()))
  .sort((a, b) => b.vulns.length - a.vulns.length || Number(b.direct) - Number(a.direct) || a.name.localeCompare(b.name)))
// Languages, largest first; packages found only through a report sort last.
const ecosystems = computed(() => {
  const c: Record<string, number> = {}
  for (const r of rows.value) c[r.ecosystem || 'other'] = (c[r.ecosystem || 'other'] ?? 0) + 1
  return Object.entries(c).sort((a, b) => b[1] - a[1] || a[0].localeCompare(b[0]))
})
// One table per language: all of them, or the one picked.
const groups = computed(() => ecosystems.value
  .filter(([e]) => ecosystem.value === 'all' || e === ecosystem.value)
  .map(([e]) => ({ ecosystem: e, rows: shown.value.filter((r) => (r.ecosystem || 'other') === e) }))
  .filter((g) => g.rows.length))
const shownCount = computed(() => groups.value.reduce((n, g) => n + g.rows.length, 0))
</script>

<template>
  <div class="stack">
    <PageHeader eyebrow="Dependencies" title="What this repository depends on" lede="Every package it uses, with known vulnerabilities, licences and whether updates are automated. Fix vulnerabilities your code calls first." :crumbs="[{ label: 'Repositories', to: '/' }, { label: repo, to: `/repos/${encodeURIComponent(repo)}` }, { label: 'Dependencies' }]">
      <a :href="sbomHref" download class="btn btn--ghost" title="Dependencies resolved by go.mod and the lockfiles, as CycloneDX 1.5 JSON. Not the contents of a built artifact.">Download SBOM</a>
    </PageHeader>
    <p v-if="error" class="notice notice--danger">{{ apiErrorMessage(error) }}</p>
    <div class="grid cols-4">
      <StatTile label="Dependencies" :value="String(total)" :hint="ecosystems.map(([e, c]) => `${c} ${ecosystemLabel(e)}`).join(' · ') || '–'" />
      <StatTile label="Vulnerable" :value="String(vulnsByModule.size)" :hint="`${(sec?.vulnerabilities ?? []).filter((v) => v.reachability === 'called').length} called by your code`" />
      <StatTile label="Licence issues" :value="String((lic?.conflict ?? 0) + (lic?.review ?? 0))" :hint="`${lic?.conflict ?? 0} conflict · ${lic?.review ?? 0} to review`" />
      <StatTile label="Update automation" :value="missing ? 'Missing' : automation.length ? 'Needs care' : 'In place'" :hint="missing ? 'no Renovate or Dependabot config' : automation.length ? `${automation.length} configuration issue(s)` : 'Renovate or Dependabot is configured'" />
    </div>

    <section v-if="automation.length" class="panel edge--amber">
      <h2>Keeping dependencies up to date</h2>
      <p class="muted small">Security fixes in dependencies only help once they are installed. Renovate or Dependabot opens the update pull requests for you.</p>
      <ul class="plain">
        <li v-for="f in automation" :key="f.id"><span :class="`sev sev--${f.severity}`">{{ f.severity }}</span> <strong>{{ f.title }}</strong> <span class="muted small">{{ f.location }}</span><p class="muted small">{{ f.detail }}</p></li>
      </ul>
    </section>

    <section class="panel">
      <div class="panel__header">
        <h2>Packages <span class="muted">· {{ shownCount }}</span></h2>
        <div class="row">
          <button v-for="s in (['attention', 'direct', 'all'] as Show[])" :key="s" class="chip" :class="{ active: show === s }" @click="show = s">{{ s === 'attention' ? 'Needs attention' : s === 'direct' ? 'Direct' : 'All' }}</button>
          <input v-model="query" class="search" placeholder="Filter by name" aria-label="Filter packages">
        </div>
      </div>
      <div class="row langs" role="tablist" aria-label="Language">
        <button class="chip" role="tab" :aria-selected="ecosystem === 'all'" :class="{ active: ecosystem === 'all' }" @click="ecosystem = 'all'">All languages</button>
        <button v-for="[e, c] in ecosystems" :key="e" class="chip" role="tab" :aria-selected="ecosystem === e" :class="{ active: ecosystem === e }" @click="ecosystem = e">{{ ecosystemLabel(e) }} <span class="muted">{{ c }}</span></button>
      </div>
      <p v-if="licenseFilter" class="row filter">
        <span>Licence <span class="mono">{{ licenseFilter }}</span></span>
        <button class="chip" aria-label="Show every licence" @click="licenseFilter = ''">clear ×</button>
      </p>
      <div v-for="g in groups" :key="g.ecosystem" class="group">
        <h3 v-if="ecosystem === 'all'">{{ ecosystemLabel(g.ecosystem) }} <span class="muted">· {{ g.rows.length }}</span></h3>
        <div class="table-wrap">
          <table>
            <thead><tr><th>Package</th><th>Version</th><th>Licence</th><th>Vulnerabilities</th></tr></thead>
            <tbody>
              <tr v-for="r in g.rows" :key="r.ecosystem + r.name">
                <td><span class="mono">{{ r.name }}</span><span v-if="!r.direct" class="badge" :title="r.via ? `Via ${r.via}${r.roots > 1 ? ` · ${r.roots} of your dependencies lead to it` : ''}` : 'Not declared by this repository'">indirect</span><span v-if="r.via" class="muted small"> via {{ r.via }}</span><span v-if="r.dev" class="badge">dev</span></td>
                <td class="mono">{{ r.version || '–' }}</td>
                <td>
                  {{ r.license || 'unknown' }}
                  <span v-if="r.licence && r.licence.level !== 'ok'" :class="r.licence.level === 'conflict' ? 'badge badge--pink' : 'badge badge--amber'" :title="r.licence.reason">{{ r.licence.level }}</span>
                </td>
                <td>
                  <span v-if="!r.vulns.length" class="muted">none known</span>
                  <div v-for="v in r.vulns" :key="v.id" class="vuln">
                    <span :class="`sev sev--${v.severity}`">{{ v.severity }}</span>
                    <a :href="v.url" target="_blank" rel="noopener noreferrer" class="mono">{{ v.id }}</a>
                    <span class="small" :class="v.reachability === 'called' ? 'error' : 'muted'">{{ REACH[v.reachability] }}</span>
                    <span v-if="v.fixed" class="small">· fixed in <span class="mono">{{ v.fixed }}</span></span>
                  </div>
                </td>
              </tr>
            </tbody>
          </table>
        </div>
      </div>
      <p v-if="deps?.truncated" class="muted small">The list stops at the store's dependency limit; run <code>codesynapse sbom</code> for every package.</p>
      <div v-if="!shownCount" class="empty"><strong>{{ show === 'attention' && !licenseFilter ? 'Nothing needs attention' : 'No packages' }}</strong>{{ show === 'attention' && !licenseFilter ? 'No vulnerable packages and no licence issues.' : rows.length ? 'Nothing matches the current filter.' : 'No dependency manifests were found.' }}</div>
    </section>
  </div>
</template>

<style scoped>
.plain { list-style: none; padding: 0; display: grid; gap: 10px; }
.search { width: 200px; min-height: 30px; padding: 4px 10px; }
.vuln { display: flex; flex-wrap: wrap; gap: 6px; align-items: baseline; }
td .badge { margin-left: 4px; }
.langs { flex-wrap: wrap; margin: 8px 0 4px; }
.filter { gap: 8px; margin: 4px 0; }
.group + .group { margin-top: 18px; }
.group h3 { margin: 12px 0 6px; font-size: 14px; }
</style>
