<script setup lang="ts">
import type { AuditEvent } from '~/utils/types'

const action = ref('')
const actor = ref('')
const outcome = ref('')
const days = ref(7)
const ACTIONS = [
  { id: '', label: 'Everything' }, { id: 'user.login', label: 'Sign-ins' }, { id: 'user.', label: 'Accounts' },
  { id: 'key.', label: 'API keys' }, { id: 'member.', label: 'Members' }, { id: 'team.', label: 'Teams' },
  { id: 'repo.', label: 'Repository access' }, { id: 'org.', label: 'Organisations' }, { id: 'auth.denied', label: 'Refused requests' }, { id: 'http ', label: 'Changes (HTTP)' },
]
const since = computed(() => new Date(Date.now() - days.value * 864e5).toISOString().replace(/\.\d+Z$/, 'Z'))
const query = computed(() => {
  const p = new URLSearchParams({ limit: '200', since: since.value })
  if (action.value) p.set('action', action.value)
  if (actor.value.trim()) p.set('actor', actor.value.trim())
  if (outcome.value) p.set('outcome', outcome.value)
  return p.toString()
})
const { data, error, pending } = useApiGet<{ events: AuditEvent[] }>(() => `admin/audit?${query.value}`)
const { data: verify, refresh: reverify } = useApiGet<{ ok: boolean; checked: number; reason?: string; broken_at?: number }>('admin/audit/verify')
const exportUrl = (format: string) => apiPath(`admin/audit?${query.value.replace('limit=200', 'limit=10000')}&format=${format}`)
const open = ref<number | null>(null)
</script>

<template>
  <div class="stack">
    <section class="panel">
      <div class="panel__header">
        <h2>Audit log</h2>
        <div class="row">
          <span v-if="verify" class="pill"><span class="dot" :class="verify.ok ? 'dot--ok' : 'dot--bad'" />{{ verify.ok ? `Hash chain intact (${fmtInt(verify.checked)} entries)` : `Chain broken at entry ${verify.broken_at}: ${verify.reason}` }}</span>
          <button class="secondary tiny" @click="reverify()">Verify again</button>
        </div>
      </div>
      <p class="muted small">Every change and security event, append-only and hash-chained: altering or removing an entry breaks the chain. Keep exports as evidence (NIS2).</p>
      <div class="row filters">
        <div class="field"><label for="act">What</label><select id="act" v-model="action"><option v-for="a in ACTIONS" :key="a.id" :value="a.id">{{ a.label }}</option></select></div>
        <div class="field"><label for="who">Who (email)</label><input id="who" v-model.lazy="actor" type="email" placeholder="anyone"></div>
        <div class="field"><label for="out">Outcome</label><select id="out" v-model="outcome"><option value="">Any</option><option value="success">Succeeded</option><option value="failure">Failed or refused</option></select></div>
        <div class="field"><label for="days">Period</label><select id="days" v-model.number="days"><option :value="1">Last 24 hours</option><option :value="7">Last 7 days</option><option :value="30">Last 30 days</option><option :value="400">Last 400 days</option></select></div>
        <div class="field export"><span class="label">Export</span><div class="row"><a class="btn btn--ghost" :href="exportUrl('csv')">CSV</a><a class="btn btn--ghost" :href="exportUrl('jsonl')">JSON Lines</a></div></div>
      </div>
    </section>
    <p v-if="error" class="notice notice--danger">{{ apiErrorMessage(error) }}</p>
    <section class="panel" :class="{ refreshing: pending }">
      <div v-if="data?.events.length" class="table-wrap">
        <table>
          <thead><tr><th>When</th><th>Who</th><th>What</th><th>Target</th><th>Outcome</th></tr></thead>
          <tbody>
            <template v-for="e in data.events" :key="e.id">
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
                  </dl>
                </td>
              </tr>
            </template>
          </tbody>
        </table>
        <p v-if="data.events.length === 200" class="muted small">Showing the newest 200; export to get everything in the period.</p>
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
