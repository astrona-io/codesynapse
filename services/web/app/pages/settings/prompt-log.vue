<script setup lang="ts">
import type { Membership, PromptLogEntry, PromptLogSetting } from '~/utils/types'

// The organisation's gateway prompt log: off unless an owner or admin turns
// it on. Owners and admins only (the API checks the role and refuses API
// keys); every view and export is written to the audit log.
const session = useSession()
const org = computed(() => session.current.value?.org)
const slug = computed(() => org.value?.slug ?? '-')
const base = computed(() => `accounts/orgs/${slug.value}/gateway/prompt-log`)

const { data: setting, refresh: refreshSetting } = useApiGet<PromptLogSetting>(() => `${base.value}/settings`)
const days = ref(30)
watch(setting, (s) => { if (s) days.value = s.retention_days }, { immediate: true })
const saved = ref(false)
const saveError = ref('')
async function save(change: { enabled?: boolean; retention_days?: number }) {
  saved.value = false
  saveError.value = ''
  try {
    await apiPut(`${base.value}/settings`, change)
    saved.value = true
  } catch (err) {
    saveError.value = plainError(err)
  }
  await refreshSetting()
}

const { data: members } = useApiGet<{ members: Membership[] }>(() => `accounts/orgs/${slug.value}/members`)
const names = computed(() => new Map((members.value?.members ?? []).map(m => [m.user.id, m.user.name || m.user.email])))
const who = (id?: string) => (id ? names.value.get(id) ?? id : '')

const user = ref('')
const chat = ref('')
const model = ref('')
const routeFilter = ref('')
const shortened = ref('')
const fallthrough = ref('')
const period = ref(7)
const FALLTHROUGH = ['not_eligible', 'bad_request', 'system_not_text', 'unsupported_block', 'unsupported_tool', 'too_large', 'has_thinking', 'has_image', 'excluded', 'no_local_model', 'local_first_off', 'queue_timeout', 'answer_timeout', 'cancelled', 'model_error', 'bad_status', 'empty_answer', 'truncated', 'bad_answer', 'count_tokens']
const since = computed(() => new Date(Date.now() - period.value * 864e5).toISOString().replace(/\.\d+Z$/, 'Z'))
const filters = computed(() => {
  const p = new URLSearchParams({ since: since.value })
  for (const [k, v] of [['user', user], ['session', chat], ['model', model], ['route', routeFilter], ['shortened', shortened], ['fallthrough', fallthrough]] as const) {
    if (v.value.trim()) p.set(k, v.value.trim())
  }
  return p.toString()
})
const enabled = computed(() => !!setting.value)
const { data, error, pending } = useApiGet<{ entries: PromptLogEntry[]; next_before_id: number }>(
  () => `${base.value}?limit=100&${filters.value}`, { immediate: session.can('admin') })
const rows = computed(() => data.value?.entries ?? [])
const exportUrl = (format: string) => apiPath(`${base.value}/export?format=${format}&${filters.value}`)

const open = ref<number | null>(null)
const toggle = (id: number) => { open.value = open.value === id ? null : id }
const saving = (e: PromptLogEntry) => (e.shortened && e.prompt_bytes ? Math.round((1 - e.forwarded_bytes / e.prompt_bytes) * 100) : 0)
const routeLabel: Record<string, string> = { local: 'Local model', cloud: 'Cloud', rejected: 'Refused' }
const uid = useId()
</script>

