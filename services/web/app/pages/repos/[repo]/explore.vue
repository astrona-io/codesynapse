<script setup lang="ts">
import { onBeforeUnmount } from 'vue'
import type { FileOutline, GraphView, NodeRef, OutlineEntry, SymbolCard } from '~/utils/types'

const route = useRoute()
const router = useRouter()
const repo = computed(() => route.params.repo as string)
const selected = computed(() => (route.query.id as string | undefined) ?? '')
const selectedFile = computed(() => (selected.value ? '' : (route.query.file as string | undefined) ?? ''))
const q = ref('')
const results = ref<NodeRef[]>([])
const searchError = ref('')
const searching = ref(false)

let debounce: ReturnType<typeof setTimeout> | undefined
// Answers can arrive out of order; only the newest search may fill the list.
let searchSeq = 0
watch(q, (val) => {
  clearTimeout(debounce)
  const seq = ++searchSeq
  if (val.trim().length < 2) { results.value = []; searchError.value = ''; searching.value = false; return }
  searching.value = true
  debounce = setTimeout(async () => {
    try {
      const res = await apiGet<{ results: NodeRef[] }>(`${repoPath(repo.value, 'search')}?${new URLSearchParams({ q: val, limit: '30' })}`)
      if (seq !== searchSeq) return
      results.value = res.results ?? []
      searchError.value = ''
    } catch (e) {
      if (seq === searchSeq) searchError.value = apiErrorMessage(e)
    } finally {
      if (seq === searchSeq) searching.value = false
    }
  }, 200)
})
// Results belong to one repository.
watch(repo, () => { q.value = ''; results.value = []; searchError.value = ''; searching.value = false })

// A file, document or doc section has no symbol card; picking one opens the
// outline of its file instead. Results without a file (modules, external
// packages) have nothing to open, so they are listed but not clickable.
const fileOf = (r: NodeRef) => (r.location ?? '').replace(/:\d+(-\d+)?$/, '')
// The outline request follows the last file opened, so leaving the file for
// a symbol does not send a request without one.
const outlineFile = ref(selectedFile.value)
watch(selectedFile, (f) => { if (f) outlineFile.value = f })
const { data: outline, error: outlineError, pending: outlinePending } = useApiGet<FileOutline>(
  () => repoPath(repo.value, `outline?${new URLSearchParams({ file: outlineFile.value })}`),
  { immediate: !!outlineFile.value },
)
const outlineEntries = computed<OutlineEntry[]>(() => (outline.value?.file === selectedFile.value ? outline.value.entries ?? [] : []))
onBeforeUnmount(() => clearTimeout(debounce))

const card = ref<SymbolCard | null>(null)
const graph = ref<GraphView | null>(null)
const loadError = ref('')
const graphError = ref('')
const loading = ref(false)
let loadVersion = 0
async function load(id: string) {
  const version = ++loadVersion
  card.value = null
  graph.value = null
  loadError.value = ''
  graphError.value = ''
  loading.value = !!id
  if (!id) return
  try {
    await Promise.all([
      apiGet<SymbolCard>(`${repoPath(repo.value, 'symbol')}?${new URLSearchParams({ id })}`).then(
        (c) => { if (version === loadVersion) card.value = c },
        (e) => { if (version === loadVersion) loadError.value = apiErrorMessage(e) },
      ),
      apiGet<GraphView>(`${repoPath(repo.value, 'graph')}?${new URLSearchParams({ id, depth: '2' })}`).then(
        (g) => { if (version === loadVersion) graph.value = g },
        (e) => { if (version === loadVersion) graphError.value = apiErrorMessage(e) },
      ),
    ])
  } finally {
    if (version === loadVersion) loading.value = false
  }
}
watch([repo, selected], ([, id]) => load(id), { immediate: true })

function pick(id: string) {
  router.push({ query: { id } })
}
function pickFile(file: string) {
  router.push({ query: { file } })
}
const isCode = (r: { kind: string }) => ['Function', 'Method', 'InterfaceMethod', 'Type'].includes(r.kind)
function open(r: NodeRef) {
  if (isCode(r)) pick(r.id)
  else if (fileOf(r)) pickFile(fileOf(r))
}
</script>

