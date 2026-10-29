<script setup lang="ts">
import type { CodeRecordPage, IdentityPage, IdentityReviewPage } from '~/utils/types'

const route = useRoute()
const repo = computed(() => route.params.repo as string)
const selected = computed(() => typeof route.query.id === 'string' ? route.query.id : '')
const identityReview = reactive({ from: '', to: '', relation: 'same-symbol', reason: '' })
const reviewOffset = ref(0)
const reviewError = ref('')
const reviewing = ref(false)
const reviewSaved = ref(false)
const { data: reviews, error: reviewsError, refresh: refreshReviews } = useApiGet<IdentityReviewPage>(() => repoPath(repo.value, `identities/reviews?symbol=${encodeURIComponent(selected.value)}&offset=${reviewOffset.value}&limit=20`))
watch(selected, () => { reviewOffset.value = 0; reviewSaved.value = false })
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
async function submitIdentityReview() {
  if (!page.value?.available) return
  reviewing.value = true
  reviewError.value = ''
  reviewSaved.value = false
  try {
    await apiPost(repoPath(repo.value, 'identities/reviews'), { ...identityReview, revision: page.value.revision || '', indexed_at: page.value.indexed_at || '' })
    await refreshReviews()
    reviewSaved.value = true
  } catch (e) { reviewError.value = apiErrorMessage(e) }
  finally { reviewing.value = false }
}
useHead({ title: () => `Identity and reuse · ${repo.value}` })
// Paging and messages belong to one repository.
watch(repo, () => {
  offset.value = 0; reviewOffset.value = 0; recordOffset.value = 0
  saved.value = false; saveError.value = ''; reviewSaved.value = false; reviewError.value = ''
})
const idLink = (id: string, r = repo.value) => `/repos/${encodeURIComponent(r)}/identities?${new URLSearchParams({ id })}`
function when(iso?: string): string {
  if (!iso) return 'at an unknown time'
  const d = new Date(iso)
  return Number.isNaN(d.getTime()) ? iso : d.toLocaleString([], { year: 'numeric', month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit', timeZoneName: 'short' })
}
const FIELD_LABEL: Record<string, string> = { reason: 'Reason', modifications: 'Modifications', compatibility: 'Compatibility', license: 'Licence', attribution: 'Attribution', validation: 'Validation' }
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
    <DataState :error="error" :pending="pending" :empty="!page" loading="Loading identities…">
      <template v-if="page">
        <p v-if="!page.available" class="panel">Identity analysis is not available for this index. Sync the repository to initialise it.</p>
        <template v-else>
          <p class="small muted">Indexed {{ when(page.indexed_at) }} · revision {{ page.revision || 'unavailable' }}. This describes the indexed source; sync after changes.</p>
          <p v-if="!page.symbols.length" class="panel">No matching symbol identities.</p>
          <section v-for="symbol in page.symbols" :key="symbol.id" class="panel stack">
            <div class="row">
              <h2 class="mono wrap">{{ symbol.current.name }}</h2>
              <span class="tag">{{ symbol.active ? 'Current' : 'Historical · removed' }}</span>
              <NuxtLink :to="idLink(symbol.id)" class="mono small wrap">{{ symbol.id }}</NuxtLink>
            </div>
            <p class="mono small wrap">{{ symbol.current.file || 'Package' }}<template v-if="symbol.current.start_line">:{{ symbol.current.start_line }}–{{ symbol.current.end_line }}</template></p>
            <p class="small">{{ symbol.match }}</p>
            <p v-if="symbol.current.neighbour_count" class="small muted">{{ plural(symbol.current.neighbour_count ?? 0, 'call neighbour') }}<template v-if="symbol.current.neighbours_omitted"> · incomplete evidence</template></p>
            <NuxtLink v-if="symbol.active" :to="{ path: `${base}/explore`, query: { id: symbol.id } }">Explore current code and callers</NuxtLink>
            <div v-if="symbol.candidates?.length || symbol.candidates_omitted">
              <strong>Review required</strong>
              <p>These are possible prior identities or copies. No identity was transferred and no lineage is verified.</p>
              <ul><li v-for="id in symbol.candidates" :key="id"><NuxtLink :to="idLink(id)" class="mono wrap">{{ id }}</NuxtLink> <button v-if="symbol.active" type="button" class="secondary" @click="identityReview.from = id; identityReview.to = symbol.id">Review this match</button></li></ul>
              <p v-if="symbol.candidates_omitted">Up to {{ fmtInt(symbol.candidates_omitted) }} additional candidates were not examined or displayed.</p>
            </div>
            <details v-if="symbol.history?.length">
              <summary>Earlier occurrences ({{ fmtInt(symbol.history.length) }})</summary>
              <ul><li v-for="(old, i) in symbol.history" :key="i" class="small mono wrap">{{ old.name }} · {{ old.file }} · {{ old.revision || 'revision unavailable' }}</li></ul>
              <p v-if="symbol.history_omitted">{{ fmtInt(symbol.history_omitted) }} older occurrences omitted by the retention bound.</p>
            </details>
          </section>
          <nav class="row" aria-label="Identity pages">
            <button type="button" class="secondary" :disabled="offset === 0" @click="offset = Math.max(0, offset - limit)">Previous</button>
            <span>{{ plural(page.total, 'matching identity', 'matching identities') }}</span>
            <button type="button" class="secondary" :disabled="offset + limit >= page.total" @click="offset += limit">Next</button>
          </nav>
        </template>
      </template>
    </DataState>

    <section class="panel stack">
      <h2>Human identity reviews</h2>
      <p class="small muted">Reviews belong to the selected organisation. A same-symbol review lets the prior permanent ID resolve to current code here. Copy and distinct reviews keep the identities separate. Earlier reviews remain in history. A copy classification needs a separate reuse record for licence and validation details.</p>
      <DataState :error="reviewsError" :pending="!reviews" :empty="!reviews?.reviews.length" loading="Loading reviews…" text="No identity reviews in this scope." compact />
      <article v-for="review in reviews?.reviews" :key="review.item.id">
        <h3>{{ review.item.identity_review.relation }} · {{ review.state }}</h3>
        <p class="small mono wrap">{{ review.item.identity_review.from }} → {{ review.item.identity_review.to }}</p>
        <p>{{ review.item.identity_review.reason }}</p>
        <p class="small muted">{{ when(review.item.created_at) }} · reviewed revision {{ review.item.identity_review.revision || 'unavailable' }}</p>
      </article>
      <nav v-if="reviews" class="row" aria-label="Identity review pages">
        <button type="button" class="secondary" :disabled="reviewOffset === 0" @click="reviewOffset = Math.max(0, reviewOffset - 20)">Previous reviews</button>
        <span>{{ plural(reviews.total, 'review') }}</span>
        <button type="button" class="secondary" :disabled="reviewOffset + 20 >= reviews.total" @click="reviewOffset += 20">Next reviews</button>
      </nav>
    </section>
    <form v-if="page?.available" class="panel stack" @submit.prevent="submitIdentityReview">
      <h2>Review an identity match</h2>
      <p class="small muted">Requires repository administration and, in accounts mode, a signed-in session. Same-symbol links require a retired prior identity and a current target in this repository. Saving replaces earlier effective reviews for either endpoint and preserves their history.</p>
      <label>Prior permanent ID <input v-model="identityReview.from" required maxlength="36"></label>
      <label>Current permanent ID <input v-model="identityReview.to" required maxlength="36"></label>
      <label>Conclusion <select v-model="identityReview.relation"><option value="same-symbol">Same symbol</option><option value="copy">Copied implementation</option><option value="distinct">Different symbols</option></select></label>
      <label>Evidence and reason <textarea v-model="identityReview.reason" required maxlength="1000" /></label>
      <p v-if="reviewError" class="error" role="alert">{{ reviewError }}</p>
      <p v-if="reviewSaved" role="status">Identity review saved.</p>
      <button :disabled="reviewing">{{ reviewing ? 'Saving…' : 'Save human review' }}</button>
    </form>

    <section class="panel stack">
      <h2>Annotations and recorded reuse</h2>
      <p class="muted small">Attributed evidence from people and agents. Licence, compatibility and validation text records what the author checked; it does not independently verify those claims. Changes are never propagated automatically.</p>
      <DataState :error="recordError" :pending="!records" :empty="!records?.records.length" loading="Loading records…" text="No recorded annotations or reuse in this scope." compact />
      <p v-if="records?.storage_truncated">The storage query reached its limit; this list may be incomplete.</p>
      <p v-if="records?.sources_restricted" role="status">Some reuse records are unavailable because their sources are outside your current access.</p>
      <article v-for="record in records?.records" :key="record.item.id" class="stack">
        <h3>{{ record.item.code.method }} · {{ record.freshness }}</h3>
        <p class="small muted">{{ record.item.client || 'Unknown client' }} · {{ when(record.item.created_at) }}</p>
        <p v-if="record.source_unavailable" role="status">Source analysis is unavailable. This record needs review.</p>
        <p>{{ record.item.code.purpose || record.item.code.reason }}</p>
        <p v-if="record.item.code.labels.length" class="small">Labels: {{ record.item.code.labels.join(', ') }}</p>
        <p v-for="change in record.changed" :key="change" class="small">{{ change }}</p>
        <details>
          <summary>Provenance and validation</summary>
          <p v-for="source in record.item.code.sources" :key="source.id" class="small mono wrap">Origin: <NuxtLink :to="idLink(source.id, source.repo || repo)">{{ source.repo || repo }} · {{ source.id }}</NuxtLink> · {{ source.revision || 'revision unavailable' }} · {{ source.fingerprint || 'fingerprint unavailable' }}</p>
          <p v-for="target in record.item.code.targets" :key="target.id" class="small mono wrap">Destination: {{ target.id }} · {{ target.revision || 'revision unavailable' }}</p>
          <dl><template v-for="field in ['modifications', 'compatibility', 'license', 'attribution', 'validation'] as const" :key="field"><dt>{{ FIELD_LABEL[field] }}</dt><dd>{{ record.item.code[field] || 'Not recorded' }}</dd></template></dl>
        </details>
      </article>
      <nav v-if="records" class="row" aria-label="Record pages">
        <button type="button" class="secondary" :disabled="recordOffset === 0" @click="recordOffset = Math.max(0, recordOffset - 20)">Previous records</button>
        <span>{{ plural(records.total, 'record') }} in the storage window</span>
        <button type="button" class="secondary" :disabled="recordOffset + 20 >= records.total" @click="recordOffset += 20">Next records</button>
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
        <label v-for="field in ['reason', 'modifications', 'compatibility', 'license', 'attribution', 'validation'] as const" :key="field">{{ FIELD_LABEL[field] }} <textarea v-model="form[field]" :required="field !== 'modifications'" maxlength="1000" /></label>
        <p class="small muted">If a check was unavailable or not run, say so explicitly.</p>
      </template>
      <p v-if="saveError" class="error" role="alert">{{ saveError }}</p>
      <p v-if="saved" role="status">Record saved.</p>
      <button :disabled="saving">{{ saving ? 'Saving…' : 'Save record' }}</button>
    </form>
  </div>
</template>

<style scoped>
.wrap { overflow-wrap: anywhere; min-width: 0; }
</style>
