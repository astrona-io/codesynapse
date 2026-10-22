<script setup lang="ts">
import type { ArchIssue, ArchPage } from '~/utils/types'

const route = useRoute()
const repo = computed(() => route.params.repo as string)
useHead({ title: () => `Architecture · ${repo.value}` })

const type = ref('')
const status = ref('')
const component = ref('')
const q = ref('')
const bindingOnly = ref(false)
const offset = ref(0)
const LIMIT = 25
const query = computed(() => {
  const p = new URLSearchParams({ limit: String(LIMIT), offset: String(offset.value) })
  if (type.value) p.set('type', type.value)
  if (status.value) p.set('status', status.value)
  if (component.value) p.set('component', component.value)
  if (q.value.trim()) p.set('q', q.value.trim())
  if (bindingOnly.value) p.set('binding', '1')
  return p.toString()
})
watch([type, status, component, q, bindingOnly], () => { offset.value = 0 })
const { data: page, error, pending } = useApiGet<ArchPage>(() => repoPath(repo.value, `architecture?${query.value}`))
const { data: issuesData } = useApiGet<{ issues: ArchIssue[] }>(() => repoPath(repo.value, 'architecture/issues'))

const TYPE: Record<string, string> = { adr: 'Decisions (ADR)', rfc: 'RFCs', standard: 'Standards', policy: 'Policies', overview: 'Overviews', runbook: 'Runbooks', diagram: 'Diagrams' }
const total = computed(() => Object.values(page.value?.facets.types ?? {}).reduce((a, b) => a + b, 0))
const inForce = computed(() => (page.value?.entries ?? []).filter((e) => e.binding).length)
const stale = computed(() => {
  const t = page.value?.source.indexed_at ? Date.parse(page.value.source.indexed_at) : NaN
  return !Number.isNaN(t) && Date.now() - t > 7 * 86400_000
})
const statusClass = (s: string) => ({ accepted: 'badge--green', active: 'badge--green', current: 'badge--green', implemented: 'badge--green', proposed: 'badge--amber', draft: 'badge--amber', 'in-review': 'badge--amber', unknown: 'badge--pink', rejected: 'badge--pink', superseded: '', deprecated: '', retired: '', outdated: 'badge--amber', withdrawn: '' }[s] ?? '')
const detail = (id: string) => `/repos/${encodeURIComponent(repo.value)}/architecture/${encodeURIComponent(id)}`
const when = (iso?: string) => (iso ? new Date(iso).toLocaleString([], { month: 'short', day: 'numeric', year: 'numeric' }) : '')
const issueOf = (id: string) => (issuesData.value?.issues ?? []).filter((i) => i.records.includes(id))
const example = `---
status: accepted
components: [api]
topic: storage
supersedes: ADR-0001
implements: [internal/store]
---
# Use PostgreSQL for accounts`
</script>

