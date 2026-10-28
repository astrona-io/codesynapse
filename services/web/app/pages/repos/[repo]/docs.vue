<script setup lang="ts">
import type { GraphView, Ranked } from '~/utils/types'

interface OutlineEntry { id: string; kind: string; name: string; lines: string; summary?: string; level?: number }

const route = useRoute()
const router = useRouter()
const repo = computed(() => route.params.repo as string)
useHead({ title: () => `Docs · ${repo.value}` })
const { data: docs, error } = useApiGet<{ results: Ranked[] }>(() => repoPath(repo.value, 'list?kind=Document&metric=loc&limit=200'))
const selected = computed(() => (route.query.file as string | undefined) ?? docs.value?.results[0]?.node.file ?? '')
const outline = ref<OutlineEntry[]>([])
const mentions = ref<Record<string, { id: string; name: string; kind: string }[]>>({})
const filter = ref('')

const outlineError = ref('')
const outlineLoading = ref(false)
// Each change of repository or document starts a new load; answers of an older one are dropped.
let loadSeq = 0
watch([repo, selected], async ([name, file]) => {
  const seq = ++loadSeq
  outline.value = []
  mentions.value = {}
  outlineError.value = ''
  outlineLoading.value = false
  if (!file) return
  outlineLoading.value = true
  try {
    const res = await apiGet<{ entries: OutlineEntry[] }>(repoPath(name, `outline?file=${encodeURIComponent(file)}`))
    if (seq !== loadSeq) return
    outline.value = res.entries.filter((e) => e.kind === 'Section')
  } catch (e) {
    if (seq === loadSeq) outlineError.value = apiErrorMessage(e)
    return
  } finally {
    if (seq === loadSeq) outlineLoading.value = false
  }
  // Code each section mentions, fetched in parallel; a section without an answer shows no mentions.
  await Promise.all(outline.value.map(async (s) => {
    try {
      const v = await apiGet<GraphView>(repoPath(name, `graph/expand?id=${encodeURIComponent(s.id)}&edge=MENTIONS&limit=20`))
      const code = v.nodes.filter((n) => n.id !== s.id && ['Function', 'Method', 'InterfaceMethod', 'Type'].includes(n.kind))
      if (code.length && seq === loadSeq) mentions.value = { ...mentions.value, [s.id]: code }
    } catch { /* mentions are optional */ }
  }))
}, { immediate: true })

const visibleDocs = computed(() => (docs.value?.results ?? []).filter((d) => !filter.value || (d.node.file ?? '').toLowerCase().includes(filter.value.toLowerCase()) || d.node.name.toLowerCase().includes(filter.value.toLowerCase())))
const current = computed(() => docs.value?.results.find((d) => d.node.file === selected.value)?.node)
const indent = (s: OutlineEntry) => Math.max(0, (s.level ?? 1) - 1) * 16
const explore = (id: string) => ({ path: `/repos/${encodeURIComponent(repo.value)}/explore`, query: { id } })
const tokens = (bytes?: number) => Math.round((bytes ?? 0) / 4)
</script>

