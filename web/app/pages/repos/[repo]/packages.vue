<script setup lang="ts">
import type { GraphView } from '~/utils/types'

const route = useRoute()
const repo = computed(() => route.params.repo as string)
const { data, error } = useApiGet<GraphView>(() => repoPath(repo.value, 'packages/graph'))

const fanIn = computed(() => {
  const counts = new Map<string, number>()
  for (const e of data.value?.edges ?? []) counts.set(e.to, (counts.get(e.to) ?? 0) + 1)
  return counts
})
const rows = computed(() =>
  [...(data.value?.nodes ?? [])].sort((a, b) => (fanIn.value.get(b.id) ?? 0) - (fanIn.value.get(a.id) ?? 0)),
)
</script>

<template>
  <div v-if="error" class="panel error">{{ apiErrorMessage(error) }}</div>
  <div v-else-if="data" class="stack">
    <PageHeader eyebrow="Architecture" title="Package imports" lede="In-repository import graph and which packages everything else depends on."
      :crumbs="[{ label: 'Repositories', to: '/' }, { label: repo, to: `/repos/${encodeURIComponent(repo)}` }, { label: 'Packages' }]" />
    <div class="panel">
      <h2>Package imports <span class="muted">· {{ data.nodes.length }} packages, {{ data.edges.length }} in-repo imports</span></h2>
      <GraphCanvas :nodes="data.nodes" :edges="data.edges" :height="620" />
    </div>
    <div class="panel">
      <h2>Packages by importers</h2>
      <div class="table-wrap">
        <table>
          <thead><tr><th>Package</th><th>Summary</th><th class="num">Imported by</th></tr></thead>
          <tbody>
            <tr v-for="p in rows" :key="p.id">
              <td class="mono">{{ p.name }}</td>
              <td class="muted">{{ p.summary }}</td>
              <td class="num">{{ fanIn.get(p.id) ?? 0 }}</td>
            </tr>
          </tbody>
        </table>
      </div>
    </div>
  </div>
</template>
