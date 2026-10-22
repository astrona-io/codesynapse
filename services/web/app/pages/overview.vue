<script setup lang="ts">
import type { Attention, AttentionItem } from '~/utils/types'

useHead({ title: 'Overview · CodeSynapse' })
const session = useSession()
const { data, error, pending } = useApiGet<Attention>('attention')
type Filter = 'all' | 'security' | 'dependencies' | 'licences' | 'operations'
const filter = ref<Filter>('all')
const groups: Record<Exclude<Filter, 'all'>, AttentionItem['kind'][]> = {
  security: ['security'],
  dependencies: ['vulnerability', 'automation'],
  licences: ['licence'],
  operations: ['sync', 'stale', 'access'],
}
const filters: { id: Filter; label: string }[] = [
  { id: 'all', label: 'Everything' }, { id: 'security', label: 'Security' }, { id: 'dependencies', label: 'Dependencies' },
  { id: 'licences', label: 'Licences' }, { id: 'operations', label: 'Upkeep' },
]
const items = computed(() => (data.value?.items ?? []).filter((it) => filter.value === 'all' || groups[filter.value].includes(it.kind)))
const urgent = computed(() => (data.value?.levels.critical ?? 0) + (data.value?.levels.high ?? 0))
const affected = computed(() => (data.value ? data.value.repos - data.value.clean.length : 0))
const where = computed(() => session.allOrgs.value ? 'all organisations' : session.current.value?.org.name ?? 'your repositories')
</script>

<template>
  <div class="stack">
    <PageHeader eyebrow="Overview" title="What needs attention" :lede="`Across ${where}, the most urgent first. Each item says why it matters and where to fix it.`">
      <NuxtLink to="/" class="btn btn--ghost">All repositories</NuxtLink>
    </PageHeader>

    <p v-if="error" class="notice notice--danger">{{ apiErrorMessage(error) }}</p>
    <template v-else-if="data">
      <div v-if="!data.repos" class="empty">
        <strong>No repositories yet</strong>
        Add one on <NuxtLink to="/">Repositories</NuxtLink>; this page then shows what to fix first.
      </div>
      <template v-else>
        <div class="grid cols-4">
          <StatTile label="Urgent" :value="String(urgent)" :hint="urgent ? 'critical and high items below' : 'nothing urgent'" />
          <StatTile label="Repositories affected" :value="`${affected} of ${data.repos}`" :hint="`${data.clean.length} with nothing to report`" />
          <StatTile label="Security findings" :value="fmtCompact(data.findings)" :hint="`${fmtCompact(data.vulns)} known vulnerabilities in dependencies`" />
          <StatTile label="Tokens saved by agents" :value="fmtCompact(data.tokens_saved)" hint="vs reading the files instead (estimate)" />
        </div>

        <section class="panel">
          <div class="panel__header">
            <h2>Needs attention <span class="muted">{{ items.length }}</span></h2>
            <div class="row" role="tablist" aria-label="Filter">
              <button v-for="f in filters" :key="f.id" class="chip" :class="{ active: filter === f.id }" role="tab" :aria-selected="filter === f.id" @click="filter = f.id">{{ f.label }}</button>
            </div>
          </div>
          <AttentionList v-if="items.length" :items="items" :limit="12" />
          <div v-else class="empty"><strong>Nothing here</strong>{{ filter === 'all' ? 'Every repository is in good shape. New findings appear here after each sync.' : 'Nothing to fix in this area.' }}</div>
        </section>

        <section v-if="data.clean.length" class="panel">
          <h2>All clear <span class="muted">{{ data.clean.length }}</span></h2>
          <p class="muted small">Nothing flagged: no critical or high findings, no vulnerable or conflicting dependencies, update automation in place, and indexed recently.</p>
          <div class="clean">
            <NuxtLink v-for="r in data.clean" :key="r" :to="`/repos/${encodeURIComponent(r)}`" class="badge badge--green mono">{{ r }}</NuxtLink>
          </div>
        </section>
        <p class="muted small">Updated {{ relTime(data.generated_at) }}<span v-if="pending"> · refreshing…</span>. <NuxtLink to="/help#terms">What do these terms mean?</NuxtLink></p>
      </template>
    </template>
  </div>
</template>

<style scoped>
.clean { display: flex; flex-wrap: wrap; gap: 6px; }
.clean .badge { text-transform: none; }
</style>
