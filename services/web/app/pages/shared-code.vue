<script setup lang="ts">
import type { SharedCodeReport } from '~/utils/types'

useHead({ title: 'Shared code · CodeSynapse' })
const session = useSession()
const { data, error } = useApiGet<SharedCodeReport>('shared-code')
const where = computed(() => session.current.value?.org.name ?? 'your repositories')
const explore = (repo: string, id: string) => ({ path: `/repos/${encodeURIComponent(repo)}/explore`, query: { id } })
</script>

<template>
  <div class="stack">
    <PageHeader eyebrow="Overview" title="Shared code" :lede="`Code that more than one repository in ${where} contains. Where it repeats enough, we recommend moving it into a library or framework they all use.`">
      <NuxtLink to="/overview" class="btn btn--ghost">What needs attention</NuxtLink>
    </PageHeader>

    <p v-if="error" class="notice notice--danger">{{ apiErrorMessage(error) }}</p>
    <template v-else-if="data">
      <div v-if="data.repos < 2" class="empty">
        <strong>Add a second repository</strong>
        Shared code is found by comparing repositories. Index at least two, then press Sync.
      </div>
      <template v-else>
        <div class="grid grid--stats">
          <StatTile label="Repositories compared" :value="data.repos" />
          <StatTile label="Shared groups" :value="data.groups.length" :hint="`${data.suggestions.length} worth extracting`" />
          <StatTile label="Redundant lines" :value="fmtInt(data.redundant_loc)" hint="what is left when one copy is kept" />
        </div>
        <p v-if="data.partial?.length" class="notice">Smaller functions of {{ data.partial.join(', ') }} were not compared (report limit).</p>

        <section v-if="data.suggestions.length" class="panel edge--green">
          <p class="eyebrow eyebrow--green">Recommended</p><h2>Share this work</h2>
          <article v-for="s in data.suggestions" :key="s.repos.join()" class="suggestion">
            <header class="row">
              <span class="badge" :class="s.kind === 'framework' ? 'badge--violet' : 'badge--green'">{{ s.kind }}</span>
              <strong class="mono">{{ s.name }}</strong>
              <span class="muted">{{ s.groups }} functions · {{ s.redundant_loc }} redundant lines</span>
            </header>
            <p>{{ s.rationale }}</p>
            <p class="muted small">Repositories: <span v-for="r in s.repos" :key="r" class="tag">{{ r }}</span></p>
            <ul class="members">
              <li v-for="c in s.candidates" :key="c" class="mono">{{ c }}</li>
            </ul>
          </article>
        </section>

        <section class="panel edge--orange">
          <p class="eyebrow">Code</p><h2>Shared groups <span class="muted">· {{ data.groups.length }}</span></h2>
          <div v-if="data.groups.length" class="stack">
            <article v-for="g in data.groups" :key="g.hash" class="group">
              <header class="row">
                <strong>{{ g.copies }} copies of {{ g.loc }} lines</strong>
                <span v-if="g.kind === 'near'" class="badge badge--violet" title="Copies that were edited after copying; the variants differ a little">edited · ~{{ Math.round(g.similarity * 100) }}% alike</span>
                <span class="muted">in {{ g.repos.join(', ') }} · {{ g.redundant_loc }} redundant lines</span>
              </header>
              <ul class="members">
                <li v-for="m in g.members" :key="m.repo + m.id">
                  <span class="tag">{{ m.repo }}</span>
                  <NuxtLink :to="explore(m.repo, m.id)" class="mono">{{ m.name }}</NuxtLink>
                  <span class="mono muted">{{ m.file }}:{{ m.start_line }}-{{ m.end_line }}</span>
                </li>
              </ul>
            </article>
          </div>
          <div v-else class="empty"><strong>Nothing shared</strong>No function appears in two repositories.</div>
        </section>
      </template>
    </template>
    <p v-else class="muted">Loading…</p>
  </div>
</template>

<style scoped>
.group, .suggestion { border-top: 1px solid var(--border); padding-top: 12px; display: grid; gap: 8px; }
.members { margin: 0; padding-left: 18px; display: grid; gap: 4px; }
.members li { display: flex; gap: 12px; flex-wrap: wrap; align-items: baseline; }
.small { font-size: 12.5px; }
</style>
