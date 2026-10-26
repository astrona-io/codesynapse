<script setup lang="ts">
import { useStatistics } from '~/composables/useStatistics'
const { days, s, error, pending } = useStatistics()
const r = computed(() => s.value?.reviews)
const range = computed(() => seriesRangeFor(days.value))
const { ready, usageOf } = useModelUsage(range)
// From the metrics store when it is on (every process, this window), else the
// API process's own counters since it started.
const usage = (p: NonNullable<typeof s.value>['ai'][number]) => usageOf(p.provider)
  ?? { requests: p.usage.requests, failures: p.usage.failures, input: p.usage.input_tokens, output: p.usage.output_tokens }
const entries = (o?: Record<string, number>) => Object.entries(o ?? {}).map(([label, value]) => ({ label, value })).sort((x, y) => y.value - x.value)
</script>

<template>
  <p v-if="error" class="panel error">{{ apiErrorMessage(error) }}</p>
  <div v-else-if="s" :class="{ refreshing: pending }" class="stack">
    <div class="grid cols-2">
              <section v-if="r" class="panel">
                <p class="eyebrow">Reviews</p><h2>{{ r.reviews }} reviews, {{ r.comments }} comments</h2>
                <p class="label">Comment decisions</p><RankList :rows="entries(r.by_status)" color="var(--info)" />
                <p class="label">Source</p><RankList :rows="entries(r.by_source)" color="var(--muted)" />
                <p v-if="Object.keys(r.fixes).length" class="label">Fix branches</p><RankList v-if="Object.keys(r.fixes).length" :rows="entries(r.fixes)" color="var(--success)" />
              </section>
              <section class="panel">
                <p class="eyebrow">AI providers</p><h2>Model usage</h2>
                <p class="muted small">{{ ready ? `Last ${range === '7d' ? 7 : 30} days, every process${days > 30 ? ' (the metrics store keeps 30 days)' : ''}.` : 'Since the API started; live metrics are off.' }}</p>
                <table v-if="s.ai.length">
                  <thead><tr><th>Provider</th><th class="num">Requests</th><th class="num">Tokens in / out</th></tr></thead>
                  <tbody>
                    <tr v-for="p in s.ai" :key="p.provider">
                      <td>{{ p.provider }} <div class="muted small mono">{{ p.model }}{{ p.local ? ' · local' : '' }}</div></td>
                      <td class="num">{{ fmtInt(usage(p).requests) }}<div v-if="usage(p).failures" class="muted small">{{ usage(p).failures }} failed</div></td>
                      <td class="num">{{ fmtCompact(usage(p).input) }} / {{ fmtCompact(usage(p).output) }}</td>
                    </tr>
                  </tbody>
                </table>
                <p v-else class="muted small">No AI providers configured.</p>
              </section>
    </div>
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
