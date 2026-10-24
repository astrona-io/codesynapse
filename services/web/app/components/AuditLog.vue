<script setup lang="ts">
import type { AuditEvent } from '~/utils/types'

// The audit log with filters, MCP request details and export. base is the
// API path: admin/audit (everything) or accounts/orgs/<slug>/audit.
const props = defineProps<{ base: string; verifyChain?: boolean }>()
const action = ref('')
const actor = ref('')
const outcome = ref('')
const session = ref('')
const tool = ref('')
const repo = ref('')
const days = ref(7)
const ACTIONS = [
  { id: '', label: 'Everything' }, { id: 'user.login', label: 'Sign-ins' }, { id: 'user.', label: 'Accounts' },
  { id: 'key.', label: 'API keys' }, { id: 'member.', label: 'Members' }, { id: 'team.', label: 'Teams' },
  { id: 'repo.', label: 'Repository access' }, { id: 'org.', label: 'Organisations' }, { id: 'auth.denied', label: 'Refused requests' }, { id: 'http ', label: 'Changes (HTTP)' },
  { id: 'mcp.', label: 'Agent (MCP) requests' }, { id: 'mcp.tools.call', label: 'Agent tool calls' }, { id: 'sso.', label: 'Single sign-on' },
]
const since = computed(() => new Date(Date.now() - days.value * 864e5).toISOString().replace(/\.\d+Z$/, 'Z'))
const query = computed(() => {
  const p = new URLSearchParams({ limit: '200', since: since.value })
  if (action.value) p.set('action', action.value)
  if (actor.value.trim()) p.set('actor', actor.value.trim())
  if (outcome.value) p.set('outcome', outcome.value)
  for (const [k, v] of [['session', session], ['tool', tool], ['repo', repo]] as const) if (v.value.trim()) p.set(k, v.value.trim())
  return p.toString()
})
const { data, error, pending } = useApiGet<{ events: AuditEvent[] }>(() => `${props.base}?${query.value}`)
const events = computed(() => data.value?.events ?? [])
const { data: verify, refresh: reverify } = useApiGet<{ ok: boolean; checked: number; reason?: string; broken_at?: number }>('admin/audit/verify', { immediate: !!props.verifyChain })
const exportUrl = (format: string) => apiPath(`${props.base}?${query.value.replace('limit=200', 'limit=10000')}&format=${format}`)
// Clicking a chat or tool in the details narrows the log to it.
const narrowers = { session, tool, repo }
function only(kind: keyof typeof narrowers, value?: string) {
  if (!value) return
  narrowers[kind].value = value
  action.value = 'mcp.'
}
const open = ref<number | null>(null)
</script>

