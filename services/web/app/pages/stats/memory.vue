<script setup lang="ts">
import { useStatistics } from '~/composables/useStatistics'
const { days, s, error, pending } = useStatistics()
const m = computed(() => s.value?.memory)
const memRepos = computed(() => (m.value?.repos ?? []).map((r) => ({ label: r.repo, value: r.facts, detail: `${r.entities} entities · ${r.confirmations} confirmations · updated ${relTime(r.last_updated)}` })))
const memSources = computed(() => Object.entries(m.value?.sources ?? {}).map(([label, value]) => ({ label, value })).sort((x, y) => y.value - x.value))
</script>

<template>
  <p v-if="error" class="panel error">{{ apiErrorMessage(error) }}</p>
  <div v-else-if="s" :class="{ refreshing: pending }" class="stack">
    <section v-if="m" class="panel">
      <p class="eyebrow">Context offloading</p><h2>Agent memory</h2>
      <div class="grid kpis">
        <StatTile label="Facts stored" :value="fmtCompact(m.facts)" :hint="`${fmtInt(m.entities)} entities · ${fmtInt(m.confirmations)} confirmations`" />
        <StatTile label="Saves" :value="fmtCompact(m.saves)" :hint="`upsert_graph_triples, last ${days} days`" />
        <StatTile label="Recalls" :value="fmtCompact(m.recalls)" :hint="`${m.model.recalls_per_session} per session`" />
        <StatTile label="Tokens per recall" :value="fmtInt(Math.round(m.model.tokens_per_recall))" :hint="`${m.model.tokens_per_fact} tokens per fact`" />
      </div>
      <h3>Automatic</h3>
      <p class="muted small">What the Claude Code hooks did on their own, last {{ days }} days: looking up the chat's memory for each prompt, and handing older turns to a local model before a compaction.</p>
      <div class="grid kpis">
        <StatTile label="Prompts looked up" :value="fmtCompact(m.auto_recalls ?? 0)" :hint="`${fmtCompact(m.auto_recall_tokens ?? 0)} tokens of facts added`" />
        <StatTile label="Conversations offloaded" :value="fmtCompact(m.offloads ?? 0)" :hint="`${fmtCompact(m.offloaded_turns ?? 0)} turns`" />
        <StatTile label="Facts from offloading" :value="fmtCompact(m.offload_facts ?? 0)" hint="extracted by the local model" />
      </div>
      <p v-if="!(m.offloads ?? 0) && (m.auto_recalls ?? 0)" class="muted small">No offloading yet: it runs when a chat compacts. With the 150k limit that happens in long sessions.</p>
      <div v-if="m.facts" class="grid cols-2">
        <div><p class="label">Facts per repository</p><RankList :rows="memRepos" color="var(--muted)" /></div>
        <div><p class="label">Saved by</p><RankList :rows="memSources" color="var(--muted)" /></div>
      </div>
      <p class="muted small">What offloading could save in a long session, with these measured costs, is worked through on the <NuxtLink to="/agents">Agents page</NuxtLink> under “Keep each chat small”.</p>
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
</style>
