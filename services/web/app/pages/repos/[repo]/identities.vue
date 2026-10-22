<script setup lang="ts">
import type { CodeRecordPage, IdentityPage } from '~/utils/types'

const route = useRoute()
const repo = computed(() => route.params.repo as string)
const selected = computed(() => typeof route.query.id === 'string' ? route.query.id : '')
const reviewOnly = ref(false)
const offset = ref(0)
const limit = 20
watch([selected, reviewOnly], () => { offset.value = 0 })
const { data: page, pending, error } = useApiGet<IdentityPage>(() => repoPath(repo.value, `identities?${new URLSearchParams({ id: selected.value, review: reviewOnly.value ? '1' : '0', offset: String(offset.value), limit: String(limit) })}`))
const recordOffset = ref(0)
watch(selected, () => { recordOffset.value = 0 })
const { data: records, error: recordError, refresh } = useApiGet<CodeRecordPage>(() => repoPath(repo.value, `code-records?symbol=${encodeURIComponent(selected.value)}&offset=${recordOffset.value}&limit=20`))
const form = reactive({ method: 'annotation', origin_repo: '', origins: '', destinations: '', purpose: '', labels: '', reason: '', modifications: '', compatibility: '', license: '', attribution: '', validation: '' })
watch(selected, (id) => { form.destinations = id }, { immediate: true })
const saving = ref(false)
const saveError = ref('')
const saved = ref(false)
const list = (value: string) => value.split(',').map((s) => s.trim()).filter(Boolean)
async function save() {
  saving.value = true
  saveError.value = ''
  saved.value = false
  try {
    await apiPost(repoPath(repo.value, 'code-records'), { ...form, origin_repo: form.method === 'annotation' ? '' : form.origin_repo, origins: form.method === 'annotation' ? [] : list(form.origins), destinations: list(form.destinations), labels: list(form.labels) })
    saved.value = true
    await refresh()
  } catch (e) { saveError.value = apiErrorMessage(e) }
  finally { saving.value = false }
}
const base = computed(() => `/repos/${encodeURIComponent(repo.value)}`)
useHead({ title: () => `Identity and reuse · ${repo.value}` })
</script>

