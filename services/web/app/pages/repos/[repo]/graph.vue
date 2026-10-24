<script setup lang="ts">
import type { GraphView, NodeRef, ViewEdge, ViewNode } from '~/utils/types'

const route = useRoute()
const repo = computed(() => route.params.repo as string)

const NODE_KINDS = ['Function', 'Method', 'InterfaceMethod', 'Type', 'External', 'Package', 'File', 'Section', 'Document', 'Commit', 'Finding', 'Module', 'Vulnerability', 'Config', 'ConfigEntry', 'Template', 'TemplateDef']
const EDGE_KINDS = ['CALLS', 'DISPATCHES_TO', 'IMPLEMENTS', 'HAS_METHOD', 'DEFINES', 'IMPORTS', 'MENTIONS', 'HAS_FINDING', 'AFFECTS', 'INCLUDES', 'USES_VALUE', 'DEPENDS_ON', 'CONTAINS', 'MODIFIES']
const KIND_VAR: Record<string, string> = {
  Function: '--k-func', Method: '--k-method', InterfaceMethod: '--k-iface', Type: '--k-type', External: '--k-ext',
  Package: '--k-pkg', File: '--k-file', Section: '--k-doc', Document: '--k-doc', Commit: '--k-commit',
  Finding: '--k-finding', Module: '--k-module', Vulnerability: '--k-vuln',
  Config: '--k-config', ConfigEntry: '--k-config', Template: '--k-template', TemplateDef: '--k-template',
}
const MAX_NODES = 1200

const nodes = ref<ViewNode[]>([])
const edges = ref<ViewEdge[]>([])
const selected = ref<string | null>(null)
const hiddenKinds = ref(new Set<string>())
const hiddenEdges = ref(new Set<string>(['CONTAINS', 'MODIFIES', 'DEPENDS_ON']))
const seedSize = ref(80)
const includeTests = ref(false)
const busy = ref(false)
const error = ref('')
const explorer = ref<{ fit: () => void; centerOn: (id: string) => void; unpin: (id: string) => void } | null>(null)

const byId = computed(() => new Map(nodes.value.map((n) => [n.id, n])))
const selectedNode = computed(() => (selected.value ? byId.value.get(selected.value) : undefined))
const selectedEdges = computed(() => {
  if (!selected.value) return { in: [] as ViewEdge[], out: [] as ViewEdge[] }
  return {
    in: edges.value.filter((e) => e.to === selected.value),
    out: edges.value.filter((e) => e.from === selected.value),
  }
})
// The same nodes as a list, so the graph can be read and explored without a
// pointer: hubs first, kinds you hid left out, the selected one marked.
const shownNodes = computed(() => orderNodes(nodes.value.filter((n) => !hiddenKinds.value.has(n.kind)), edges.value.filter((e) => !hiddenEdges.value.has(e.kind))))
const connections = computed(() => (selected.value ? edges.value.filter((e) => e.from === selected.value || e.to === selected.value).length : 0))
const announcement = computed(() => announce({ busy: busy.value, nodes: nodes.value.length, edges: edges.value.length, selected: selectedNode.value, connections: connections.value }))
const kindCounts = computed(() => {
  const c: Record<string, number> = {}
  for (const n of nodes.value) c[n.kind] = (c[n.kind] ?? 0) + 1
  return c
})

function edgeKey(e: ViewEdge) { return `${e.from}>${e.kind}>${e.to}` }

async function load() {
  busy.value = true
  error.value = ''
  try {
    const v = await $fetch<GraphView>(apiPath(repoPath(repo.value, 'graph/overview')), {
      query: { limit: seedSize.value, tests: includeTests.value ? '1' : '0' },
    })
    nodes.value = v.nodes
    edges.value = v.edges
    selected.value = null
    setTimeout(() => explorer.value?.fit(), 1200)
  } catch (e) {
    error.value = apiErrorMessage(e)
  } finally {
    busy.value = false
  }
}

