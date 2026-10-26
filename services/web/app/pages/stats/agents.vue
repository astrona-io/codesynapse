<script setup lang="ts">
import { useStatistics } from '~/composables/useStatistics'
const { days, s, error, pending } = useStatistics()

const a = computed(() => s.value?.agents)
const tokens = computed(() => a.value?.tokens ?? { input: 0, output: 0, cached_input: 0, turns: 0, total: 0, input_pct: 0, output_pct: 0 })
const edits = computed(() => a.value?.edits ?? { edits: 0, writes: 0, failed: 0, baseline_bytes: 0, estimated_output_saved: 0, avoided_pct: 0 })
const contextTotal = computed(() => a.value?.context_total_tokens ?? ((a.value?.response_tokens ?? 0) + (a.value?.avoided_tokens ?? 0)))
const contextSavedPct = computed(() => a.value?.context_saved_pct ?? (contextTotal.value ? Math.round(100 * (a.value?.avoided_tokens ?? 0) / contextTotal.value) : 0))
const errorRate = computed(() => (a.value?.calls ? `${((a.value.errors / a.value.calls) * 100).toFixed(1)}%` : '–'))
const callsPerDay = computed(() => (a.value?.daily ?? []).map((d) => ({ label: d.day, value: d.calls, detail: `${d.sessions} session(s), ${d.errors} error(s)` })))
const avoidedPerDay = computed(() => (a.value?.daily ?? []).map((d) => ({ label: d.day, value: d.avoided_tokens, detail: `${fmtInt(d.response_tokens)} served` })))
// Share of the line ranges each tool's answers gave that the agent then
// read (Claude Code plugin hooks), most read first.
const readShare = computed(() => (a.value?.tools ?? []).filter((t) => t.ranges_returned > 0)
  .map((t) => ({ label: t.tool, value: Math.round((100 * Math.min(t.ranges_read, t.ranges_returned)) / t.ranges_returned), detail: `${fmtInt(t.ranges_read)} of ${fmtInt(t.ranges_returned)} line ranges read` }))
  .sort((x, y) => y.value - x.value))
// Shell commands a CodeSynapse tool would have answered, most frequent first.
const bashRows = computed(() => Object.entries(a.value?.workflow.bash ?? {})
  .map(([label, value]) => ({ label: label.replace('_', ' '), value, detail: `${fmtInt(value)} command(s)` }))
  .sort((x, y) => y.value - x.value))
const tools = computed(() => (a.value?.tools ?? []).map((t) => ({ label: t.tool, value: t.calls, detail: `${t.kind} · ${fmtInt(Math.round(t.avg_tokens))} tokens/call · ${t.avg_ms} ms avg${t.errors ? ` · ${t.errors} errors` : ''}` })))
const clients = computed(() => (a.value?.clients ?? []).map((c) => ({ label: c.client, value: c.calls, detail: `${c.sessions} session(s)` })))
// Local models: CodeSynapse's own AI work (reviews, triage, memory extraction)
// that never left this network.
const modelRange = computed(() => seriesRangeFor(days.value))
const { ready: modelsReady, usageOf } = useModelUsage(modelRange)
const local = computed(() => {
  const ai = s.value?.ai ?? []
  if (!ai.some(p => p.local)) return null
  const sum = (ps: typeof ai) => ps.reduce((t, p) => {
    const u = usageOf(p.provider) ?? { requests: p.usage.requests, failures: p.usage.failures, input: p.usage.input_tokens, output: p.usage.output_tokens }
    return { requests: t.requests + u.requests, failures: t.failures + u.failures, tokens: t.tokens + u.input + u.output }
  }, { requests: 0, failures: 0, tokens: 0 })
  const mine = sum(ai.filter(p => p.local))
  const all = sum(ai)
  return { ...mine, models: ai.filter(p => p.local).map(p => p.model || p.provider), sharePct: all.requests ? Math.round((100 * mine.requests) / all.requests) : null }
})
const repoUse = computed(() => (a.value?.repos ?? []).map((r) => ({ label: r.repo || '(no repo argument)', value: r.calls, detail: `${fmtCompact(r.response_tokens)} served · ~${fmtCompact(r.avoided_tokens)} avoided` })))
</script>