<template>
  <div class="stack">
    <PageHeader
      eyebrow="Architecture" title="Decisions, standards and design"
      lede="The architecture records this repository keeps as files: decisions (ADRs), RFCs, standards, policies, overviews, runbooks and diagrams. Only an accepted decision or an active standard is a rule; drafts, proposals and replaced records are shown for what they are."
      :crumbs="[{ label: 'Repositories', to: '/' }, { label: repo, to: `/repos/${encodeURIComponent(repo)}` }, { label: 'Architecture' }]"
    >
      <SyncControl :repo="repo" />
    </PageHeader>

    <p v-if="error" class="notice notice--danger">{{ apiErrorMessage(error) }}</p>
    <template v-else-if="page">
      <p class="muted small">
        Indexed at commit <span class="mono">{{ page.source.head?.slice(0, 8) || 'unknown' }}</span>
        <template v-if="page.source.indexed_at"> · {{ when(page.source.indexed_at) }}</template>.
        <span v-if="stale" class="badge badge--amber">Index is more than a week old: Sync to refresh</span>
      </p>

      <div v-if="!total && !pending" class="empty">
        <strong>No architecture records found</strong>
        Put decisions in <span class="mono">docs/adr/</span>, RFCs in <span class="mono">docs/rfcs/</span>, standards in <span class="mono">docs/standards/</span> or overviews in <span class="mono">docs/architecture/</span>, then Sync. Metadata goes in front matter:
        <pre class="sig mono">{{ example }}</pre>
        Plain <span class="mono">- Status: accepted</span> lines and a <span class="mono">## Status</span> section work too.
      </div>

      <template v-else>
        <div class="grid cols-4">
          <StatTile label="Records" :value="String(total)" :hint="`${page.total} match the filters`" />
          <StatTile label="Rules in force on this page" :value="String(inForce)" hint="accepted decisions and active standards" />
          <StatTile label="Needs a look" :value="String(issuesData?.issues.length ?? page.issues)" hint="conflicts, mismatches, broken references" />
          <StatTile label="Types" :value="String(Object.keys(page.facets.types).length)" />
        </div>

        <section v-if="issuesData?.issues.length" class="panel edge--amber">
          <p class="eyebrow">Needs a look</p>
          <h2>Possible problems in the records</h2>
          <p class="muted small">These are reported, not decided. A person chooses which record is right.</p>
          <ul class="plain">
            <li v-for="i in issuesData.issues" :key="i.kind + i.records.join()">
              <span class="badge badge--amber">{{ i.kind.replace(/-/g, ' ') }}</span> {{ i.message }}
              <span v-for="r in i.records" :key="r"> <NuxtLink :to="detail(r)" class="mono">{{ r }}</NuxtLink></span>
            </li>
          </ul>
        </section>

        <section class="panel">
          <div class="row" role="group" aria-label="Filter by type">
            <span class="label">Type</span>
            <button class="chip" :class="{ active: !type }" @click="type = ''">All</button>
            <button v-for="(n, t) in page.facets.types" :key="t" class="chip" :class="{ active: type === t }" @click="type = type === t ? '' : (t as string)">{{ TYPE[t] ?? t }} · {{ n }}</button>
          </div>
          <div class="row" role="group" aria-label="Filter by status">
            <span class="label">Status</span>
            <button class="chip" :class="{ active: !status }" @click="status = ''">Any</button>
            <button v-for="(n, s) in page.facets.statuses" :key="s" class="chip" :class="{ active: status === s }" @click="status = status === s ? '' : (s as string)">{{ s }} · {{ n }}</button>
          </div>
          <div class="row">
            <label class="row compact"><input v-model="bindingOnly" type="checkbox"> Rules in force only</label>
            <label class="row compact"><span class="label">Component</span>
              <select v-model="component" aria-label="Filter by component"><option value="">Any</option><option v-for="(n, c) in page.facets.components" :key="c" :value="c">{{ c }} ({{ n }})</option></select>
            </label>
            <input v-model="q" class="search" placeholder="Search records" aria-label="Search records">
          </div>

          <div v-if="page.entries.length" class="table-wrap">
            <table>
              <thead><tr><th>Record</th><th>Type</th><th>Status</th><th>Applies to</th><th>Where</th></tr></thead>
              <tbody>
                <tr v-for="e in page.entries" :key="e.id">
                  <td>
                    <NuxtLink :to="detail(e.id)" class="mono">{{ e.id }}</NuxtLink>
                    <div>{{ e.title }}</div>
                    <div v-if="e.summary" class="muted small">{{ e.summary }}</div>
                  </td>
                  <td>{{ TYPE[e.type] ?? e.type }}</td>
                  <td>
                    <span class="badge" :class="statusClass(e.status)">{{ e.status }}</span>
                    <div v-if="e.binding" class="small"><strong>Rule in force</strong></div>
                    <div v-else-if="e.superseded_by?.length" class="small muted">Replaced by {{ e.superseded_by.join(', ') }}</div>
                    <div v-if="issueOf(e.id).length" class="small"><span class="badge badge--amber">needs a look</span></div>
                  </td>
                  <td class="small">
                    <button v-for="c in e.components" :key="c" class="chip" @click="component = c">{{ c }}</button>
                    <span v-if="!e.components?.length" class="muted">whole system</span>
                  </td>
                  <td class="mono muted small">{{ e.location }}</td>
                </tr>
              </tbody>
            </table>
          </div>
          <div v-else class="empty"><strong>No records match</strong>Change the filters.</div>

          <div class="row">
            <button class="chip" :disabled="offset === 0" @click="offset = Math.max(0, offset - LIMIT)">Previous</button>
            <span class="muted small">{{ page.total ? offset + 1 : 0 }}–{{ Math.min(offset + LIMIT, page.total) }} of {{ page.total }}</span>
            <button class="chip" :disabled="offset + LIMIT >= page.total" @click="offset += LIMIT">Next</button>
          </div>
          <p v-if="page.truncated" class="notice">This repository has more records than can be read at once; narrow the filters.</p>
        </section>
      </template>
    </template>
    <p v-else class="muted">Loading…</p>
  </div>
</template>

<style scoped>
.plain { list-style: none; padding: 0; margin: 0; display: grid; gap: 8px; }
.compact { gap: 6px; }
.search { width: 220px; min-height: 30px; padding: 4px 10px; }
.small { font-size: 12.5px; }
</style>
