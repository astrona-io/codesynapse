<script setup lang="ts">
import type { SecuritySection } from '~/utils/types'

// The security tab of the platform statistics: counts from each graph's
// latest snapshot, never a finding, path or repository name. The platform
// counts a graph once; an organisation counts each of its entries.
const props = defineProps<{ filter: string; page: number }>()
const emit = defineEmits<{ 'update:page': [number] }>()
const sort = ref('name')
const { data, error, refresh } = useApiGet<SecuritySection>(() => `admin/stats/security?${props.filter}&sort=${sort.value}&page=${props.page}`)
defineExpose({ refresh, error, data })

const grades = [['a', 'A'], ['b', 'B'], ['c', 'C'], ['d', 'D'], ['f', 'F'], ['na', 'Not graded'], ['none', 'No snapshot yet']] as const
const gradeBars = computed(() => grades.map(([k, label]) => ({ label, value: data.value?.totals.grades[k] ?? 0 })))
const ruleBars = computed(() => (data.value?.rules ?? []).map((r) => ({ label: r.name, value: r.findings })))
const vulns = (v: Record<string, number>) => Object.values(v).reduce((a, b) => a + b, 0)
const dayKeys = [
  { key: 'critical', label: 'Critical', color: 'var(--s-1)' }, { key: 'high', label: 'High', color: 'var(--s-2)' },
  { key: 'medium', label: 'Medium', color: 'var(--s-3)' }, { key: 'low', label: 'Low', color: 'var(--s-4)' },
]
const days = computed(() => (data.value?.days ?? []).map((d) => ({ label: d.day, values: d.findings })))
</script>

<template>
  <div class="stack">
    <div class="row sort">
      <select v-model="sort" class="inline" aria-label="Sort security">
        <option value="name">By name</option><option value="graphs">Most graphs</option><option value="critical">Most critical</option>
        <option value="high">Most high</option><option value="vulns">Most vulnerabilities</option><option value="worse">Most graded worse</option>
      </select>
    </div>
    <template v-if="data">
      <div class="grid cols-4">
        <StatTile label="Graphs" :value="fmtCompact(data.totals.graphs)" :hint="`${data.totals.grades.none ?? 0} without a snapshot yet`" />
        <StatTile label="Graded worse" :value="fmtCompact(data.totals.worse)" hint="latest ingest in this window"
          :tone="data.totals.worse ? 'warning' : undefined" tip="Graphs whose latest snapshot in the window has a worse security grade than the one before it on the same branch." />
        <StatTile label="Vulnerable dependencies" :value="fmtCompact(vulns(data.totals.vulns))" :hint="`${fmtCompact(data.totals.vulns_called)} called from code`" />
        <StatTile label="Suppressed findings" :value="fmtCompact(data.totals.suppressed.total ?? 0)"
          :hint="`${data.totals.suppressed.accepted_risk ?? 0} accepted risk · ${data.totals.suppressed.false_positive ?? 0} false positive · ${data.totals.suppressed.fixed_by ?? 0} fixed by`" />
      </div>
      <div class="grid cols-2">
        <div><p class="eyebrow">Open findings</p><SeverityStrip :counts="data.totals.findings" label="Open findings by severity" /></div>
        <div><p class="eyebrow">Vulnerable dependencies</p><SeverityStrip :counts="data.totals.vulns" label="Vulnerable dependencies by severity" :levels="['critical', 'high', 'medium', 'low']" /></div>
      </div>
      <div class="grid cols-2">
        <div><p class="eyebrow">Security grades</p><BarList :items="gradeBars" /></div>
        <div v-if="data.rules">
          <p class="eyebrow">Findings by rule</p>
          <BarList :items="ruleBars" />
          <p class="muted small">
            Catalogue rules only; a rule fewer than three organisations have findings of is other.
            <template v-if="data.rules_unknown">{{ data.rules_unknown }} graphs were indexed before rules were counted and are in no rule.</template>
          </p>
        </div>
      </div>
      <StatColumns v-if="data.days" :buckets="days" :keys="dayKeys" title="Open findings per day" />
      <div class="table-wrap">
        <table>
          <thead><tr><th>Organisation</th><th class="num">Graphs</th><th class="num">A or B</th><th class="num">D or F</th><th class="num"><SevBadge severity="critical" /></th><th class="num"><SevBadge severity="high" /></th><th class="num">Vulnerable</th><th class="num">Worse</th><th class="num">Suppressed</th></tr></thead>
          <tbody>
            <tr v-for="o in data.rows" :key="o.org_id">
              <td>{{ o.name }} <span class="muted mono small">{{ o.slug }}</span></td>
              <td class="num">{{ o.graphs }}</td>
              <td class="num">{{ (o.grades.a ?? 0) + (o.grades.b ?? 0) }}</td>
              <td class="num">{{ (o.grades.d ?? 0) + (o.grades.f ?? 0) }}</td>
              <td class="num">{{ o.findings.critical ?? 0 }}</td>
              <td class="num">{{ o.findings.high ?? 0 }}</td>
              <td class="num">{{ vulns(o.vulns) }}</td>
              <td class="num">{{ o.worse }}</td>
              <td class="num">{{ o.suppressed.total ?? 0 }}</td>
            </tr>
          </tbody>
        </table>
      </div>
      <div v-if="data.pages > 1" class="row pager">
        <button class="secondary tiny" :disabled="page <= 1" @click="emit('update:page', page - 1)">Previous</button>
        <span class="muted small">Page {{ data.page }} of {{ data.pages }} · {{ data.total }} organisations</span>
        <button class="secondary tiny" :disabled="page >= data.pages" @click="emit('update:page', page + 1)">Next</button>
      </div>
    </template>
  </div>
</template>

<style scoped>
.sort { justify-content: flex-end; }
.pager { gap: 10px; justify-content: flex-end; }
</style>