async function refreshEdges() {
  const res = await apiPost<{ edges: ViewEdge[] }>(repoPath(repo.value, 'graph/edges'), { ids: nodes.value.map((n) => n.id) })
  const seen = new Set(edges.value.map(edgeKey))
  edges.value = [...edges.value, ...res.edges.filter((e) => !seen.has(edgeKey(e)))]
}

async function expand(id: string) {
  if (nodes.value.length >= MAX_NODES) {
    error.value = `Graph holds ${MAX_NODES} nodes; remove some or reset before expanding further.`
    return
  }
  busy.value = true
  error.value = ''
  try {
    const v = await $fetch<GraphView>(apiPath(repoPath(repo.value, 'graph/expand')), { query: { id, limit: 40 } })
    const known = new Set(nodes.value.map((n) => n.id))
    const fresh = v.nodes.filter((n) => !known.has(n.id))
    nodes.value = [...nodes.value, ...fresh]
    for (const n of fresh) hiddenKinds.value.delete(n.kind)
    await refreshEdges()
    selected.value = v.root ?? id
  } catch (e) {
    error.value = apiErrorMessage(e)
  } finally {
    busy.value = false
  }
}

function removeNode(id: string) {
  nodes.value = nodes.value.filter((n) => n.id !== id)
  edges.value = edges.value.filter((e) => e.from !== id && e.to !== id)
  if (selected.value === id) selected.value = null
}
function removeIsolated() {
  const linked = new Set(edges.value.filter((e) => !hiddenEdges.value.has(e.kind)).flatMap((e) => [e.from, e.to]))
  nodes.value = nodes.value.filter((n) => linked.has(n.id) || n.id === selected.value)
}
function toggle(set: Set<string>, key: string) {
  const next = new Set(set)
  if (next.has(key)) next.delete(key)
  else next.add(key)
  return next
}

// Search adds a symbol to the canvas (with its neighbours).
const q = ref('')
const results = ref<NodeRef[]>([])
let debounce: ReturnType<typeof setTimeout> | undefined
watch(q, (val) => {
  clearTimeout(debounce)
  if (val.trim().length < 2) { results.value = []; return }
  debounce = setTimeout(async () => {
    try {
      const res = await $fetch<{ results: NodeRef[] }>(apiPath(repoPath(repo.value, 'search')), { query: { q: val, limit: 12 } })
      results.value = res.results
    } catch (e) {
      error.value = apiErrorMessage(e)
    }
  }, 200)
})
async function addFromSearch(r: NodeRef) {
  q.value = ''
  results.value = []
  await expand(r.id)
  setTimeout(() => explorer.value?.centerOn(r.id), 400)
}

const nameOf = (id: string) => byId.value.get(id)?.name ?? id.split('|').pop()
const explore = (id: string) => ({ path: `/repos/${encodeURIComponent(repo.value)}/explore`, query: { id } })
const isCode = (kind?: string) => !!kind && ['Function', 'Method', 'InterfaceMethod', 'Type'].includes(kind)

onMounted(load)
</script>

