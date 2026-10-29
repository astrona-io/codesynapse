<script setup lang="ts">
import type { CleanupReport, LicenseReport, Ranked, SecurityReport, SecVuln } from '~/utils/types'
import { LOW_SCORECARD, scorecardBadge, scorecardOfNode, scorecardTitle, type ScorecardScore } from '~/utils/scorecard'

const route = useRoute()
const repo = computed(() => route.params.repo as string)
useHead({ title: () => `Dependency review · ${repo.value}` })
const sbomHref = computed(() => apiPath(repoPath(repo.value, 'sbom')))
const { data: mods, error } = useApiGet<{ results: Ranked[] }>(() => repoPath(repo.value, 'list?kind=Module&metric=name&limit=500'))
const { data: sec, error: secError } = useApiGet<SecurityReport>(() => repoPath(repo.value, 'security'))
const { data: lic, error: licError } = useApiGet<LicenseReport>(() => repoPath(repo.value, 'licenses'))
const { data: clean, error: cleanError } = useApiGet<CleanupReport>(() => repoPath(repo.value, 'cleanup'))

type Verdict = { level: string; reason: string }
type Row = {
  key: string; name: string; version: string; ecosystem: string; direct: boolean; dev: boolean; license: string
  vulns: SecVuln[]; verdict?: Verdict; unused: boolean; via: string; roots: number; deprecated?: string; retracted?: string; latest?: string; scorecard?: ScorecardScore
}

// What the review table covers: packages with an advisory, a licence to
// look at, a deprecated or retracted version, or that nothing imports.
// Everything else needs no action. A low OpenSSF Scorecard only ranks a
// row that is here for another reason a little higher; it never adds one.
const rows = computed<Row[]>(() => {
  const byKey = new Map<string, Row>()
  const eco = (e?: string) => (e || 'go').toLowerCase()
  const row = (name: string, e?: string): Row => {
    const key = `${eco(e)}:${name}`
    let r = byKey.get(key)
    if (!r) { r = { key, name, version: '', ecosystem: eco(e), direct: true, dev: false, license: '', vulns: [], unused: false, via: '', roots: 0 }; byKey.set(key, r) }
    return r
  }
  const known = new Map<string, Row>()
  for (const { node: n } of mods.value?.results ?? []) {
    known.set(n.name, { key: '', name: n.name, version: n.version ?? '', ecosystem: eco(n.ecosystem), direct: !n.indirect, dev: !!n.dev, license: n.license ?? '', vulns: [], unused: false, via: n.via ?? '', roots: n.roots ?? 0, deprecated: n.deprecated, retracted: n.retracted, latest: n.latest, scorecard: scorecardOfNode(n) })
  }
  const fill = (r: Row) => {
    const k = known.get(r.name)
    if (k) Object.assign(r, { version: r.version || k.version, direct: k.direct, dev: k.dev, license: r.license || k.license, ecosystem: r.ecosystem || k.ecosystem, via: k.via, roots: k.roots, deprecated: k.deprecated, retracted: k.retracted, latest: k.latest, scorecard: k.scorecard })
    return r
  }
  for (const v of sec.value?.vulnerabilities ?? []) {
    const r = fill(row(v.module, known.get(v.module)?.ecosystem))
    r.version = r.version || v.version
    r.vulns.push(v)
  }
  for (const i of lic.value?.issues ?? []) {
    if (i.verdict.level === 'ok') continue
    const r = row(i.name, i.ecosystem)
    const k = known.get(i.name)
    Object.assign(r, { version: i.version, direct: i.direct, dev: !!i.dev, license: i.license ?? '', verdict: i.verdict, via: k?.via ?? '', roots: k?.roots ?? 0, scorecard: k?.scorecard })
  }
  for (const k of known.values()) {
    if (k.deprecated || k.retracted) fill(row(k.name, k.ecosystem))
  }
  for (const m of clean.value?.modules ?? []) {
    const r = fill(row(m.path, known.get(m.path)?.ecosystem))
    r.version = r.version || m.version
    r.unused = true
  }
  return [...byKey.values()].sort((a, b) => score(b) - score(a) || a.name.localeCompare(b.name))
})
const lowScorecard = (r: Row) => !!r.scorecard && r.scorecard.score < LOW_SCORECARD
function score(r: Row) {
  const calls = r.vulns.some((v) => v.reachability === 'called') ? 100 : 0
  return calls + r.vulns.length * 10 + (r.verdict?.level === 'conflict' ? 20 : r.verdict ? 5 : 0) + (r.deprecated ? 8 : 0) + (r.retracted ? 8 : 0) + (lowScorecard(r) ? 3 : 0) + (r.unused ? 1 : 0)
}