<template>
  <p v-if="error" class="panel error">{{ apiErrorMessage(error) }}</p>
  <div v-else-if="s" :class="{ refreshing: pending }" class="stack">
    <section v-if="local" class="panel">
      <p class="eyebrow">Local models</p><h2>AI work that stayed on your network</h2>
      <p class="muted small">Reviews, security triage, briefings and memory extraction CodeSynapse ran on {{ local.models.join(', ') }}. {{ modelsReady ? `Last ${modelRange === '7d' ? 7 : 30} days, every process.` : 'Since the API started; live metrics are off.' }}</p>
      <div class="grid kpis">
        <StatTile label="Local requests" :value="fmtCompact(local.requests)" :hint="local.failures ? `${local.failures} failed` : 'no failures'" />
        <StatTile label="Tokens processed locally" :value="fmtCompact(local.tokens)" hint="input + output, no cloud bill" />
        <StatTile label="Share of AI requests" :value="local.sharePct === null ? '–' : `${local.sharePct}%`" hint="local ÷ all AI providers" />
      </div>
    </section>
    <section v-if="a" class="panel">
      <p class="eyebrow">Agents &amp; context</p><h2>MCP sessions and tool calls</h2>
      <p class="muted small">Counted by the MCP server per day, repository, tool and client. Arguments and results are never stored, only their size.</p>
      <div class="grid kpis">
        <StatTile label="Sessions" :value="fmtCompact(a.sessions)" :hint="`${a.clients.length} client(s)`" />
        <StatTile label="Tool calls" :value="fmtCompact(a.calls)" :hint="`${errorRate} errors · ${a.avg_latency_ms} ms avg`" />
        <StatTile label="Tokens served" :value="fmtCompact(a.response_tokens)" :hint="a.calls ? `${fmtInt(Math.round(a.response_tokens / a.calls))} per call` : 'no calls yet'" />
        <StatTile label="Tokens avoided (est.)" :value="fmtCompact(a.avoided_tokens)" hint="vs reading the files instead" />
      </div>
      <div class="token-usage">
        <p class="label">Model tokens reported by hooks</p>
        <template v-if="tokens.total > 0 || tokens.turns > 0">
          <div class="grid kpis">
            <StatTile label="Input tokens" :value="fmtCompact(tokens.input)" :hint="`${tokens.input_pct}% of reported tokens · includes cached input`" />
            <StatTile label="Output tokens" :value="fmtCompact(tokens.output)" :hint="`${tokens.output_pct}% of reported tokens`" />
            <StatTile label="Total model tokens" :value="fmtCompact(tokens.total)" hint="reported input + output" />
            <StatTile label="Cached input tokens" :value="fmtCompact(tokens.cached_input)" hint="included in input; separate from context savings" />
          </div>
          <div class="token-split" role="img" :aria-label="`Input ${tokens.input_pct}%, output ${tokens.output_pct}%`">
            <span :style="{ width: `${tokens.input_pct}%` }" />
            <span :style="{ width: `${tokens.output_pct}%` }" />
          </div>
          <p class="muted small">Input {{ tokens.input_pct }}% · Output {{ tokens.output_pct }}%. Covers usage supplied by agent hooks; other clients and unreported turns are not included.</p>
        </template>
        <div v-else class="empty"><strong>Model usage unavailable</strong>The updated Claude Code Stop hook reports input and output usage from the local transcript. MCP tool responses alone cannot measure an agent’s model usage.</div>
        <p class="label">Context savings estimate</p>
        <div class="grid kpis">
          <StatTile label="Tokens saved (est.)" :value="fmtCompact(a.avoided_tokens)" hint="file-reading context avoided" />
          <StatTile label="Context total (est.)" :value="fmtCompact(contextTotal)" hint="MCP tokens served + estimated saved" />
          <StatTile label="Context saved" :value="contextTotal ? `${contextSavedPct}%` : '–'" hint="saved ÷ estimated context total" />
        </div>
        <p class="muted small">Savings compare graph answers with reading repository files. They estimate reduced context, not measured model output or billing savings.</p>
      </div>
      <div class="edit-usage">
        <p class="label">Small edits and output</p>
        <div v-if="edits.edits + edits.writes" class="grid kpis">
          <StatTile label="Targeted edits" :value="fmtCompact(edits.edits)" hint="Edit and patch attempts" />
          <StatTile label="Full file writes" :value="fmtCompact(edits.writes)" hint="new files and full replacements" />
          <StatTile label="Failed attempts" :value="fmtCompact(edits.failed)" hint="earn no estimated savings" />
          <StatTile label="Output avoided (est.)" :value="edits.baseline_bytes ? fmtCompact(edits.estimated_output_saved) : '–'" :hint="edits.baseline_bytes ? `${edits.avoided_pct}% versus eligible full-file rewrites` : 'no comparable file size reported'" />
        </div>
        <div v-else class="empty">The updated edit hooks report patch and full-write sizes without storing code.</div>
        <p class="muted small">For successful edits with a known file size, compare old + new text bytes with rewriting the file; divide avoided bytes by 4 to estimate tokens. Failed edits and patches without a comparable file earn no savings. This hypothetical estimate is separate from measured output and context savings.</p>
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
      <div class="workflow">
        <p class="label">Do agents use CodeSynapse first?</p>
        <template v-if="a.workflow.first_codesynapse + a.workflow.first_native">
          <div class="grid kpis">
            <StatTile label="Began with CodeSynapse" :value="`${a.workflow.first_share_pct}%`" :hint="`${fmtInt(a.workflow.first_codesynapse)} of ${fmtInt(a.workflow.first_codesynapse + a.workflow.first_native)} sessions that looked at code`" />
            <StatTile label="Began with Read or Grep" :value="fmtInt(a.workflow.first_native)" hint="sessions whose first lookup skipped CodeSynapse" />
            <StatTile label="Whole-file reads turned back" :value="fmtInt(a.workflow.reads_turned_back)" hint="large files the agent was sent to the outline of" />
          </div>
          <p class="muted small">Counted by the plugin's hooks as numbers only, never paths or prompts. A low first share means the ruleset is not reaching agents or its wording is not working: it is served from the API (<code>/api/v1/agent-rules</code>) and can be changed with <code>CODESYNAPSE_AGENT_RULES_FILE</code>, then watch this figure.</p>
        </template>
        <div v-else class="empty"><strong>Nothing reported yet</strong>Install the Claude Code plugin: its hooks put the ruleset in every session and report how sessions begin.</div>
      </div>
      <div class="workflow">
        <p class="label">Did CodeSynapse point to the code agents changed?</p>
        <template v-if="a.workflow.files_edited">
          <div class="grid kpis">
            <StatTile label="Edited files it pointed to" :value="`${a.workflow.edit_recall_pct}%`" :hint="`${fmtInt(a.workflow.files_edited_found)} of ${fmtInt(a.workflow.files_edited)} files sessions changed`" />
          </div>
          <p class="muted small">Of the files a session changed (code, Markdown and YAML that CodeSynapse indexes; not those already changed when it began), the share a CodeSynapse answer of the same session had named. The task eval measures this on old commits; this is the same question on your agents' work. Counted by the plugin's hooks as numbers only, never paths.</p>
        </template>
        <div v-else class="empty"><strong>Nothing reported yet</strong>The plugin reports this when a session that used CodeSynapse changes files.</div>
      </div>
      <div class="workflow">
        <p class="label">Shell commands a CodeSynapse tool answers with less output</p>
        <template v-if="a.workflow.bash_total">
          <RankList :rows="bashRows" empty="" />
          <p class="muted small">Commands agents ran in the shell (git status, diff, log, blame, text search, reading a file, a whole test run) where a CodeSynapse tool answers with less output. The plugin's Bash hook hints at the tool once per session and counts every one, classes only, never the commands.</p>
        </template>
        <div v-else class="empty"><strong>Nothing reported yet</strong>The plugin's Bash hook reports these when agents reach for the shell.</div>
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
.workflow { margin-top: 16px; }
.read-share { margin-top: 16px; max-width: 560px; }
.method summary { cursor: pointer; color: var(--muted); font-size: 12.5px; margin-top: 12px; }
.token-split { display: flex; height: 12px; border-radius: 999px; overflow: hidden; background: var(--panel-2); }
.token-split span:first-child { background: var(--s-1); }
.token-split span:last-child { background: var(--s-2); }
</style>