<template>
  <div class="stack">
    <PageHeader eyebrow="Explore" title="Symbol explorer" lede="Signature, callers, callees, tests and docs for any symbol — the same card agents get over MCP."
      :crumbs="[{ label: 'Repositories', to: '/' }, { label: repo, to: `/repos/${encodeURIComponent(repo)}` }, { label: 'Explore' }]" />
    <div class="panel">
      <input v-model="q" type="search" placeholder="Search functions, types, files, doc sections…" aria-label="Search">
      <DataState v-if="q.trim().length >= 2" :error="searchError" :pending="searching" :empty="!results.length" loading="Searching…" compact :text="`Nothing matches “${q.trim()}”.`">
        <div class="results">
          <template v-for="r in results" :key="r.id">
            <button v-if="isCode(r) || fileOf(r)" class="secondary result" :title="isCode(r) ? 'Open its symbol card' : 'Open the outline of its file'" @click="open(r)">
              <span class="tag">{{ r.kind }}</span> <span class="mono name">{{ r.name }}</span>
              <span class="muted mono loc">{{ r.location }}</span>
            </button>
            <div v-else class="result result--static">
              <span class="tag">{{ r.kind }}</span> <span class="mono name">{{ r.name }}</span>
            </div>
          </template>
        </div>
      </DataState>
    </div>

    <section v-if="selectedFile" class="panel">
      <p class="eyebrow">File outline</p>
      <h2 class="mono symbol-name">{{ selectedFile }}</h2>
      <DataState :error="outlineError" :pending="outlinePending" :empty="!outlineEntries.length" loading="Loading the outline…" title="Nothing indexed in this file" text="The graph holds no functions, types, sections or keys for it.">
        <div class="table-wrap"><table><tbody>
          <tr v-for="e in outlineEntries" :key="e.id">
            <td><span class="tag">{{ e.kind }}</span></td>
            <td>
              <a v-if="isCode(e)" href="#" class="mono" @click.prevent="pick(e.id)">{{ e.name }}</a>
              <span v-else class="mono">{{ e.name }}</span>
              <p v-if="e.summary" class="muted small">{{ e.summary }}</p>
            </td>
            <td class="mono muted num">L{{ e.lines }}</td>
          </tr>
        </tbody></table></div>
      </DataState>
    </section>

    <template v-if="selected">
      <p v-if="graphError && card" class="notice notice--warn" role="status">Call neighbourhood unavailable: {{ graphError }}</p>
      <DataState :error="loadError" :pending="loading" :empty="!card" loading="Loading symbol details…" title="Symbol not found" text="It may have been renamed or removed since the last sync.">
        <template v-if="card">
          <div class="panel">
            <div class="row" style="justify-content: space-between">
              <h2 class="mono symbol-name">{{ shortName(card.symbol) }}</h2>
              <span class="tag">{{ card.symbol.kind }}</span>
            </div>
            <p class="mono muted path">{{ location(card.symbol) }}</p>
            <p v-if="card.resolved_from" class="small" role="status">Resolved through a human identity review from <span class="mono">{{ card.resolved_from }}</span>.</p>
            <p v-if="card.symbol.symbol_id" class="small">
              <NuxtLink :to="`/repos/${encodeURIComponent(repo)}/identities?id=${encodeURIComponent(card.symbol.symbol_id)}`">Identity, annotations and reuse: <span class="mono">{{ card.symbol.symbol_id }}</span></NuxtLink>
              <span v-if="card.symbol.revision" class="muted"> · revision {{ card.symbol.revision.slice(0, 12) }}</span>
            </p>
            <pre v-if="card.symbol.signature" class="sig mono">{{ card.symbol.signature }}</pre>
            <p v-if="card.symbol.summary">{{ card.symbol.summary }}</p>
            <div class="row">
              <span v-if="card.symbol.complexity" class="tag">complexity {{ card.symbol.complexity }}</span>
              <span v-if="card.symbol.loc" class="tag">{{ card.symbol.loc }} lines</span>
              <span v-if="card.symbol.exported" class="tag">exported</span>
              <span v-for="t in card.symbol.security_tags" :key="t" class="tag sec">{{ t }}</span>
            </div>
          </div>

          <section v-if="card.duplicate" class="panel">
            <p class="eyebrow">Duplicate code</p>
            <h2>
              {{ card.duplicate.kind === 'near' ? 'Edited copies exist' : 'Exact copies exist' }}
              <span class="muted">· {{ card.duplicate.copies }} in total<template v-if="card.duplicate.kind === 'near'">, ~{{ Math.round(card.duplicate.similarity * 100) }}% alike</template></span>
            </h2>
            <p class="muted">{{ card.duplicate.kind === 'near' ? 'Copied and then changed: look at what differs before merging them.' : 'The same logic with other names or values: keep one and call it.' }}</p>
            <div class="table-wrap"><table><tbody>
              <tr v-for="o in card.duplicate.others" :key="o.id">
                <td><a href="#" class="mono" @click.prevent="pick(o.id)">{{ o.name }}</a></td>
                <td class="mono muted path">{{ o.file }}:{{ o.start_line }}-{{ o.end_line }}</td>
              </tr>
            </tbody></table></div>
            <NuxtLink :to="`/repos/${encodeURIComponent(repo)}/duplicates`" class="small">All duplicates in this repository</NuxtLink>
          </section>

          <div v-if="graph" class="panel">
            <h2>Call neighbourhood <span class="muted">· click a node to follow it</span></h2>
            <GraphCanvas :nodes="graph.nodes" :edges="graph.edges" :root="graph.root" @select="pick" />
          </div>

          <div class="grid cols-2">
            <div v-for="section in [
              { title: 'Callers', items: card.callers ?? [] },
              { title: 'Callees', items: card.callees ?? [] },
              { title: 'Tests that reach it', items: card.tests ?? [] },
              { title: 'Implementations', items: card.implementations ?? [] },
              { title: 'Docs that mention it', items: card.docs ?? [] },
            ].filter((s) => s.items.length || s.title === 'Tests that reach it')" :key="section.title" class="panel">
              <h2>{{ section.title }} <span class="muted">· {{ section.items.length }}</span></h2>
              <p v-if="!section.items.length" class="error">None. This symbol is not reached by any test.</p>
              <div class="table-wrap">
                <table>
                  <tbody>
                    <tr v-for="r in section.items" :key="r.id">
                      <td>
                        <a v-if="isCode(r)" href="#" class="mono" @click.prevent="pick(r.id)">{{ r.name }}</a>
                        <span v-else class="mono">{{ r.name }}</span>
                      </td>
                      <td class="mono muted path">{{ r.location }}</td>
                      <td v-if="r.depth && r.depth > 1" class="num muted">d{{ r.depth }}</td>
                    </tr>
                  </tbody>
                </table>
              </div>
            </div>
          </div>
        </template>
      </DataState>
    </template>
    <p v-else-if="!selectedFile" class="muted">Search for a symbol, file or doc section. A symbol shows its signature, callers, callees, tests and docs; a file shows its outline.</p>
  </div>
</template>

<style scoped>
.results { display: grid; gap: 4px; margin-top: 10px; max-height: 320px; overflow-y: auto; }
.result { text-align: left; display: flex; gap: 8px; align-items: center; min-width: 0; }
.result .name { overflow-wrap: anywhere; }
.result--static { padding: 6px 14px; }
.symbol-name { font-size: 17px; margin: 0; overflow-wrap: anywhere; }
.path { overflow-wrap: anywhere; }
.result .loc { margin-left: auto; font-size: 11.5px; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.sig { background: var(--panel-2); padding: 8px 10px; border-radius: var(--radius-xs); white-space: pre-wrap; word-break: break-word; }
</style>