<template>
  <div class="graph-page">
    <nav class="crumbs" aria-label="Breadcrumb">
      <NuxtLink to="/">Repositories</NuxtLink><span class="sep">/</span>
      <NuxtLink :to="`/repos/${encodeURIComponent(repo)}`">{{ repo }}</NuxtLink><span class="sep">/</span><span class="current">Graph</span>
    </nav>
    <div class="panel toolbar">
      <div class="search">
        <input v-model="q" type="search" placeholder="Add a symbol, file or doc section…" aria-label="Add node">
        <div v-if="results.length" class="dropdown">
          <button v-for="r in results" :key="r.id" class="secondary" @click="addFromSearch(r)">
            <span class="tag">{{ r.kind }}</span> <span class="mono">{{ r.name }}</span>
          </button>
        </div>
      </div>
      <label class="row compact">Seed
        <select v-model.number="seedSize" @change="load">
          <option :value="40">40</option><option :value="80">80</option><option :value="150">150</option><option :value="300">300</option>
        </select>
      </label>
      <label class="row compact"><input v-model="includeTests" type="checkbox" @change="load"> tests</label>
      <button class="secondary" @click="explorer?.fit()">Fit</button>
      <button class="secondary" @click="removeIsolated">Drop isolated</button>
      <button class="secondary" :disabled="busy" @click="load">Reset</button>
      <span class="muted">{{ nodes.length }} nodes · {{ edges.length }} edges{{ busy ? ' · loading…' : '' }}</span>
    </div>
    <p v-if="error" class="error">{{ error }}</p>

    <div class="layout">
      <div class="panel canvas-panel">
        <ClientOnly>
          <GraphExplorer
            ref="explorer"
            :nodes="nodes" :edges="edges" :selected="selected"
            :hidden-kinds="hiddenKinds" :hidden-edges="hiddenEdges"
            @select="(id) => (selected = id)" @expand="expand"
          />
        </ClientOnly>
        <p class="sr-only" role="status" aria-live="polite">{{ announcement }}</p>
        <p class="muted hint">Keyboard: focus the graph, then arrow keys move between nodes · Enter expands · Escape clears · F fits. Pointer: scroll to zoom · drag background to pan · drag a node to pin it · click to inspect · double-click to expand</p>
      </div>

      <aside class="side">
        <div class="panel">
          <h2>Nodes</h2>
          <div class="chips">
            <button
              v-for="k in NODE_KINDS.filter((k) => kindCounts[k])" :key="k"
              class="chip" :class="{ off: hiddenKinds.has(k) }" @click="hiddenKinds = toggle(hiddenKinds, k)"
            >
              <i :style="{ background: `var(${KIND_VAR[k]})` }" />{{ k }} <span class="muted">{{ kindCounts[k] }}</span>
            </button>
          </div>
          <h2 style="margin-top: 12px">Edges</h2>
          <div class="chips">
            <button v-for="k in EDGE_KINDS" :key="k" class="chip" :class="{ off: hiddenEdges.has(k) }" @click="hiddenEdges = toggle(hiddenEdges, k)">
              {{ k }}
            </button>
          </div>
          <p class="muted small">Red ring = calls security-sensitive APIs. Dashed = interface dispatch.</p>
        </div>

        <div class="panel">
          <h2 id="node-list-title">Nodes in the graph <span class="muted">{{ shownNodes.length }}</span></h2>
          <p class="muted small">Select a node to see its connections below; Expand loads its neighbours.</p>
          <ul class="node-list" aria-labelledby="node-list-title">
            <li v-for="n in shownNodes" :key="n.id" :class="{ current: n.id === selected }">
              <button type="button" class="pick" :aria-pressed="n.id === selected" @click="selected = n.id">
                <span class="tag">{{ n.kind }}</span> <span class="mono">{{ n.name }}</span>
              </button>
              <button type="button" class="secondary tiny" :disabled="busy" :aria-label="`Expand ${n.name}`" @click="expand(n.id)">Expand</button>
            </li>
          </ul>
        </div>

        <div v-if="selectedNode" class="panel">
          <div class="row" style="justify-content: space-between">
            <span class="tag">{{ selectedNode.kind }}</span>
            <div class="row">
              <button class="secondary" :disabled="busy" @click="expand(selectedNode.id)">Expand</button>
              <button class="secondary" @click="explorer?.unpin(selectedNode.id)">Unpin</button>
              <button class="secondary" @click="removeNode(selectedNode.id)">Remove</button>
            </div>
          </div>
          <h2 class="mono" style="margin-top: 8px; word-break: break-word">{{ selectedNode.name }}</h2>
          <p v-if="selectedNode.location" class="mono muted">{{ selectedNode.location }}</p>
          <p v-if="selectedNode.summary">{{ selectedNode.summary }}</p>
          <div class="row">
            <span v-if="selectedNode.complexity" class="tag">complexity {{ selectedNode.complexity }}</span>
            <span v-if="selectedNode.is_test" class="tag">test</span>
            <span v-for="t in selectedNode.security_tags" :key="t" class="tag sec">{{ t }}</span>
          </div>
          <p v-if="isCode(selectedNode.kind)"><NuxtLink :to="explore(selectedNode.id)">Open symbol card →</NuxtLink></p>
          <div v-for="dir in (['in', 'out'] as const)" :key="dir">
            <h2 v-if="selectedEdges[dir].length" style="margin-top: 12px">{{ dir === 'in' ? 'Incoming' : 'Outgoing' }} · {{ selectedEdges[dir].length }}</h2>
            <ul class="edges">
              <li v-for="e in selectedEdges[dir].slice(0, 30)" :key="edgeKey(e)">
                <span class="tag">{{ e.kind }}</span>
                <a href="#" class="mono" @click.prevent="selected = dir === 'in' ? e.from : e.to">{{ nameOf(dir === 'in' ? e.from : e.to) }}</a>
              </li>
            </ul>
          </div>
        </div>
        <div v-else class="panel muted">Select a node to see its details and connections.</div>
      </aside>
    </div>
  </div>
