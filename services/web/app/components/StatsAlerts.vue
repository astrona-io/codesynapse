<script setup lang="ts">
import type { StatsAlerts } from '~/utils/types'

// Alerts section of the platform statistics: what the internal vmalert
// evaluates. Only rule names, states, severity, value and times reach the
// page; other labels and annotations stay in vmalert.
const props = defineProps<{ tick: number }>()
const { data, error, refresh } = useApiGet<StatsAlerts>('admin/stats/alerts')
watch(() => props.tick, () => refresh())

const stateBadge: Record<string, string> = { firing: 'badge--pink', pending: 'badge--amber', inactive: 'badge--green' }
const when = (iso: string) => (!iso || iso.startsWith('0001-')) ? '–'
  : new Date(iso).toLocaleString([], { month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit', timeZone: 'UTC' }) + ' UTC'
const failing = computed(() => data.value?.rules.filter((r) => r.failing || r.health === 'err').length ?? 0)
</script>

<template>
  <div class="stack">
    <p v-if="error" class="notice notice--danger">{{ apiErrorMessage(error) }}</p>
    <template v-if="data">
      <p v-if="!data.configured" class="muted small">
        Alerts are off: set CODESYNAPSE_ALERTS_URL to the internal vmalert (docs/operations/alerting.md).
      </p>
      <p v-else-if="!data.reachable" class="notice notice--warn">vmalert did not answer. Alerts may still be evaluated and sent; this page cannot show them.</p>
      <template v-else>
        <div class="grid cols-3">
          <StatTile label="Firing" :value="data.firing" :tone="data.firing ? 'warning' : undefined" hint="sent to the receiver" />
          <StatTile label="Pending" :value="data.pending" hint="waiting out their for: duration" />
          <StatTile label="Rules" :value="data.rules.length" :hint="failing ? `${failing} failed to evaluate` : 'all evaluating'" :tone="failing ? 'warning' : undefined" />
        </div>
        <div class="table-wrap">
          <table>
            <thead><tr><th>Alert</th><th>State</th><th>Severity</th><th class="num">Value</th><th>Since</th></tr></thead>
            <tbody>
              <tr v-for="a in data.alerts" :key="`${a.name}/${a.active_at}`">
                <td class="mono small">{{ a.name }}</td>
                <td><span class="badge" :class="stateBadge[a.state]">{{ a.state }}</span></td>
                <td>{{ a.severity || '–' }}</td>
                <td class="num">{{ a.value || '–' }}</td>
                <td class="small">{{ when(a.active_at) }}</td>
              </tr>
              <tr v-if="!data.alerts.length"><td colspan="5" class="muted small">Nothing firing or pending.</td></tr>
            </tbody>
          </table>
        </div>
        <div class="table-wrap">
          <table>
            <thead><tr><th>Rule</th><th>Group</th><th>State</th><th>Severity</th><th>Evaluation</th><th>Last evaluated</th></tr></thead>
            <tbody>
              <tr v-for="r in data.rules" :key="`${r.group}/${r.name}`">
                <td class="mono small">{{ r.name }}</td>
                <td class="small muted">{{ r.group }}</td>
                <td><span class="badge" :class="stateBadge[r.state]">{{ r.state || '–' }}</span></td>
                <td>{{ r.severity || '–' }}</td>
                <td><span v-if="r.failing || r.health === 'err'" class="badge badge--amber">failing</span><span v-else class="small">{{ r.health || '–' }}</span></td>
                <td class="small">{{ when(r.last_evaluation) }}</td>
              </tr>
            </tbody>
          </table>
        </div>
      </template>
    </template>
  </div>
</template>
