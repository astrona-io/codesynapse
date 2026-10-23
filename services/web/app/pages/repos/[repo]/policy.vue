<script setup lang="ts">
import type { RepoPolicy, RepoPolicyPreview, RepositoryAnnotation, Role } from '~/utils/types'

const route = useRoute()
const repo = computed(() => route.params.repo as string)
const path = computed(() => repoPath(repo.value, 'policy'))
const session = useSession()
const { data, error, refresh } = useApiGet<RepoPolicy>(() => path.value)
const { data: annotations, refresh: refreshAnnotations } = useApiGet<{ annotations: RepositoryAnnotation[]; notice: string }>(() => repoPath(repo.value, 'annotations'))
const { data: access } = useApiGet<{ effective?: { role: Role } }>(() => `accounts/orgs/${session.current.value?.org?.slug ?? '-'}/repos/${encodeURIComponent(repo.value)}/access`)
const canManage = computed(() => access.value?.effective?.role === 'admin')
const config = ref('version: 1\n')
const preview = ref<RepoPolicyPreview | null>(null)
const exclusions = ref(false)
const suppressions = ref(false)
const annotation = ref(false)
const reason = ref('')
const expires = ref('')
const message = ref('')
const failure = ref('')
const busy = ref(false)
watch(config, () => { preview.value = null })
async function inspect() {
  busy.value = true
  failure.value = ''
  try { preview.value = await apiPost<RepoPolicyPreview>(`${path.value}/preview`, { config: config.value }) }
  catch (err) { failure.value = apiErrorMessage(err) }
  finally { busy.value = false }
}
async function approve() {
  if (!preview.value) return
  busy.value = true
  failure.value = ''
  try {
    await apiPut(path.value, { config: config.value, digest: preview.value.digest, allow_exclusions: exclusions.value, allow_suppressions: suppressions.value, allow_annotation: annotation.value, reason: reason.value, expires_at: expires.value ? new Date(expires.value).toISOString() : null })
    message.value = 'Approval saved for this organisation. Exclusion changes take effect after the next successful ingest.'
    await refresh()
  } catch (err) { failure.value = apiErrorMessage(err) }
  finally { busy.value = false }
}
async function revoke() {
  busy.value = true
  failure.value = ''
  try { await apiDelete(path.value); message.value = 'Approval revoked. Findings are active again and new annotation work is blocked.'; await refresh() }
  catch (err) { failure.value = apiErrorMessage(err) }
  finally { busy.value = false }
}
</script>

<template>
  <div class="stack">
    <PageHeader eyebrow="Repository settings" title="Indexing policy & annotations" lede="Review repository requests before they change scanner coverage or run a local annotation model." :crumbs="[{ label: 'Repositories', to: '/' }, { label: repo, to: `/repos/${encodeURIComponent(repo)}` }, { label: 'Indexing policy' }]" />
    <p v-if="error" class="notice notice--danger">{{ apiErrorMessage(error) }}</p>
    <p v-if="failure" class="notice notice--danger">{{ failure }}</p>
    <p v-if="message" class="notice">{{ message }}</p>
    <section v-if="data" class="panel stack">
      <h2>Last successful ingest</h2>
      <p v-if="!data.indexed.present" class="muted">No repository configuration was indexed. Default scanner settings apply; annotation is disabled.</p>
      <template v-else>
        <p class="small">Configuration fingerprint: <code>{{ data.indexed.digest }}</code></p>
        <p>{{ data.approval_active ? 'This organisation has an active approval for this configuration.' : 'No active approval matches the indexed configuration.' }}</p>
        <p class="muted">Excluded files: {{ data.indexed.excluded_files }}. Kubernetes-excluded files: {{ data.indexed.kubernetes_excluded_files }}. Kubernetes scanning: {{ data.indexed.kubernetes_disabled ? 'disabled' : 'enabled' }}.</p>
        <p v-if="data.indexed.inventory_incomplete" class="notice notice--warning">The coverage inventory reached its limit. These counts are incomplete.</p>
        <details><summary>Indexed configuration requests</summary><pre>{{ JSON.stringify(data.indexed.config, null, 2) }}</pre></details>
      </template>
      <p v-if="data.approval?.reason" class="small">Approval reason: {{ data.approval.reason }}<span v-if="data.approval.expires_at"> · Expires {{ data.approval.expires_at }}</span></p>
    </section>
    <section v-if="canManage" class="panel stack">
      <h2>Review a configuration</h2>
      <p class="muted">Paste the repository-root .codesynapse.yaml. Preview it, then choose which requests to approve. Shared graph exclusions require approval from every linked organisation. Findings remain stored even when their presentation is suppressed.</p>
      <form class="stack" @submit.prevent="inspect">
        <div class="field"><label for="policy-config">Repository configuration</label><textarea id="policy-config" v-model="config" rows="12" maxlength="65536" spellcheck="false" required /></div>
        <button type="submit" :disabled="busy">Preview configuration</button>
      </form>
      <form v-if="preview" class="stack" @submit.prevent="approve">
        <details open><summary>Validated requests</summary><pre>{{ JSON.stringify(preview.config, null, 2) }}</pre></details>
        <p v-for="diagnostic in preview.diagnostics" :key="diagnostic.field + diagnostic.code" class="small muted">{{ diagnostic.field }}: {{ diagnostic.code }}</p>
        <label><input v-model="exclusions" type="checkbox"> Approve requested indexing and Kubernetes exclusions</label>
        <label><input v-model="suppressions" type="checkbox"> Approve requested security finding suppressions</label>
        <label><input v-model="annotation" type="checkbox"> Allow requested local annotation jobs</label>
        <div class="field"><label for="policy-reason">Reason</label><input id="policy-reason" v-model="reason" maxlength="2048" required></div>
        <div class="field"><label for="policy-expiry">Expiry (optional)</label><input id="policy-expiry" v-model="expires" type="datetime-local"></div>
        <button type="submit" :disabled="busy || !reason.trim()">Save this approval</button>
      </form>
      <button v-if="data?.approval?.digest" class="btn--danger" :disabled="busy" @click="revoke">Revoke approval</button>
    </section>
    <section class="panel stack">
      <div class="row"><h2>Current annotations</h2><button class="btn--ghost" @click="refreshAnnotations()">Refresh</button></div>
      <p class="muted">Approved jobs run after ingestion using a local model. Start or cancel them through the API or an agent.</p>
      <p v-if="annotations?.notice" class="small muted">{{ annotations.notice }}</p>
      <p v-if="!annotations?.annotations.length" class="muted">No annotations match the current ingest revision.</p>
      <article v-for="item in annotations?.annotations" :key="item.symbol_id" class="stack">
        <code>{{ item.symbol_id }}</code><p>{{ item.summary }}</p><p class="small muted">{{ item.taxonomy.join(' · ') }}</p>
      </article>
    </section>
  </div>
</template>

<style scoped>
pre { overflow: auto; max-height: 28rem; white-space: pre-wrap; overflow-wrap: anywhere; }
code { overflow-wrap: anywhere; }
textarea { width: 100%; font-family: var(--font-mono); }
</style>
