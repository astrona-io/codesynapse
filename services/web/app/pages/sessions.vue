<script setup lang="ts">
useHead({ title: 'Agent sessions · CodeSynapse' })
import type { AgentSessionDetail, AgentSessionList, SessionCall } from '~/utils/types'

// What each agent chat asked CodeSynapse, in order (from the MCP audit
// log): for reviewing an agent's pull request. Members see their own
// sessions, owners and admins everyone's; the API enforces it.
const session = useSession()
const org = computed(() => session.current.value?.org)
const route = useRoute()
const router = useRouter()
const days = ref(7)
const selected = computed(() => typeof route.query.id === 'string' ? route.query.id : '')
const base = computed(() => `accounts/orgs/${org.value?.slug ?? '-'}/agent-sessions`)
const { data: list, error: listError, pending } = useApiGet<AgentSessionList>(() => `${base.value}?days=${days.value}`, { immediate: !!org.value })
// "-" stands for no selection; the timeline shows only once one is made.
const { data: detail, error: detailError } = useApiGet<AgentSessionDetail>(() => `${base.value}/${encodeURIComponent(selected.value || '-')}`, { immediate: !!selected.value })

function pick(id: string) {
  router.replace({ query: { ...route.query, id: selected.value === id ? undefined : id } })
}
// Tokens are estimated as bytes/4, like every CodeSynapse budget.
const tokens = (bytes: number) => fmtCompact(Math.round(bytes / 4))
const topTools = (tools: Record<string, number>) => Object.entries(tools).sort((a, b) => b[1] - a[1] || a[0].localeCompare(b[0])).slice(0, 3)
const largest = computed(() => Math.max(1, ...(detail.value?.calls ?? []).map(c => c.bytes)))
const offset = (c: SessionCall) => {
  const first = detail.value?.calls[0]
  if (!first) return ''
  const s = Math.round((Date.parse(c.at) - Date.parse(first.at)) / 1000)
  return s < 60 ? `+${s}s` : s < 3600 ? `+${Math.floor(s / 60)}m ${s % 60}s` : `+${Math.floor(s / 3600)}h ${Math.floor((s % 3600) / 60)}m`
}
const label = (c: SessionCall) => c.name || c.method
const open = ref<number | null>(null)
watch(selected, () => { open.value = null })
</script>