<template>
  <div class="stack">
    <PageHeader
      eyebrow="Knowledge" title="Documentation"
      lede="Markdown indexed into sections with line ranges and summaries, linked to the code they mention. Agents read one section instead of a whole file."
      :crumbs="[{ label: 'Repositories', to: '/' }, { label: repo, to: `/repos/${encodeURIComponent(repo)}` }, { label: 'Docs' }]"
    />
    <p v-if="error" class="notice notice--danger" role="alert">{{ apiErrorMessage(error) }}</p>
    <div v-else class="docs-layout">
      <aside class="panel doc-list">
        <p class="eyebrow">Documents · {{ fmtInt(docs?.results.length ?? 0) }}</p>
        <input v-model="filter" type="search" placeholder="Filter…" aria-label="Filter documents">
        <nav aria-label="Documents">
          <button
            v-for="d in visibleDocs" :key="d.node.id" class="doc-link" :class="{ 'is-active': d.node.file === selected }"
            :aria-current="d.node.file === selected ? 'page' : undefined"
            @click="router.push({ query: { file: d.node.file } })"
          >
            <span class="doc-title">{{ d.node.name }}</span>
            <span class="mono dim">{{ d.node.file }} · {{ plural(d.node.loc ?? 0, 'line') }}</span>
          </button>
        </nav>
        <p v-if="!docs" class="muted" role="status">Loading…</p>
        <p v-else-if="!docs.results.length" class="muted">No Markdown documents indexed.</p>
        <p v-else-if="!visibleDocs.length" class="muted">No document matches “{{ filter }}”.</p>
      </aside>

      <section class="panel">
        <template v-if="current">
          <div class="panel__header">
            <div class="head"><p class="eyebrow path">{{ current.file }}</p><h2>{{ current.name }}</h2></div>
            <span class="badge badge--violet">{{ plural(outline.length, 'section') }} · ~{{ fmtInt(tokens(current.bytes)) }} tokens</span>
          </div>
          <p v-if="current.summary" class="muted">{{ current.summary }}</p>
          <p v-if="outlineError" class="notice notice--danger" role="alert">{{ outlineError }}</p>
          <p v-else-if="outlineLoading" class="muted" role="status">Loading sections…</p>
          <p v-else-if="!outline.length" class="muted">This document has no sections.</p>
          <ol class="outline">
            <li v-for="s in outline" :key="s.id" :style="{ marginLeft: `${indent(s)}px` }">
              <div class="row between">
                <strong>{{ s.name }}</strong>
                <span class="mono dim">L{{ s.lines }}</span>
              </div>
              <p v-if="s.summary" class="muted summary">{{ s.summary }}</p>
              <div v-if="mentions[s.id]" class="row mentions">
                <span class="label">mentions</span>
                <NuxtLink v-for="c in mentions[s.id]" :key="c.id" :to="explore(c.id)" class="tag mono">{{ c.name }}</NuxtLink>
              </div>
            </li>
          </ol>
        </template>
        <div v-else-if="docs?.results.length" class="empty"><strong>No document selected</strong>Pick one from the list.</div>
      </section>
    </div>
  </div>
</template>

<style scoped>
.docs-layout { display: grid; grid-template-columns: 320px minmax(0, 1fr); gap: 20px; align-items: start; }
.doc-list { position: sticky; top: 24px; max-height: calc(100vh - 48px); overflow-y: auto; }
.doc-list nav { display: flex; flex-direction: column; gap: 2px; }
.doc-link { all: unset; cursor: pointer; display: flex; flex-direction: column; gap: 2px; padding: 8px 12px; border-radius: var(--radius-sm); }
.doc-link:hover { background: var(--hover); }
.doc-link:focus-visible { box-shadow: var(--focus); }
.doc-link .mono { overflow-wrap: anywhere; }
.doc-link.is-active { background: var(--primary); color: var(--on-primary); }
.doc-link.is-active :is(.muted, .dim, .small, .doc-title) { color: inherit; }
.head { min-width: 0; }
.path { overflow-wrap: anywhere; }
.doc-title { font-size: 14px; font-weight: 500; color: var(--ink); }
.outline { list-style: none; padding: 0; margin: 0; display: flex; flex-direction: column; }
.outline li { padding: 12px 0; border-bottom: 1px dashed var(--line); }
.outline li:last-child { border-bottom: 0; }
.between { justify-content: space-between; }
.summary { font-size: 13.5px; margin-top: 4px; }
.mentions { margin-top: 8px; gap: 4px; }
.mentions .tag { color: var(--info); background: var(--info-soft); }
@media (max-width: 1000px) { .docs-layout { grid-template-columns: 1fr; } .doc-list { position: static; max-height: none; } }
</style>