<template>
  <div class="stack">
    <PageHeader eyebrow="Code health" title="Identity and reuse" lede="Permanent symbol identities, revision evidence and attributed reuse records. Uncertain matches need review."
      :crumbs="[{ label: 'Repositories', to: '/' }, { label: repo, to: base }, { label: 'Identity and reuse' }]" />
    <CodeHealthTabs :repo="repo" />
    <div class="panel row">
      <label><input v-model="reviewOnly" type="checkbox"> Only matches needing review</label>
      <NuxtLink v-if="selected" :to="`${base}/identities`">All identities</NuxtLink>
    </div>
    <p v-if="pending" role="status">Loading identities…</p>
    <p v-else-if="error" class="panel error" role="alert">{{ apiErrorMessage(error) }}</p>
    <template v-else-if="page">
      <p v-if="!page.available" class="panel">Identity analysis is not available for this index. Sync the repository to initialise it.</p>
      <template v-else>
        <p class="small muted">Indexed {{ page.indexed_at || 'at an unknown time' }} · revision {{ page.revision || 'unavailable' }}. This describes the indexed source; sync after changes.</p>
        <p v-if="!page.symbols.length" class="panel">No matching symbol identities.</p>
        <section v-for="symbol in page.symbols" :key="symbol.id" class="panel stack">
          <div class="row">
            <h2 class="mono">{{ symbol.current.name }}</h2>
            <span class="tag">{{ symbol.active ? 'Current' : 'Historical · removed' }}</span>
            <NuxtLink :to="`${base}/identities?id=${symbol.id}`" class="mono small">{{ symbol.id }}</NuxtLink>
          </div>
          <p class="mono small">{{ symbol.current.file || 'Package' }}<template v-if="symbol.current.start_line">:{{ symbol.current.start_line }}–{{ symbol.current.end_line }}</template></p>
          <p class="small">{{ symbol.match }}</p>
          <NuxtLink v-if="symbol.active" :to="`${base}/explore?id=${symbol.id}`">Explore current code and callers</NuxtLink>
          <div v-if="symbol.candidates?.length || symbol.candidates_omitted">
            <strong>Review required</strong>
            <p>These are possible prior identities or copies. No identity was transferred and no lineage is verified.</p>
            <ul><li v-for="id in symbol.candidates" :key="id"><NuxtLink :to="`${base}/identities?id=${id}`" class="mono">{{ id }}</NuxtLink></li></ul>
            <p v-if="symbol.candidates_omitted">Up to {{ symbol.candidates_omitted }} additional candidates were not examined or displayed.</p>
          </div>
          <details v-if="symbol.history?.length">
            <summary>Earlier occurrences ({{ symbol.history.length }})</summary>
            <ul><li v-for="(old, i) in symbol.history" :key="i" class="small mono">{{ old.name }} · {{ old.file }} · {{ old.revision || 'revision unavailable' }}</li></ul>
            <p v-if="symbol.history_omitted">{{ symbol.history_omitted }} older occurrences omitted by the retention bound.</p>
          </details>
        </section>
        <nav class="row" aria-label="Identity pages">
          <button class="secondary" :disabled="offset === 0" @click="offset = Math.max(0, offset - limit)">Previous</button>
          <span>{{ page.total }} matching identities</span>
          <button class="secondary" :disabled="offset + limit >= page.total" @click="offset += limit">Next</button>
        </nav>
      </template>
    </template>

    <section class="panel stack">
      <h2>Annotations and recorded reuse</h2>
      <p class="muted small">Attributed evidence from people and agents. Licence, compatibility and validation text records what the author checked; it does not independently verify those claims. Changes are never propagated automatically.</p>
      <p v-if="recordError" class="error" role="alert">{{ apiErrorMessage(recordError) }}</p>
      <p v-else-if="!records">Loading records…</p>
      <p v-else-if="!records.records.length">No recorded annotations or reuse in this scope.</p>
      <p v-if="records?.storage_truncated">The storage query reached its limit; this list may be incomplete.</p>
      <p v-if="records?.sources_restricted" role="status">Some reuse records are unavailable because their sources are outside your current access.</p>
      <article v-for="record in records?.records" :key="record.item.id" class="stack">
        <h3>{{ record.item.code.method }} · {{ record.freshness }}</h3>
        <p class="small muted">{{ record.item.client || 'Unknown client' }} · {{ record.item.created_at }}</p>
        <p v-if="record.source_unavailable" role="status">Source analysis is unavailable. This record needs review.</p>
        <p>{{ record.item.code.purpose || record.item.code.reason }}</p>
        <p v-if="record.item.code.labels.length" class="small">Labels: {{ record.item.code.labels.join(', ') }}</p>
        <p v-for="change in record.changed" :key="change" class="small">{{ change }}</p>
        <details>
          <summary>Provenance and validation</summary>
          <p v-for="source in record.item.code.sources" :key="source.id" class="small mono">Origin: <NuxtLink :to="`/repos/${encodeURIComponent(source.repo || repo)}/identities?id=${source.id}`">{{ source.repo || repo }} · {{ source.id }}</NuxtLink> · {{ source.revision || 'revision unavailable' }} · {{ source.fingerprint || 'fingerprint unavailable' }}</p>
          <p v-for="target in record.item.code.targets" :key="target.id" class="small mono">Destination: {{ target.id }} · {{ target.revision || 'revision unavailable' }}</p>
          <dl><template v-for="field in ['modifications', 'compatibility', 'license', 'attribution', 'validation'] as const" :key="field"><dt>{{ field }}</dt><dd>{{ record.item.code[field] || 'Not recorded' }}</dd></template></dl>
        </details>
      </article>
      <nav v-if="records" class="row" aria-label="Record pages">
        <button class="secondary" :disabled="recordOffset === 0" @click="recordOffset = Math.max(0, recordOffset - 20)">Previous records</button>
        <span>{{ records.total }} records in the storage window</span>
        <button class="secondary" :disabled="recordOffset + 20 >= records.total" @click="recordOffset += 20">Next records</button>
      </nav>
    </section>

    <form v-if="page?.available" class="panel stack" @submit.prevent="save">
      <h2>Add an annotation or reuse record</h2>
      <p class="small muted">Needs write access to this repository and read access to the source in the same organisation. An origin may be historical; destinations must be current.</p>
      <label>Method <select v-model="form.method"><option value="annotation">Annotation</option><option value="shared-api">Shared API</option><option value="copy">Copy</option><option value="adapt">Adaptation</option><option value="split">Split</option><option value="merge">Merge</option></select></label>
      <label>Destination IDs, separated by commas <input v-model="form.destinations" required maxlength="800"></label>
      <template v-if="form.method === 'annotation'">
        <label>Purpose or annotation <textarea v-model="form.purpose" required maxlength="1000" /></label>
        <label>Labels, separated by commas <input v-model="form.labels" maxlength="1000"></label>
      </template>
      <template v-else>
        <label>Source repository (leave empty for this repository) <input v-model="form.origin_repo" maxlength="300" placeholder="owner/repository"></label>
        <label>Origin IDs, separated by commas <input v-model="form.origins" required maxlength="800"></label>
        <label v-for="field in ['reason', 'modifications', 'compatibility', 'license', 'attribution', 'validation'] as const" :key="field">{{ field }} <textarea v-model="form[field]" :required="field !== 'modifications'" maxlength="1000" /></label>
        <p class="small muted">If a check was unavailable or not run, say so explicitly.</p>
      </template>
      <p v-if="saveError" class="error" role="alert">{{ saveError }}</p>
      <p v-if="saved" role="status">Record saved.</p>
      <button :disabled="saving">{{ saving ? 'Saving…' : 'Save record' }}</button>
    </form>
  </div>
</template>
