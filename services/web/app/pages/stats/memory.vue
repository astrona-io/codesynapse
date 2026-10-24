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
      <div v-if="m.facts" class="grid cols-2">
        <div><p class="label">Facts per repository</p><RankList :rows="memRepos" color="var(--muted)" /></div>
        <div><p class="label">Saved by</p><RankList :rows="memSources" color="var(--muted)" /></div>
      </div>
      <h3>What offloading should save — theoretical</h3>
      <p class="muted small">A what-if calculator, nothing is saved or configured here. Adjust the session to match how you work. The recall cost comes from your agents once they have recalled facts.</p>
      <OffloadModel :tokens-per-recall="m.model.tokens_per_recall" :recalls-per-session="m.model.recalls_per_session" :tokens-per-fact="m.model.tokens_per_fact" :measured="m.model.measured" />
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
