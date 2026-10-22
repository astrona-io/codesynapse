<script setup lang="ts">
import type { DuplicatesReport } from '~/utils/types'

const route = useRoute()
const repo = computed(() => route.params.repo as string)
const { data: rep, error } = useApiGet<DuplicatesReport>(() => repoPath(repo.value, 'duplicates'))
const scope = ref('all')
const kind = ref('all')
const groups = computed(() => (rep.value?.groups ?? []).filter((g) =>
  (scope.value === 'all' || g.scope === scope.value) && (kind.value === 'all' || g.kind === kind.value)))
const explore = (id: string) => ({ path: `/repos/${encodeURIComponent(repo.value)}/explore`, query: { id } })
const scopeClass = (s: string) => ({ file: 'badge--green', package: 'badge--amber', repository: 'badge--pink' }[s] ?? '')
const scopeHint: Record<string, string> = {
  file: 'same file: extract a helper',
  package: 'same package: extract a shared function',
  repository: 'different packages: move to a shared package',
}
</script>

<template>
  <div class="stack">
    <PageHeader
      eyebrow="Code health" title="Duplicate code"
      lede="Functions that repeat the same logic with other names or values. Every copy has to be read, tested and fixed separately, and copies drift apart. Keep one and call it."
      :crumbs="[{ label: 'Repositories', to: '/' }, { label: repo, to: `/repos/${encodeURIComponent(repo)}` }, { label: 'Code health' }]"
    >
      <SyncControl :repo="repo" />
    </PageHeader>
    <CodeHealthTabs :repo="repo" />

    <p v-if="error" class="panel error">{{ apiErrorMessage(error) }}</p>
    <template v-else-if="rep">
      <div class="grid grid--stats">
        <StatTile label="Duplicate groups" :value="rep.summary.groups" :hint="`${rep.summary.duplicated_functions} functions involved`" />
        <StatTile label="Redundant lines" :value="fmtInt(rep.summary.redundant_loc + rep.summary.near_redundant_loc)" :hint="`${rep.summary.percent.toFixed(1)}% of ${fmtInt(rep.summary.function_loc)} function lines`" />
        <StatTile label="Edited copies" :value="rep.summary.near_groups" :hint="`${fmtInt(rep.summary.near_redundant_loc)} lines differ between the variants`" />
      </div>
      <p v-if="rep.truncated" class="notice">Only the largest groups are listed.</p>

      <section class="panel edge--orange">
        <div class="panel__header">
          <div><p class="eyebrow">Code</p><h2>Copies <span class="muted">· {{ groups.length }}</span></h2></div>
        </div>
        <div class="row">
          <span class="label">Kind</span>
          <button v-for="c in ['all', 'exact', 'near']" :key="c" class="chip" :class="{ active: kind === c }" @click="kind = c">{{ c === 'near' ? 'edited' : c }}</button>
          <span class="label">Where</span>
          <button v-for="c in ['all', 'file', 'package', 'repository']" :key="c" class="chip" :class="{ active: scope === c }" @click="scope = c">{{ c }}</button>
        </div>
        <div v-if="groups.length" class="stack">
          <article v-for="g in groups" :key="g.hash" class="group">
            <header class="row">
              <strong>{{ g.copies }} copies of {{ g.loc }} lines</strong>
              <span class="badge" :class="scopeClass(g.scope)" :title="scopeHint[g.scope]">{{ g.scope }}</span>
              <span v-if="g.kind === 'near'" class="badge badge--violet" title="Copies that were edited after copying; the variants differ a little">edited · ~{{ Math.round(g.similarity * 100) }}% alike</span>
              <span class="muted">{{ g.redundant_loc }} redundant lines · {{ scopeHint[g.scope] }}</span>
            </header>
            <ul class="members">
              <li v-for="m in g.members" :key="m.id">
                <NuxtLink :to="explore(m.id)" class="mono">{{ m.name }}</NuxtLink>
                <span class="mono muted">{{ m.file }}:{{ m.start_line }}-{{ m.end_line }}</span>
              </li>
            </ul>
          </article>
        </div>
        <div v-else class="empty"><strong>No duplicates</strong>for the current filter.</div>
      </section>
    </template>
    <p v-else class="muted">Loading…</p>
  </div>
</template>

<style scoped>
.group { border-top: 1px solid var(--border); padding-top: 12px; display: grid; gap: 8px; }
.members { margin: 0; padding-left: 18px; display: grid; gap: 4px; }
.members li { display: flex; gap: 12px; flex-wrap: wrap; align-items: baseline; }
</style>