const REACH: Record<string, string> = {
  called: 'Your code calls a vulnerable symbol',
  imported: 'Imported, no vulnerable symbol found being called',
  required: 'Present, but no import of it was found',
}
function usage(r: Row): string {
  const order = ['called', 'imported', 'required']
  const best = order.find((o) => r.vulns.some((v) => v.reachability === o))
  if (best) return REACH[best]!
  return r.unused ? 'Nothing imports it' : '–'
}
function actions(r: Row): string[] {
  const out: string[] = []
  if (r.unused) out.push('Remove it from the manifest and lockfile, then Sync: nothing imports it.')
  const fixes = [...new Set(r.vulns.map((v) => v.fixed).filter(Boolean))]
  if (r.vulns.length && fixes.length) out.push(`${r.vulns.some((v) => v.reachability === 'called') ? 'Urgent: u' : 'U'}pgrade to ${fixes.join(' / ')} or later, if compatible.`)
  else if (r.vulns.length) out.push('No fixed version listed: remove, replace or mitigate it, and check whether the flaw is reachable.')
  if (r.deprecated) out.push(`Deprecated by its publisher: ${r.deprecated} Move to the replacement it names${r.latest && r.latest !== r.version ? `, or check whether ${r.latest} is still deprecated` : ''}.`)
  if (r.retracted) out.push(`Retracted by its author (${r.retracted}): upgrade to ${r.latest && r.latest !== r.version ? r.latest : 'a version the retraction does not cover'}.`)
  if (r.scorecard && lowScorecard(r)) out.push(`Its source project scores ${r.scorecard.score.toFixed(1)} of 10 on OpenSSF Scorecard: a hygiene note, not a vulnerability. Weigh it when choosing between this package and an alternative.`)
  if (r.verdict?.level === 'conflict') out.push('Licence conflicts with yours: ask whoever owns licensing, then replace or comply. This is a flag, not a legal finding.')
  else if (r.verdict) out.push('Read the licence terms before relying on it.')
  return out
}
// Where the dependency came from is not stored for indirect packages.
const WHY: Record<string, (n: string) => string> = {
  go: (n) => `go mod why -m ${n}`,
  npm: (n) => `npm explain ${n}`,
  pypi: (n) => `pipdeptree -r -p ${n}`,
  'crates.io': (n) => `cargo tree -i ${n}`,
  packagist: (n) => `composer why ${n}`,
  rubygems: (n) => `bundle exec gem dependency ${n} --reverse-dependencies`,
  nuget: (n) => `dotnet nuget why <project> ${n}`,
  maven: (n) => `gradle dependencyInsight --dependency ${n.split(':')[1] ?? n}`,
}
const why = (r: Row) => (WHY[r.ecosystem] ?? WHY.go!)(r.name)

// Until every report has answered, an empty table would claim there is nothing to do.
const ready = computed(() => !!mods.value && !!(sec.value || secError.value) && !!(lic.value || licError.value) && !!(clean.value || cleanError.value))
const missing = computed(() => [secError.value && 'vulnerabilities', licError.value && 'licences', cleanError.value && 'unused dependencies'].filter(Boolean).join(', '))
const actionable = computed(() => rows.value.filter((r) => actions(r).length))
const called = computed(() => rows.value.filter((r) => r.vulns.some((v) => v.reachability === 'called')).length)
const unusedCount = computed(() => rows.value.filter((r) => r.unused).length)
const conflicts = computed(() => rows.value.filter((r) => r.verdict?.level === 'conflict').length)

