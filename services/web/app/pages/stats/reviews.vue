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
const entries = (o?: Record<string, number>) => Object.entries(o ?? {}).map(([label, value]) => ({ label: label.replace(/_/g, ' '), value })).sort((x, y) => y.value - x.value)
const n = (o: Record<string, number> | undefined, k: string) => o?.[k] ?? 0

const decisions = computed(() => [
  { label: 'Accepted', value: n(r.value?.by_status, 'accepted'), color: 'var(--success)' },
  { label: 'Open', value: n(r.value?.by_status, 'open'), color: 'var(--info)' },
  { label: 'Rejected', value: n(r.value?.by_status, 'rejected'), color: 'var(--s-other)' },
])
const decided = computed(() => n(r.value?.by_status, 'accepted') + n(r.value?.by_status, 'rejected'))
const acceptPct = computed(() => (decided.value ? Math.round((100 * n(r.value?.by_status, 'accepted')) / decided.value) : 0))
const fixTotal = computed(() => Object.values(r.value?.fixes ?? {}).reduce((t, x) => t + x, 0))
const perReview = computed(() => (r.value?.reviews ? (r.value.comments / r.value.reviews).toFixed(1) : '0'))

const providers = computed(() => (s.value?.ai ?? []).map((p) => ({ p, u: usage(p) })))
const allRequests = computed(() => providers.value.reduce((t, x) => t + x.u.requests, 0))
const share = (q: number) => (allRequests.value ? Math.round((100 * q) / allRequests.value) : 0)
</script>

<template>
  <p v-if="error" class="panel error">{{ apiErrorMessage(error) }}</p>
  <div v-else-if="s" :class="{ refreshing: pending }" class="stack">
    <section v-if="r" class="panel">
      <p class="eyebrow">Reviews</p><h2>{{ fmtInt(r.reviews) }} reviews, {{ fmtInt(r.comments) }} comments</h2>
      <p class="muted lead">Reviews written by agents through <span class="mono">submit_review</span>, by the AI review task and by people, and what became of their comments.</p>
      <div class="grid kpis">
        <StatTile label="Comments per review" :value="perReview" :hint="`${fmtInt(r.comments)} in all`" />
        <StatTile label="Accepted" :value="decided ? `${acceptPct}%` : '–'" :hint="`of ${fmtInt(decided)} decided comments`" />
        <StatTile label="Still open" :value="fmtInt(n(r.by_status, 'open'))" hint="comments waiting for a decision" />
        <StatTile label="Fix branches" :value="fmtInt(fixTotal)" hint="from accepted comments" />
      </div>
    </section>

    <div v-if="r" class="grid cols-2">
      <section class="panel">
        <p class="eyebrow">Decisions</p><h2>What happened to comments</h2>
        <DonutChart v-if="r.comments" :segments="decisions" title="Comment decisions" unit="comments" :center="decided ? `${acceptPct}%` : '–'" center-hint="accepted" :size="150" />
        <div v-else class="empty"><strong>No comments yet</strong>Comments appear when a review is submitted.</div>
      </section>
      <section class="panel">
        <p class="eyebrow">Verdicts</p><h2>How reviews ended</h2>
        <RankList :rows="entries(r.verdicts)" color="var(--s-1)" empty="No verdicts yet." />
        <p class="label">Who wrote them</p>
        <RankList :rows="entries(r.by_source)" color="var(--s-2)" empty="No reviews yet." />
      </section>
    </div>

    <div v-if="r" class="grid cols-2">
      <section class="panel">
        <p class="eyebrow">Comment kinds</p><h2>What reviewers flag</h2>
        <RankList :rows="entries(r.by_kind)" color="var(--s-4)" empty="No comments yet." />
      </section>
      <section class="panel">
        <p class="eyebrow">Fix branches</p><h2>Accepted comments turned into code</h2>
        <RankList :rows="entries(r.fixes)" color="var(--success)" empty="No fix branches yet." />
      </section>
    </div>

    <section class="panel">
      <p class="eyebrow">AI providers</p><h2>Model usage</h2>
      <p class="muted small">{{ ready ? `${seriesRangeLabel(range)}, every process${days > 30 ? ' (the metrics store keeps 30 days)' : ''}.` : 'Since the API started; live metrics are off.' }}</p>
      <div v-if="providers.length" class="table-wrap">
        <table>
          <thead><tr><th>Provider</th><th>Share of requests</th><th class="num">Requests</th><th class="num">Tokens in / out</th></tr></thead>
          <tbody>
            <tr v-for="{ p, u } in providers" :key="p.provider">
              <td>{{ p.provider }} <span v-if="p.local" class="badge badge--green">local</span><div class="muted small mono">{{ p.model }}</div></td>
              <td><span class="share"><span class="share__track"><span :style="{ width: `${share(u.requests)}%` }" /></span>{{ share(u.requests) }}%</span></td>
              <td class="num">{{ fmtInt(u.requests) }}<div v-if="u.failures" class="muted small">{{ u.failures }} failed</div></td>
              <td class="num">{{ fmtCompact(u.input) }} / {{ fmtCompact(u.output) }}</td>
            </tr>
          </tbody>
        </table>
      </div>
      <p v-else class="muted small">No AI providers configured.</p>
    </section>
  </div>
  <p v-else class="muted">Loading…</p>
</template>

<style scoped>
.small { font-size: 12.5px; }
.lead { max-width: 72ch; margin: 4px 0 0; }
.kpis { grid-template-columns: repeat(auto-fit, minmax(170px, 1fr)); margin: 16px 0 0; }
.label { display: block; margin: 14px 0 8px; }
.table-wrap { margin-top: 10px; overflow-x: auto; }
.share { display: inline-flex; align-items: center; gap: 8px; font-variant-numeric: tabular-nums; }
.share__track { width: 90px; height: 6px; border-radius: 999px; background: var(--line); overflow: hidden; display: inline-block; }
.share__track span { display: block; height: 100%; border-radius: 999px; background: var(--s-1); }
.refreshing { opacity: 0.6; transition: opacity 150ms; }
</style>
