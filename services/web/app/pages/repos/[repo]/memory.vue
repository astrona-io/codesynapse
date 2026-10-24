<script setup lang="ts">
import type { MemoryFact, MemoryGraph } from '~/utils/types'

const route = useRoute()
const repo = computed(() => route.params.repo as string)
const query = ref('')
const applied = ref('')
const { data: g, error, refresh } = useApiGet<MemoryGraph>(() => repoPath(repo.value, `memory?limit=100${applied.value ? `&q=${encodeURIComponent(applied.value)}` : ''}`))

const names = computed(() => new Map((g.value?.entities ?? []).map((e) => [e.id, e])))
const label = (id: string) => names.value.get(id)?.name ?? id
const type = (id: string) => names.value.get(id)?.type ?? ''

const busy = ref('')
const failed = ref('')
async function remove(kind: 'fact' | 'entity', id: string, what: string) {
  if (!confirm(kind === 'entity' ? `Delete “${what}” and every fact about it?` : `Delete the fact “${what}”?`)) return
  busy.value = id
  failed.value = ''
  try {
    await $fetch(apiPath(repoPath(repo.value, `memory?${kind}=${encodeURIComponent(id)}`)), { method: 'DELETE' })
    await refresh()
  } catch (e) {
    failed.value = apiErrorMessage(e)
  } finally {
    busy.value = ''
  }
}
const factText = (f: MemoryFact) => `${label(f.subject)} ${f.predicate} ${label(f.object)}`
</script>

<template>
  <div class="stack">
    <PageHeader
      eyebrow="Agents" title="Agent memory"
      lede="Facts that coding agents saved about this repository so later sessions can recall them instead of re-reading the conversation: decisions, preferences, ownership, open issues. Agents may get things wrong, so review what is here and delete what is stale."
      :crumbs="[{ label: 'Repositories', to: '/' }, { label: repo, to: `/repos/${encodeURIComponent(repo)}` }, { label: 'Agent memory' }]"
    />

    <p v-if="error" class="panel error">{{ apiErrorMessage(error) }}</p>
    <template v-else-if="g">
      <form class="row" @submit.prevent="applied = query.trim()">
        <input v-model="query" class="grow" placeholder="Search entities, e.g. payments team" aria-label="Search memory">
        <button type="submit">Search</button>
        <button v-if="applied" type="button" class="secondary" @click="query = ''; applied = ''">Clear</button>
      </form>
      <p v-if="failed" class="error">{{ failed }}</p>

      <section v-if="g.facts.length" class="panel">
        <p class="eyebrow">Facts <span class="muted">· {{ g.facts.length }}</span></p>
        <div class="table-wrap">
          <table>
            <thead><tr><th>Subject</th><th>Relation</th><th>Object</th><th>Saved by</th><th class="num">Seen</th><th>Updated</th><th /></tr></thead>
            <tbody>
              <tr v-for="f in g.facts" :key="f.id">
                <td>{{ label(f.subject) }} <div class="mono muted small">{{ type(f.subject) }}</div></td>
                <td class="mono pred">{{ f.predicate }}</td>
                <td>{{ label(f.object) }} <div class="mono muted small">{{ type(f.object) }}</div></td>
                <td class="mono small">{{ f.source }}</td>
                <td class="num" :title="`saved ${f.confirmations} time(s)`">{{ f.confirmations }}</td>
                <td class="muted small" :title="f.updated_at">{{ relTime(f.updated_at) }}</td>
                <td><button class="secondary tiny danger" :disabled="busy === f.id" @click="remove('fact', f.id, factText(f))">Delete</button></td>
              </tr>
            </tbody>
          </table>
        </div>
      </section>

      <section v-if="g.entities.length" class="panel">
        <p class="eyebrow">Entities <span class="muted">· {{ g.entities.length }}</span></p>
        <ul class="entities">
          <li v-for="e in g.entities" :key="e.id">
            <span>{{ e.name }}</span> <span class="badge badge--violet">{{ e.type }}</span>
            <span class="muted small">{{ relTime(e.updated_at) }}</span>
            <button class="secondary tiny danger" :disabled="busy === e.id" @click="remove('entity', e.id, e.name)">Delete</button>
          </li>
        </ul>
      </section>

      <div v-if="!g.entities.length" class="empty">
        <template v-if="applied"><strong>No match</strong>Nothing saved matches “{{ applied }}”.</template>
        <template v-else>
          <strong>Nothing saved yet</strong>
          Agents save facts with the <code>upsert_graph_triples</code> MCP tool. It is on by default; ask your agent to use the <code>context-offload</code> prompt. To turn it off, set <code>CODESYNAPSE_ALLOW_MEMORY=false</code> for the MCP service (or run it with <code>--allow-memory=false</code>).
        </template>
      </div>
      <p class="muted small">{{ g.notice }}</p>
    </template>
    <p v-else class="muted">Loading…</p>
  </div>
</template>

<style scoped>
.small { font-size: 12.5px; }
.pred { color: var(--info); white-space: nowrap; }
.entities { list-style: none; margin: 0; padding: 0; display: grid; }
.entities li { display: flex; flex-wrap: wrap; align-items: center; gap: 10px; padding: 8px 0; border-bottom: 1px dashed var(--line); }
.entities li button { margin-left: auto; }
.danger { color: var(--danger); }
</style>