// Commands for the ecosystems this repository uses; other ecosystems are not
// read by CodeSynapse at all and need their own tooling.
const GUIDE: Record<string, { label: string; graph: string; audit: string; licences: string }> = {
  go: { label: 'Go', graph: 'go list -m -json all · go mod graph · go mod why -m <module>', audit: 'govulncheck ./...', licences: 'go-licenses report ./...' },
  npm: { label: 'npm', graph: 'npm ls --all · npm explain <package>', audit: 'npm audit · osv-scanner', licences: 'license-checker' },
  pypi: { label: 'Python', graph: 'uv tree · pipdeptree · poetry show --tree', audit: 'pip-audit · osv-scanner', licences: 'pip-licenses' },
  'crates.io': { label: 'Rust', graph: 'cargo tree · cargo tree -i <crate>', audit: 'cargo audit · cargo deny check advisories', licences: 'cargo deny check licenses' },
  packagist: { label: 'PHP', graph: 'composer show --tree · composer why <package>', audit: 'composer audit', licences: 'composer licenses' },
  rubygems: { label: 'Ruby', graph: 'bundle list · bundle viz', audit: 'bundler-audit', licences: 'license_finder' },
  nuget: { label: '.NET', graph: 'dotnet list package --include-transitive', audit: 'dotnet list package --vulnerable --include-transitive', licences: 'nuget-license' },
  maven: { label: 'Java / Kotlin', graph: 'gradle dependencies · gradle dependencyInsight --dependency <name>', audit: 'osv-scanner · OWASP dependency-check', licences: 'a licence report plugin' },
}
const present = computed(() => {
  const seen = new Set((mods.value?.results ?? []).map(({ node: n }) => (n.ecosystem || 'go').toLowerCase()))
  if (!seen.size) seen.add('go')
  return [...seen].map((e) => GUIDE[e]).filter((g): g is NonNullable<typeof g> => !!g)
})
const copied = ref('')
async function copy(text: string) {
  try { await navigator.clipboard.writeText(text); copied.value = text; setTimeout(() => { if (copied.value === text) copied.value = '' }, 1500) } catch { /* clipboard unavailable */ }
}
const agentText = computed(() => `Run the dependency-review prompt for ${repo.value}: analyse the dependencies recursively for licences and vulnerabilities and look for smaller alternatives.`)
</script>

