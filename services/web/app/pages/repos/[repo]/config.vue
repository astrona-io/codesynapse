<script setup lang="ts">
import type { CleanupReport, Ranked, SecurityReport } from '~/utils/types'

interface OutlineEntry { id: string; kind: string; name: string; lines: string; signature?: string; summary?: string }

const route = useRoute()
const repo = computed(() => route.params.repo as string)
const base = computed(() => `/repos/${encodeURIComponent(repo.value)}`)
const { data: files, error } = useApiGet<{ results: Ranked[] }>(() => repoPath(repo.value, 'list?kind=Config&metric=loc&limit=500'))
const { data: templates } = useApiGet<{ results: Ranked[] }>(() => repoPath(repo.value, 'list?kind=Template&metric=loc&limit=500'))
const { data: entries } = useApiGet<{ results: Ranked[] }>(() => repoPath(repo.value, 'list?kind=ConfigEntry&metric=name&limit=500'))
const { data: sec } = useApiGet<SecurityReport>(() => repoPath(repo.value, 'security'))
const { data: clean } = useApiGet<CleanupReport>(() => repoPath(repo.value, 'cleanup'))

const isResource = (sig?: string) => !!sig && !['map', 'list', 'scalar', 'alias'].includes(sig)
const resources = computed(() => (entries.value?.results ?? []).map((r) => r.node).filter((n) => isResource(n.signature)))
const byKind = computed(() => {
  const m: Record<string, number> = {}
  for (const r of resources.value) {
    const k = r.name.split('/')[0] ?? r.name
    m[k] = (m[k] ?? 0) + 1
  }
  return Object.entries(m).sort((a, b) => b[1] - a[1]).map(([label, value]) => ({ label, value }))
})

// Classify each YAML file for people: what is it?
function fileType(file: string, summary?: string): { label: string; cls: string } {
  if (file.includes('.github/workflows/')) return { label: 'GitHub Actions', cls: 'badge--violet' }
  if (/(^|\/)(docker-)?compose[^/]*\.ya?ml$/.test(file)) return { label: 'Compose', cls: 'badge--cyan' }
  if (summary === 'Helm values') return { label: 'Helm values', cls: 'badge--orange' }
  if (summary?.startsWith('Kubernetes')) return { label: 'Kubernetes', cls: 'badge--green' }
  if (file.endsWith('Chart.yaml')) return { label: 'Helm chart', cls: 'badge--orange' }
  return { label: 'YAML', cls: '' }
}
const typeFilter = ref('all')
const q = ref('')
const yamlFiles = computed(() => (files.value?.results ?? []).map((r) => ({ ...r.node, type: fileType(r.node.file ?? '', r.node.summary) })))
const typeCounts = computed(() => {
  const m: Record<string, number> = {}
  for (const f of yamlFiles.value) m[f.type.label] = (m[f.type.label] ?? 0) + 1
  return m
})
const shownFiles = computed(() => yamlFiles.value.filter((f) =>
  (typeFilter.value === 'all' || f.type.label === typeFilter.value) && (!q.value || (f.file ?? '').toLowerCase().includes(q.value.toLowerCase()))))

const charts = computed(() => {
  const m = new Map<string, { chart: string; templates: number; lines: number }>()
  for (const t of templates.value?.results ?? []) {
    const chart = (t.node as { package?: string }).package || ''
    if (!chart) continue
    const c = m.get(chart) ?? { chart, templates: 0, lines: 0 }
    c.templates++
    c.lines += t.node.loc ?? 0
    m.set(chart, c)
  }
  return [...m.values()].sort((a, b) => a.chart.localeCompare(b.chart))
})
const plainTemplates = computed(() => (templates.value?.results ?? []).filter((t) => !(t.node as { package?: string }).package))
const unusedValuesByChart = computed(() => {
  const m: Record<string, number> = {}
  for (const v of clean.value?.unused_values ?? []) {
    const dir = (v.location ?? '').split('/values.yaml')[0] ?? ''
    m[dir] = (m[dir] ?? 0) + 1
  }
  return m
})

const configRule = (rule: string) => /^(k8s-|compose-|gha-|yaml-)/.test(rule)
const findings = computed(() => (sec.value?.findings ?? []).filter((f) => configRule(f.rule)))

const openFile = ref('')
const outline = ref<OutlineEntry[]>([])
async function toggleFile(file: string) {
  if (openFile.value === file) { openFile.value = ''; return }
  openFile.value = file
  outline.value = []
  const res = await $fetch<{ entries: OutlineEntry[] }>(apiPath(repoPath(repo.value, 'outline')), { query: { file } })
  outline.value = res.entries
}
</script>

