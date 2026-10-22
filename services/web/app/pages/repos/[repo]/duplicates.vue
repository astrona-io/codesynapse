<script setup lang="ts">
import type { DuplicatesReport, RefactorPlan } from '~/utils/types'

const route = useRoute()
const repo = computed(() => route.params.repo as string)
const { data: rep, error } = useApiGet<DuplicatesReport>(() => repoPath(repo.value, 'duplicates'))
const scope = ref('all')
const kind = ref('all')
const groups = computed(() => (rep.value?.groups ?? []).filter((g) =>
  (scope.value === 'all' || g.scope === scope.value) && (kind.value === 'all' || g.kind === kind.value)))
// How to merge one group, fetched when asked for.
const plans = reactive<Record<string, RefactorPlan | string>>({})
const open = ref<string>('')
const key = (g: { hash: string; kind: string }) => `${g.hash}:${g.kind}`
async function showPlan(g: { hash: string; kind: string }) {
  const k = key(g)
  open.value = open.value === k ? '' : k
  if (open.value !== k || plans[k]) return
  try {
    plans[k] = await $fetch<RefactorPlan>(apiPath(repoPath(repo.value, 'duplicates/refactor')), { query: { hash: g.hash, kind: g.kind } })
  } catch (e) {
    plans[k] = apiErrorMessage(e)
  }
}
const asked = computed(() => ({ hash: String(route.query.group ?? ''), kind: String(route.query.kind ?? 'exact') }))
watch([rep, asked], () => {
  const g = rep.value?.groups.find((x) => x.hash === asked.value.hash && x.kind === asked.value.kind)
  if (g && open.value !== key(g)) { open.value = ''; showPlan(g) }
}, { immediate: true })
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
            <div>
              <button class="chip" :class="{ active: open === key(g) }" @click="showPlan(g)">{{ open === key(g) ? 'Hide' : 'How to merge' }}</button>
            </div>
            <div v-if="open === key(g)" class="plan">
              <p v-if="!plans[key(g)]" class="muted">Working it out…</p>
              <p v-else-if="typeof plans[key(g)] === 'string'" class="error">{{ plans[key(g)] }}</p>
              <template v-else>
                <template v-for="p in [plans[key(g)] as RefactorPlan]" :key="p.hash">
                  <p><strong>Suggested:</strong> a function called <span class="mono">{{ p.name }}</span> in {{ p.home }}.</p>
                  <p>{{ p.approach }}</p>
                  <ol class="steps"><li v-for="s in p.steps" :key="s">{{ s.replace(/^\d+\.\s*/, '') }}</li></ol>
                  <div v-if="p.risks.length"><strong>Watch for</strong><ul><li v-for="r in p.risks" :key="r">{{ r }}</li></ul></div>
                  <div class="table-wrap"><table>
                    <thead><tr><th>Copy</th><th class="num">Callers</th><th class="num">Tests</th></tr></thead>
                    <tbody><tr v-for="c in p.copies" :key="c.id">
                      <td><NuxtLink :to="explore(c.id)" class="mono">{{ c.name }}</NuxtLink><div class="mono muted">{{ c.location }}</div></td>
                      <td class="num">{{ c.callers.length }}</td>
                      <td class="num" :class="{ error: !c.tests.length }">{{ c.tests.length }}</td>
                    </tr></tbody>
                  </table></div>
                  <p class="muted small">A suggestion from the graph, not a change: nothing is edited. Read the code before you move it.</p>
                </template>
              </template>
            </div>
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
.plan { background: var(--surface-2, transparent); border-left: 3px solid var(--accent); padding: 8px 14px; display: grid; gap: 8px; }
.plan p, .plan ul, .plan ol { margin: 0; }
.steps { padding-left: 20px; display: grid; gap: 2px; }
.small { font-size: 12.5px; }
</style>
