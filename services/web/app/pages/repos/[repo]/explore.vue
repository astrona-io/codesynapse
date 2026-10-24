<script setup lang="ts">
import type { GraphView, NodeRef, SymbolCard } from '~/utils/types'

const route = useRoute()
const router = useRouter()
const repo = computed(() => route.params.repo as string)
const selected = computed(() => (route.query.id as string | undefined) ?? '')
const q = ref('')
const results = ref<NodeRef[]>([])
const searchError = ref('')

let debounce: ReturnType<typeof setTimeout> | undefined
watch(q, (val) => {
  clearTimeout(debounce)
  if (val.trim().length < 2) { results.value = []; return }
  debounce = setTimeout(async () => {
    try {
      const res = await $fetch<{ results: NodeRef[] }>(apiPath(repoPath(repo.value, 'search')), { query: { q: val, limit: 30 } })
      results.value = res.results
      searchError.value = ''
    } catch (e) {
      searchError.value = apiErrorMessage(e)
    }
  }, 200)
})

const card = ref<SymbolCard | null>(null)
const graph = ref<GraphView | null>(null)
const loadError = ref('')
async function load(id: string) {
  if (!id) { card.value = null; graph.value = null; return }
  loadError.value = ''
  try {
    const [c, g] = await Promise.all([
      $fetch<SymbolCard>(apiPath(repoPath(repo.value, 'symbol')), { query: { id } }),
      $fetch<GraphView>(apiPath(repoPath(repo.value, 'graph')), { query: { id, depth: 2 } }),
    ])
    card.value = c
    graph.value = g
  } catch (e) {
    loadError.value = apiErrorMessage(e)
  }
}
watch(selected, load, { immediate: true })

function pick(id: string) {
  router.push({ query: { id } })
}
const isCode = (r: NodeRef) => ['Function', 'Method', 'InterfaceMethod', 'Type'].includes(r.kind)
</script>

<template>
  <div class="stack">
    <PageHeader eyebrow="Explore" title="Symbol explorer" lede="Signature, callers, callees, tests and docs for any symbol — the same card agents get over MCP."
      :crumbs="[{ label: 'Repositories', to: '/' }, { label: repo, to: `/repos/${encodeURIComponent(repo)}` }, { label: 'Explore' }]" />
    <div class="panel">
      <input v-model="q" type="search" placeholder="Search functions, types, files, doc sections…" aria-label="Search">
      <p v-if="searchError" class="error">{{ searchError }}</p>
      <div v-if="results.length" class="results">
        <button v-for="r in results" :key="r.id" class="secondary result" :disabled="!isCode(r)" @click="pick(r.id)">
          <span class="tag">{{ r.kind }}</span> <span class="mono">{{ r.name }}</span>
          <span class="muted mono loc">{{ r.location }}</span>
        </button>
      </div>
    </div>

    <p v-if="loadError" class="panel error">{{ loadError }}</p>

    <template v-if="card">
      <div class="panel">
        <div class="row" style="justify-content: space-between">
          <h1 class="mono" style="font-size: 17px">{{ shortName(card.symbol) }}</h1>
          <span class="tag">{{ card.symbol.kind }}</span>
        </div>
        <p class="mono muted">{{ location(card.symbol) }}</p>
        <p v-if="card.resolved_from" class="small" role="status">Resolved through a human identity review from <span class="mono">{{ card.resolved_from }}</span>.</p>
        <p v-if="card.symbol.symbol_id" class="small">
          <NuxtLink :to="`/repos/${encodeURIComponent(repo)}/identities?id=${card.symbol.symbol_id}`">Identity, annotations and reuse: <span class="mono">{{ card.symbol.symbol_id }}</span></NuxtLink>
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
            <td class="mono muted">{{ o.file }}:{{ o.start_line }}-{{ o.end_line }}</td>
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
          { title: 'Callers', items: card.callers },
          { title: 'Callees', items: card.callees },
          { title: 'Tests that reach it', items: card.tests },
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
                  <td class="mono muted">{{ r.location }}</td>
                  <td v-if="r.depth && r.depth > 1" class="num muted">d{{ r.depth }}</td>
                </tr>
              </tbody>
            </table>
          </div>
        </div>
      </div>
    </template>
    <p v-else-if="!selected" class="muted">Search for a symbol to see its signature, callers, callees, tests and docs.</p>
  </div>
</template>

<style scoped>
.results { display: grid; gap: 4px; margin-top: 10px; max-height: 320px; overflow-y: auto; }
.result { text-align: left; display: flex; gap: 8px; align-items: center; }
.result .loc { margin-left: auto; font-size: 11.5px; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.sig { background: var(--panel-2); padding: 8px 10px; border-radius: 8px; white-space: pre-wrap; word-break: break-word; }
</style>