<template>
  <div class="stack">
    <section class="panel">
      <div class="panel__header">
        <h2>Audit log</h2>
        <div class="row">
          <span v-if="verifyChain && verify" class="pill"><span class="dot" :class="verify.ok ? 'dot--ok' : 'dot--bad'" />{{ verify.ok ? `Hash chain intact (${fmtInt(verify.checked)} entries)` : `Chain broken at entry ${verify.broken_at}: ${verify.reason}` }}</span>
          <button v-if="verifyChain" class="secondary tiny" @click="reverify()">Verify again</button>
        </div>
      </div>
      <p class="muted small">Every change, security event and agent (MCP) request, append-only and hash-chained: altering or removing an entry breaks the chain. Agent requests record the person, API key, chat session, client, tool, repository and arguments; code and conversations appear only as a fingerprint. Keep exports as evidence (NIS2).</p>
      <div class="row filters">
        <div class="field"><label for="act">What</label><select id="act" v-model="action"><option v-for="a in ACTIONS" :key="a.id" :value="a.id">{{ a.label }}</option></select></div>
        <div class="field"><label for="who">Who (email)</label><input id="who" v-model.lazy="actor" type="email" placeholder="anyone"></div>
        <div class="field"><label for="out">Outcome</label><select id="out" v-model="outcome"><option value="">Any</option><option value="success">Succeeded</option><option value="failure">Failed or refused</option></select></div>
        <div class="field"><label for="ses">Chat session</label><input id="ses" v-model.lazy="session" placeholder="any" autocomplete="off"></div>
        <div class="field"><label for="tool">Tool</label><input id="tool" v-model.lazy="tool" placeholder="any" autocomplete="off"></div>
        <div class="field"><label for="repo">Repository</label><input id="repo" v-model.lazy="repo" placeholder="any" autocomplete="off"></div>
        <div class="field"><label for="days">Period</label><select id="days" v-model.number="days"><option :value="1">Last 24 hours</option><option :value="7">Last 7 days</option><option :value="30">Last 30 days</option><option :value="400">Last 400 days</option></select></div>
        <div class="field export"><span class="label">Export</span><div class="row"><a class="btn btn--ghost" :href="exportUrl('csv')">CSV</a><a class="btn btn--ghost" :href="exportUrl('jsonl')">JSON Lines</a></div></div>
      </div>
    </section>
    <p v-if="error" class="notice notice--danger">{{ apiErrorMessage(error) }}</p>
    <section class="panel" :class="{ refreshing: pending }">
      <div v-if="events.length" class="table-wrap">
        <table>
          <thead><tr><th>When</th><th>Who</th><th>What</th><th>Target</th><th>Outcome</th></tr></thead>
          <tbody>
            <template v-for="e in events" :key="e.id">
              <tr class="ev" @click="open = open === e.id ? null : e.id">
                <td class="nowrap" :title="e.at">{{ new Date(e.at).toLocaleString() }}</td>
                <td>{{ e.actor_email || e.via }}<div v-if="e.actor_email" class="muted small">{{ e.via }}</div></td>
                <td class="mono">{{ e.action }}</td>
                <td class="small target">{{ e.target }}</td>
                <td class="nowrap"><span class="badge" :class="e.outcome === 'failure' ? 'badge--pink' : 'badge--green'">{{ e.outcome }}</span><span v-if="e.status" class="muted small"> {{ e.status }}</span></td>
              </tr>
              <tr v-if="open === e.id" class="detail">
                <td colspan="5">
                  <dl class="facts">
                    <dt>Entry</dt><dd class="mono">#{{ e.id }} · {{ e.hash.slice(0, 16) }}…</dd>
                    <dt>Address</dt><dd>{{ e.ip }} · {{ e.user_agent }}</dd>
                    <dt>Request</dt><dd class="mono">{{ e.request_id }}</dd>
                    <template v-if="e.key_id"><dt>API key</dt><dd class="mono">{{ e.key_id }}</dd></template>
                    <template v-if="e.detail"><dt>Detail</dt><dd>{{ e.detail }}</dd></template>
                    <template v-if="e.mcp">
                      <dt>MCP request</dt><dd class="mono">{{ e.mcp.method }}<template v-if="e.mcp.name"> · <a href="#" @click.prevent="only('tool', e.mcp.name)">{{ e.mcp.name }}</a></template></dd>
                      <template v-if="e.mcp.repo"><dt>Repository</dt><dd class="mono"><a href="#" @click.prevent="only('repo', e.mcp.repo)">{{ e.mcp.repo }}</a></dd></template>
                      <template v-if="e.mcp.session_id"><dt>Chat session</dt><dd class="mono"><a href="#" @click.prevent="only('session', e.mcp.session_id)">{{ e.mcp.session_id }}</a></dd></template>
                      <template v-if="e.mcp.transport_id"><dt>Connection</dt><dd class="mono">{{ e.mcp.transport_id }}</dd></template>
                      <template v-if="e.mcp.client"><dt>Client</dt><dd>{{ e.mcp.client }}</dd></template>
                      <dt>Took</dt><dd>{{ e.mcp.duration_ms }} ms · {{ fmtInt(e.mcp.bytes) }} bytes returned</dd>
                      <template v-if="e.mcp.args"><dt>Arguments</dt><dd><div v-for="(v, k) in e.mcp.args" :key="k" class="mono small"><strong>{{ k }}</strong> {{ v }}</div></dd></template>
                      <template v-if="e.mcp.args_digest"><dt>Arguments fingerprint</dt><dd class="mono small">{{ e.mcp.args_digest }}</dd></template>
                      <template v-if="e.mcp.error"><dt>Error</dt><dd>{{ e.mcp.error }}</dd></template>
                    </template>
                  </dl>
                </td>
              </tr>
            </template>
          </tbody>
        </table>
        <p v-if="events.length === 200" class="muted small">Showing the newest 200; export to get everything in the period.</p>
      </div>
      <p v-else class="empty"><strong>No entries</strong>Nothing matches these filters in this period.</p>
    </section>
  </div>
</template>

<style scoped>
.filters { align-items: flex-end; }
.export { margin-left: auto; }
.nowrap { white-space: nowrap; }
.ev { cursor: pointer; }
.target { word-break: break-all; }
.detail td { background: var(--panel-2); }
.facts { display: grid; grid-template-columns: max-content 1fr; gap: 4px 16px; margin: 0; font-size: 13px; }
.facts dd { margin: 0; word-break: break-all; }
.refreshing { opacity: 0.7; }
</style>
