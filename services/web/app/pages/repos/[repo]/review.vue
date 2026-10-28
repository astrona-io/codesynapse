<script setup lang="ts">
import type { Report } from '~/utils/types'

const route = useRoute()
const repo = computed(() => route.params.repo as string)
useHead({ title: () => `Impact analysis · ${repo.value}` })
const mode = ref<'refs' | 'diff'>('refs')
const base = ref('origin/main')
const head = ref('HEAD')
const diff = ref('')
const report = ref<Report | null>(null)
const error = ref('')
const busy = ref(false)
// A report describes one repository; drop it when another one is opened.
watch(repo, () => { report.value = null; error.value = '' })

async function run() {
  error.value = ''
  busy.value = true
  try {
    const body = mode.value === 'diff' ? { diff: diff.value } : { base: base.value, head: head.value }
    const name = repo.value
    const res = await apiPost<Report>(repoPath(name, 'impact'), body)
    if (name === repo.value) report.value = res
  } catch (e) {
    error.value = apiErrorMessage(e)
    report.value = null
  } finally {
    busy.value = false
  }
}
const explore = (id: string) => ({ path: `/repos/${encodeURIComponent(repo.value)}/explore`, query: { id } })
</script>

<template>
  <div class="stack">
    <PageHeader eyebrow="Change review" title="Impact analysis" lede="Map a diff onto the graph: changed symbols with risk, blast radius, tests, docs and security findings."
      :crumbs="[{ label: 'Repositories', to: '/' }, { label: repo, to: `/repos/${encodeURIComponent(repo)}` }, { label: 'Review' }]" />
    <div class="panel">
      <div class="segmented mode" role="group" aria-label="What to analyse">
        <button type="button" :aria-pressed="mode === 'refs'" @click="mode = 'refs'">Git refs</button>
        <button type="button" :aria-pressed="mode === 'diff'" @click="mode = 'diff'">Paste diff</button>
      </div>
      <form @submit.prevent="run">
        <div v-if="mode === 'refs'" class="row">
          <label class="ref">Base <input v-model="base" required></label>
          <label class="ref">Head <input v-model="head" required></label>
        </div>
        <textarea v-else v-model="diff" placeholder="Paste `git diff` output" aria-label="Diff to analyse" required />
        <div class="row actions">
          <button type="submit" :disabled="busy">{{ busy ? 'Analysing…' : 'Analyse' }}</button>
          <span class="muted">The graph reflects the last ingest; re-ingest after large changes.</span>
        </div>
      </form>
      <p v-if="error" class="error" role="alert">{{ error }}</p>
    </div>

    <template v-if="report">
      <div class="grid cols-4">
        <StatTile label="Max risk" :value="report.summary.max_level" />
        <StatTile label="Changed symbols" :value="fmtInt(report.summary.changed_symbols)" :hint="plural(report.summary.changed_files, 'file')" />
        <StatTile label="Impacted" :value="fmtInt(report.summary.impacted_symbols)" />
        <StatTile label="Untested changes" :value="fmtInt(report.summary.untested_changes)" />
        <StatTile label="Security-sensitive" :value="fmtInt(report.summary.security_sensitive)" />
        <StatTile label="With copies" :value="fmtInt(report.summary.duplicated_changes ?? 0)" hint="changed code that exists elsewhere" />
      </div>

      <div class="panel">
        <h2>Changed symbols</h2>
        <div class="table-wrap">
          <table>
            <thead><tr><th>Risk</th><th>Symbol</th><th class="num">Callers</th><th class="num">Tests</th><th>Why</th></tr></thead>
            <tbody>
              <tr v-for="c in report.changes" :key="c.id">
                <td :class="`lvl-${c.level}`">{{ c.level }} {{ c.risk }}</td>
                <td><NuxtLink :to="explore(c.id)" class="mono">{{ c.name }}</NuxtLink><div class="mono muted path">{{ c.location }}</div></td>
                <td class="num">{{ fmtInt(c.fan_in) }}</td>
                <td class="num" :class="{ error: !c.tests.length }">{{ c.tests.length }}</td>
                <td class="muted">
                  {{ c.reasons.join('; ') }}
                  <div v-if="c.duplicate" class="dup">
                    <span class="badge badge--violet">{{ c.duplicate.kind === 'near' ? `edited copies · ~${Math.round(c.duplicate.similarity * 100)}% alike` : 'exact copies' }}</span>
                    {{ fmtInt(c.duplicate.copies) }} in total: change them too, or merge them.
                    <span v-for="o in c.duplicate.others" :key="o.id" class="mono small"><br>{{ o.name }} {{ o.file }}:{{ o.start_line }}-{{ o.end_line }}</span>
                  </div>
                </td>
              </tr>
            </tbody>
          </table>
        </div>
      </div>

      <div class="grid cols-2">
        <div class="panel">
          <h2>Impacted code</h2>
          <div class="table-wrap">
            <table>
              <tbody>
                <tr v-for="i in report.impacted" :key="i.id">
                  <td class="num muted">d{{ i.depth }}</td>
                  <td><NuxtLink :to="explore(i.id)" class="mono">{{ i.name }}</NuxtLink></td>
                  <td class="mono muted">{{ i.location }}</td>
                </tr>
              </tbody>
            </table>
          </div>
        </div>
        <div class="panel">
          <h2>Review checklist</h2>
          <ul><li v-for="c in report.checklist" :key="c">{{ c }}</li></ul>
          <h2 v-if="report.docs?.length" class="next">Docs to re-check</h2>
          <ul><li v-for="d in report.docs" :key="d.id">{{ d.name }} <span class="mono muted">{{ d.location }}</span></li></ul>
          <h2 class="next">Files</h2>
          <ul>
            <li v-for="f in report.files" :key="f.path">
              <span class="tag">{{ f.status }}</span> <span class="mono path">{{ f.path }}</span>
              <span class="muted"> · {{ plural(f.symbols, 'symbol') }}{{ !f.indexed && f.status !== 'deleted' ? ' (not in graph)' : '' }}</span>
            </li>
          </ul>
        </div>
      </div>
    </template>
  </div>
</template>

<style scoped>
.dup { margin-top: 6px; font-size: 12.5px; }
.mode { margin-bottom: 10px; }
.ref { flex: 1 1 160px; min-width: 0; }
.actions { margin-top: 10px; }
.next { margin-top: 16px; }
.path { overflow-wrap: anywhere; }
</style>
