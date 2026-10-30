<script setup lang="ts">
import type { AuditSection, AuditVerification, SeriesRange, StatsRange } from '~/utils/types'

// The audit tab of the platform statistics: entries by family and outcome,
// failed sign-ins by cause, security actions, and the chain status. The
// chain status is the stored result of the last verification (daily, or a
// platform admin's check); opening this tab never walks the chain.
const props = defineProps<{ range: StatsRange }>()
const { data, error, refresh } = useApiGet<AuditSection>(() => `admin/stats/audit?range=${props.range}`)

const live = computed(() => props.range !== '90d' && props.range !== '365d')
const seriesRange = computed<SeriesRange>(() => (live.value ? props.range as SeriesRange : '30d'))
const panels = ['audit_writes', 'audit_seals']
const { panels: charts, pending: chartsPending, refresh: refreshCharts } = useSeriesBatch(panels, seriesRange, undefined, { base: 'admin/stats/series/batch' })
const chartsOff = ref(false)
defineExpose({ refresh: () => { refresh(); refreshCharts() }, error, data })

const keys = [{ key: 'success', label: 'Succeeded', color: 'var(--s-1)' }, { key: 'failure', label: 'Refused or failed', color: 'var(--s-3)' }]
const when = (iso: string, step: 'hour' | 'day') => new Date(iso).toLocaleString([], step === 'hour'
  ? { month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit' }
  : { year: 'numeric', month: 'short', day: 'numeric', timeZone: 'UTC' })
const buckets = computed(() => (data.value?.buckets ?? []).map((b) => ({ label: when(b.at, data.value!.step), values: { success: b.success, failure: b.failure } })))
const familyBars = computed(() => (data.value?.families ?? []).map((f) => ({ label: f.name, value: f.success + f.failure, title: `${f.name}: ${f.success} succeeded, ${f.failure} refused or failed` }))
  .sort((a, b) => b.value - a.value))
const named = (rows: { name: string; n: number }[]) => rows.map((r) => ({ label: r.name.replaceAll('_', ' '), value: r.n }))
const chainState = (v: AuditVerification | null | undefined) => (!v ? 'Not verified yet' : v.run_error ? 'Could not finish' : v.ok ? 'Intact' : `Broken at entry ${v.broken_at}`)
const stamp = (iso: string | null | undefined) => (iso ? new Date(iso).toLocaleString() : '–')
</script>

<template>
  <div class="stack">
    <template v-if="data">
      <div class="grid cols-4">
        <StatTile label="Audit chain" :value="chainState(data.chain)" :hint="data.chain ? `checked ${stamp(data.chain.at)} · ${fmtCompact(data.chain.checked)} entries` : 'the first check runs within a day'"
          :tone="data.chain && data.chain.ok ? undefined : 'warning'" tip="The stored result of the last verification: daily on schedule, or when a platform admin checks the log. Opening this page does not walk the chain." />
        <StatTile label="Entries" :value="fmtCompact(buckets.reduce((t, b) => t + b.values.success + b.values.failure, 0))" :hint="`${fmtCompact(buckets.reduce((t, b) => t + b.values.failure, 0))} refused or failed`" />
        <StatTile label="Failed sign-ins" :value="fmtCompact(data.logins.reduce((t, l) => t + l.n, 0))" hint="by cause below" />
        <StatTile label="Archive" :value="data.archive.configured ? `${fmtCompact(data.archive.sealed)} sealed` : 'Not configured'"
          :hint="data.archive.configured ? `${data.archive.segments} files · last ${stamp(data.archive.last_seal)}` : `oldest entry ${stamp(data.archive.oldest_hot)}`" />
      </div>
      <StatColumns :buckets="buckets" :keys="keys" :title="data.step === 'hour' ? 'Entries per hour' : 'Entries per day (UTC)'" />
      <div class="grid cols-3">
        <div><p class="eyebrow">By family</p><BarList :items="familyBars" /></div>
        <div><p class="eyebrow">Failed sign-ins</p><BarList :items="named(data.logins)" /></div>
        <div><p class="eyebrow">Security actions</p><BarList :items="named(data.security)" /></div>
      </div>
      <div class="table-wrap">
        <table>
          <thead><tr><th>Verified</th><th>How</th><th>Result</th><th class="num">Entries</th><th class="num">Sealed files</th><th class="num">Took</th></tr></thead>
          <tbody>
            <tr v-for="v in data.verifications.slice(0, 10)" :key="v.id">
              <td>{{ stamp(v.at) }}</td>
              <td>{{ v.trigger === 'scheduled' ? 'Daily' : 'On demand' }}{{ v.deep ? ', with archive files' : '' }}</td>
              <td><span class="badge" :class="v.run_error ? 'badge--amber' : v.ok ? 'badge--green' : 'badge--pink'">{{ chainState(v) }}</span></td>
              <td class="num">{{ fmtInt(v.checked) }}</td>
              <td class="num">{{ v.segments ?? 0 }}</td>
              <td class="num">{{ v.duration_ms }} ms</td>
            </tr>
            <tr v-if="!data.verifications.length"><td colspan="6" class="muted small">No verification stored yet.</td></tr>
          </tbody>
        </table>
      </div>
    </template>
    <section v-if="!chartsOff">
      <p class="muted small">Audit writes and archive seals, from live statistics<template v-if="!live">, which keep 30 days: the charts show the last 30</template>.</p>
      <div class="grid cols-2">
        <SeriesCard v-for="p in panels" :key="p" :panel="p" :range="seriesRange" :tick="0" :batched="charts[p] ?? null" :batch-pending="chartsPending" @unavailable="chartsOff = true" />
      </div>
    </section>
  </div>
</template>
