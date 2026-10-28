<script setup lang="ts">
import type { GraphView } from '~/utils/types'

const route = useRoute()
const repo = computed(() => route.params.repo as string)
useHead({ title: () => `Packages · ${repo.value}` })
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
  <div class="stack">
    <PageHeader eyebrow="Architecture" title="Package imports" lede="In-repository import graph and which packages everything else depends on."
      :crumbs="[{ label: 'Repositories', to: '/' }, { label: repo, to: `/repos/${encodeURIComponent(repo)}` }, { label: 'Packages' }]" />
    <p v-if="error" class="notice notice--danger" role="alert">{{ apiErrorMessage(error) }}</p>
    <p v-else-if="!data" class="muted" role="status">Loading packages…</p>
    <div v-else-if="!data.nodes.length" class="empty">
      <strong>No packages indexed</strong>
      This repository has no packages in its graph yet. Sync it, or check that it holds code in a supported language.
    </div>
    <template v-else>
      <div class="panel">
        <h2>Package imports <span class="muted">· {{ plural(data.nodes.length, 'package') }}, {{ plural(data.edges.length, 'in-repo import') }}</span></h2>
        <GraphCanvas :nodes="data.nodes" :edges="data.edges" :height="620" />
      </div>
      <div class="panel">
        <h2>Packages by importers</h2>
        <div class="table-wrap">
          <table>
            <thead><tr><th>Package</th><th>Summary</th><th class="num">Imported by</th></tr></thead>
            <tbody>
              <tr v-for="p in rows" :key="p.id">
                <td class="mono wrap">{{ p.name }}</td>
                <td class="muted">{{ p.summary }}</td>
                <td class="num">{{ fmtInt(fanIn.get(p.id) ?? 0) }}</td>
              </tr>
            </tbody>
          </table>
        </div>
      </div>
    </template>
  </div>
</template>

<style scoped>
.wrap { overflow-wrap: anywhere; }
</style>