<template>
  <div class="stack">
    <header class="page-head">
      <h1>Agent sessions</h1>
      <p class="muted">Every chat an agent had with CodeSynapse, and what it asked for in order: the tools, the repositories, how much came back and what failed. Use it to see what an agent looked at before it made a change. It is read from the audit log, so arguments are already redacted and code never appears.</p>
    </header>
    <p v-if="!org" class="notice">Choose an organisation in the sidebar first.</p>
    <template v-else>
      <section class="panel" :class="{ refreshing: pending }">
        <div class="panel__header">
          <h2>{{ list?.scope === 'organisation' ? `Sessions in ${org.name}` : 'Your sessions' }}</h2>
          <div class="field inline"><label for="days">Period</label>
            <select id="days" v-model.number="days"><option :value="1">Last 24 hours</option><option :value="7">Last 7 days</option><option :value="30">Last 30 days</option></select>
          </div>
        </div>
        <p v-if="list?.scope === 'own'" class="muted small">You see your own sessions; owners and admins see everyone's in {{ org.name }}.</p>
        <DataState :error="listError ? plainError(listError) : ''" :pending="pending" :empty="!list?.sessions.length" title="No agent sessions" text="Nothing called CodeSynapse through an agent in this period. Connect one on the Agents page.">
          <div v-if="list" class="table-wrap">
            <table>
              <thead><tr><th>Last call</th><th v-if="list.scope === 'organisation'">Who</th><th>Client</th><th>API key</th><th class="num">Calls</th><th class="num">Returned</th><th>Most used</th><th>Repositories</th></tr></thead>
              <tbody>
                <tr v-for="s in list.sessions" :key="s.id" class="row-link" :class="{ active: s.id === selected }" tabindex="0" @click="pick(s.id)" @keydown.enter="pick(s.id)">
                  <td class="nowrap" :title="new Date(s.last).toLocaleString()">{{ relTime(s.last) }}</td>
                  <td v-if="list.scope === 'organisation'">{{ s.actor }}</td>
                  <td class="small">{{ s.client || '–' }}</td>
                  <td class="small mono">{{ s.key || '–' }}</td>
                  <td class="num">{{ fmtInt(s.calls) }}<span v-if="s.errors" class="badge badge--pink small">{{ s.errors }} failed</span></td>
                  <td class="num" :title="`${fmtInt(s.bytes)} bytes`">~{{ tokens(s.bytes) }} tokens</td>
                  <td><span v-for="[t, n] in topTools(s.tools)" :key="t" class="chip mono">{{ t }} ×{{ n }}</span></td>
                  <td class="small mono">{{ s.repos.join(', ') || '–' }}</td>
                </tr>
              </tbody>
            </table>
            <p v-if="list.truncated" class="muted small">The period has more requests than one page reads; older sessions may look shorter than they were. Choose a shorter period.</p>
          </div>
        </DataState>
      </section>

      <section v-if="selected" class="panel">
        <div class="panel__header">
          <h2>Timeline</h2>
          <button class="secondary tiny" @click="pick(selected)">Close</button>
        </div>
        <p class="muted small mono">{{ selected }}<template v-if="detail"> · {{ detail.actor }}<template v-if="detail.key"> · key {{ detail.key }}</template></template></p>
        <p v-if="detailError" class="notice notice--danger">{{ plainError(detailError) }}</p>
        <ol v-if="detail" class="timeline">
          <li v-for="(c, i) in detail.calls" :key="i" :class="{ failed: c.outcome === 'failure' }">
            <button class="call" :aria-expanded="open === i" @click="open = open === i ? null : i">
              <span class="when mono small" :title="new Date(c.at).toLocaleString()">{{ offset(c) }}</span>
              <span class="what mono">{{ label(c) }}</span>
              <span class="repo small muted mono">{{ c.repo }}</span>
              <span class="size"><span class="bar" :style="{ width: `${Math.max(2, (c.bytes / largest) * 100)}%` }" /></span>
              <span class="num small">~{{ tokens(c.bytes) }} tok · {{ c.duration_ms }} ms</span>
              <span class="status"><span v-if="c.outcome === 'failure'" class="badge badge--pink small">failed</span></span>
            </button>
            <div v-if="open === i" class="call-detail">
              <p v-if="c.error" class="small"><strong>Error</strong> {{ c.error }}</p>
              <div v-for="(v, k) in c.args" :key="k" class="mono small"><strong>{{ k }}</strong> {{ v }}</div>
              <p v-if="!c.args && !c.error" class="muted small">No arguments.</p>
            </div>
          </li>
        </ol>
        <p v-if="detail?.truncated" class="muted small">Showing the first {{ fmtInt(detail.limit) }} calls; export the session from the audit log for all of them.</p>
      </section>
    </template>
  </div>
</template>

<style scoped>
.page-head h1 { margin-bottom: 4px; }
.inline { display: flex; align-items: center; gap: 8px; }
.num { text-align: right; white-space: nowrap; }
.nowrap { white-space: nowrap; }
.row-link { cursor: pointer; }
.row-link:hover td, .row-link:focus-visible td, .row-link.active td { background: var(--panel-2); }
.row-link:focus-visible { outline: 2px solid var(--accent); outline-offset: -2px; }
.chip { display: inline-block; margin: 0 4px 2px 0; padding: 1px 8px; border-radius: 999px; background: var(--panel-2); font-size: 12px; }
.badge { margin-left: 6px; }
.timeline { list-style: none; margin: 0; padding: 0; display: grid; gap: 2px; }
.call { all: unset; box-sizing: border-box; width: 100%; display: grid; grid-template-columns: 72px minmax(120px, 1.2fr) minmax(0, 1fr) minmax(60px, 1fr) 148px 56px; align-items: center; gap: 12px; padding: 6px 8px; border-radius: 8px; cursor: pointer; }
.call:hover, .call:focus-visible { background: var(--panel-2); }
.call:focus-visible { outline: 2px solid var(--accent); }
.what { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.failed .what { color: var(--danger); }
.repo { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.size { height: 8px; border-radius: 4px; background: var(--panel-2); overflow: hidden; }
.bar { display: block; height: 100%; background: var(--primary); border-radius: 4px; }
.failed .bar { background: var(--danger); }
.call-detail { margin: 0 8px 8px 92px; padding: 8px 12px; border-radius: 8px; background: var(--panel-2); word-break: break-all; }
.refreshing { opacity: 0.7; }
@media (max-width: 720px) {
  .call { grid-template-columns: 44px minmax(0, 1fr) auto 60px; gap: 8px; }
  .call .num { white-space: normal; text-align: right; }
  .repo, .size { display: none; }
  .call-detail { margin-left: 8px; }
}
</style>