<template>
  <div class="stack">
    <PageHeader
      eyebrow="Configuration" title="Kubernetes & Helm"
      lede="Every YAML manifest, Helm chart, compose file and CI workflow in the repository: what it deploys, where it lives, and what is risky about it. Values are never stored — only structure."
      :crumbs="[{ label: 'Repositories', to: '/' }, { label: repo, to: base }, { label: 'Kubernetes & Helm' }]"
    />
    <p v-if="error" class="panel error">{{ apiErrorMessage(error) }}</p>
    <template v-else>
      <div class="grid grid--stats">
        <StatTile label="YAML files" :value="yamlFiles.length" :hint="Object.entries(typeCounts).map(([k, v]) => `${v} ${k}`).join(' · ') || 'none'" />
        <StatTile label="Kubernetes resources" :value="resources.length" :hint="byKind.slice(0, 3).map((k) => `${k.value} ${k.label}`).join(' · ') || 'none'" />
        <StatTile label="Helm charts" :value="charts.length" :hint="`${templates?.results.length ?? 0} templates`" />
        <StatTile label="Config findings" :value="findings.length" :hint="findings.length ? 'misconfigurations to review' : 'no misconfigurations found'" />
      </div>

      <section v-if="findings.length" class="panel edge--pink">
        <div class="panel__header">
          <div><p class="eyebrow">Risks</p><h2>Misconfigurations</h2></div>
          <NuxtLink :to="`${base}/security`" class="btn btn--ghost">How to fix them</NuxtLink>
        </div>
        <div class="table-wrap">
          <table>
            <thead><tr><th>Severity</th><th>What</th><th>Where</th><th>Detail</th></tr></thead>
            <tbody>
              <tr v-for="f in findings.slice(0, 30)" :key="f.id">
                <td><SevBadge :severity="f.severity" /></td>
                <td>{{ f.title }}<div class="mono dim">{{ f.rule }}</div></td>
                <td class="mono">{{ f.location }}<div v-if="f.owner" class="dim">{{ f.owner.name }}</div></td>
                <td class="muted small">{{ f.detail }}</td>
              </tr>
            </tbody>
          </table>
        </div>
        <p v-if="findings.length > 30" class="muted small">{{ findings.length - 30 }} more on the Security page.</p>
      </section>

      <div class="grid cols-2">
        <section class="panel">
          <p class="eyebrow">Kubernetes</p><h2>Resources by kind</h2>
          <BarList :items="byKind" />
        </section>
        <section class="panel">
          <p class="eyebrow">Helm</p><h2>Charts</h2>
          <div v-if="charts.length" class="table-wrap">
            <table>
              <thead><tr><th>Chart</th><th class="num">Templates</th><th class="num">Lines</th><th class="num">Unused values</th></tr></thead>
              <tbody>
                <tr v-for="c in charts" :key="c.chart">
                  <td class="mono">{{ c.chart }}</td><td class="num">{{ c.templates }}</td><td class="num">{{ fmtInt(c.lines) }}</td>
                  <td class="num"><NuxtLink v-if="unusedValuesByChart[c.chart]" :to="`${base}/cleanup`">{{ unusedValuesByChart[c.chart] }}</NuxtLink><span v-else class="dim">0</span></td>
                </tr>
              </tbody>
            </table>
          </div>
          <p v-else class="muted">No Helm charts (no Chart.yaml) in this repository.</p>
          <p v-if="plainTemplates.length" class="muted small">{{ plural(plainTemplates.length, 'standalone Go template') }} outside charts.</p>
        </section>
      </div>

      <section class="panel">
        <div class="panel__header">
          <div><p class="eyebrow">Files</p><h2>YAML files <span class="muted">· click one to see its resources and keys</span></h2></div>
        </div>
        <div class="row">
          <button class="chip" :class="{ active: typeFilter === 'all' }" @click="typeFilter = 'all'">All · {{ yamlFiles.length }}</button>
          <button v-for="(n, t) in typeCounts" :key="t" class="chip" :class="{ active: typeFilter === t }" @click="typeFilter = t as string">{{ t }} · {{ n }}</button>
          <input v-model="q" type="search" placeholder="Filter by path…" class="filter" aria-label="Filter files">
        </div>
        <ul v-if="shownFiles.length" class="files">
          <li v-for="f in shownFiles" :key="f.id">
            <button class="file" :aria-expanded="openFile === f.file" @click="toggleFile(f.file!)">
              <span class="badge" :class="f.type.cls">{{ f.type.label }}</span>
              <span class="mono path">{{ f.file }}</span>
              <span class="muted summary">{{ f.summary }}</span>
              <span class="dim mono">{{ f.loc }} lines</span>
            </button>
            <div v-if="openFile === f.file" class="outline">
              <p v-if="!outline.length" class="muted small">Loading…</p>
              <div v-for="e in outline" :key="e.id" class="entry" :class="{ res: isResource(e.signature) }">
                <span class="mono">{{ e.name }}</span>
                <span v-if="isResource(e.signature)" class="badge badge--green">{{ e.signature }}</span>
                <span class="dim mono">L{{ e.lines }}</span>
              </div>
            </div>
          </li>
        </ul>
        <div v-else class="empty"><strong>No YAML files</strong>{{ yamlFiles.length ? 'match the filter.' : 'were found in this repository.' }}</div>
      </section>
    </template>
  </div>
</template>

<style scoped>
.small { font-size: 12.5px; }
.filter { max-width: 260px; margin-left: auto; }
.files { list-style: none; margin: 0; padding: 0; display: grid; }
.files li { border-bottom: 1px dashed var(--line); }
.files li:last-child { border-bottom: 0; }
.file { all: unset; box-sizing: border-box; cursor: pointer; width: 100%; display: grid; grid-template-columns: auto minmax(0, 1.2fr) minmax(0, 2fr) auto; gap: 12px; align-items: center; padding: 10px 6px; }
.file:hover { background: rgba(236, 231, 245, 0.03); }
.file:focus-visible { box-shadow: var(--focus); }
.path { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.summary { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; font-size: 13px; }
.outline { padding: 4px 6px 14px 24px; display: grid; gap: 4px; }
.entry { display: flex; gap: 10px; align-items: center; font-size: 13px; }
.entry.res .mono:first-child { color: var(--green); }
@media (max-width: 800px) { .file { grid-template-columns: auto 1fr; } .summary, .file .dim { display: none; } }
</style>
