<script setup lang="ts">
import type { AIProvider } from '~/utils/types'

const { data, refresh } = useApiGet<{ providers: AIProvider[]; tasks: string[]; routing: { default: string; tasks: Record<string, string> } }>('ai/providers')
interface ProviderStatus { name: string; checked: boolean; online: boolean; model_available: boolean; latency_ms?: number; error?: string }
// The day's totals from every process (p.usage counts the API process only).
const { usageOf: dayUsage } = useModelUsage('24h')
const { data: status, refresh: refreshStatus } = useApiGet<{ providers: ProviderStatus[] }>('ai/status')
let statusTimer: ReturnType<typeof setInterval> | undefined
onMounted(() => { statusTimer = setInterval(() => refreshStatus(), 15000) })
onBeforeUnmount(() => statusTimer && clearInterval(statusTimer))
const statusOf = (n: string) => status.value?.providers.find(p => p.name === n)
function health(n: string): { label: string; tone: string; note: string } {
  const st = statusOf(n)
  if (!st) return { label: 'Checking…', tone: 'badge--violet', note: '' }
  if (!st.checked) return { label: 'Not checked', tone: 'badge--violet', note: 'Cloud endpoints are not probed.' }
  if (!st.online) return { label: 'Offline', tone: 'badge--pink', note: 'The endpoint does not answer. Is the model server running?' }
  if (!st.model_available) return { label: 'Online, model missing', tone: 'badge--amber', note: 'The server answers but does not list the configured model.' }
  return { label: 'Online', tone: 'badge--green', note: st.latency_ms !== undefined ? `Answered in ${st.latency_ms} ms.` : '' }
}
const AREAS = [
  { task: 'review', label: 'Code review', hint: 'Reviews of changes and risky code' },
  { task: 'security-triage', label: 'Security analysis', hint: 'Triage of findings and fixes' },
  { task: 'briefing', label: 'Briefings', hint: 'Architecture and risk overviews' },
  { task: 'pr-summary', label: 'Change summaries', hint: 'Plain-language summary of a diff (local models only)', local: true },
  { task: 'cleanup-briefing', label: 'Cleanup briefings', hint: 'What to clean up first, from the cleanup plan (local models only)', local: true },
  { task: 'local-answer', label: 'Agent questions', hint: 'Reads files for coding agents (ask_local) so they spend fewer cloud tokens (local models only)', local: true },
  { task: 'fix', label: 'Fixes', hint: 'Writes fix branches for accepted review comments (needs source: use a local model)' },
]
const routing = reactive<{ default: string; tasks: Record<string, string> }>({ default: '', tasks: {} })
// Local-only areas follow the global model when it is local.
const globalIsLocal = computed(() => !!data.value?.providers.find(p => p.name === routing.default)?.local)
watch(() => data.value?.routing, (r) => {
  if (!r) return
  routing.default = r.default
  routing.tasks = { ...r.tasks }
}, { immediate: true })
const saving = ref(false)
const saved = ref('')
const routeError = ref('')
async function saveRouting() {
  saving.value = true
  saved.value = ''
  routeError.value = ''
  try {
    await $fetch(apiPath('ai/routing'), { method: 'PUT', body: { default: routing.default, tasks: routing.tasks } })
    saved.value = 'Saved'
    refresh()
  } catch (e) {
    routeError.value = apiErrorMessage(e)
  } finally {
    saving.value = false
  }
}
const TASK_LABEL: Record<string, string> = { 'security-triage': 'Security triage', review: 'Code review', briefing: 'Briefing', fix: 'Fixes', 'pr-summary': 'Change summary', 'cleanup-briefing': 'Cleanup briefing', 'local-answer': 'Agent questions' }
interface TestResult { ok: boolean; error?: string; reply?: string; models?: string[]; missing?: string[]; seconds?: number; model?: string }
const tests = ref<Record<string, TestResult | 'running'>>({})
async function test(name: string) {
  tests.value = { ...tests.value, [name]: 'running' }
  try {
    const r = await apiPost<TestResult>(`ai/providers/${encodeURIComponent(name)}/test`, {})
    tests.value = { ...tests.value, [name]: r }
  } catch (e) {
    tests.value = { ...tests.value, [name]: { ok: false, error: apiErrorMessage(e) } }
  } finally {
    refresh()
    refreshStatus()
  }
}
const resultOf = (n: string) => tests.value[n]
</script>