</template>

<style scoped>
.graph-page { display: grid; gap: 12px; }
.toolbar { flex-direction: row; }
.toolbar { display: flex; gap: 10px; align-items: center; flex-wrap: wrap; padding: 10px 12px; }
.search { position: relative; flex: 1 1 260px; }
.dropdown { position: absolute; z-index: 10; top: 100%; left: 0; right: 0; margin-top: 4px; display: grid; gap: 2px; background: var(--panel); border: 1px solid var(--line); border-radius: 8px; padding: 4px; max-height: 320px; overflow-y: auto; }
.dropdown button { text-align: left; }
.compact { gap: 6px; white-space: nowrap; }
.compact select { width: auto; }
.compact input[type='checkbox'] { width: auto; }
.layout { display: grid; grid-template-columns: minmax(0, 1fr) 320px; gap: 12px; }
.canvas-panel { padding: 8px; height: calc(100vh - 170px); min-height: 480px; display: flex; flex-direction: column; }
.canvas-panel > :first-child { flex: 1; }
.hint { font-size: 12px; margin: 6px 4px 0; }
.side { display: grid; gap: 12px; align-content: start; max-height: calc(100vh - 170px); overflow-y: auto; }
.chips { display: flex; flex-wrap: wrap; gap: 4px; }
.chip { background: var(--panel-2); color: var(--ink); border: 1px solid var(--line); padding: 3px 9px; font-size: 12px; border-radius: 999px; }
.chip.off { opacity: .4; }
.chip i { display: inline-block; width: 9px; height: 9px; border-radius: 50%; margin-right: 5px; vertical-align: middle; }
.small { font-size: 12px; margin-top: 10px; }
.node-list { list-style: none; padding: 0; margin: 8px 0 0; display: grid; gap: 3px; max-height: 280px; overflow-y: auto; }
.node-list li { display: flex; gap: 6px; align-items: center; justify-content: space-between; }
.node-list li.current { background: var(--accent-soft); border-radius: var(--radius-xs); }
.node-list .pick { flex: 1; min-width: 0; background: none; border: 0; color: var(--ink); text-align: left; padding: 4px 6px; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; font-weight: 500; }
.node-list .pick:hover:not(:disabled) { background: var(--hover); }
.edges { list-style: none; padding: 0; margin: 0; display: grid; gap: 3px; }
.edges li { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
@media (max-width: 900px) {
  .layout { grid-template-columns: 1fr; }
  .canvas-panel { height: 70vh; }
  .side { max-height: none; }
}
</style>