<template>
  <div class="stack">
    <PageHeader eyebrow="Dependencies" title="Dependency review" lede="Packages that need a decision: known vulnerabilities, licences to check and dependencies nothing imports, each with a suggested action. The suggestions come from CodeSynapse's data; check them before you change anything."
      :crumbs="[{ label: 'Repositories', to: '/' }, { label: repo, to: `/repos/${encodeURIComponent(repo)}` }, { label: 'Dependency review' }]">
      <a :href="sbomHref" download class="btn btn--ghost" title="Dependencies resolved by go.mod and the lockfiles, as CycloneDX 1.5 JSON. Not the contents of a built artifact.">Download SBOM</a>
    </PageHeader>
    <DataState :error="error" :pending="!ready" :empty="!ready" loading="Loading the dependency reports…" />
    <p v-if="ready && missing" class="notice notice--warn" role="status">Some reports could not be loaded ({{ missing }}), so the list below may be incomplete.</p>

    <div v-if="ready" class="grid cols-4">
      <StatTile label="Need a decision" :value="fmtInt(actionable.length)" :hint="`of ${plural(sec?.modules.total ?? lic?.total ?? 0, 'dependency', 'dependencies')}`" />
      <StatTile label="Called vulnerable code" :value="fmtInt(called)" hint="fix these first" />
      <StatTile label="Licence conflicts" :value="fmtInt(conflicts)" :hint="`${fmtInt(lic?.review ?? 0)} more to review`" />
      <StatTile label="Unused" :value="fmtInt(unusedCount)" hint="nothing imports them" />
    </div>

    <section v-if="ready" class="panel">
      <div class="panel__header"><h2>Findings</h2><NuxtLink :to="`/repos/${encodeURIComponent(repo)}/dependencies`" class="small">All packages</NuxtLink></div>
      <div v-if="actionable.length" class="table-wrap">
        <table>
          <thead><tr><th>Dependency and version</th><th>Introduced by</th><th>Usage</th><th>Licence</th><th>Advisory</th><th>Proposed action</th><th>Verified outcome</th></tr></thead>
          <tbody>
            <tr v-for="r in actionable" :key="r.key">
              <td><span class="mono wrap">{{ r.name }}</span><div class="mono muted">{{ r.version || '–' }}</div><span class="badge">{{ ecosystemLabel(r.ecosystem) }}</span><span v-if="r.dev" class="badge">dev</span><span v-if="r.deprecated" class="badge badge--amber" :title="r.deprecated">deprecated</span><span v-if="r.retracted" class="badge badge--amber" :title="r.retracted">retracted</span><span v-if="r.scorecard" :class="scorecardBadge(r.scorecard.score)" :title="scorecardTitle(r.scorecard)">Scorecard {{ r.scorecard.score.toFixed(1) }}</span></td>
              <td>
                <template v-if="r.direct">Direct: your manifest</template>
                <template v-else-if="r.via">
                  Indirect, via <span class="mono wrap">{{ r.via }}</span>
                  <div v-if="r.roots > 1" class="muted small">{{ fmtInt(r.roots) }} of your dependencies lead to it</div>
                  <div v-else class="muted small">only through that chain</div>
                </template>
                <template v-else>Indirect: path not recorded<div><button class="chip" :title="`Copy: ${why(r)}`" @click="copy(why(r))">{{ copied === why(r) ? 'Copied' : 'Copy how to find it' }}</button></div></template>
              </td>
              <td class="small" :class="{ error: r.vulns.some((v) => v.reachability === 'called') }">{{ usage(r) }}</td>
              <td>
                {{ r.license || 'unknown' }}
                <span v-if="r.verdict" :class="r.verdict.level === 'conflict' ? 'badge badge--pink' : 'badge badge--amber'" :title="r.verdict.reason">{{ r.verdict.level }}</span>
                <div v-if="r.verdict" class="muted small">{{ r.verdict.reason }}</div>
              </td>
              <td>
                <span v-if="!r.vulns.length" class="muted">none known</span>
                <div v-for="v in r.vulns" :key="v.id" class="vuln">
                  <SevBadge :severity="v.severity" />
                  <a :href="v.url" target="_blank" rel="noopener noreferrer" class="mono">{{ v.id }}<span class="sr-only"> (opens in a new tab)</span></a>
                  <span v-if="v.fixed" class="small">fixed in <span class="mono">{{ v.fixed }}</span></span>
                </div>
              </td>
              <td><p v-for="a in actions(r)" :key="a" class="small">{{ a }}</p></td>
              <td class="small muted" title="Run the tests, build and scans after changing it, then press Sync">Not verified</td>
            </tr>
          </tbody>
        </table>
      </div>
      <div v-else class="empty"><strong>Nothing needs a decision</strong>No known advisories, no licence issues and no unused dependencies in what CodeSynapse reads. That is not proof of compliance: see below.</div>
    </section>

    <div class="grid cols-2">
      <section class="panel">
        <p class="eyebrow">By hand</p><h2>Check it with your package manager</h2>
        <p class="muted small">CodeSynapse shows how an indirect dependency got in when the package graph says so (the Go packages your build imports and the lockfiles of npm, Python, Rust, PHP, Ruby and .NET). Yarn, pnpm and Gradle lockfiles carry no paths, and it only reads go.mod and those lockfiles. These commands resolve the real graph; confirm names and flags against your installed version.</p>
        <div class="table-wrap"><table>
          <thead><tr><th>Ecosystem</th><th>Graph and why</th><th>Advisories</th><th>Licences</th></tr></thead>
          <tbody><tr v-for="g in present" :key="g.label"><td>{{ g.label }}</td><td class="mono small">{{ g.graph }}</td><td class="mono small">{{ g.audit }}</td><td class="mono small">{{ g.licences }}</td></tr></tbody>
        </table></div>
        <p class="muted small">Other languages (Rust, Java, .NET, Ruby, PHP and more) need their own tools: the agent prompt lists them.</p>
      </section>
      <section class="panel">
        <p class="eyebrow">With your agent</p><h2>Run the full review</h2>
        <p class="muted small">The <span class="mono">dependency-review</span> prompt walks an agent through the whole analysis for any language: dependency graph, licences, advisories, smaller alternatives, validation and the findings table. Connect an agent on the Agents page, then ask:</p>
        <pre class="sig mono">{{ agentText }}</pre>
        <button class="chip" @click="copy(agentText)">{{ copied === agentText ? 'Copied' : 'Copy' }}</button>
        <p class="muted small">A clean list does not mean the project is free of vulnerabilities or compliant with a licence policy; it means nothing was found in the data CodeSynapse reads.</p>
      </section>
    </div>
  </div>
</template>

<style scoped>
.vuln { display: flex; flex-wrap: wrap; gap: 6px; align-items: baseline; }
td .badge { margin-right: 4px; }
td p { margin: 0 0 4px; }
.sig { white-space: pre-wrap; overflow-wrap: anywhere; }
.wrap { overflow-wrap: anywhere; }
</style>