<template>
  <div class="stack">
    <PageHeader eyebrow="Workspace" title="AI models"
      lede="Language models CodeSynapse uses to triage findings, review code and write briefings. Local models (MLX, Ollama, vLLM) keep code on your machine; cloud models only get what you allow." />

    <div v-if="!data?.providers.length" class="panel how">
      <p class="eyebrow eyebrow--green">No model configured</p>
      <h2>Connect a local or cloud model</h2>
      <p class="muted">With nothing configured, CodeSynapse uses a local model server it finds when the API starts: Ollama on port 11434 or LM Studio on 1234. Start one, pull a model (<code>ollama pull qwen3.8:27b</code>) and restart the API. To choose yourself:</p>
      <ol class="steps">
        <li>Copy <code>config/llm.example.yaml</code> to <code>config/llm.yaml</code> in the project folder.</li>
        <li>Point <code>base_url</code> at your server. For a model server on this Mac use <code>http://host.docker.internal:&lt;port&gt;/v1</code> — for example MLX on 9080 or Ollama on 11434.</li>
        <li>For a cloud provider, put its key in <code>.env</code> and name that variable in <code>api_key_env</code>.</li>
        <li>Restart the API: <code>docker compose up -d api</code> (or <code>podman compose</code>).</li>
      </ol>
    </div>

    <section v-if="data?.providers.length" class="panel routing">
      <div class="panel__header">
        <div><p class="eyebrow">Routing</p><h2>Which model does what</h2></div>
        <div class="row">
          <span v-if="saved" class="ok small">{{ saved }}</span>
          <button :disabled="saving" @click="saveRouting">{{ saving ? 'Saving…' : 'Save routing' }}</button>
        </div>
      </div>
      <p class="muted small">The global model handles everything. Pick a different model for an area when it needs one — for example a cloud model for security analysis and a fast local one for reviews.</p>
      <div class="route route--global">
        <div><strong>Global</strong><p class="muted small">Fallback for every area without its own model</p></div>
        <select v-model="routing.default" aria-label="Global model">
          <option v-for="p in data.providers" :key="p.name" :value="p.name">{{ p.name }} · {{ p.model || 'server default' }} · {{ p.local ? 'local' : 'cloud' }}</option>
        </select>
      </div>
      <div v-for="a in AREAS" :key="a.task" class="route">
        <div><strong>{{ a.label }}</strong><p class="muted small">{{ a.hint }}</p></div>
        <select v-model="routing.tasks[a.task]" :aria-label="`Model for ${a.label}`">
          <option value="">{{ a.local && !globalIsLocal ? 'First local model' : `Use global (${routing.default})` }}</option>
          <option v-for="p in data.providers.filter(x => !a.local || x.local)" :key="p.name" :value="p.name">{{ p.name }} · {{ p.local ? 'local' : 'cloud' }}</option>
        </select>
      </div>
      <p v-if="routeError" class="error small">{{ routeError }}</p>
    </section>

    <div v-if="data?.providers.length" class="grid cols-2">
      <section v-for="p in data.providers" :key="p.name" class="panel provider">
        <div class="panel__header">
          <div>
            <p class="eyebrow">{{ p.kind === 'anthropic' ? 'Anthropic' : 'OpenAI-compatible' }} · {{ p.local ? 'local' : 'cloud' }}</p>
            <h2 class="mono">{{ p.name }}</h2>
            <p class="status-line"><span class="badge" :class="health(p.name).tone">{{ health(p.name).label }}</span><span class="muted small">{{ health(p.name).note }}</span></p>
          </div>
          <button class="secondary" :disabled="resultOf(p.name) === 'running'" @click="test(p.name)">{{ resultOf(p.name) === 'running' ? 'Testing…' : 'Test connection' }}</button>
        </div>
        <dl class="facts">
          <dt>Model<InfoTip v-if="p.auto" text="model: auto in config/llm.yaml. CodeSynapse asks the server what it serves every five minutes and uses the chat model with the most parameters, and an embedding model for recall, so a model you pull is used without a restart." /></dt>
          <dd class="mono">{{ p.model === 'auto' ? 'none yet' : (p.model || '(server default)') }}<span v-if="p.auto" class="badge badge--green auto">picked automatically</span></dd>
          <dt>Endpoint</dt><dd class="mono">{{ p.endpoint }}</dd>
          <dt>API key</dt><dd>{{ p.has_key ? `set (${p.key_env})` : p.key_env ? `missing — set ${p.key_env}` : 'none needed' }}</dd>
          <dt>Code snippets</dt><dd><span class="badge" :class="p.sends_source ? 'badge--amber' : 'badge--green'">{{ p.sends_source ? 'sent (short excerpts)' : 'never sent' }}</span></dd>
          <dt>Context window<InfoTip text="The most the model can read and write in one request. Set context_window in config/llm.yaml. CodeSynapse reserves answer space and checks the final prompt estimate." /></dt><dd>{{ p.context_window ? `${fmtInt(p.context_window)} tokens` : 'not set; model capacity unverified' }}</dd>
          <dt>Budget per request<InfoTip text="The final redacted request is checked before sending. Input counts are conservative estimates and may differ from the model tokenizer. Exact-only policy refuses calls without a verified counter. Lower max_input_tokens in config/llm.yaml to limit context." /></dt><dd>{{ fmtInt(p.max_input_tokens) }} estimated in · {{ fmtInt(p.max_output_tokens) }} tokens out</dd>
          <dt>Input policy</dt><dd>{{ p.input_budget_policy === 'require_exact' ? 'Exact count required; unavailable' : 'Estimated count allowed' }}</dd>
          <dt>Used for</dt><dd><span v-for="t in p.tasks" :key="t" class="tag">{{ TASK_LABEL[t] ?? t }}</span><span v-if="!p.tasks.length" class="muted">no tasks (only by explicit choice)</span></dd>
          <dt>Usage<InfoTip text="Last 24 hours from the metrics store, counting every process (the worker runs the AI jobs). Without metrics it shows what the API process itself has done since it started." /></dt>
          <dd v-if="dayUsage(p.name)">{{ plural(dayUsage(p.name)!.requests, 'request') }} · {{ fmtInt(dayUsage(p.name)!.input) }} in / {{ fmtInt(dayUsage(p.name)!.output) }} out<span v-if="dayUsage(p.name)!.failures" class="error"> · {{ dayUsage(p.name)!.failures }} failed</span><span class="muted small"> · last 24 h</span></dd>
          <dd v-else>{{ plural(p.usage.requests, 'request') }} · {{ fmtInt(p.usage.input_tokens) }} in / {{ fmtInt(p.usage.output_tokens) }} out<span v-if="p.usage.requests"> · avg {{ p.usage.avg_seconds.toFixed(1) }} s</span><span v-if="p.usage.failures" class="error"> · {{ p.usage.failures }} failed</span><span class="muted small"> · this API process only</span></dd>
        </dl>
        <details v-if="p.served" class="served" :open="p.model === 'auto'">
          <summary>What the server offers ({{ p.served.models.length }})<span v-if="p.served.error" class="error"> · last check failed</span></summary>
          <p v-if="p.served.error" class="error small">{{ p.served.error }}</p>
          <table v-if="p.served.models.length">
            <thead><tr><th>Model</th><th>Can</th><th class="num">Size</th><th class="num">Context</th><th>Used for</th></tr></thead>
            <tbody>
              <tr v-for="m in p.served.models" :key="m.name">
                <td class="mono">{{ m.name }}</td>
                <td>{{ [m.chat && 'chat', m.embedding && 'embeddings', m.thinking && 'thinking'].filter(Boolean).join(', ') }}</td>
                <td class="num">{{ m.params_b ? `${m.params_b >= 1 ? m.params_b.toFixed(1) + 'B' : Math.round(m.params_b * 1000) + 'M'}` : '–' }}</td>
                <td class="num">{{ m.context ? fmtCompact(m.context) : '–' }}</td>
                <td><span v-if="m.name === p.model" class="badge badge--green">answers</span><span v-if="m.name === p.embedding_model" class="badge badge--violet">recall</span></td>
              </tr>
            </tbody>
          </table>
          <p v-else-if="!p.served.error" class="muted small">The server offers no models yet. On Ollama: <code>ollama pull qwen3.8:27b</code>.</p>
        </details>
        <template v-if="resultOf(p.name) && resultOf(p.name) !== 'running'">
          <p v-if="(resultOf(p.name) as TestResult).ok" class="ok">Connected in {{ (resultOf(p.name) as TestResult).seconds }} s · replied “{{ (resultOf(p.name) as TestResult).reply }}”</p>
          <p v-else class="error">{{ (resultOf(p.name) as TestResult).error }}</p>
          <p v-if="(resultOf(p.name) as TestResult).missing?.length" class="error">Not on this server: {{ (resultOf(p.name) as TestResult).missing!.join(', ') }}. On Ollama, run <code>ollama pull {{ (resultOf(p.name) as TestResult).missing![0] }}</code>; otherwise fix the name in <code>config/llm.yaml</code>.</p>
          <p v-if="(resultOf(p.name) as TestResult).models?.length" class="muted small">Server offers: {{ (resultOf(p.name) as TestResult).models!.join(', ') }}</p>
        </template>
        <p v-else-if="p.usage.last_error" class="error small">Last error: {{ p.usage.last_error }}</p>
      </section>
    </div>

    <section class="panel">
      <p class="eyebrow">How data is handled</p>
      <ul class="facts-list">
        <li>Prompts are built from the graph — findings, signatures, summaries — not whole files, and every prompt is scrubbed for secrets.</li>
        <li>Code snippets go only to providers marked “sent”: local ones by default, cloud ones only with <code>send_source: true</code>.</li>
        <li>Answers are treated as untrusted text: they become reviews you accept or reject, never actions.</li>
      </ul>
    </section>
  </div>
</template>

<style scoped>
.auto { margin-left: 8px; }
.served summary { cursor: pointer; font-size: 13px; color: var(--muted); }
.served table { margin-top: 8px; font-size: 13px; }
.served .badge + .badge { margin-left: 4px; }
.steps { margin: 0; padding-left: 20px; display: grid; gap: 6px; }
.facts { display: grid; grid-template-columns: max-content 1fr; gap: 6px 14px; margin: 0; font-size: 13.5px; }
.facts dt { color: var(--muted); }
.facts dd { margin: 0; }
.ok { color: var(--success); }
.status-line { display: flex; flex-wrap: wrap; align-items: center; gap: 8px; margin: 6px 0 0; }
.small { font-size: 12.5px; }
.facts-list { display: grid; gap: 6px; }
.routing { gap: 12px; }
.route { display: grid; grid-template-columns: minmax(0, 1fr) minmax(220px, 360px); gap: 14px; align-items: center; padding: 10px 0; border-top: 1px dashed var(--line); }
.route--global strong { color: var(--accent-ink); }
@media (max-width: 700px) { .route { grid-template-columns: 1fr; } }
</style>