<template>
  <div class="stack">
    <p v-if="!org" class="notice">Choose an organisation in the sidebar first.</p>
    <p v-else-if="!session.can('admin')" class="notice">Only owners and admins of {{ org.name }} can see its prompt log.<template v-if="setting"> It is {{ setting.enabled ? `on: prompts sent through the gateway are kept for ${setting.retention_days} days` : 'off: no prompt text is kept' }}.</template></p>
    <template v-else>
      <section class="panel">
        <h2>Prompt log</h2>
        <p class="muted small">When this is on, CodeSynapse keeps what people of {{ org.name }} type to their agents through the gateway: the prompt, the shorter text sent on when the gateway shortened it, and the local model's answer. Secrets and personal data are removed and each text is cut at 64 KiB. Tool results, file contents and system prompts are never kept. The audit log always holds a fingerprint (SHA-256) of each prompt, never its text, so every kept prompt can be checked against it.</p>
        <label class="check"><input type="checkbox" :checked="setting?.enabled ?? false" :disabled="!enabled" @change="save({ enabled: ($event.target as HTMLInputElement).checked })"> Keep prompts sent through the gateway</label>
        <div class="row retention">
          <div class="field">
            <label :for="`${uid}-days`">Keep for (days)</label>
            <input :id="`${uid}-days`" v-model.number="days" type="number" :min="setting?.min_days ?? 1" :max="setting?.max_days ?? 400" :disabled="!enabled">
          </div>
          <button type="button" class="secondary" :disabled="!enabled || days === setting?.retention_days" @click="save({ retention_days: days })">Save</button>
        </div>
        <p class="muted small">Older prompts are deleted every hour.<template v-if="setting?.archive"> Prompts older than the audit log's hot window are moved to the audit archive first, with a checksum, and deleted from it at the end of this period.</template></p>
        <p v-if="saved" class="notice notice--ok" role="status">Prompt log setting saved.</p>
        <p v-if="saveError" class="notice notice--danger" role="alert">{{ saveError }}</p>
      </section>

      <section class="panel">
        <div class="row filters">
          <div class="field"><label :for="`${uid}-user`">Person</label><select :id="`${uid}-user`" v-model="user"><option value="">Anyone</option><option v-for="m in members?.members ?? []" :key="m.user.id" :value="m.user.id">{{ m.user.name || m.user.email }}</option></select></div>
          <div class="field"><label :for="`${uid}-ses`">Chat session</label><input :id="`${uid}-ses`" v-model.lazy="chat" placeholder="any" autocomplete="off"></div>
          <div class="field"><label :for="`${uid}-model`">Model</label><input :id="`${uid}-model`" v-model.lazy="model" placeholder="any" autocomplete="off"></div>
          <div class="field"><label :for="`${uid}-route`">Answered by</label><select :id="`${uid}-route`" v-model="routeFilter"><option value="">Any</option><option value="local">Local model</option><option value="cloud">Cloud</option><option value="rejected">Refused</option></select></div>
          <div class="field"><label :for="`${uid}-short`">Shortened</label><select :id="`${uid}-short`" v-model="shortened"><option value="">Any</option><option value="true">Yes</option><option value="false">No</option></select></div>
          <div class="field"><label :for="`${uid}-fall`">Why not local</label><select :id="`${uid}-fall`" v-model="fallthrough"><option value="">Any</option><option v-for="f in FALLTHROUGH" :key="f" :value="f">{{ f.replaceAll('_', ' ') }}</option></select></div>
          <div class="field"><label :for="`${uid}-period`">Period</label><select :id="`${uid}-period`" v-model.number="period"><option :value="1">Last 24 hours</option><option :value="7">Last 7 days</option><option :value="30">Last 30 days</option><option :value="400">Last 400 days</option></select></div>
          <div class="field export"><span class="label">Export</span><div class="row"><a class="btn btn--ghost" :href="exportUrl('csv')">CSV</a><a class="btn btn--ghost" :href="exportUrl('jsonl')">JSON Lines</a></div></div>
        </div>
        <p class="muted small">Opening this page and exporting are recorded in the audit log.</p>
      </section>

      <DataState v-if="error" :error="error" />
      <section class="panel" :class="{ refreshing: pending }" :aria-busy="pending">
        <DataState :pending="pending" :empty="!rows.length" title="No prompts" :text="setting?.enabled ? 'Nothing matches these filters in this period.' : 'The prompt log is off, so no prompts are kept.'">
          <div class="table-wrap">
            <table>
              <thead><tr><th>When</th><th>Who</th><th>Answered by</th><th>Model</th><th class="num">Prompt</th><th class="num">Saved</th><th class="num">Time</th></tr></thead>
              <tbody>
                <template v-for="e in rows" :key="e.id">
                  <tr class="ev" @click="toggle(e.id)">
                    <td class="nowrap" :title="e.at"><button type="button" class="link" :aria-expanded="open === e.id" @click.stop="toggle(e.id)">{{ new Date(e.at).toLocaleString() }}</button></td>
                    <td>{{ who(e.user_id) }}</td>
                    <td class="nowrap"><span class="badge" :class="e.route === 'local' ? 'badge--green' : e.route === 'rejected' ? 'badge--pink' : ''">{{ routeLabel[e.route] ?? e.route }}</span><div v-if="e.fallthrough" class="muted small">{{ e.fallthrough.replaceAll('_', ' ') }}</div></td>
                    <td class="mono small">{{ e.model }}</td>
                    <td class="num">{{ fmtInt(e.prompt_bytes) }} B</td>
                    <td class="num">{{ e.shortened ? `${saving(e)} %` : '–' }}</td>
                    <td class="num">{{ fmtInt(e.duration_ms) }} ms</td>
                  </tr>
                  <tr v-if="open === e.id" class="detail">
                    <td colspan="7">
                      <div class="texts" :class="{ 'texts--two': e.shortened }">
                        <div>
                          <h3>{{ e.shortened ? 'What they wrote' : 'Prompt' }} <span class="muted small">{{ fmtInt(e.prompt_bytes) }} bytes</span></h3>
                          <pre>{{ e.prompt || '(no text)' }}</pre>
                        </div>
                        <div v-if="e.shortened">
                          <h3>What the gateway sent <span class="muted small">{{ fmtInt(e.forwarded_bytes) }} bytes, {{ saving(e) }} % shorter</span></h3>
                          <pre>{{ e.forwarded || '(no text)' }}</pre>
                        </div>
                      </div>
                      <div v-if="e.answer" class="answer">
                        <h3>Local model's answer</h3>
                        <pre>{{ e.answer }}</pre>
                      </div>
                      <dl class="facts">
                        <template v-if="e.audit_id"><dt>Audit entry</dt><dd><NuxtLink :to="{ path: '/settings/audit', query: { entry: e.audit_id } }">#{{ e.audit_id }}</NuxtLink></dd></template>
                        <template v-if="e.session_id"><dt>Chat session</dt><dd class="mono"><a href="#" @click.prevent="chat = e.session_id ?? ''">{{ e.session_id }}</a></dd></template>
                        <template v-if="e.key_id"><dt>API key</dt><dd class="mono">{{ e.key_id }}</dd></template>
                        <dt>Request</dt><dd class="mono">{{ e.path }} · {{ e.status }}</dd>
                        <template v-if="e.prompt_sha256"><dt>Prompt fingerprint</dt><dd class="mono small">{{ e.prompt_sha256 }}</dd></template>
                        <template v-if="e.forwarded_sha256"><dt>Sent fingerprint</dt><dd class="mono small">{{ e.forwarded_sha256 }}</dd></template>
                      </dl>
                    </td>
                  </tr>
                </template>
              </tbody>
            </table>
            <p v-if="rows.length === 100" class="muted small">Showing the newest 100; narrow the filters or export to get everything in the period.</p>
          </div>
        </DataState>
      </section>
    </template>
  </div>
</template>

<style scoped>
.filters { align-items: flex-end; }
.retention { align-items: flex-end; }
.retention input { width: 8rem; }
.export { margin-left: auto; }
.nowrap { white-space: nowrap; }
.num { text-align: right; white-space: nowrap; }
.ev { cursor: pointer; }
.detail td { background: var(--panel-2); }
.texts { display: grid; gap: 16px; grid-template-columns: 1fr; }
.texts--two { grid-template-columns: repeat(auto-fit, minmax(16rem, 1fr)); }
.texts h3, .answer h3 { font-size: 14px; margin: 0 0 6px; }
.texts pre, .answer pre { margin: 0; padding: 12px; max-height: 24rem; overflow: auto; white-space: pre-wrap; word-break: break-word; background: var(--panel); border: 2px solid var(--border); border-radius: var(--radius, 12px); font-size: 13px; }
.answer { margin-top: 16px; }
.facts { display: grid; grid-template-columns: max-content 1fr; gap: 4px 16px; margin: 16px 0 0; font-size: 13px; }
.facts dd { margin: 0; word-break: break-all; }
.refreshing { opacity: 0.7; }
</style>
