<script setup lang="ts">
import { useStatistics } from '~/composables/useStatistics'
const { days, s, error, pending } = useStatistics()

const a = computed(() => s.value?.agents)
const errorRate = computed(() => (a.value?.calls ? `${((a.value.errors / a.value.calls) * 100).toFixed(1)}%` : '–'))
const callsPerDay = computed(() => (a.value?.daily ?? []).map((d) => ({ label: d.day, value: d.calls, detail: `${d.sessions} session(s), ${d.errors} error(s)` })))
const avoidedPerDay = computed(() => (a.value?.daily ?? []).map((d) => ({ label: d.day, value: d.avoided_tokens, detail: `${fmtInt(d.response_tokens)} served` })))
// Share of the line ranges each tool's answers gave that the agent then
// read (Claude Code plugin hooks), most read first.
const readShare = computed(() => (a.value?.tools ?? []).filter((t) => t.ranges_returned > 0)
  .map((t) => ({ label: t.tool, value: Math.round((100 * Math.min(t.ranges_read, t.ranges_returned)) / t.ranges_returned), detail: `${fmtInt(t.ranges_read)} of ${fmtInt(t.ranges_returned)} line ranges read` }))
  .sort((x, y) => y.value - x.value))
const tools = computed(() => (a.value?.tools ?? []).map((t) => ({ label: t.tool, value: t.calls, detail: `${t.kind} · ${fmtInt(Math.round(t.avg_tokens))} tokens/call · ${t.avg_ms} ms avg${t.errors ? ` · ${t.errors} errors` : ''}` })))
const clients = computed(() => (a.value?.clients ?? []).map((c) => ({ label: c.client, value: c.calls, detail: `${c.sessions} session(s)` })))
const repoUse = computed(() => (a.value?.repos ?? []).map((r) => ({ label: r.repo || '(no repo argument)', value: r.calls, detail: `${fmtCompact(r.response_tokens)} served · ~${fmtCompact(r.avoided_tokens)} avoided` })))
</script>

<template>
  <p v-if="error" class="panel error">{{ apiErrorMessage(error) }}</p>
  <div v-else-if="s" :class="{ refreshing: pending }" class="stack">
    <section v-if="a" class="panel">
      <p class="eyebrow">Agents &amp; context</p><h2>MCP sessions and tool calls</h2>
      <p class="muted small">Counted by the MCP server per day, repository, tool and client. Arguments and results are never stored, only their size.</p>
      <div class="grid kpis">
        <StatTile label="Sessions" :value="fmtCompact(a.sessions)" :hint="`${a.clients.length} client(s)`" />
        <StatTile label="Tool calls" :value="fmtCompact(a.calls)" :hint="`${errorRate} errors · ${a.avg_latency_ms} ms avg`" />
        <StatTile label="Tokens served" :value="fmtCompact(a.response_tokens)" :hint="a.calls ? `${fmtInt(Math.round(a.response_tokens / a.calls))} per call` : 'no calls yet'" />
        <StatTile label="Tokens avoided (est.)" :value="fmtCompact(a.avoided_tokens)" hint="vs reading the files instead" />
      </div>
      <div v-if="a && (a.calls || a.sessions)" class="grid cols-2">
        <div><p class="label">Tool calls per day</p><ColumnChart :points="callsPerDay" title="Tool calls per day" unit="calls" /></div>
        <div><p class="label">Tokens avoided per day (estimate)</p><ColumnChart :points="avoidedPerDay" title="Tokens avoided per day" unit="tokens" color="var(--success)" /></div>
      </div>
      <div v-else class="empty"><strong>No agent activity yet</strong>Connect an agent (<code>just mcp</code>) and use it on an indexed repository; counts appear here within a minute.</div>
      <div v-if="a.calls" class="grid cols-3">
        <div><p class="label">Most used tools</p><RankList :rows="tools" /></div>
        <div><p class="label">Clients</p><RankList :rows="clients" color="var(--muted)" /></div>
        <div><p class="label">Repositories asked about</p><RankList :rows="repoUse" /></div>
      </div>
      <div v-if="a.calls" class="read-share">
        <p class="label">Answers the agent read</p>
        <RankList :rows="readShare" suffix="%" :scale="100" color="var(--success)" empty="Install the Claude Code plugin to see which tools point agents to code they then read." />
        <p v-if="readShare.length" class="muted small">Of the file line ranges a tool's answers gave, the share the agent read afterwards. A low share means the tool answers with places that do not help; reported by the plugin's hooks as counts only.</p>
      </div>
      <details class="method"><summary>How tokens avoided are estimated</summary><p class="muted small">{{ a.avoided_method }}</p></details>
    </section>

  </div>
  <p v-else class="muted">Loading…</p>
</template>

<style scoped>
.small { font-size: 12.5px; }
.kpis { grid-template-columns: repeat(auto-fit, minmax(200px, 1fr)); margin: 12px 0; }
.label { display: block; margin: 12px 0 8px; }
h3 { margin: 18px 0 4px; font-size: 15px; }
.refreshing { opacity: 0.6; transition: opacity 150ms; }
.read-share { margin-top: 16px; max-width: 560px; }
.method summary { cursor: pointer; color: var(--muted); font-size: 12.5px; margin-top: 12px; }
</style>
